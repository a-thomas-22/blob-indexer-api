package db

import (
	"context"
	"fmt"

	"github.com/lib/pq"

	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
)

// builderBackfillTimestampLayout renders a block timestamp for the text[]
// argument the insert unnests. block_builders.block_timestamp is TIMESTAMP
// (no zone), so the value is formatted in UTC without an offset and cast
// back on the server; passing a []time.Time through pq.Array would depend on
// the generic-array fallback's element formatting instead.
const builderBackfillTimestampLayout = "2006-01-02 15:04:05.999999"

// BlobTxIndexUpdate carries one blob transaction's position in the block that
// included it. Every blob row of that transaction receives the same index.
type BlobTxIndexUpdate struct {
	BlockNumber int64
	TxHash      string
	TxIndex     int
}

// BlocksMissingBlockBuilders lists, in ascending order, the blocks in the
// closed range [fromBlock, toBlock] that this network has indexed but for
// which no block_builders row exists. Blocks indexed before migration 000017
// have none; so does any block whose builder write was lost. The anti-join
// runs on both tables' (chain_id, block_number) primary keys, so the cost is
// bounded by the range width rather than by history.
func (db *DB) BlocksMissingBlockBuilders(ctx context.Context, networkID int, fromBlock, toBlock int64) ([]int64, error) {
	if toBlock < fromBlock {
		return nil, fmt.Errorf("builder backfill window for network %d has inverted bounds [%d, %d]", networkID, fromBlock, toBlock)
	}
	var blocks []int64
	if err := db.SelectContext(ctx, &blocks, blocksMissingBlockBuilders, networkID, fromBlock, toBlock); err != nil {
		return nil, fmt.Errorf("failed to list blocks missing block builders for network %d [%d, %d]: %w",
			networkID, fromBlock, toBlock, err)
	}
	return blocks, nil
}

// InsertBackfilledBlockBuilders writes one batch of historical builder rows
// and, in the same transaction, the blob transaction positions read from the
// very same fetched blocks.
//
// The insert is ON CONFLICT DO NOTHING on purpose: live indexing may have
// written the block's builder row (with a candidate snapshot the backfill
// cannot reconstruct) between the listing and this write, and that row must
// win. Backfilled rows therefore always carry candidate_snapshot = FALSE and
// NULL aggregates, which is what "the pending pool was never observed for
// this block" means.
//
// The tx_index update touches only that one column and only rows that do not
// have it yet, so the guarded UPDATE trigger functions from migration 000016
// treat it as a no-op for every aggregate, and a reorg replay landing between
// the fetch and this write keeps its canonical value. first_seen_at is never
// written here: it records an observation that a historical fetch cannot make.
//
// Returns the number of builder rows inserted and blob rows given an index.
func (db *DB) InsertBackfilledBlockBuilders(ctx context.Context, networkID int, rows []models.BlockBuilder, txIndexes []BlobTxIndexUpdate) (inserted, indexed int64, err error) {
	if len(rows) == 0 && len(txIndexes) == 0 {
		return 0, 0, nil
	}

	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to begin builder backfill transaction for network %d: %w", networkID, err)
	}
	defer func() { _ = tx.Rollback() }()

	if len(rows) > 0 {
		numbers := make([]int64, 0, len(rows))
		timestamps := make([]string, 0, len(rows))
		feeRecipients := make([]string, 0, len(rows))
		extraData := make([]string, 0, len(rows))
		keys := make([]string, 0, len(rows))
		names := make([]string, 0, len(rows))
		txCounts := make([]int64, 0, len(rows))
		payments := make([]string, 0, len(rows))
		paymentTargets := make([]string, 0, len(rows))
		for idx := range rows {
			row := &rows[idx]
			numbers = append(numbers, row.BlockNumber)
			timestamps = append(timestamps, row.BlockTimestamp.UTC().Format(builderBackfillTimestampLayout))
			feeRecipients = append(feeRecipients, row.FeeRecipient)
			extraData = append(extraData, row.ExtraData)
			keys = append(keys, row.BuilderKey)
			names = append(names, row.BuilderName)
			txCounts = append(txCounts, int64(row.TxCount))
			payments = append(payments, derefOrEmpty(row.ProposerPaymentWei))
			paymentTargets = append(paymentTargets, derefOrEmpty(row.ProposerPaymentTo))
		}

		result, execErr := tx.ExecContext(ctx, insertBackfilledBlockBuilders, networkID,
			pq.Array(numbers), pq.Array(timestamps), pq.Array(feeRecipients), pq.Array(extraData),
			pq.Array(keys), pq.Array(names), pq.Array(txCounts), pq.Array(payments), pq.Array(paymentTargets))
		if execErr != nil {
			return 0, 0, fmt.Errorf("failed to insert %d backfilled block builder rows for network %d: %w",
				len(rows), networkID, execErr)
		}
		if inserted, err = result.RowsAffected(); err != nil {
			return 0, 0, fmt.Errorf("failed to read backfilled block builder insert count for network %d: %w", networkID, err)
		}
	}

	if len(txIndexes) > 0 {
		blocks := make([]int64, 0, len(txIndexes))
		hashes := make([]string, 0, len(txIndexes))
		positions := make([]int64, 0, len(txIndexes))
		for _, update := range txIndexes {
			blocks = append(blocks, update.BlockNumber)
			hashes = append(hashes, update.TxHash)
			positions = append(positions, int64(update.TxIndex))
		}

		result, execErr := tx.ExecContext(ctx, updateBlobTxIndexes, networkID,
			pq.Array(blocks), pq.Array(hashes), pq.Array(positions))
		if execErr != nil {
			return 0, 0, fmt.Errorf("failed to set blob tx indexes for network %d (%d transactions): %w",
				networkID, len(txIndexes), execErr)
		}
		if indexed, err = result.RowsAffected(); err != nil {
			return 0, 0, fmt.Errorf("failed to read blob tx index update count for network %d: %w", networkID, err)
		}
	}

	if err = tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("failed to commit builder backfill batch for network %d: %w", networkID, err)
	}
	return inserted, indexed, nil
}

