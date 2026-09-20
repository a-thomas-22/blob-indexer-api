package indexer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/builders"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

const (
	// defaultCandidateSnapshotMaxLag bounds how old a block may be for its
	// pending-pool snapshot to mean anything. A block indexed minutes (or
	// months) after it was produced is compared against a pool that moved
	// on long ago, so the snapshot is skipped rather than fabricated.
	defaultCandidateSnapshotMaxLag = 60 * time.Second
	// defaultCandidateMinAge is how long a pending transaction must have
	// been visible to this node before a block's builder is held to have
	// had a chance to include it.
	defaultCandidateMinAge = 6 * time.Second
	// defaultCandidateRetention bounds how long the per-transaction
	// candidate detail is kept; the per-block aggregates on block_builders
	// are permanent.
	defaultCandidateRetention = 7 * 24 * time.Hour
	// defaultCandidatePruneInterval is how often the candidate detail prune
	// runs when the mempool cleanup interval, which normally drives the
	// maintenance loop, is not configured.
	defaultCandidatePruneInterval = 5 * time.Minute
	// minCandidateLivenessWindow floors the window within which the node
	// must have re-reported a pending transaction for it to count as a
	// candidate. It matters only when the liveness refresh is configured
	// faster than this; below it, ordinary jitter would drop live rows.
	minCandidateLivenessWindow = 30 * time.Second
	// candidateInsertColumns is the number of columns written per
	// blob_inclusion_candidates row.
	candidateInsertColumns = 13
	// candidateInsertChunk bounds how many candidate rows one INSERT
	// carries, keeping the statement well under the protocol's parameter
	// limit however large the pending pool grows.
	candidateInsertChunk = 400
)

// durationOrDefault applies a package default to an unset (zero) duration.
// A negative value is kept: it is how a deployment turns the feature off.
func durationOrDefault(value, fallback time.Duration) time.Duration {
	if value == 0 {
		return fallback
	}
	return value
}

// blockBuilderRow builds the block_builders row for a freshly fetched block
// from its header and transactions. The candidate snapshot fields are left
// zero here; insertBlockData fills them when it takes a snapshot.
func (i *Indexer) blockBuilderRow(block *types.Block, timestamp time.Time) *models.BlockBuilder {
	header := block.Header()
	feeRecipient := header.Coinbase.Hex()
	resolved := builders.Resolve(header.Extra, feeRecipient)

	row := &models.BlockBuilder{
		ChainID:        i.network.ChainID,
		BlockNumber:    header.Number.Int64(),
		BlockTimestamp: timestamp,
		FeeRecipient:   feeRecipient,
		ExtraData:      hexEncodeExtraData(header.Extra),
		BuilderKey:     resolved.Key,
		BuilderName:    resolved.Name,
		TxCount:        len(block.Transactions()),
	}
	row.ProposerPaymentWei, row.ProposerPaymentTo = proposerPayment(block.Transactions(), feeRecipient, i.getSender)
	return row
}

// hexEncodeExtraData renders header.Extra the way block_builders stores it:
// 0x-prefixed hex, "0x" when empty.
func hexEncodeExtraData(extra []byte) string {
	return hexutil.Encode(extra)
}

// equalAddress compares two hex addresses without caring about checksum
// casing; header fields and recovered senders are both checksummed today,
// but nothing guarantees a caller passes them that way.
func equalAddress(a, b string) bool {
	return a != "" && strings.EqualFold(a, b)
}

// proposerPayment applies the MEV-Boost payment heuristic: under a relay the
// builder sets the coinbase to its own address and pays the proposer with
// the block's last transaction. When that last transaction is sent by the
// fee recipient, its value and recipient are the proposer payment; otherwise
// there is none to record (a locally built block pays the proposer through
// the coinbase itself).
//
// senderOf resolves a transaction's sender; a failure is treated as "not the
// fee recipient", since a payment cannot be attributed to an unknown sender.
func proposerPayment(txs types.Transactions, feeRecipient string, senderOf func(*types.Transaction) (string, error)) (wei, to *string) {
	if len(txs) == 0 || senderOf == nil {
		return nil, nil
	}
	last := txs[len(txs)-1]
	sender, err := senderOf(last)
	if err != nil || !equalAddress(sender, feeRecipient) {
		return nil, nil
	}
	value := last.Value()
	if value == nil {
		value = new(big.Int)
	}
	amount := value.String()
	wei = &amount
	if recipient := last.To(); recipient != nil {
		hex := recipient.Hex()
		to = &hex
	}
	return wei, to
}

