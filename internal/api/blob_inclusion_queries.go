package api

import (
	"database/sql"
	"time"
)

// blobInclusionSkippedLimit caps how many skipped blocks /blob/{txHash}/inclusion
// lists. A transaction stuck behind a nonce gap for the whole retention
// window would otherwise return every block of the week; the summary counts
// still describe the full history, and the list keeps the most recent
// blocks, which are the ones the timeline ends on.
const blobInclusionSkippedLimit = 500

// queryBlobInclusionSummary sizes a transaction's wait. It returns the block
// window the transaction sat pending through, how many of those blocks the
// indexer classified its pending pool against (only those can carry a
// candidate row for the transaction), and how many blocks did carry one.
//
// The window starts at the earlier of the first block whose timestamp is at
// or after first_seen_at and the first block that recorded the transaction
// as a candidate: a block processed just after the transaction arrived can
// classify it too_recent although its own timestamp predates first_seen_at,
// and the window must cover every row the skipped list returns. It ends at
// the block before inclusion, or at the newest indexed block while the
// transaction is still pending. When first_seen_at is known but no block has
// been produced since (and none recorded the transaction), the window is the
// empty span just past the newest block rather than NULL, so a client can
// tell "no block yet" from "never seen pending". The window's block count
// reads block_metrics, the table every indexed block has a row in, so a
// block whose builder row is missing still counts as waited through.
//
// Plans: the candidate aggregates are one probe on
// idx_blob_inclusion_candidates_chain_tx_block; the first block after a
// timestamp is an ordered probe on idx_block_builders_chain_timestamp; the
// newest block and the window counts are ranges on the block_metrics and
// block_builders primary keys. Every subquery is bounded by the wait, never
// by the chain's history.
//
// Args: $1 chain id, $2 tx hash, $3 first_seen_at (NULL when unknown; the
// timestamp bound then contributes nothing), $4 the including block number
// (NULL while pending).
var queryBlobInclusionSummary = `
	WITH candidates AS (
		SELECT
			COUNT(*)::bigint AS skipped_blocks,
			COUNT(*) FILTER (WHERE reason = 'eligible')::bigint AS eligible_skipped_blocks,
			MIN(block_number) AS first_candidate_block
		FROM blob_inclusion_candidates
		WHERE chain_id = $1 AND tx_hash = $2
		  AND ($4::bigint IS NULL OR block_number < $4::bigint)
	),
	bounds AS (
		SELECT
			c.skipped_blocks,
			c.eligible_skipped_blocks,
			LEAST(
				(SELECT block_number FROM block_builders
				 WHERE chain_id = $1 AND block_timestamp >= $3::timestamp
				 ORDER BY block_timestamp ASC
				 LIMIT 1),
				c.first_candidate_block
			) AS seen_from_block,
			COALESCE(
				$4::bigint - 1,
				(SELECT MAX(block_number) FROM block_metrics WHERE chain_id = $1)
			) AS to_block
		FROM candidates c
	),
	span AS (
		SELECT
			b.skipped_blocks,
			b.eligible_skipped_blocks,
			COALESCE(
				b.seen_from_block,
				CASE WHEN $3::timestamp IS NOT NULL THEN b.to_block + 1 END
			) AS from_block,
			b.to_block
		FROM bounds b
	)
	SELECT
		s.from_block,
		s.to_block,
		(SELECT COUNT(*) FROM block_metrics bm
		 WHERE bm.chain_id = $1
		   AND bm.block_number BETWEEN s.from_block AND s.to_block)::bigint AS window_blocks,
		(SELECT COUNT(*) FROM block_builders bb
		 WHERE bb.chain_id = $1
		   AND bb.block_number BETWEEN s.from_block AND s.to_block
		   AND bb.candidate_snapshot)::bigint AS window_snapshot_blocks,
		s.skipped_blocks,
		s.eligible_skipped_blocks
	FROM span s
`

// blobInclusionSummaryRow is the queryBlobInclusionSummary result. from_block
// and to_block are NULL when nothing bounds the window (the transaction was
// never seen pending and has no candidate rows, or the chain has no indexed
// block yet); the counts are then zero.
type blobInclusionSummaryRow struct {
	FromBlock             sql.NullInt64 `db:"from_block"`
	ToBlock               sql.NullInt64 `db:"to_block"`
	WindowBlocks          int64         `db:"window_blocks"`
	WindowSnapshotBlocks  int64         `db:"window_snapshot_blocks"`
	SkippedBlocks         int64         `db:"skipped_blocks"`
	EligibleSkippedBlocks int64         `db:"eligible_skipped_blocks"`
}