// derefOrEmpty renders a nullable NUMERIC/TEXT value for the text[] arguments
// above; the statements turn the empty string back into NULL.
func derefOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

const blocksMissingBlockBuilders = `
	SELECT ib.block_number
	FROM indexed_blocks ib
	LEFT JOIN block_builders bb
		ON bb.chain_id = ib.chain_id
		AND bb.block_number = ib.block_number
	WHERE ib.chain_id = $1
		AND ib.block_number >= $2
		AND ib.block_number <= $3
		AND bb.block_number IS NULL
	ORDER BY ib.block_number ASC
`

const insertBackfilledBlockBuilders = `
	INSERT INTO block_builders (
		chain_id, block_number, block_timestamp, fee_recipient, extra_data,
		builder_key, builder_name, tx_count, proposer_payment_wei, proposer_payment_to,
		candidate_snapshot, pending_candidate_txs, eligible_skipped_txs,
		eligible_skipped_blobs, eligible_skipped_max_tip
	)
	SELECT $1, u.block_number, u.block_timestamp::timestamp, u.fee_recipient, u.extra_data,
		u.builder_key, u.builder_name, u.tx_count::int,
		NULLIF(u.proposer_payment_wei, '')::numeric, NULLIF(u.proposer_payment_to, ''),
		FALSE, NULL, NULL, NULL, NULL
	FROM unnest($2::bigint[], $3::text[], $4::text[], $5::text[], $6::text[], $7::text[],
		$8::bigint[], $9::text[], $10::text[])
		AS u(block_number, block_timestamp, fee_recipient, extra_data, builder_key,
			builder_name, tx_count, proposer_payment_wei, proposer_payment_to)
	ON CONFLICT (chain_id, block_number) DO NOTHING
`

const updateBlobTxIndexes = `
	UPDATE blobs b
	SET tx_index = u.tx_index::int
	FROM unnest($2::bigint[], $3::text[], $4::bigint[])
		AS u(block_number, tx_hash, tx_index)
	WHERE b.chain_id = $1
		AND b.block_number = u.block_number
		AND b.tx_hash = u.tx_hash
		AND b.tx_index IS NULL
`
