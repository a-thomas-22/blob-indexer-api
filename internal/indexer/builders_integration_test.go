//go:build integration

package indexer

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/a-thomas-22/blob-indexer-api/internal/builders"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
)

// These tests exercise the builder write path against a real Postgres: the
// multi-row candidate insert, the RETURNING-based first-seen propagation, the
// COALESCE guard on reprocess, the new deletes in the reorg and reindex
// cleanups, the retention prune, and the relabel UPDATE. None of it is
// observable through sqlmock.

// integrationBlockMetrics builds a block_metrics row whose fees leave every
// pending transaction in the fixtures affordable and whose blob capacity has
// room to spare.
func integrationBlockMetrics(blockNumber int64, timestamp time.Time) *models.BlockMetrics {
	return &models.BlockMetrics{
		ChainID:          integrationChainID,
		BlockNumber:      blockNumber,
		BlockTimestamp:   timestamp,
		BlobCount:        0,
		BlobGasUsed:      0,
		BlobGasTarget:    3 * 131072,
		BlobGasLimit:     6 * 131072,
		ExcessBlobGas:    0,
		BlobBaseFee:      "1",
		BaseFeeWei:       "1",
		UtilizationRatio: "0.000000",
		BlobParamsTarget: 3,
		BlobParamsMax:    6,
		UpdateFraction:   5007716,
	}
}

func integrationBuilder(blockNumber int64, timestamp time.Time, extraData []byte, feeRecipient string) *models.BlockBuilder {
	resolved := builders.Resolve(extraData, feeRecipient)
	return &models.BlockBuilder{
		ChainID:        integrationChainID,
		BlockNumber:    blockNumber,
		BlockTimestamp: timestamp,
		FeeRecipient:   feeRecipient,
		ExtraData:      hexEncodeExtraData(extraData),
		BuilderKey:     resolved.Key,
		BuilderName:    resolved.Name,
		TxCount:        3,
	}
}

type builderRow struct {
	BuilderKey            string  `db:"builder_key"`
	BuilderName           string  `db:"builder_name"`
	FeeRecipient          string  `db:"fee_recipient"`
	ExtraData             string  `db:"extra_data"`
	TxCount               int     `db:"tx_count"`
	ProposerPaymentWei    *string `db:"proposer_payment_wei"`
	ProposerPaymentTo     *string `db:"proposer_payment_to"`
	CandidateSnapshot     bool    `db:"candidate_snapshot"`
	PendingCandidateTxs   *int    `db:"pending_candidate_txs"`
	EligibleSkippedTxs    *int    `db:"eligible_skipped_txs"`
	EligibleSkippedBlobs  *int    `db:"eligible_skipped_blobs"`
	EligibleSkippedMaxTip *string `db:"eligible_skipped_max_tip"`
}

func readBuilderRow(t *testing.T, idx *Indexer, blockNumber int64) builderRow {
	t.Helper()
	var row builderRow
	err := idx.db.GetContext(context.Background(), &row, `
		SELECT builder_key, builder_name, fee_recipient, extra_data, tx_count,
			proposer_payment_wei::text AS proposer_payment_wei, proposer_payment_to,
			candidate_snapshot, pending_candidate_txs, eligible_skipped_txs,
			eligible_skipped_blobs, eligible_skipped_max_tip::text AS eligible_skipped_max_tip
		FROM block_builders WHERE chain_id = $1 AND block_number = $2`,
		integrationChainID, blockNumber)
	if err != nil {
		t.Fatalf("read block_builders row %d: %v", blockNumber, err)
	}
	return row
}

// describeBuilderRow renders a row by value, so two reads can be compared
// without the nullable columns' pointers standing in for their contents.
func describeBuilderRow(row builderRow) string {
	nullableInt := func(v *int) string {
		if v == nil {
			return "NULL"
		}
		return fmt.Sprintf("%d", *v)
	}
	nullableText := func(v *string) string {
		if v == nil {
			return "NULL"
		}
		return *v
	}
	return fmt.Sprintf("key=%s name=%s fee_recipient=%s extra=%s tx_count=%d payment=%s payment_to=%s "+
		"snapshot=%t pending=%s skipped_txs=%s skipped_blobs=%s max_tip=%s",
		row.BuilderKey, row.BuilderName, row.FeeRecipient, row.ExtraData, row.TxCount,
		nullableText(row.ProposerPaymentWei), nullableText(row.ProposerPaymentTo),
		row.CandidateSnapshot, nullableInt(row.PendingCandidateTxs), nullableInt(row.EligibleSkippedTxs),
		nullableInt(row.EligibleSkippedBlobs), nullableText(row.EligibleSkippedMaxTip))
}

// A block stores who built it alongside its metrics, in the same transaction,
// and a reprocess overwrites every column from the freshly fetched header.
func TestIntegrationBlockBuilderRowWritten(t *testing.T) {
	idx, database := newIntegrationIndexer(t)
	ctx := context.Background()

	timestamp := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	indexed := models.IndexedBlock{ChainID: integrationChainID, BlockNumber: 100, BlockHash: "0xh100", ParentHash: "0xp99"}
	builder := integrationBuilder(100, timestamp, []byte("Titan (titanbuilder.xyz)"), "0xBuilder")
	payment, to := "1230000000000000000", "0xProposer"
	builder.ProposerPaymentWei, builder.ProposerPaymentTo = &payment, &to

	if err := idx.insertBlockData(nil, indexed, integrationBlockMetrics(100, timestamp), builder, 0); err != nil {
		t.Fatalf("insertBlockData() error = %v", err)
	}

	row := readBuilderRow(t, idx, 100)
	if row.BuilderKey != "titan" || row.BuilderName != "Titan" {
		t.Fatalf("labels = (%q, %q), want (titan, Titan)", row.BuilderKey, row.BuilderName)
	}
	if row.ProposerPaymentWei == nil || *row.ProposerPaymentWei != payment {
		t.Fatalf("proposer payment = %v, want %s", row.ProposerPaymentWei, payment)
	}
	if row.ProposerPaymentTo == nil || *row.ProposerPaymentTo != to {
		t.Fatalf("proposer payment to = %v, want %s", row.ProposerPaymentTo, to)
	}
	// An old block gets no snapshot, and the aggregates stay NULL so no
	// consumer mistakes "not observed" for "nothing was pending".
	if row.CandidateSnapshot || row.PendingCandidateTxs != nil || row.EligibleSkippedMaxTip != nil {
		t.Fatalf("expected no snapshot for a historical block, got %+v", row)
	}

	// A reprocess (reorg replay, reindex) re-derives every column.
	replacement := integrationBuilder(100, timestamp, []byte("beaverbuild.org"), "0xOtherBuilder")
	replacement.TxCount = 9
	if err := idx.insertBlockData(nil, indexed, integrationBlockMetrics(100, timestamp), replacement, 0); err != nil {
		t.Fatalf("insertBlockData() reprocess error = %v", err)
	}
	row = readBuilderRow(t, idx, 100)
	if row.BuilderKey != "beaverbuild" || row.FeeRecipient != "0xOtherBuilder" || row.TxCount != 9 {
		t.Fatalf("reprocess did not overwrite the row: %+v", row)
	}
	if row.ProposerPaymentWei != nil {
		t.Fatalf("reprocess left a stale proposer payment: %v", row.ProposerPaymentWei)
	}

	var rows int
	if err := database.GetContext(ctx, &rows,
		"SELECT COUNT(*) FROM block_builders WHERE chain_id = $1", integrationChainID); err != nil {
		t.Fatalf("count block_builders: %v", err)
	}
	if rows != 1 {
		t.Fatalf("expected exactly one builder row, got %d", rows)
	}
}