// blobInclusionBlockColumns projects one timeline block from its builder row
// (bb) and metrics row (bm), both outer-joined: a candidate row is written in
// the same transaction as both, but the builder relabel pass and reorg
// cleanup can leave a block momentarily without one, and a timeline entry
// with the reason alone still beats a hole.
const blobInclusionBlockColumns = `
		bb.fee_recipient,
		bb.extra_data,
		bb.builder_key,
		bb.builder_name,
		bb.tx_count,
		bb.proposer_payment_wei::text AS proposer_payment_wei,
		bb.proposer_payment_to,
		COALESCE(bb.candidate_snapshot, FALSE) AS candidate_snapshot,
		bb.pending_candidate_txs,
		bb.eligible_skipped_txs,
		bb.eligible_skipped_blobs,
		bb.eligible_skipped_max_tip::text AS eligible_skipped_max_tip,
		bm.blob_count,
		bm.blob_gas_limit,
		bm.blob_params_max,
		bm.blob_base_fee::text AS blob_base_fee
`

// queryBlobInclusionBlocks reads every block on a transaction's timeline in
// one round trip: the blocks that classified it as a candidate and did not
// include it (newest first, capped), each with its builder and its blob
// occupancy and base fee (the inputs behind a priced_out or no_room reason),
// plus — when the transaction is confirmed — the including block itself,
// flagged by `included`. The including block has no candidate row, so it
// comes from its own arm keyed on the block number alone; a pending
// transaction (NULL $3) contributes no such row.
//
// Args: $1 chain id, $2 tx hash, $3 the including block number (NULL while
// pending), $4 skipped-row limit.
var queryBlobInclusionBlocks = `
	SELECT * FROM (
		SELECT
			c.block_number,
			c.block_timestamp,
			c.reason,
			FALSE AS included,
			` + blobInclusionBlockColumns + `
		FROM blob_inclusion_candidates c
		LEFT JOIN block_builders bb
			ON bb.chain_id = c.chain_id AND bb.block_number = c.block_number
		LEFT JOIN block_metrics bm
			ON bm.chain_id = c.chain_id AND bm.block_number = c.block_number
		WHERE c.chain_id = $1
			AND c.tx_hash = $2
			AND ($3::bigint IS NULL OR c.block_number < $3::bigint)
		ORDER BY c.block_number DESC
		LIMIT $4
	) skipped
	UNION ALL
	SELECT
		i.block_number,
		COALESCE(bm.block_timestamp, bb.block_timestamp) AS block_timestamp,
		'' AS reason,
		TRUE AS included,
		` + blobInclusionBlockColumns + `
	FROM (SELECT $3::bigint AS block_number) i
	LEFT JOIN block_builders bb
		ON bb.chain_id = $1 AND bb.block_number = i.block_number
	LEFT JOIN block_metrics bm
		ON bm.chain_id = $1 AND bm.block_number = i.block_number
	WHERE i.block_number IS NOT NULL
`

// blobInclusionBlockRow is one queryBlobInclusionBlocks result. The builder
// columns are all NULL together when the block has no block_builders row;
// the metrics columns likewise when it has no block_metrics row. Only the
// included row can have a NULL timestamp (both rows missing); the handler
// already knows that block's timestamp from the blob row.
type blobInclusionBlockRow struct {
	BlockNumber           int64          `db:"block_number"`
	BlockTimestamp        sql.NullTime   `db:"block_timestamp"`
	Reason                string         `db:"reason"`
	Included              bool           `db:"included"`
	FeeRecipient          sql.NullString `db:"fee_recipient"`
	ExtraData             sql.NullString `db:"extra_data"`
	BuilderKey            sql.NullString `db:"builder_key"`
	BuilderName           sql.NullString `db:"builder_name"`
	TxCount               *int           `db:"tx_count"`
	ProposerPaymentWei    *string        `db:"proposer_payment_wei"`
	ProposerPaymentTo     *string        `db:"proposer_payment_to"`
	CandidateSnapshot     bool           `db:"candidate_snapshot"`
	PendingCandidateTxs   *int           `db:"pending_candidate_txs"`
	EligibleSkippedTxs    *int           `db:"eligible_skipped_txs"`
	EligibleSkippedBlobs  *int           `db:"eligible_skipped_blobs"`
	EligibleSkippedMaxTip *string        `db:"eligible_skipped_max_tip"`
	BlobCount             *int           `db:"blob_count"`
	BlobGasLimit          *int64         `db:"blob_gas_limit"`
	BlobParamsMax         *int           `db:"blob_params_max"`
	BlobBaseFee           *string        `db:"blob_base_fee"`
}

// timestamp returns the row's block timestamp, or the fallback when the
// row carried none.
func (r blobInclusionBlockRow) timestamp(fallback time.Time) time.Time {
	if r.BlockTimestamp.Valid {
		return r.BlockTimestamp.Time
	}
	return fallback
}
