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
// classify.
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