// Promotion is the only moment the first-seen timestamp can be saved: the
// pending rows are deleted in the same statement that reports it.
func TestIntegrationPromotionCopiesFirstSeenAt(t *testing.T) {
	idx, database := newIntegrationIndexer(t)
	ctx := context.Background()

	firstSeen := time.Date(2026, 7, 1, 11, 59, 48, 0, time.UTC).UTC()
	pending := integrationBlob(models.PendingBlockNumber, 0, "0xpromoted", "0xsender", false)
	pending.Nonce = 4
	pending.Timestamp = firstSeen
	if err := idx.insertPendingBlobs([]models.Blob{pending}); err != nil {
		t.Fatalf("insertPendingBlobs() error = %v", err)
	}

	confirmed := integrationBlob(200, 0, "0xpromoted", "0xsender", true)
	confirmed.Nonce = 4
	txIndex := 6
	confirmed.TxIndex = &txIndex
	indexed := models.IndexedBlock{ChainID: integrationChainID, BlockNumber: 200, BlockHash: "0xh200", ParentHash: "0xp199"}
	if err := idx.insertBlockData([]models.Blob{confirmed}, indexed, nil, nil, 0); err != nil {
		t.Fatalf("insertBlockData() error = %v", err)
	}

	type storedRow struct {
		FirstSeenAt *time.Time `db:"first_seen_at"`
		TxIndex     *int       `db:"tx_index"`
	}
	read := func() storedRow {
		t.Helper()
		var row storedRow
		if err := database.GetContext(ctx, &row,
			"SELECT first_seen_at, tx_index FROM blobs WHERE chain_id = $1 AND block_number = 200 AND blob_index = 0",
			integrationChainID); err != nil {
			t.Fatalf("read promoted blob: %v", err)
		}
		return row
	}

	row := read()
	if row.FirstSeenAt == nil || !row.FirstSeenAt.UTC().Equal(firstSeen) {
		t.Fatalf("first_seen_at = %v, want %s", row.FirstSeenAt, firstSeen)
	}
	if row.TxIndex == nil || *row.TxIndex != txIndex {
		t.Fatalf("tx_index = %v, want %d", row.TxIndex, txIndex)
	}

	// Reprocessing the block finds no pending row left, so the upsert passes
	// NULL — which must not erase the observation.
	reprocessed := integrationBlob(200, 0, "0xpromoted", "0xsender", true)
	reprocessed.Nonce = 4
	newIndex := 2
	reprocessed.TxIndex = &newIndex
	if err := idx.insertBlockData([]models.Blob{reprocessed}, indexed, nil, nil, 0); err != nil {
		t.Fatalf("insertBlockData() reprocess error = %v", err)
	}
	row = read()
	if row.FirstSeenAt == nil || !row.FirstSeenAt.UTC().Equal(firstSeen) {
		t.Fatalf("reprocess erased first_seen_at: %v", row.FirstSeenAt)
	}
	if row.TxIndex == nil || *row.TxIndex != newIndex {
		t.Fatalf("tx_index = %v, want the reprocessed %d", row.TxIndex, newIndex)
	}

	// The eviction log carries the fee context of both sides of a bump.
	bump := integrationBlob(models.PendingBlockNumber, 0, "0xbumped", "0xbumper", false)
	bump.Nonce = 11
	bump.Timestamp = firstSeen
	bumpTip, bumpBlobFee := "5", "50"
	bump.MaxPriorityFeePerGas, bump.MaxFeePerBlobGas = &bumpTip, &bumpBlobFee
	if err := idx.insertPendingBlobs([]models.Blob{bump}); err != nil {
		t.Fatalf("insertPendingBlobs(bump) error = %v", err)
	}
	winner := integrationBlob(201, 0, "0xwinner", "0xbumper", true)
	winner.Nonce = 11
	winnerTip, winnerBlobFee := "25", "80"
	winner.MaxPriorityFeePerGas, winner.MaxFeePerBlobGas = &winnerTip, &winnerBlobFee
	if err := idx.insertBlockData([]models.Blob{winner},
		models.IndexedBlock{ChainID: integrationChainID, BlockNumber: 201, BlockHash: "0xh201", ParentHash: "0xh200"}, nil, nil, 0); err != nil {
		t.Fatalf("insertBlockData(winner) error = %v", err)
	}

	var replacement struct {
		ReplacedTip     *string    `db:"replaced_max_priority_fee_per_gas"`
		ReplacedBlobFee *string    `db:"replaced_max_fee_per_blob_gas"`
		ReplacedSeenAt  *time.Time `db:"replaced_first_seen_at"`
		NewTip          *string    `db:"replacement_max_priority_fee_per_gas"`
		NewBlobFee      *string    `db:"replacement_max_fee_per_blob_gas"`
	}
	if err := database.GetContext(ctx, &replacement, `
		SELECT replaced_max_priority_fee_per_gas::text AS replaced_max_priority_fee_per_gas,
			replaced_max_fee_per_blob_gas::text AS replaced_max_fee_per_blob_gas,
			replaced_first_seen_at,
			replacement_max_priority_fee_per_gas::text AS replacement_max_priority_fee_per_gas,
			replacement_max_fee_per_blob_gas::text AS replacement_max_fee_per_blob_gas
		FROM blob_replacements WHERE chain_id = $1 AND replaced_tx_hash = $2`,
		integrationChainID, "0xbumped"); err != nil {
		t.Fatalf("read blob_replacements: %v", err)
	}
	if replacement.ReplacedTip == nil || *replacement.ReplacedTip != bumpTip ||
		replacement.ReplacedBlobFee == nil || *replacement.ReplacedBlobFee != bumpBlobFee {
		t.Fatalf("replaced-side fees = (%v, %v), want (%s, %s)",
			replacement.ReplacedTip, replacement.ReplacedBlobFee, bumpTip, bumpBlobFee)
	}
	if replacement.ReplacedSeenAt == nil || !replacement.ReplacedSeenAt.UTC().Equal(firstSeen) {
		t.Fatalf("replaced first seen = %v, want %s", replacement.ReplacedSeenAt, firstSeen)
	}
	if replacement.NewTip == nil || *replacement.NewTip != winnerTip ||
		replacement.NewBlobFee == nil || *replacement.NewBlobFee != winnerBlobFee {
		t.Fatalf("replacement-side fees = (%v, %v), want (%s, %s)",
			replacement.NewTip, replacement.NewBlobFee, winnerTip, winnerBlobFee)
	}
}

// A pending fee bump seen while still pending records the same fee context
// from the other write site.
func TestIntegrationPendingReplacementRecordsFeeContext(t *testing.T) {
	idx, database := newIntegrationIndexer(t)
	ctx := context.Background()

	firstSeen := time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC).UTC()
	original := integrationBlob(models.PendingBlockNumber, 0, "0xoriginal", "0xsender", false)
	original.Nonce = 3
	original.Timestamp = firstSeen
	origTip, origBlobFee := "2", "20"
	original.MaxPriorityFeePerGas, original.MaxFeePerBlobGas = &origTip, &origBlobFee
	if err := idx.insertPendingBlobs([]models.Blob{original}); err != nil {
		t.Fatalf("insertPendingBlobs(original) error = %v", err)
	}

	bump := integrationBlob(models.PendingBlockNumber, 0, "0xbump", "0xsender", false)
	bump.Nonce = 3
	bump.Timestamp = firstSeen.Add(time.Minute)
	bumpTip, bumpBlobFee := "12", "60"
	bump.MaxPriorityFeePerGas, bump.MaxFeePerBlobGas = &bumpTip, &bumpBlobFee
	if err := idx.insertPendingBlobs([]models.Blob{bump}); err != nil {
		t.Fatalf("insertPendingBlobs(bump) error = %v", err)
	}

	var row struct {
		ReplacedTip    *string    `db:"replaced_max_priority_fee_per_gas"`
		ReplacedSeenAt *time.Time `db:"replaced_first_seen_at"`
		NewTip         *string    `db:"replacement_max_priority_fee_per_gas"`
		NewBlobFee     *string    `db:"replacement_max_fee_per_blob_gas"`
	}
	if err := database.GetContext(ctx, &row, `
		SELECT replaced_max_priority_fee_per_gas::text AS replaced_max_priority_fee_per_gas,
			replaced_first_seen_at,
			replacement_max_priority_fee_per_gas::text AS replacement_max_priority_fee_per_gas,
			replacement_max_fee_per_blob_gas::text AS replacement_max_fee_per_blob_gas
		FROM blob_replacements WHERE chain_id = $1 AND replaced_tx_hash = $2`,
		integrationChainID, "0xoriginal"); err != nil {
		t.Fatalf("read blob_replacements: %v", err)
	}
	if row.ReplacedTip == nil || *row.ReplacedTip != origTip {
		t.Fatalf("replaced tip = %v, want %s", row.ReplacedTip, origTip)
	}
	if row.ReplacedSeenAt == nil || !row.ReplacedSeenAt.UTC().Equal(firstSeen) {
		t.Fatalf("replaced first seen = %v, want %s", row.ReplacedSeenAt, firstSeen)
	}
	if row.NewTip == nil || *row.NewTip != bumpTip || row.NewBlobFee == nil || *row.NewBlobFee != bumpBlobFee {
		t.Fatalf("replacement fees = (%v, %v), want (%s, %s)", row.NewTip, row.NewBlobFee, bumpTip, bumpBlobFee)
	}
}