// candidateTx is one pending blob transaction, collapsed from its per-blob
// mempool_blobs rows, as the classifier sees it.
type candidateTx struct {
	TxHash               string    `db:"tx_hash"`
	FromAddress          string    `db:"from_address"`
	UserAttribution      string    `db:"user_attribution"`
	Nonce                *int64    `db:"nonce"`
	BlobCount            int       `db:"blob_count"`
	MaxPriorityFeePerGas *string   `db:"max_priority_fee_per_gas"`
	MaxFeePerGas         *string   `db:"max_fee_per_gas"`
	MaxFeePerBlobGas     *string   `db:"max_fee_per_blob_gas"`
	FirstSeenAt          time.Time `db:"first_seen_at"`
}

// candidateBlockContext is everything about the block a pending transaction
// is classified against.
type candidateBlockContext struct {
	ChainID        int
	BlockNumber    int64
	BlockTimestamp time.Time
	// MinAge is the grace period below which a transaction is considered
	// too recent for the builder to have seen it.
	MinAge time.Duration
	// BlobBaseFee and BaseFee are the block's blob and execution base fees
	// in wei; nil means the comparison cannot be made and no transaction is
	// priced out on it.
	BlobBaseFee *big.Int
	BaseFee     *big.Int
	// RemainingBlobGas is the block's unused blob gas (limit minus used).
	RemainingBlobGas int64
}

// classifyCandidates labels every pending blob transaction a block did not
// include with the reason it was not included, in the precedence migration
// 000017 documents: too_recent, nonce_gap, priced_out_blob_fee,
// priced_out_exec_fee, no_room, eligible.
//
// Only 'eligible' rows say anything about the builder; the others could not
// have been included (or not included first) whatever the builder did.
// A missing fee cap never counts as priced out: it is an unobserved value,
// not a low one.
func classifyCandidates(txs []candidateTx, blockCtx candidateBlockContext) []models.BlobInclusionCandidate {
	if len(txs) == 0 {
		return nil
	}

	// A sender's lowest pending nonce is the only one that could go first;
	// every higher one sits behind a gap. Transactions whose nonce was
	// never recorded take part in neither side of the comparison.
	lowestNonce := make(map[string]int64, len(txs))
	for _, tx := range txs {
		if tx.Nonce == nil {
			continue
		}
		if current, ok := lowestNonce[tx.FromAddress]; !ok || *tx.Nonce < current {
			lowestNonce[tx.FromAddress] = *tx.Nonce
		}
	}

	ageCutoff := blockCtx.BlockTimestamp.Add(-blockCtx.MinAge)
	out := make([]models.BlobInclusionCandidate, 0, len(txs))
	for _, tx := range txs {
		out = append(out, models.BlobInclusionCandidate{
			ChainID:              blockCtx.ChainID,
			BlockNumber:          blockCtx.BlockNumber,
			BlockTimestamp:       blockCtx.BlockTimestamp,
			TxHash:               tx.TxHash,
			FromAddress:          tx.FromAddress,
			UserAttribution:      tx.UserAttribution,
			Nonce:                tx.Nonce,
			BlobCount:            tx.BlobCount,
			MaxPriorityFeePerGas: tx.MaxPriorityFeePerGas,
			MaxFeePerGas:         tx.MaxFeePerGas,
			MaxFeePerBlobGas:     tx.MaxFeePerBlobGas,
			FirstSeenAt:          tx.FirstSeenAt,
			Reason:               candidateReason(tx, blockCtx, ageCutoff, lowestNonce),
		})
	}
	return out
}

func candidateReason(tx candidateTx, blockCtx candidateBlockContext, ageCutoff time.Time, lowestNonce map[string]int64) string {
	switch {
	case tx.FirstSeenAt.After(ageCutoff):
		return models.CandidateTooRecent
	case tx.Nonce != nil && *tx.Nonce > lowestNonce[tx.FromAddress]:
		return models.CandidateNonceGap
	case feeBelow(tx.MaxFeePerBlobGas, blockCtx.BlobBaseFee):
		return models.CandidatePricedOutBlobFee
	case feeBelow(tx.MaxFeePerGas, blockCtx.BaseFee):
		return models.CandidatePricedOutExecFee
	case int64(tx.BlobCount)*int64(params.BlobTxBlobGasPerBlob) > blockCtx.RemainingBlobGas:
		return models.CandidateNoRoom
	default:
		return models.CandidateEligible
	}
}