// seedPendingCandidate inserts a pending blob transaction the snapshot will
// classify. seenAt is the first-seen instant; last_seen is then bumped to now
// the way every poll's liveness refresh does for a transaction the node still
// reports, because the candidate snapshot only counts rows seen recently.
func seedPendingCandidate(t *testing.T, idx *Indexer, txHash, sender string, nonce uint64, blobCount int, tip string, seenAt time.Time) {
	t.Helper()
	blobs := make([]models.Blob, 0, blobCount)
	for i := 0; i < blobCount; i++ {
		b := integrationBlob(models.PendingBlockNumber, i, txHash, sender, false)
		b.Nonce = nonce
		b.Timestamp = seenAt
		tipCopy := tip
		b.MaxPriorityFeePerGas = &tipCopy
		blobs = append(blobs, b)
	}
	if err := idx.insertPendingBlobs(blobs); err != nil {
		t.Fatalf("insertPendingBlobs(%s) error = %v", txHash, err)
	}
	setPendingLastSeen(t, idx, txHash, time.Now().UTC())
}

// setPendingLastSeen rewrites a tracked pending transaction's liveness
// watermark, standing in for the poll that did (or did not) re-report it.
func setPendingLastSeen(t *testing.T, idx *Indexer, txHash string, lastSeen time.Time) {
	t.Helper()
	if _, err := idx.db.ExecContext(context.Background(),
		"UPDATE mempool_blobs SET last_seen = $3 WHERE chain_id = $1 AND tx_hash = $2",
		integrationChainID, txHash, lastSeen); err != nil {
		t.Fatalf("set last_seen for %s: %v", txHash, err)
	}
}

// seedCommittedPredecessors records every block in blockNumber's snapshot
// window as indexed, standing in for the in-order commits a live block
// normally follows. Without them the snapshot gate would (rightly) refuse
// to take the snapshot.
func seedCommittedPredecessors(t *testing.T, idx *Indexer, blockNumber int64) {
	t.Helper()
	from, to, ok := idx.snapshotPredecessorRange(blockNumber)
	if !ok {
		return
	}
	for block := from; block <= to; block++ {
		if _, err := idx.db.ExecContext(context.Background(), `
			INSERT INTO indexed_blocks (chain_id, block_number, block_hash, parent_hash)
			VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`,
			integrationChainID, block, fmt.Sprintf("0xh%d", block), fmt.Sprintf("0xh%d", block-1)); err != nil {
			t.Fatalf("seed indexed block %d: %v", block, err)
		}
	}
}