// parseWei turns a stored NUMERIC-as-string fee into a big.Int, or nil when
// it is absent or unparsable. A nil fee disables the comparison that reads
// it rather than defaulting it to zero.
func parseWei(value string) *big.Int {
	if value == "" {
		return nil
	}
	parsed, ok := new(big.Int).SetString(value, 10)
	if !ok {
		return nil
	}
	return parsed
}

// feeBelow reports whether a transaction's fee cap is strictly below the
// block's base fee. An absent or unparsable cap, or an absent base fee, is
// never below: an unobserved value must not be read as a low one.
func feeBelow(feeCap *string, baseFee *big.Int) bool {
	if feeCap == nil || baseFee == nil {
		return false
	}
	value, ok := new(big.Int).SetString(*feeCap, 10)
	if !ok {
		return false
	}
	return value.Cmp(baseFee) < 0
}

// candidateAggregates summarizes a classification for the block_builders row,
// which outlives the per-transaction detail.
func candidateAggregates(candidates []models.BlobInclusionCandidate) (pending, skippedTxs, skippedBlobs int, maxTip *string) {
	pending = len(candidates)
	var highest *big.Int
	for idx := range candidates {
		if candidates[idx].Reason != models.CandidateEligible {
			continue
		}
		skippedTxs++
		skippedBlobs += candidates[idx].BlobCount
		tip := candidates[idx].MaxPriorityFeePerGas
		if tip == nil {
			continue
		}
		value, ok := new(big.Int).SetString(*tip, 10)
		if !ok {
			continue
		}
		if highest == nil || value.Cmp(highest) > 0 {
			highest = value
		}
	}
	if highest != nil {
		formatted := highest.String()
		maxTip = &formatted
	}
	return pending, skippedTxs, skippedBlobs, maxTip
}

// candidateSnapshotDue reports whether a block is recent enough for its
// pending-pool snapshot to describe the pool the builder actually faced.
// Historical catch-up and backfills index blocks long after the fact; those
// blocks get candidate_snapshot = false and NULL aggregates instead of a
// snapshot of an unrelated pool.
func (i *Indexer) candidateSnapshotDue(blockTimestamp time.Time) bool {
	if i.candidateSnapshotMaxLag <= 0 {
		return false
	}
	lag := time.Now().UTC().Sub(blockTimestamp)
	return lag <= i.candidateSnapshotMaxLag && lag >= -i.candidateSnapshotMaxLag
}

// candidateLivenessWindow is how recently the node must have re-reported a
// pending transaction for it to count as something the builder could have
// included. It is derived from the configured refresh cadence rather than
// being its own knob: two refreshes is the shortest window that survives one
// missed or slow one, floored so a fast cadence does not make the window
// shorter than ordinary jitter.
//
// last_seen is bumped by refreshPendingBlobLiveness, which runs on the
// mempool poll ticker in polling mode and on the slower reconcile ticker in
// websocket mode — and on both when the websocket fallback is active. The
// slower of the two is therefore the cadence a live row is guaranteed to be
// refreshed at, whichever mode this process ends up in.
func (i *Indexer) candidateLivenessWindow() time.Duration {
	refresh := i.mempoolPollingInterval
	if i.mempoolReconcileInterval > refresh {
		refresh = i.mempoolReconcileInterval
	}
	window := 2 * refresh
	if window < minCandidateLivenessWindow {
		window = minCandidateLivenessWindow
	}
	return window
}

// selectPendingCandidates reads the pending blob pool as one row per
// transaction, inside the block's own transaction and after the promotion
// deletes, so it holds exactly the transactions this block left behind.
//
// Only rows the node still reported within the liveness window count. A
// transaction the pool dropped — a privately delivered same-nonce
// cancellation confirmed, say — stops being refreshed but survives in
// mempool_blobs until the far longer TTL sweep reaches it; counting it
// would keep blaming builders for skipping a transaction they could no
// longer see. COALESCE covers legacy rows written before last_seen existed.
func (i *Indexer) selectPendingCandidates(tx *sqlx.Tx) ([]candidateTx, error) {
	var rows []candidateTx
	err := tx.SelectContext(i.ctx, &rows, `
		SELECT tx_hash,
			MIN(from_address) AS from_address,
			COALESCE(MIN(user_attribution), '') AS user_attribution,
			MIN(nonce) AS nonce,
			COUNT(*) AS blob_count,
			MIN(max_priority_fee_per_gas)::text AS max_priority_fee_per_gas,
			MIN(max_fee_per_gas)::text AS max_fee_per_gas,
			MIN(max_fee_per_blob_gas)::text AS max_fee_per_blob_gas,
			MIN(timestamp) AS first_seen_at
		FROM mempool_blobs
		WHERE chain_id = $1
			AND COALESCE(last_seen, timestamp) >= $2
		GROUP BY tx_hash
	`, i.network.ChainID, time.Now().UTC().Add(-i.candidateLivenessWindow()))
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// blockHasCandidateSnapshot reports whether a candidate snapshot has already
// been stored for this block, read inside the block's own transaction.
//
// The first snapshot for a block wins. The polling walker and the WebSocket
// follower can both enqueue the same live height, so an ordinary live block
// may be processed twice inside the snapshot lag; the pool has moved on by
// the second pass (transactions the block genuinely left pending may since
// have been promoted, and new ones arrived) and re-snapshotting would
// rewrite the aggregates against a pool that is not the one the builder
// faced, while leaving the first pass's candidate rows in place.
func (i *Indexer) blockHasCandidateSnapshot(tx *sqlx.Tx, blockNumber int64) (bool, error) {
	var taken bool
	err := tx.QueryRowContext(i.ctx, `
		SELECT COALESCE(
			(SELECT candidate_snapshot FROM block_builders WHERE chain_id = $1 AND block_number = $2),
			FALSE)
	`, i.network.ChainID, blockNumber).Scan(&taken)
	if err != nil {
		return false, err
	}
	return taken, nil
}

// snapshotBlockCandidates classifies the pending pool this block left behind,
// writes the per-transaction detail, and fills the snapshot columns on the
// builder row the caller is about to upsert.
func (i *Indexer) snapshotBlockCandidates(tx *sqlx.Tx, indexedBlock models.IndexedBlock, blockMetrics *models.BlockMetrics, builderRow *models.BlockBuilder) error {
	pending, err := i.selectPendingCandidates(tx)
	if err != nil {
		return fmt.Errorf("failed to read pending blob candidates (block: %d): %w", indexedBlock.BlockNumber, err)
	}
	candidates := classifyCandidates(pending, candidateBlockContext{
		ChainID:          indexedBlock.ChainID,
		BlockNumber:      indexedBlock.BlockNumber,
		BlockTimestamp:   blockMetrics.BlockTimestamp,
		MinAge:           i.candidateMinAge,
		BlobBaseFee:      parseWei(blockMetrics.BlobBaseFee),
		BaseFee:          parseWei(blockMetrics.BaseFeeWei),
		RemainingBlobGas: blockMetrics.BlobGasLimit - blockMetrics.BlobGasUsed,
	})
	if err := i.insertCandidates(tx, candidates); err != nil {
		return fmt.Errorf("failed to insert blob inclusion candidates (block: %d): %w", indexedBlock.BlockNumber, err)
	}

	pendingTxs, skippedTxs, skippedBlobs, maxTip := candidateAggregates(candidates)
	builderRow.CandidateSnapshot = true
	builderRow.PendingCandidateTxs = &pendingTxs
	builderRow.EligibleSkippedTxs = &skippedTxs
	builderRow.EligibleSkippedBlobs = &skippedBlobs
	builderRow.EligibleSkippedMaxTip = maxTip
	return nil
}

// insertCandidates writes the per-transaction candidate detail, chunked so
// one oversized pool cannot exceed the bind-parameter limit.
func (i *Indexer) insertCandidates(tx *sqlx.Tx, candidates []models.BlobInclusionCandidate) error {
	for start := 0; start < len(candidates); start += candidateInsertChunk {
		end := start + candidateInsertChunk
		if end > len(candidates) {
			end = len(candidates)
		}
		chunk := candidates[start:end]

		query := `
			INSERT INTO blob_inclusion_candidates (
				chain_id, block_number, block_timestamp, tx_hash, from_address,
				user_attribution, nonce, blob_count, max_priority_fee_per_gas,
				max_fee_per_gas, max_fee_per_blob_gas, first_seen_at, reason
			) VALUES ` + valuesPlaceholders(len(chunk), candidateInsertColumns, nil) + `
			ON CONFLICT (chain_id, block_number, tx_hash) DO UPDATE SET
				block_timestamp = EXCLUDED.block_timestamp,
				from_address = EXCLUDED.from_address,
				user_attribution = EXCLUDED.user_attribution,
				nonce = EXCLUDED.nonce,
				blob_count = EXCLUDED.blob_count,
				max_priority_fee_per_gas = EXCLUDED.max_priority_fee_per_gas,
				max_fee_per_gas = EXCLUDED.max_fee_per_gas,
				max_fee_per_blob_gas = EXCLUDED.max_fee_per_blob_gas,
				first_seen_at = EXCLUDED.first_seen_at,
				reason = EXCLUDED.reason
		`
		args := make([]interface{}, 0, len(chunk)*candidateInsertColumns)
		for idx := range chunk {
			c := &chunk[idx]
			args = append(args,
				c.ChainID, c.BlockNumber, c.BlockTimestamp, c.TxHash, c.FromAddress,
				c.UserAttribution, c.Nonce, c.BlobCount, c.MaxPriorityFeePerGas,
				c.MaxFeePerGas, c.MaxFeePerBlobGas, c.FirstSeenAt, c.Reason)
		}
		if _, err := tx.ExecContext(i.ctx, query, args...); err != nil {
			return err
		}
	}
	return nil
}

// repairIncludedCandidates removes candidate rows for transactions that a
// block below the recording one had already included or superseded, and
// recomputes the aggregates those rows fed. Start runs it once
// unconditionally; the maintenance loop runs it on every tick, ahead of the
// prune that would delete its evidence. The commit-order wait and the
// snapshot gate (commit_order.go) make such rows rare; this is the backstop
// for what still slips past them, and for rows an earlier binary wrote. A
// non-zero repair is logged at Warn because it means the gate let something
// through.
func (i *Indexer) repairIncludedCandidates(ctx context.Context) {
	unlockWrites := i.lockDBWrites()
	removed, blocks, err := i.db.RepairIncludedBlobInclusionCandidates(ctx, i.network.ChainID)
	unlockWrites()
	if err != nil {
		if ctx.Err() == nil {
			logger.Error("Failed to repair blob inclusion candidates",
				zap.String("network", i.network.Name),
				zap.Error(err))
		}
		return
	}
	if removed > 0 {
		logger.Warn("Removed candidate rows for transactions included before the block that recorded them",
			zap.String("network", i.network.Name),
			zap.Int64("removed_rows", removed),
			zap.Int("blocks", len(blocks)))
	}
}

// upsertBlockBuilder writes the block's builder row. The identity columns are
// overwritten on conflict, like block_metrics: a reprocessed block re-derives
// all of them from the header it just fetched.
//
// The candidate snapshot columns are the exception. They record an
// observation of the pending pool at one moment, not a derivation of the
// block, so a stored snapshot survives every later write to the row: the
// flag ORs, and each aggregate keeps its stored value whenever the stored
// flag is set. That makes any reprocess path safe, whichever one takes it —
// a duplicate live pass, the historical backfill, a manual reindex. A reorg
// replacement block is unaffected because handleReorg deletes the row first,
// so its snapshot starts from nothing.
func (i *Indexer) upsertBlockBuilder(tx *sqlx.Tx, builder *models.BlockBuilder) error {
	_, err := tx.ExecContext(i.ctx, `
		INSERT INTO block_builders (
			chain_id, block_number, block_timestamp, fee_recipient, extra_data,
			builder_key, builder_name, tx_count, proposer_payment_wei, proposer_payment_to,
			candidate_snapshot, pending_candidate_txs, eligible_skipped_txs,
			eligible_skipped_blobs, eligible_skipped_max_tip
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		ON CONFLICT (chain_id, block_number) DO UPDATE SET
			block_timestamp = EXCLUDED.block_timestamp,
			fee_recipient = EXCLUDED.fee_recipient,
			extra_data = EXCLUDED.extra_data,
			builder_key = EXCLUDED.builder_key,
			builder_name = EXCLUDED.builder_name,
			tx_count = EXCLUDED.tx_count,
			proposer_payment_wei = EXCLUDED.proposer_payment_wei,
			proposer_payment_to = EXCLUDED.proposer_payment_to,
			candidate_snapshot = block_builders.candidate_snapshot OR EXCLUDED.candidate_snapshot,
			pending_candidate_txs = CASE WHEN block_builders.candidate_snapshot
				THEN block_builders.pending_candidate_txs ELSE EXCLUDED.pending_candidate_txs END,
			eligible_skipped_txs = CASE WHEN block_builders.candidate_snapshot
				THEN block_builders.eligible_skipped_txs ELSE EXCLUDED.eligible_skipped_txs END,
			eligible_skipped_blobs = CASE WHEN block_builders.candidate_snapshot
				THEN block_builders.eligible_skipped_blobs ELSE EXCLUDED.eligible_skipped_blobs END,
			eligible_skipped_max_tip = CASE WHEN block_builders.candidate_snapshot
				THEN block_builders.eligible_skipped_max_tip ELSE EXCLUDED.eligible_skipped_max_tip END
	`, builder.ChainID, builder.BlockNumber, builder.BlockTimestamp, builder.FeeRecipient, builder.ExtraData,
		builder.BuilderKey, builder.BuilderName, builder.TxCount, builder.ProposerPaymentWei, builder.ProposerPaymentTo,
		builder.CandidateSnapshot, builder.PendingCandidateTxs, builder.EligibleSkippedTxs,
		builder.EligibleSkippedBlobs, builder.EligibleSkippedMaxTip)
	return err
}

// runBuilderRelabel re-resolves builder_key and builder_name for rows whose
// labels were written by a different registry. The raw header fields stay
// authoritative, so a registry change is a cheap in-place relabel: one
// distinct scan per network plus one UPDATE per raw pair that actually
// changed. It runs before any historical backfill so backfilled rows are
// labeled once, by the current registry.
func (i *Indexer) runBuilderRelabel() {
	version := builders.RegistryVersion()
	stored, err := i.db.GetNetworkMetadata(i.ctx, i.network.ChainID, models.MetadataBlockBuilderRegistryVersion)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		if i.ctx.Err() == nil {
			logger.Warn("Failed to read builder registry version; skipping relabel",
				zap.String("network", i.network.Name),
				zap.Error(err))
		}
		return
	}
	if stored == version {
		return
	}

	updated, err := i.relabelBlockBuilders()
	if err != nil {
		if i.ctx.Err() == nil {
			logger.Error("Failed to relabel block builders",
				zap.String("network", i.network.Name),
				zap.Error(err))
		}
		return
	}

	i.mu.Lock()
	err = i.db.SetNetworkMetadata(i.ctx, i.network.ChainID, models.MetadataBlockBuilderRegistryVersion, version)
	i.mu.Unlock()
	if err != nil {
		if i.ctx.Err() == nil {
			logger.Warn("Failed to record builder registry version; the relabel repeats on the next start",
				zap.String("network", i.network.Name),
				zap.Error(err))
		}
		return
	}

	logger.Info("Relabeled block builders for the current registry",
		zap.String("network", i.network.Name),
		zap.String("registry_version", version),
		zap.Int64("rows_updated", updated))
}