// A live block classifies and stores the pending pool it left behind; a block
// indexed long after the fact stores no snapshot at all.
func TestIntegrationCandidateSnapshot(t *testing.T) {
	idx, database := newIntegrationIndexer(t)
	ctx := context.Background()
	idx.candidateSnapshotMaxLag = time.Minute
	idx.candidateMinAge = 6 * time.Second

	blockTime := time.Now().UTC().Truncate(time.Second)
	seenAt := blockTime.Add(-time.Minute)

	// Two eligible transactions from different senders, one transaction the
	// node saw only moments ago, and one behind its own sender's lower nonce.
	seedPendingCandidate(t, idx, "0xeligible1", "0xs1", 1, 2, "9", seenAt)
	seedPendingCandidate(t, idx, "0xeligible2", "0xs2", 5, 1, "30", seenAt)
	seedPendingCandidate(t, idx, "0xgap", "0xs2", 6, 1, "99", seenAt)
	seedPendingCandidate(t, idx, "0xfresh", "0xs3", 1, 1, "77", blockTime.Add(-time.Second))

	// The block itself confirms one other transaction, whose pending rows the
	// promotion removes before the snapshot is taken.
	confirmed := integrationBlob(300, 0, "0xincluded", "0xs4", true)
	confirmed.Nonce = 2
	confirmed.Timestamp = blockTime
	seedPendingCandidate(t, idx, "0xincluded", "0xs4", 2, 1, "1", seenAt)

	seedCommittedPredecessors(t, idx, 300)
	indexed := models.IndexedBlock{ChainID: integrationChainID, BlockNumber: 300, BlockHash: "0xh300", ParentHash: "0xp299"}
	builder := integrationBuilder(300, blockTime, []byte("rsync-builder"), "0xRsync")
	if err := idx.insertBlockData([]models.Blob{confirmed}, indexed, integrationBlockMetrics(300, blockTime), builder, 0); err != nil {
		t.Fatalf("insertBlockData() error = %v", err)
	}

	row := readBuilderRow(t, idx, 300)
	if !row.CandidateSnapshot {
		t.Fatal("expected a snapshot for a block that just arrived")
	}
	if row.PendingCandidateTxs == nil || *row.PendingCandidateTxs != 4 {
		t.Fatalf("pending candidate txs = %v, want 4 (the included tx must be promoted away first)", row.PendingCandidateTxs)
	}
	if row.EligibleSkippedTxs == nil || *row.EligibleSkippedTxs != 2 {
		t.Fatalf("eligible skipped txs = %v, want 2", row.EligibleSkippedTxs)
	}
	if row.EligibleSkippedBlobs == nil || *row.EligibleSkippedBlobs != 3 {
		t.Fatalf("eligible skipped blobs = %v, want 3", row.EligibleSkippedBlobs)
	}
	if row.EligibleSkippedMaxTip == nil || *row.EligibleSkippedMaxTip != "30" {
		t.Fatalf("eligible skipped max tip = %v, want 30", row.EligibleSkippedMaxTip)
	}

	type candidateRow struct {
		TxHash    string `db:"tx_hash"`
		Reason    string `db:"reason"`
		BlobCount int    `db:"blob_count"`
		Nonce     *int64 `db:"nonce"`
	}
	var candidates []candidateRow
	if err := database.SelectContext(ctx, &candidates,
		"SELECT tx_hash, reason, blob_count, nonce FROM blob_inclusion_candidates WHERE chain_id = $1 AND block_number = 300 ORDER BY tx_hash",
		integrationChainID); err != nil {
		t.Fatalf("read candidates: %v", err)
	}
	want := map[string]string{
		"0xeligible1": models.CandidateEligible,
		"0xeligible2": models.CandidateEligible,
		"0xfresh":     models.CandidateTooRecent,
		"0xgap":       models.CandidateNonceGap,
	}
	if len(candidates) != len(want) {
		t.Fatalf("expected %d candidate rows, got %d (%+v)", len(want), len(candidates), candidates)
	}
	for _, got := range candidates {
		if want[got.TxHash] != got.Reason {
			t.Errorf("%s: reason = %q, want %q", got.TxHash, got.Reason, want[got.TxHash])
		}
	}

	// The same pool against a block indexed from history: no snapshot, no
	// candidate rows, and the aggregates stay NULL.
	oldTime := blockTime.Add(-24 * time.Hour)
	oldIndexed := models.IndexedBlock{ChainID: integrationChainID, BlockNumber: 301, BlockHash: "0xh301", ParentHash: "0xh300"}
	oldBuilder := integrationBuilder(301, oldTime, []byte("beaverbuild.org"), "0xBeaver")
	if err := idx.insertBlockData(nil, oldIndexed, integrationBlockMetrics(301, oldTime), oldBuilder, 0); err != nil {
		t.Fatalf("insertBlockData() historical error = %v", err)
	}
	oldRow := readBuilderRow(t, idx, 301)
	if oldRow.CandidateSnapshot || oldRow.PendingCandidateTxs != nil || oldRow.EligibleSkippedTxs != nil ||
		oldRow.EligibleSkippedBlobs != nil || oldRow.EligibleSkippedMaxTip != nil {
		t.Fatalf("expected no snapshot for a historical block, got %+v", oldRow)
	}
	var oldCandidates int
	if err := database.GetContext(ctx, &oldCandidates,
		"SELECT COUNT(*) FROM blob_inclusion_candidates WHERE chain_id = $1 AND block_number = 301", integrationChainID); err != nil {
		t.Fatalf("count historical candidates: %v", err)
	}
	if oldCandidates != 0 {
		t.Fatalf("expected no candidate rows for a historical block, got %d", oldCandidates)
	}

	// The retention prune drops the per-transaction detail and leaves the
	// permanent aggregates behind.
	idx.candidateRetention = time.Nanosecond
	idx.pruneStaleCandidates(ctx)
	var remaining int
	if err := database.GetContext(ctx, &remaining,
		"SELECT COUNT(*) FROM blob_inclusion_candidates WHERE chain_id = $1", integrationChainID); err != nil {
		t.Fatalf("count candidates after prune: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("expected the prune to clear the candidate detail, got %d rows", remaining)
	}
	if pruned := readBuilderRow(t, idx, 300); !pruned.CandidateSnapshot ||
		pruned.EligibleSkippedTxs == nil || *pruned.EligibleSkippedTxs != 2 {
		t.Fatalf("the prune must leave the aggregates alone, got %+v", pruned)
	}
}

// Both cleanups that delete a block range must take the builder row and its
// candidate detail with them, or a re-indexed block would keep a stale
// fork's builder.
func TestIntegrationBuilderCleanupOnReorgAndReindex(t *testing.T) {
	idx, database := newIntegrationIndexer(t)
	ctx := context.Background()
	idx.maxReorgDepth = 64
	idx.candidateSnapshotMaxLag = time.Minute
	idx.candidateMinAge = time.Second
	idx.ethClient, _ = newMockEthClient(t, 10)

	blockTime := time.Now().UTC().Truncate(time.Second)
	seedPendingCandidate(t, idx, "0xskipped", "0xs1", 1, 1, "3", blockTime.Add(-time.Minute))

	// Blocks 1-4 are canonical; 5-8 carry stale-fork hashes.
	for blockNumber := int64(1); blockNumber <= 8; blockNumber++ {
		hash := fmt.Sprintf("0xstale%d", blockNumber)
		parent := fmt.Sprintf("0xstale%d", blockNumber-1)
		if blockNumber <= 4 {
			canonical, err := idx.ethClient.GetBlockByNumber(ctx, uint64(blockNumber))
			if err != nil {
				t.Fatalf("get canonical block %d: %v", blockNumber, err)
			}
			hash = canonical.Hash().Hex()
			parent = canonical.ParentHash().Hex()
		}
		indexed := models.IndexedBlock{ChainID: integrationChainID, BlockNumber: blockNumber, BlockHash: hash, ParentHash: parent}
		builder := integrationBuilder(blockNumber, blockTime, []byte("beaverbuild.org"), "0xBeaver")
		if err := idx.insertBlockData(nil, indexed, integrationBlockMetrics(blockNumber, blockTime), builder, 0); err != nil {
			t.Fatalf("insertBlockData(%d): %v", blockNumber, err)
		}
	}

	countFrom := func(table string, from int64) int {
		t.Helper()
		var count int
		if err := database.GetContext(ctx, &count,
			fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE chain_id = $1 AND block_number >= $2", table),
			integrationChainID, from); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return count
	}
	if countFrom("block_builders", 1) != 8 {
		t.Fatalf("expected 8 builder rows before the reorg, got %d", countFrom("block_builders", 1))
	}
	if countFrom("blob_inclusion_candidates", 1) == 0 {
		t.Fatal("expected candidate rows before the reorg")
	}

	if err := idx.handleReorg(5); !errors.Is(err, errReorgDetected) {
		t.Fatalf("expected errReorgDetected, got %v", err)
	}
	if got := countFrom("block_builders", 5); got != 0 {
		t.Fatalf("expected the reorg to delete builder rows above the fork, got %d", got)
	}
	if got := countFrom("blob_inclusion_candidates", 5); got != 0 {
		t.Fatalf("expected the reorg to delete candidate rows above the fork, got %d", got)
	}
	if got := countFrom("block_builders", 1); got != 4 {
		t.Fatalf("expected blocks 1-4 to keep their builder rows, got %d", got)
	}

	// The reindex cleanup covers a closed range and must clear both tables
	// inside it.
	if err := idx.deleteReindexRange(2, 3); err != nil {
		t.Fatalf("deleteReindexRange() error = %v", err)
	}
	var remaining []int64
	if err := database.SelectContext(ctx, &remaining,
		"SELECT block_number FROM block_builders WHERE chain_id = $1 ORDER BY block_number", integrationChainID); err != nil {
		t.Fatalf("read remaining builder rows: %v", err)
	}
	if len(remaining) != 2 || remaining[0] != 1 || remaining[1] != 4 {
		t.Fatalf("expected builder rows [1 4] after the reindex delete, got %v", remaining)
	}
	var candidatesLeft []int64
	if err := database.SelectContext(ctx, &candidatesLeft,
		"SELECT DISTINCT block_number FROM blob_inclusion_candidates WHERE chain_id = $1 AND block_number BETWEEN 2 AND 3", integrationChainID); err != nil {
		t.Fatalf("read remaining candidate rows: %v", err)
	}
	if len(candidatesLeft) != 0 {
		t.Fatalf("expected the reindex delete to clear candidate rows, got %v", candidatesLeft)
	}

	// The standalone helpers the API and tooling call must do the same.
	if err := database.DeleteBlockBuildersFromBlock(ctx, integrationChainID, 1); err != nil {
		t.Fatalf("DeleteBlockBuildersFromBlock() error = %v", err)
	}
	if err := database.DeleteBlobInclusionCandidatesFromBlock(ctx, integrationChainID, 1); err != nil {
		t.Fatalf("DeleteBlobInclusionCandidatesFromBlock() error = %v", err)
	}
	if got := countFrom("block_builders", 1) + countFrom("blob_inclusion_candidates", 1); got != 0 {
		t.Fatalf("expected the delete helpers to clear both tables, got %d rows", got)
	}
}

// A registry change relabels existing rows in place from their raw header
// fields; rows already carrying the right labels are left alone, and the new
// fingerprint is recorded so the next start skips the pass.
func TestIntegrationBuilderRelabel(t *testing.T) {
	idx, database := newIntegrationIndexer(t)
	ctx := context.Background()

	blockTime := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	insert := func(blockNumber int64, extraData []byte, feeRecipient, key, name string) {
		t.Helper()
		if _, err := database.ExecContext(ctx, `
			INSERT INTO block_builders (chain_id, block_number, block_timestamp, fee_recipient, extra_data,
				builder_key, builder_name, tx_count)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			integrationChainID, blockNumber, blockTime, feeRecipient, hexEncodeExtraData(extraData), key, name, 1); err != nil {
			t.Fatalf("seed block_builders %d: %v", blockNumber, err)
		}
	}
	// Two blocks labeled by an older registry that did not know Titan, and
	// one already correct.
	insert(1, []byte("Titan (titanbuilder.xyz)"), "0xTitan", "extra:titan-titanbuilder-xyz", "Titan (titanbuilder.xyz)")
	insert(2, []byte("Titan (titanbuilder.xyz)"), "0xTitan", "extra:titan-titanbuilder-xyz", "Titan (titanbuilder.xyz)")
	insert(3, []byte("beaverbuild.org"), "0xBeaver", "beaverbuild", "beaverbuild")

	// No stored version at all: the pass must run.
	idx.runBuilderRelabel()

	type labeled struct {
		BlockNumber int64  `db:"block_number"`
		BuilderKey  string `db:"builder_key"`
		BuilderName string `db:"builder_name"`
	}
	var rows []labeled
	if err := database.SelectContext(ctx, &rows,
		"SELECT block_number, builder_key, builder_name FROM block_builders WHERE chain_id = $1 ORDER BY block_number",
		integrationChainID); err != nil {
		t.Fatalf("read relabeled rows: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}
	for _, row := range rows[:2] {
		if row.BuilderKey != "titan" || row.BuilderName != "Titan" {
			t.Fatalf("block %d: labels = (%q, %q), want (titan, Titan)", row.BlockNumber, row.BuilderKey, row.BuilderName)
		}
	}
	if rows[2].BuilderKey != "beaverbuild" {
		t.Fatalf("block 3 lost its label: %+v", rows[2])
	}

	version, err := database.GetNetworkMetadata(ctx, integrationChainID, models.MetadataBlockBuilderRegistryVersion)
	if err != nil {
		t.Fatalf("read registry version: %v", err)
	}
	if version != builders.RegistryVersion() {
		t.Fatalf("stored registry version = %q, want %q", version, builders.RegistryVersion())
	}

	// A second run with the version already recorded must be a no-op: hand
	// it a deliberately wrong label and check it survives.
	if _, err := database.ExecContext(ctx,
		"UPDATE block_builders SET builder_key = 'stale' WHERE chain_id = $1 AND block_number = 1",
		integrationChainID); err != nil {
		t.Fatalf("scribble a stale label: %v", err)
	}
	idx.runBuilderRelabel()
	var key string
	if err := database.GetContext(ctx, &key,
		"SELECT builder_key FROM block_builders WHERE chain_id = $1 AND block_number = 1", integrationChainID); err != nil {
		t.Fatalf("read block 1 label: %v", err)
	}
	if key != "stale" {
		t.Fatalf("expected the matching version to skip the pass, got %q", key)
	}
}

// The polling walker and the WebSocket follower enqueue live heights
// independently, so an ordinary live block can be processed twice while it
// is still inside the snapshot lag. The first snapshot wins: by the second
// pass the pool has moved on (what the block left pending may since have
// been promoted, and new transactions have arrived), and re-snapshotting
// would rewrite the aggregates against a pool the builder never faced while
// the first pass's candidate rows stayed behind.
func TestIntegrationDuplicateLiveBlockKeepsTheFirstSnapshot(t *testing.T) {
	idx, database := newIntegrationIndexer(t)
	ctx := context.Background()
	idx.candidateSnapshotMaxLag = time.Minute
	idx.candidateMinAge = 6 * time.Second

	blockTime := time.Now().UTC().Truncate(time.Second)
	seenAt := blockTime.Add(-time.Minute)

	seedPendingCandidate(t, idx, "0xskipped", "0xs1", 1, 2, "9", seenAt)

	seedCommittedPredecessors(t, idx, 400)
	indexed := models.IndexedBlock{ChainID: integrationChainID, BlockNumber: 400, BlockHash: "0xh400", ParentHash: "0xp399"}
	builder := integrationBuilder(400, blockTime, []byte("Titan (titanbuilder.xyz)"), "0xTitan")
	if err := idx.insertBlockData(nil, indexed, integrationBlockMetrics(400, blockTime), builder, 0); err != nil {
		t.Fatalf("insertBlockData() first pass error = %v", err)
	}

	first := readBuilderRow(t, idx, 400)
	if !first.CandidateSnapshot || first.PendingCandidateTxs == nil || *first.PendingCandidateTxs != 1 ||
		first.EligibleSkippedTxs == nil || *first.EligibleSkippedTxs != 1 ||
		first.EligibleSkippedBlobs == nil || *first.EligibleSkippedBlobs != 2 ||
		first.EligibleSkippedMaxTip == nil || *first.EligibleSkippedMaxTip != "9" {
		t.Fatalf("first pass did not record the pool it saw: %+v", first)
	}

	// The pool moves on: the transaction the block skipped is promoted by a
	// later block, and a different one arrives.
	if _, err := database.ExecContext(ctx,
		"DELETE FROM mempool_blobs WHERE chain_id = $1 AND tx_hash = $2",
		integrationChainID, "0xskipped"); err != nil {
		t.Fatalf("promote away the skipped tx: %v", err)
	}
	seedPendingCandidate(t, idx, "0xlater", "0xs2", 1, 1, "500", blockTime.Add(-30*time.Second))

	// The duplicate task for the very same height.
	if err := idx.insertBlockData(nil, indexed, integrationBlockMetrics(400, blockTime), builder, 0); err != nil {
		t.Fatalf("insertBlockData() duplicate pass error = %v", err)
	}

	second := readBuilderRow(t, idx, 400)
	if describeBuilderRow(second) != describeBuilderRow(first) {
		t.Fatalf("the duplicate pass rewrote the snapshot:\n first  = %s\n second = %s",
			describeBuilderRow(first), describeBuilderRow(second))
	}

	var candidateHashes []string
	if err := database.SelectContext(ctx, &candidateHashes,
		"SELECT tx_hash FROM blob_inclusion_candidates WHERE chain_id = $1 AND block_number = 400 ORDER BY tx_hash",
		integrationChainID); err != nil {
		t.Fatalf("read candidates: %v", err)
	}
	if len(candidateHashes) != 1 || candidateHashes[0] != "0xskipped" {
		t.Fatalf("candidate rows = %v, want exactly [0xskipped]", candidateHashes)
	}
}

// The upsert itself preserves a stored snapshot, so every other reprocess
// path — the historical backfill, a manual reindex, a catch-up pass outside
// the snapshot lag — is safe even though it never looks the row up first.
func TestIntegrationBuilderUpsertPreservesAStoredSnapshot(t *testing.T) {
	idx, _ := newIntegrationIndexer(t)
	idx.candidateSnapshotMaxLag = time.Minute
	idx.candidateMinAge = 6 * time.Second

	blockTime := time.Now().UTC().Truncate(time.Second)
	seedPendingCandidate(t, idx, "0xskipped", "0xs1", 1, 1, "42", blockTime.Add(-time.Minute))

	seedCommittedPredecessors(t, idx, 401)
	indexed := models.IndexedBlock{ChainID: integrationChainID, BlockNumber: 401, BlockHash: "0xh401", ParentHash: "0xp400"}
	builder := integrationBuilder(401, blockTime, []byte("beaverbuild.org"), "0xBeaver")
	if err := idx.insertBlockData(nil, indexed, integrationBlockMetrics(401, blockTime), builder, 0); err != nil {
		t.Fatalf("insertBlockData() live error = %v", err)
	}
	live := readBuilderRow(t, idx, 401)
	if !live.CandidateSnapshot {
		t.Fatalf("expected a snapshot for a live block, got %+v", live)
	}

	// A later pass with no snapshot of its own (a historical replay) still
	// re-derives the identity columns, but must not clear the observation.
	idx.candidateSnapshotMaxLag = -1
	replay := integrationBuilder(401, blockTime, []byte("rsync-builder"), "0xRsync")
	replay.TxCount = 11
	if err := idx.insertBlockData(nil, indexed, integrationBlockMetrics(401, blockTime), replay, 0); err != nil {
		t.Fatalf("insertBlockData() replay error = %v", err)
	}
	after := readBuilderRow(t, idx, 401)
	if after.BuilderKey != "rsync" || after.TxCount != 11 {
		t.Fatalf("the replay did not re-derive the identity columns: %+v", after)
	}
	if !after.CandidateSnapshot || after.PendingCandidateTxs == nil || *after.PendingCandidateTxs != 1 ||
		after.EligibleSkippedMaxTip == nil || *after.EligibleSkippedMaxTip != "42" {
		t.Fatalf("the replay erased the stored snapshot: %+v", after)
	}
}

// A transaction the node stopped reporting is no longer something a builder
// could have included, whatever the far longer TTL sweep has yet to delete.
func TestIntegrationCandidateSnapshotIgnoresStalePoolRows(t *testing.T) {
	idx, database := newIntegrationIndexer(t)
	ctx := context.Background()
	idx.candidateSnapshotMaxLag = time.Minute
	idx.candidateMinAge = 6 * time.Second
	// Liveness window: the 30s floor.
	idx.mempoolPollingInterval = 15 * time.Second
	idx.mempoolReconcileInterval = 15 * time.Second

	blockTime := time.Now().UTC().Truncate(time.Second)
	seenAt := blockTime.Add(-5 * time.Minute)

	seedPendingCandidate(t, idx, "0xlive", "0xs1", 1, 1, "9", seenAt)
	seedPendingCandidate(t, idx, "0xdropped", "0xs2", 1, 2, "900", seenAt)
	// The node stopped reporting this one — a privately delivered same-nonce
	// cancellation confirmed, say — so its liveness watermark stopped moving
	// while the row itself lingers until the TTL sweep.
	setPendingLastSeen(t, idx, "0xdropped", blockTime.Add(-10*time.Minute))

	seedCommittedPredecessors(t, idx, 410)
	indexed := models.IndexedBlock{ChainID: integrationChainID, BlockNumber: 410, BlockHash: "0xh410", ParentHash: "0xp409"}
	builder := integrationBuilder(410, blockTime, []byte("Titan (titanbuilder.xyz)"), "0xTitan")
	if err := idx.insertBlockData(nil, indexed, integrationBlockMetrics(410, blockTime), builder, 0); err != nil {
		t.Fatalf("insertBlockData() error = %v", err)
	}

	row := readBuilderRow(t, idx, 410)
	if row.PendingCandidateTxs == nil || *row.PendingCandidateTxs != 1 {
		t.Fatalf("pending candidate txs = %v, want 1 (the dropped tx must not count)", row.PendingCandidateTxs)
	}
	if row.EligibleSkippedBlobs == nil || *row.EligibleSkippedBlobs != 1 {
		t.Fatalf("eligible skipped blobs = %v, want 1", row.EligibleSkippedBlobs)
	}
	if row.EligibleSkippedMaxTip == nil || *row.EligibleSkippedMaxTip != "9" {
		t.Fatalf("max tip = %v, want 9 (the dropped tx's 900 must not count)", row.EligibleSkippedMaxTip)
	}

	var hashes []string
	if err := database.SelectContext(ctx, &hashes,
		"SELECT tx_hash FROM blob_inclusion_candidates WHERE chain_id = $1 AND block_number = 410",
		integrationChainID); err != nil {
		t.Fatalf("read candidates: %v", err)
	}
	if len(hashes) != 1 || hashes[0] != "0xlive" {
		t.Fatalf("candidate rows = %v, want exactly [0xlive]", hashes)
	}

	// The stale row is still in the pool table: only the snapshot ignores it.
	var pooled int
	if err := database.GetContext(ctx, &pooled,
		"SELECT COUNT(*) FROM mempool_blobs WHERE chain_id = $1 AND tx_hash = $2",
		integrationChainID, "0xdropped"); err != nil {
		t.Fatalf("count pooled rows: %v", err)
	}
	if pooled != 2 {
		t.Fatalf("the snapshot must not delete stale pool rows, got %d", pooled)
	}
}

// The blob slot (chain_id, block_number, blob_index) is the upsert's conflict
// key, not the transaction. A sibling fork putting a different transaction in
// the same slot must not inherit the previous occupant's observation, while a
// reprocess of the same transaction must keep it.
func TestIntegrationFirstSeenAtFollowsTheTransaction(t *testing.T) {
	idx, _ := newIntegrationIndexer(t)
	ctx := context.Background()
	timestamp := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	observed := timestamp.Add(-20 * time.Second)

	readFirstSeen := func(t *testing.T) (string, *time.Time) {
		t.Helper()
		var row struct {
			TxHash      string     `db:"tx_hash"`
			FirstSeenAt *time.Time `db:"first_seen_at"`
		}
		if err := idx.db.GetContext(ctx, &row,
			"SELECT tx_hash, first_seen_at FROM blobs WHERE chain_id = $1 AND block_number = 500 AND blob_index = 0",
			integrationChainID); err != nil {
			t.Fatalf("read blob row: %v", err)
		}
		return row.TxHash, row.FirstSeenAt
	}

	indexed := models.IndexedBlock{ChainID: integrationChainID, BlockNumber: 500, BlockHash: "0xh500a", ParentHash: "0xp499"}
	first := integrationBlob(500, 0, "0xtxA", "0xs1", true)
	first.Timestamp = timestamp
	first.FirstSeenAt = &observed
	if err := idx.insertBlockData([]models.Blob{first}, indexed, integrationBlockMetrics(500, timestamp), nil, 0); err != nil {
		t.Fatalf("insertBlockData(A): %v", err)
	}
	if hash, seen := readFirstSeen(t); hash != "0xtxA" || seen == nil || !seen.UTC().Equal(observed) {
		t.Fatalf("stored (%q, %v), want (0xtxA, %s)", hash, seen, observed)
	}

	// Reprocessing the same transaction with nothing left to promote keeps
	// the stored observation.
	same := integrationBlob(500, 0, "0xtxA", "0xs1", true)
	same.Timestamp = timestamp
	if err := idx.insertBlockData([]models.Blob{same}, indexed, integrationBlockMetrics(500, timestamp), nil, 0); err != nil {
		t.Fatalf("insertBlockData(A again): %v", err)
	}
	if hash, seen := readFirstSeen(t); hash != "0xtxA" || seen == nil || !seen.UTC().Equal(observed) {
		t.Fatalf("a reprocess lost the observation: (%q, %v)", hash, seen)
	}

	// A sibling block for the same height puts an unobserved transaction in
	// the slot: it must carry NULL, not the previous occupant's timestamp.
	sibling := models.IndexedBlock{ChainID: integrationChainID, BlockNumber: 500, BlockHash: "0xh500b", ParentHash: "0xp499"}
	other := integrationBlob(500, 0, "0xtxB", "0xs2", true)
	other.Timestamp = timestamp
	if err := idx.insertBlockData([]models.Blob{other}, sibling, integrationBlockMetrics(500, timestamp), nil, 0); err != nil {
		t.Fatalf("insertBlockData(B): %v", err)
	}
	hash, seen := readFirstSeen(t)
	if hash != "0xtxB" {
		t.Fatalf("tx_hash = %q, want 0xtxB", hash)
	}
	if seen != nil {
		t.Fatalf("first_seen_at = %v, want NULL: the observation belonged to 0xtxA", seen)
	}
}

// The pending poll can lose the writer-lock race to the block worker, which
// then stores the confirmed blob with no first_seen_at. The poll's captured
// observation must still land rather than being discarded with the
// suppressed pending write.
func TestIntegrationSuppressedPendingStillRecordsFirstSeen(t *testing.T) {
	idx, _ := newIntegrationIndexer(t)
	ctx := context.Background()
	timestamp := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	observed := timestamp.Add(-15 * time.Second)

	// The block worker won the lock: the confirmed row exists with no
	// observation, because there was no pending row to promote.
	indexed := models.IndexedBlock{ChainID: integrationChainID, BlockNumber: 600, BlockHash: "0xh600", ParentHash: "0xp599"}
	confirmed := integrationBlob(600, 0, "0xraced", "0xs1", true)
	confirmed.Timestamp = timestamp
	if err := idx.insertBlockData([]models.Blob{confirmed}, indexed, integrationBlockMetrics(600, timestamp), nil, 0); err != nil {
		t.Fatalf("insertBlockData(): %v", err)
	}

	// The pending poll now gets the lock, finds the tx confirmed, and skips
	// the pending write — but records what it observed.
	pending := integrationBlob(models.PendingBlockNumber, 0, "0xraced", "0xs1", false)
	pending.Nonce = 7
	pending.Timestamp = observed
	if err := idx.insertPendingBlobs([]models.Blob{pending}); err != nil {
		t.Fatalf("insertPendingBlobs(): %v", err)
	}

	var row struct {
		FirstSeenAt *time.Time `db:"first_seen_at"`
	}
	if err := idx.db.GetContext(ctx, &row,
		"SELECT first_seen_at FROM blobs WHERE chain_id = $1 AND block_number = 600 AND blob_index = 0",
		integrationChainID); err != nil {
		t.Fatalf("read blob row: %v", err)
	}
	if row.FirstSeenAt == nil || !row.FirstSeenAt.UTC().Equal(observed) {
		t.Fatalf("first_seen_at = %v, want the observed %s", row.FirstSeenAt, observed)
	}

	// No pending rows were created, and a later, later-timestamped poll does
	// not move the value back.
	var pooled int
	if err := idx.db.GetContext(ctx, &pooled,
		"SELECT COUNT(*) FROM mempool_blobs WHERE chain_id = $1 AND tx_hash = $2",
		integrationChainID, "0xraced"); err != nil {
		t.Fatalf("count pooled rows: %v", err)
	}
	if pooled != 0 {
		t.Fatalf("a confirmed tx must not be resurrected into the pool, got %d rows", pooled)
	}

	late := pending
	late.Timestamp = timestamp.Add(-time.Second)
	if err := idx.insertPendingBlobs([]models.Blob{late}); err != nil {
		t.Fatalf("insertPendingBlobs(late): %v", err)
	}
	if err := idx.db.GetContext(ctx, &row,
		"SELECT first_seen_at FROM blobs WHERE chain_id = $1 AND block_number = 600 AND blob_index = 0",
		integrationChainID); err != nil {
		t.Fatalf("re-read blob row: %v", err)
	}
	if row.FirstSeenAt == nil || !row.FirstSeenAt.UTC().Equal(observed) {
		t.Fatalf("a later poll overwrote the earliest observation: %v", row.FirstSeenAt)
	}
}

// A live block that commits before an earlier block in its window (mainnet
// 26020403 landed 67ms before 26020402) must not snapshot the pool: the
// earlier block's transactions are still in it and would be blamed on this
// builder. The block keeps its identity with candidate_snapshot = false, and
// the earlier block, whose own window is complete, snapshots normally.
func TestIntegrationOutOfOrderCommitSkipsTheSnapshot(t *testing.T) {
	idx, database := newIntegrationIndexer(t)
	ctx := context.Background()
	idx.candidateSnapshotMaxLag = time.Minute
	idx.candidateMinAge = 6 * time.Second

	blockTime := time.Now().UTC().Truncate(time.Second)
	seenAt := blockTime.Add(-time.Minute)

	// Everything below 501 is committed; 502 arrives before 501 does.
	seedCommittedPredecessors(t, idx, 501)
	seedPendingCandidate(t, idx, "0xincluded-by-501", "0xs1", 1, 2, "9", seenAt)
	seedPendingCandidate(t, idx, "0xskipped-by-both", "0xs2", 1, 1, "30", seenAt)

	late := models.IndexedBlock{ChainID: integrationChainID, BlockNumber: 502, BlockHash: "0xh502", ParentHash: "0xh501"}
	lateBuilder := integrationBuilder(502, blockTime, []byte("Titan (titanbuilder.xyz)"), "0xTitan")
	if err := idx.insertBlockData(nil, late, integrationBlockMetrics(502, blockTime), lateBuilder, 0); err != nil {
		t.Fatalf("insertBlockData(502) error = %v", err)
	}
	lateRow := readBuilderRow(t, idx, 502)
	if lateRow.BuilderKey != "titan" {
		t.Fatalf("the builder identity must be stored regardless, got %+v", lateRow)
	}
	if lateRow.CandidateSnapshot || lateRow.PendingCandidateTxs != nil || lateRow.EligibleSkippedTxs != nil {
		t.Fatalf("expected no snapshot for a block whose predecessor is uncommitted, got %+v", lateRow)
	}
	var lateCandidates int
	if err := database.GetContext(ctx, &lateCandidates,
		"SELECT COUNT(*) FROM blob_inclusion_candidates WHERE chain_id = $1 AND block_number = 502", integrationChainID); err != nil {
		t.Fatalf("count candidates: %v", err)
	}
	if lateCandidates != 0 {
		t.Fatalf("expected no candidate rows for 502, got %d", lateCandidates)
	}

	// Now 501 commits, including one of the pending transactions.
	confirmed := integrationBlob(501, 0, "0xincluded-by-501", "0xs1", true)
	confirmed.Nonce = 1
	confirmed.Timestamp = blockTime.Add(-12 * time.Second)
	early := models.IndexedBlock{ChainID: integrationChainID, BlockNumber: 501, BlockHash: "0xh501", ParentHash: "0xh500"}
	earlyBuilder := integrationBuilder(501, confirmed.Timestamp, []byte("beaverbuild.org"), "0xBeaver")
	if err := idx.insertBlockData([]models.Blob{confirmed}, early, integrationBlockMetrics(501, confirmed.Timestamp), earlyBuilder, 0); err != nil {
		t.Fatalf("insertBlockData(501) error = %v", err)
	}
	earlyRow := readBuilderRow(t, idx, 501)
	if !earlyRow.CandidateSnapshot || earlyRow.PendingCandidateTxs == nil || *earlyRow.PendingCandidateTxs != 1 ||
		earlyRow.EligibleSkippedTxs == nil || *earlyRow.EligibleSkippedTxs != 1 ||
		earlyRow.EligibleSkippedMaxTip == nil || *earlyRow.EligibleSkippedMaxTip != "30" {
		t.Fatalf("501's window is complete, so it must snapshot the pool it left: %+v", earlyRow)
	}
	var earlyHashes []string
	if err := database.SelectContext(ctx, &earlyHashes,
		"SELECT tx_hash FROM blob_inclusion_candidates WHERE chain_id = $1 AND block_number = 501", integrationChainID); err != nil {
		t.Fatalf("read candidates: %v", err)
	}
	if len(earlyHashes) != 1 || earlyHashes[0] != "0xskipped-by-both" {
		t.Fatalf("501 candidate rows = %v, want exactly [0xskipped-by-both]", earlyHashes)
	}

	// A second live pass over 502 (the walker and the WebSocket follower
	// both queue the tip) now finds its window complete. The first pass
	// stored no snapshot, so nothing is overwritten, and the pool it reads
	// is the pool after 501 — the one 502's builder faced — so it snapshots.
	if err := idx.insertBlockData(nil, late, integrationBlockMetrics(502, blockTime), lateBuilder, 0); err != nil {
		t.Fatalf("insertBlockData(502) second pass error = %v", err)
	}
	if again := readBuilderRow(t, idx, 502); !again.CandidateSnapshot || again.PendingCandidateTxs == nil || *again.PendingCandidateTxs != 1 {
		t.Fatalf("a later live pass with a complete window must snapshot: %+v", again)
	}
}

// The maintenance repair removes candidate rows an earlier binary wrote for
// transactions a lower block had already included, and recomputes the
// aggregates from the rows that remain.
func TestIntegrationRepairIncludedCandidates(t *testing.T) {
	idx, database := newIntegrationIndexer(t)
	ctx := context.Background()
	idx.candidateSnapshotMaxLag = time.Minute
	idx.candidateMinAge = 6 * time.Second

	blockTime := time.Now().UTC().Truncate(time.Second)
	seenAt := blockTime.Add(-time.Minute)

	// 600's window reads as committed (the rows exist) although 599's blobs
	// have not been written yet — the pre-gate binary's situation.
	seedCommittedPredecessors(t, idx, 600)
	seedCommittedPredecessors(t, idx, 599)
	seedPendingCandidate(t, idx, "0xstale-eligible", "0xs1", 1, 2, "900", seenAt)
	seedPendingCandidate(t, idx, "0xstale-gap", "0xs1", 2, 1, "5", seenAt)
	seedPendingCandidate(t, idx, "0xreal", "0xs2", 1, 3, "40", seenAt)

	indexed := models.IndexedBlock{ChainID: integrationChainID, BlockNumber: 600, BlockHash: "0xh600", ParentHash: "0xh599"}
	builder := integrationBuilder(600, blockTime, []byte("Titan (titanbuilder.xyz)"), "0xTitan")
	if err := idx.insertBlockData(nil, indexed, integrationBlockMetrics(600, blockTime), builder, 0); err != nil {
		t.Fatalf("insertBlockData(600) error = %v", err)
	}
	before := readBuilderRow(t, idx, 600)
	if before.PendingCandidateTxs == nil || *before.PendingCandidateTxs != 3 ||
		before.EligibleSkippedTxs == nil || *before.EligibleSkippedTxs != 2 ||
		before.EligibleSkippedMaxTip == nil || *before.EligibleSkippedMaxTip != "900" {
		t.Fatalf("unexpected snapshot before the repair: %+v", before)
	}

	// 599 lands afterwards and confirms the first sender's nonce-1 tx.
	confirmed := integrationBlob(599, 0, "0xstale-eligible", "0xs1", true)
	confirmed.Nonce = 1
	confirmed.Timestamp = blockTime.Add(-12 * time.Second)
	early := models.IndexedBlock{ChainID: integrationChainID, BlockNumber: 599, BlockHash: "0xh599", ParentHash: "0xh598"}
	if err := idx.insertBlockData([]models.Blob{confirmed}, early, integrationBlockMetrics(599, confirmed.Timestamp),
		integrationBuilder(599, confirmed.Timestamp, []byte("beaverbuild.org"), "0xBeaver"), 0); err != nil {
		t.Fatalf("insertBlockData(599) error = %v", err)
	}

	// Nothing to repair before the stale row exists is a no-op; with it, the
	// row goes and the aggregates follow the remaining rows.
	idx.repairIncludedCandidates(ctx)

	after := readBuilderRow(t, idx, 600)
	if !after.CandidateSnapshot || after.PendingCandidateTxs == nil || *after.PendingCandidateTxs != 2 ||
		after.EligibleSkippedTxs == nil || *after.EligibleSkippedTxs != 1 ||
		after.EligibleSkippedBlobs == nil || *after.EligibleSkippedBlobs != 3 ||
		after.EligibleSkippedMaxTip == nil || *after.EligibleSkippedMaxTip != "40" {
		t.Fatalf("aggregates after the repair = %s, want pending=2 skipped_txs=1 skipped_blobs=3 max_tip=40", describeBuilderRow(after))
	}

	type candidateRow struct {
		TxHash string `db:"tx_hash"`
		Reason string `db:"reason"`
	}
	var remaining []candidateRow
	if err := database.SelectContext(ctx, &remaining,
		"SELECT tx_hash, reason FROM blob_inclusion_candidates WHERE chain_id = $1 AND block_number = 600 ORDER BY tx_hash",
		integrationChainID); err != nil {
		t.Fatalf("read candidates: %v", err)
	}
	if len(remaining) != 2 || remaining[0].TxHash != "0xreal" || remaining[1].TxHash != "0xstale-gap" {
		t.Fatalf("remaining candidate rows = %+v, want [0xreal 0xstale-gap]", remaining)
	}
	// The nonce-gap row keeps its reason: the repair does not reclassify.
	if remaining[1].Reason != models.CandidateNonceGap {
		t.Fatalf("0xstale-gap reason = %q, want it left as %q", remaining[1].Reason, models.CandidateNonceGap)
	}

	// A second run finds nothing and changes nothing.
	idx.repairIncludedCandidates(ctx)
	if again := readBuilderRow(t, idx, 600); describeBuilderRow(again) != describeBuilderRow(after) {
		t.Fatalf("an idle repair changed the row:\n before = %s\n after  = %s", describeBuilderRow(after), describeBuilderRow(again))
	}

	// 599 itself snapshotted the pool it left (its window was complete) and
	// is untouched by the repair: no row of its own names a lower block.
	if early := readBuilderRow(t, idx, 599); !early.CandidateSnapshot || early.PendingCandidateTxs == nil || *early.PendingCandidateTxs != 2 {
		t.Fatalf("599's snapshot = %s, want the two rows it left pending", describeBuilderRow(early))
	}
}

// A pending transaction superseded by its own fee bump — same sender and
// nonce, different hash, confirmed in a lower block — never appears in
// blobs under its hash. The repair finds it through blob_replacements, which
// the confirming block's superseded-delete writes in the same transaction.
func TestIntegrationRepairRemovesSupersededCandidates(t *testing.T) {
	idx, database := newIntegrationIndexer(t)
	ctx := context.Background()
	idx.candidateSnapshotMaxLag = time.Minute
	idx.candidateMinAge = 6 * time.Second

	blockTime := time.Now().UTC().Truncate(time.Second)
	seenAt := blockTime.Add(-time.Minute)

	seedCommittedPredecessors(t, idx, 700)
	seedCommittedPredecessors(t, idx, 699)
	seedPendingCandidate(t, idx, "0xold-hash", "0xs1", 7, 2, "900", seenAt)
	seedPendingCandidate(t, idx, "0xreal", "0xs2", 1, 1, "40", seenAt)

	indexed := models.IndexedBlock{ChainID: integrationChainID, BlockNumber: 700, BlockHash: "0xh700", ParentHash: "0xh699"}
	builder := integrationBuilder(700, blockTime, []byte("Titan (titanbuilder.xyz)"), "0xTitan")
	if err := idx.insertBlockData(nil, indexed, integrationBlockMetrics(700, blockTime), builder, 0); err != nil {
		t.Fatalf("insertBlockData(700) error = %v", err)
	}
	if before := readBuilderRow(t, idx, 700); before.EligibleSkippedTxs == nil || *before.EligibleSkippedTxs != 2 {
		t.Fatalf("unexpected snapshot before the repair: %s", describeBuilderRow(before))
	}

	// 699 lands afterwards and confirms the fee bump of the first sender's
	// nonce-7 transaction under a new hash.
	bump := integrationBlob(699, 0, "0xbumped-hash", "0xs1", true)
	bump.Nonce = 7
	bump.Timestamp = blockTime.Add(-12 * time.Second)
	early := models.IndexedBlock{ChainID: integrationChainID, BlockNumber: 699, BlockHash: "0xh699", ParentHash: "0xh698"}
	if err := idx.insertBlockData([]models.Blob{bump}, early, integrationBlockMetrics(699, bump.Timestamp),
		integrationBuilder(699, bump.Timestamp, []byte("beaverbuild.org"), "0xBeaver"), 0); err != nil {
		t.Fatalf("insertBlockData(699) error = %v", err)
	}
	var logged int
	if err := database.GetContext(ctx, &logged,
		"SELECT COUNT(*) FROM blob_replacements WHERE chain_id = $1 AND replaced_tx_hash = $2 AND replacement_tx_hash = $3",
		integrationChainID, "0xold-hash", "0xbumped-hash"); err != nil {
		t.Fatalf("count replacements: %v", err)
	}
	if logged != 1 {
		t.Fatalf("expected the confirming block to log the replacement, got %d rows", logged)
	}

	idx.repairIncludedCandidates(ctx)

	after := readBuilderRow(t, idx, 700)
	if after.PendingCandidateTxs == nil || *after.PendingCandidateTxs != 1 ||
		after.EligibleSkippedTxs == nil || *after.EligibleSkippedTxs != 1 ||
		after.EligibleSkippedBlobs == nil || *after.EligibleSkippedBlobs != 1 ||
		after.EligibleSkippedMaxTip == nil || *after.EligibleSkippedMaxTip != "40" {
		t.Fatalf("aggregates after the repair = %s, want pending=1 skipped_txs=1 skipped_blobs=1 max_tip=40", describeBuilderRow(after))
	}
	var remaining []string
	if err := database.SelectContext(ctx, &remaining,
		"SELECT tx_hash FROM blob_inclusion_candidates WHERE chain_id = $1 AND block_number = 700", integrationChainID); err != nil {
		t.Fatalf("read candidates: %v", err)
	}
	if len(remaining) != 1 || remaining[0] != "0xreal" {
		t.Fatalf("remaining candidate rows = %v, want exactly [0xreal]", remaining)
	}
}

// Successive fee bumps: A is in block 800's snapshot, then B bumps A while
// still pending (the pending path logs A→B and evicts A), then block 799
// confirms C, which supersedes B (B→C). C is the only hash in blobs, so the
// repair has to walk the chain from A to reach it.
func TestIntegrationRepairFollowsTheReplacementChain(t *testing.T) {
	idx, database := newIntegrationIndexer(t)
	ctx := context.Background()
	idx.candidateSnapshotMaxLag = time.Minute
	idx.candidateMinAge = 6 * time.Second

	blockTime := time.Now().UTC().Truncate(time.Second)
	seenAt := blockTime.Add(-time.Minute)

	seedCommittedPredecessors(t, idx, 800)
	seedCommittedPredecessors(t, idx, 799)
	seedPendingCandidate(t, idx, "0xchain-a", "0xs1", 9, 2, "900", seenAt)
	seedPendingCandidate(t, idx, "0xreal", "0xs2", 1, 1, "40", seenAt)

	indexed := models.IndexedBlock{ChainID: integrationChainID, BlockNumber: 800, BlockHash: "0xh800", ParentHash: "0xh799"}
	builder := integrationBuilder(800, blockTime, []byte("Titan (titanbuilder.xyz)"), "0xTitan")
	if err := idx.insertBlockData(nil, indexed, integrationBlockMetrics(800, blockTime), builder, 0); err != nil {
		t.Fatalf("insertBlockData(800) error = %v", err)
	}
	if before := readBuilderRow(t, idx, 800); before.EligibleSkippedTxs == nil || *before.EligibleSkippedTxs != 2 {
		t.Fatalf("unexpected snapshot before the repair: %s", describeBuilderRow(before))
	}

	// B bumps A in the pool: the pending path evicts A and logs A→B.
	seedPendingCandidate(t, idx, "0xchain-b", "0xs1", 9, 2, "950", seenAt.Add(time.Second))

	// 799 lands afterwards confirming C, which supersedes B: B→C.
	confirmed := integrationBlob(799, 0, "0xchain-c", "0xs1", true)
	confirmed.Nonce = 9
	confirmed.Timestamp = blockTime.Add(-12 * time.Second)
	early := models.IndexedBlock{ChainID: integrationChainID, BlockNumber: 799, BlockHash: "0xh799", ParentHash: "0xh798"}
	if err := idx.insertBlockData([]models.Blob{confirmed}, early, integrationBlockMetrics(799, confirmed.Timestamp),
		integrationBuilder(799, confirmed.Timestamp, []byte("beaverbuild.org"), "0xBeaver"), 0); err != nil {
		t.Fatalf("insertBlockData(799) error = %v", err)
	}
	var hops []string
	if err := database.SelectContext(ctx, &hops,
		"SELECT replaced_tx_hash || '>' || replacement_tx_hash FROM blob_replacements WHERE chain_id = $1 ORDER BY 1",
		integrationChainID); err != nil {
		t.Fatalf("read replacements: %v", err)
	}
	if len(hops) != 2 || hops[0] != "0xchain-a>0xchain-b" || hops[1] != "0xchain-b>0xchain-c" {
		t.Fatalf("replacement log = %v, want [0xchain-a>0xchain-b 0xchain-b>0xchain-c]", hops)
	}

	if !idx.repairIncludedCandidates(ctx) {
		t.Fatal("repair reported failure")
	}

	after := readBuilderRow(t, idx, 800)
	if after.PendingCandidateTxs == nil || *after.PendingCandidateTxs != 1 ||
		after.EligibleSkippedTxs == nil || *after.EligibleSkippedTxs != 1 ||
		after.EligibleSkippedMaxTip == nil || *after.EligibleSkippedMaxTip != "40" {
		t.Fatalf("aggregates after the repair = %s, want pending=1 skipped_txs=1 max_tip=40", describeBuilderRow(after))
	}
	var remaining []string
	if err := database.SelectContext(ctx, &remaining,
		"SELECT tx_hash FROM blob_inclusion_candidates WHERE chain_id = $1 AND block_number = 800", integrationChainID); err != nil {
		t.Fatalf("read candidates: %v", err)
	}
	if len(remaining) != 1 || remaining[0] != "0xreal" {
		t.Fatalf("remaining candidate rows = %v, want exactly [0xreal]", remaining)
	}
}