// relabelBatchPairs is how many changed (fee_recipient, extra_data) pairs one
// relabel UPDATE carries. Each statement is one scan of the chain's
// block_builders rows hash-joined against the pairs, so the pass costs one
// scan per batch however many pairs changed; four parameters per pair keeps
// a full batch far below Postgres's 65535-parameter limit.
const relabelBatchPairs = 500

// relabelBlockBuilders resolves every distinct (fee_recipient, extra_data)
// pair on this chain against the current registry and rewrites the labels of
// the rows whose resolution changed. The distinct scan also returns the
// labels the rows carry now, so the pairs that already resolve to what they
// hold cost nothing more than that scan: a registry change that affects
// nothing writes nothing, and one that relabels a builder issues a single
// UPDATE per relabelBatchPairs changed pairs. Mainnet carries ~11k distinct
// pairs; one UPDATE per pair, each a scan of the whole table, took the
// production database's CPU for the better part of an hour after every
// registry change (0.20.0).
func (i *Indexer) relabelBlockBuilders() (int64, error) {
	type labeledPair struct {
		FeeRecipient string `db:"fee_recipient"`
		ExtraData    string `db:"extra_data"`
		BuilderKey   string `db:"builder_key"`
		BuilderName  string `db:"builder_name"`
	}
	var rows []labeledPair
	if err := i.db.SelectContext(i.ctx, &rows, `
		SELECT DISTINCT fee_recipient, extra_data, builder_key, builder_name
		FROM block_builders
		WHERE chain_id = $1
		ORDER BY fee_recipient, extra_data`,
		i.network.ChainID); err != nil {
		return 0, fmt.Errorf("failed to list builder label pairs: %w", err)
	}

	// A pair appears once per label it carries; resolve it once and queue
	// it if any of its rows hold something else.
	type pairKey struct{ feeRecipient, extraData string }
	resolvedFor := make(map[pairKey]builders.Builder, len(rows))
	queued := make(map[pairKey]bool, len(rows))
	var changed []labeledPair
	for _, row := range rows {
		key := pairKey{row.FeeRecipient, row.ExtraData}
		resolved, ok := resolvedFor[key]
		if !ok {
			resolved = builders.Resolve(decodeExtraData(row.ExtraData), row.FeeRecipient)
			resolvedFor[key] = resolved
		}
		if queued[key] || (row.BuilderKey == resolved.Key && row.BuilderName == resolved.Name) {
			continue
		}
		queued[key] = true
		changed = append(changed, labeledPair{
			FeeRecipient: row.FeeRecipient,
			ExtraData:    row.ExtraData,
			BuilderKey:   resolved.Key,
			BuilderName:  resolved.Name,
		})
	}

	var updated int64
	for len(changed) > 0 {
		if i.ctx.Err() != nil {
			return updated, i.ctx.Err()
		}
		batch := changed
		if len(batch) > relabelBatchPairs {
			batch = batch[:relabelBatchPairs]
		}
		changed = changed[len(batch):]

		args := make([]interface{}, 0, 1+4*len(batch))
		args = append(args, i.network.ChainID)
		values := make([]string, 0, len(batch))
		for _, pair := range batch {
			n := len(args)
			values = append(values, fmt.Sprintf("($%d::text, $%d::text, $%d::text, $%d::text)", n+1, n+2, n+3, n+4))
			args = append(args, pair.FeeRecipient, pair.ExtraData, pair.BuilderKey, pair.BuilderName)
		}

		unlockWrites := i.lockDBWrites()
		res, err := i.db.ExecContext(i.ctx, `
			UPDATE block_builders AS bb
			SET builder_key = v.builder_key, builder_name = v.builder_name
			FROM (VALUES `+strings.Join(values, ", ")+`)
				AS v(fee_recipient, extra_data, builder_key, builder_name)
			WHERE bb.chain_id = $1
				AND bb.fee_recipient = v.fee_recipient
				AND bb.extra_data = v.extra_data
				AND (bb.builder_key <> v.builder_key OR bb.builder_name <> v.builder_name)
		`, args...)
		unlockWrites()
		if err != nil {
			return updated, fmt.Errorf("failed to relabel builder rows: %w", err)
		}
		if affected, affErr := res.RowsAffected(); affErr == nil {
			updated += affected
		}
	}
	return updated, nil
}

// decodeExtraData turns the stored 0x-hex extra data back into the raw bytes
// the registry resolves against. Anything unparsable resolves as empty,
// which falls back to the fee recipient — the same answer the row would get
// if the extra data were absent.
func decodeExtraData(value string) []byte {
	decoded, err := hexutil.Decode(value)
	if err != nil {
		return nil
	}
	return decoded
}

// pruneStaleCandidates drops per-transaction candidate detail older than the
// retention window; the block_builders aggregates it summarized stay.
//
// It runs on every maintenance tick, gated only on indexer.candidate_retention
// — not on the mempool TTL that gates the sweeps beside it, and not on those
// sweeps succeeding. blob_inclusion_candidates is LOGGED and written by every
// live snapshot, so a deployment that turns the mempool TTL off must not
// silently stop pruning it.
func (i *Indexer) pruneStaleCandidates(ctx context.Context) {
	if i.candidateRetention <= 0 {
		return
	}
	cutoff := time.Now().UTC().Add(-i.candidateRetention)
	unlockWrites := i.lockDBWrites()
	pruned, err := i.db.DeleteStaleBlobInclusionCandidates(ctx, i.network.ChainID, cutoff)
	unlockWrites()
	if err != nil {
		if ctx.Err() == nil {
			logger.Error("Failed to prune stale blob inclusion candidates",
				zap.String("network", i.network.Name),
				zap.Error(err))
		}
		return
	}
	if pruned > 0 {
		logger.Info("Pruned stale blob inclusion candidates",
			zap.String("network", i.network.Name),
			zap.Int64("pruned_count", pruned))
	}
}
