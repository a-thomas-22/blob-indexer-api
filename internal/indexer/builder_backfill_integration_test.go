//go:build integration

package indexer

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/a-thomas-22/blob-indexer-api/internal/db"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
)

// These tests exercise the historical builder backfill against a real
// Postgres: the anti-join that lists builder-less blocks, the
// ON CONFLICT DO NOTHING insert, the in-place tx_index fill, and the
// checkpoint. None of it is observable through sqlmock.

type backfilledBlobRow struct {
	TxIndex     *int       `db:"tx_index"`
	FirstSeenAt *time.Time `db:"first_seen_at"`
}

// seedBuilderlessBlock records a block and its blob rows the way history
// indexed before migration 000017 looks: an indexed block with metrics and
// blobs, but no block_builders row and no tx_index. blockHash is what
// indexed_blocks stores, which the backfill compares against the hash the
// node answers with, so it must be the fetched block's own hash.
func seedBuilderlessBlock(t *testing.T, idx *Indexer, blockNumber int64, timestamp time.Time, txHash, blockHash string) {
	t.Helper()
	blob := integrationBlob(blockNumber, 0, txHash, "0xsender", true)
	blob.Timestamp = timestamp
	indexed := models.IndexedBlock{
		ChainID:     integrationChainID,
		BlockNumber: blockNumber,
		BlockHash:   blockHash,
		ParentHash:  "0xp" + txHash,
	}
	if err := idx.insertBlockData([]models.Blob{blob}, indexed,
		integrationBlockMetrics(blockNumber, timestamp), nil, 0); err != nil {
		t.Fatalf("seed block %d: %v", blockNumber, err)
	}
}

func readBackfilledBlob(t *testing.T, idx *Indexer, blockNumber int64) backfilledBlobRow {
	t.Helper()
	var row backfilledBlobRow
	if err := idx.db.GetContext(context.Background(), &row,
		"SELECT tx_index, first_seen_at FROM blobs WHERE chain_id = $1 AND block_number = $2 AND blob_index = 0",
		integrationChainID, blockNumber); err != nil {
		t.Fatalf("read blob row %d: %v", blockNumber, err)
	}
	return row
}

// The backfill gives every builder-less indexed block a row, newest first,
// fills the blob rows' tx_index from the same fetch, checkpoints its floor,
// retires the oldest-first checkpoint earlier releases kept, and leaves the
// candidates table alone.
func TestIntegrationBuilderBackfillFillsHistory(t *testing.T) {
	idx, database := newIntegrationIndexer(t)
	ctx := context.Background()
	timestamp := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	// The fetched block carries a plain transaction ahead of the blob
	// transaction, so a correct tx_index is 1 rather than 0.
	blobTx := newSignedBlobTx(t, int64(integrationChainID), 7)
	plainTx := newSignedDynamicTx(t, int64(integrationChainID), 8)
	source := &fakeBlockSource{
		txs:      []*types.Transaction{plainTx, blobTx},
		extra:    []byte("Titan (titanbuilder.xyz)"),
		coinbase: common.HexToAddress("0xb01dface"),
	}
	idx.builderBackfill = builderBackfillSettings{
		enabled:      true,
		windowBlocks: 2,
		insertBatch:  2,
		fetchWorkers: 2,
		blockSource:  source.fetch,
	}

	for _, block := range []int64{100, 101, 102} {
		seedBuilderlessBlock(t, idx, block, timestamp, blobTx.Hash().Hex(), source.hashAt(block))
	}
	// The checkpoint an oldest-first walk of an earlier release left behind.
	if err := database.SetNetworkMetadata(ctx, integrationChainID, models.MetadataBlockBuilderBackfillBlock, "100"); err != nil {
		t.Fatalf("seed legacy checkpoint: %v", err)
	}

	var builderRows int
	if err := database.GetContext(ctx, &builderRows,
		"SELECT COUNT(*) FROM block_builders WHERE chain_id = $1", integrationChainID); err != nil {
		t.Fatalf("count block_builders: %v", err)
	}
	if builderRows != 0 {
		t.Fatalf("expected the seeded history to carry no builder rows, got %d", builderRows)
	}

	idx.runBuilderBackfill()

	for _, block := range []int64{100, 101, 102} {
		row := readBuilderRow(t, idx, block)
		if row.BuilderKey != "titan" || row.BuilderName != "Titan" {
			t.Fatalf("block %d labels = (%q, %q), want (titan, Titan)", block, row.BuilderKey, row.BuilderName)
		}
		// A historical block's pending pool was never observed, so the
		// snapshot flag is false and every aggregate stays NULL.
		if row.CandidateSnapshot || row.PendingCandidateTxs != nil || row.EligibleSkippedTxs != nil ||
			row.EligibleSkippedBlobs != nil || row.EligibleSkippedMaxTip != nil {
			t.Fatalf("block %d carries a fabricated candidate snapshot: %+v", block, row)
		}
		if row.TxCount != 2 {
			t.Fatalf("block %d tx_count = %d, want 2", block, row.TxCount)
		}
		// The last transaction is not sent by the fee recipient, so there is
		// no proposer payment to record.
		if row.ProposerPaymentWei != nil || row.ProposerPaymentTo != nil {
			t.Fatalf("block %d invented a proposer payment: %+v", block, row)
		}

		blob := readBackfilledBlob(t, idx, block)
		if blob.TxIndex == nil || *blob.TxIndex != 1 {
			t.Fatalf("block %d tx_index = %v, want 1", block, blob.TxIndex)
		}
		// first_seen_at records an observation a historical fetch cannot
		// make, so the backfill must never invent one.
		if blob.FirstSeenAt != nil {
			t.Fatalf("block %d gained a first_seen_at: %v", block, blob.FirstSeenAt)
		}
	}

	var candidates int
	if err := database.GetContext(ctx, &candidates,
		"SELECT COUNT(*) FROM blob_inclusion_candidates WHERE chain_id = $1", integrationChainID); err != nil {
		t.Fatalf("count blob_inclusion_candidates: %v", err)
	}
	if candidates != 0 {
		t.Fatalf("expected the backfill to leave the candidates table alone, got %d rows", candidates)
	}

	// Two-block windows over 100..102 walk [101,102] before [100,100], so
	// the oldest block is the last one fetched.
	order := source.fetchOrder()
	if len(order) != 3 || order[2] != 100 {
		t.Fatalf("fetch order = %v, want the oldest block 100 fetched last", order)
	}

	floor, err := database.GetNetworkMetadata(ctx, integrationChainID, models.MetadataBlockBuilderBackfillFloor)
	if err != nil {
		t.Fatalf("read floor: %v", err)
	}
	if floor != "100" {
		t.Fatalf("floor = %q, want \"100\"", floor)
	}
	if _, err := database.GetNetworkMetadata(ctx, integrationChainID, models.MetadataBlockBuilderBackfillBlock); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected the oldest-first checkpoint to be retired, got err = %v", err)
	}

	// A second run finds nothing left to do: the resume point is below the
	// earliest indexed block, so no block is fetched again.
	before := len(source.fetched)
	idx.runBuilderBackfill()
	if len(source.fetched) != before {
		t.Fatalf("a caught-up backfill refetched blocks: %v", source.fetched)
	}
}

// A restart resumes below the floor and never lists the blocks above it, so
// blocks live indexing wrote after the previous run cost the walk nothing.
func TestIntegrationBuilderBackfillResumesBelowFloor(t *testing.T) {
	idx, database := newIntegrationIndexer(t)
	ctx := context.Background()
	timestamp := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	blobTx := newSignedBlobTx(t, int64(integrationChainID), 7)
	source := &fakeBlockSource{
		txs:      []*types.Transaction{blobTx},
		extra:    []byte("beaverbuild.org"),
		coinbase: common.HexToAddress("0xb01dface"),
	}
	idx.builderBackfill = builderBackfillSettings{
		enabled:      true,
		windowBlocks: 2,
		insertBatch:  2,
		fetchWorkers: 1,
		blockSource:  source.fetch,
	}

	// History 200..205 with no builder rows anywhere, and a floor saying the
	// previous run verified 204 and up; block 204 and 205 are deliberately
	// left builder-less to prove the walk does not look there.
	for _, block := range []int64{200, 201, 202, 203, 204, 205} {
		seedBuilderlessBlock(t, idx, block, timestamp, blobTx.Hash().Hex(), source.hashAt(block))
	}
	if err := database.SetNetworkMetadata(ctx, integrationChainID, models.MetadataBlockBuilderBackfillFloor, "204"); err != nil {
		t.Fatalf("seed floor: %v", err)
	}
	// A legacy checkpoint whose delete failed on the start that wrote the
	// floor: this start must still remove it.
	if err := database.SetNetworkMetadata(ctx, integrationChainID, models.MetadataBlockBuilderBackfillBlock, "150"); err != nil {
		t.Fatalf("seed legacy checkpoint: %v", err)
	}

	idx.runBuilderBackfill()

	if _, err := database.GetNetworkMetadata(ctx, integrationChainID, models.MetadataBlockBuilderBackfillBlock); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected the oldest-first checkpoint to be retired even with a floor present, got err = %v", err)
	}

	for _, block := range []int64{200, 201, 202, 203} {
		if row := readBuilderRow(t, idx, block); row.BuilderKey != "beaverbuild" {
			t.Fatalf("block %d builder_key = %q, want beaverbuild", block, row.BuilderKey)
		}
	}
	for _, block := range []int64{204, 205} {
		if source.count(uint64(block)) != 0 {
			t.Fatalf("block %d above the floor was fetched: %v", block, source.fetched)
		}
	}
	var above int
	if err := database.GetContext(ctx, &above,
		"SELECT COUNT(*) FROM block_builders WHERE chain_id = $1 AND block_number >= 204", integrationChainID); err != nil {
		t.Fatalf("count rows above the floor: %v", err)
	}
	if above != 0 {
		t.Fatalf("expected no rows above the floor, got %d", above)
	}
	order := source.fetchOrder()
	if len(order) != 4 || order[0] != 202 || order[1] != 203 || order[2] != 200 || order[3] != 201 {
		t.Fatalf("fetch order = %v, want [202 203 200 201]", order)
	}
	floor, err := database.GetNetworkMetadata(ctx, integrationChainID, models.MetadataBlockBuilderBackfillFloor)
	if err != nil {
		t.Fatalf("read floor: %v", err)
	}
	if floor != "200" {
		t.Fatalf("floor = %q, want \"200\"", floor)
	}
}

// A live insert that raced ahead of the backfill keeps its row, candidate
// snapshot and all: the backfill's insert is ON CONFLICT DO NOTHING, and its
// tx_index update only fills rows that have none.
func TestIntegrationBuilderBackfillDoesNotOverwriteLiveRow(t *testing.T) {
	idx, _ := newIntegrationIndexer(t)
	timestamp := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	blobTx := newSignedBlobTx(t, int64(integrationChainID), 3)
	blob := integrationBlob(300, 0, blobTx.Hash().Hex(), "0xsender", true)
	blob.Timestamp = timestamp
	liveIndex := 5
	blob.TxIndex = &liveIndex

	live := integrationBuilder(300, timestamp, []byte("beaverbuild.org"), "0xLiveBuilder")
	live.TxCount = 42
	pending, skippedTxs, skippedBlobs, maxTip := 4, 2, 3, "9000"
	live.CandidateSnapshot = true
	live.PendingCandidateTxs, live.EligibleSkippedTxs = &pending, &skippedTxs
	live.EligibleSkippedBlobs, live.EligibleSkippedMaxTip = &skippedBlobs, &maxTip

	indexed := models.IndexedBlock{ChainID: integrationChainID, BlockNumber: 300, BlockHash: "0xh300", ParentHash: "0xp299"}
	if err := idx.insertBlockData([]models.Blob{blob}, indexed,
		integrationBlockMetrics(300, timestamp), live, 0); err != nil {
		t.Fatalf("insertBlockData(live): %v", err)
	}

	// The batch the backfill had in flight when the live write landed: a
	// different builder for the same block, plus a different tx position.
	stale := integrationBuilder(300, timestamp, []byte("Titan (titanbuilder.xyz)"), "0xStaleBuilder")
	stale.TxCount = 1
	idx.builderBackfill = builderBackfillSettings{enabled: true, windowBlocks: 2, insertBatch: 2, fetchWorkers: 1}

	inserted, indexedRows, err := idx.writeBuilderBackfillBatch([]models.BlockBuilder{*stale},
		[]db.BlobTxIndexUpdate{{BlockNumber: 300, TxHash: blobTx.Hash().Hex(), TxIndex: 0}}, 0)
	if err != nil {
		t.Fatalf("writeBuilderBackfillBatch(): %v", err)
	}
	if inserted != 0 || indexedRows != 0 {
		t.Fatalf("(inserted, indexed) = (%d, %d), want (0, 0)", inserted, indexedRows)
	}

	row := readBuilderRow(t, idx, 300)
	if row.BuilderKey != "beaverbuild" || row.FeeRecipient != "0xLiveBuilder" || row.TxCount != 42 {
		t.Fatalf("the backfill overwrote the live row: %+v", row)
	}
	if !row.CandidateSnapshot || row.EligibleSkippedMaxTip == nil || *row.EligibleSkippedMaxTip != maxTip {
		t.Fatalf("the backfill erased the live candidate snapshot: %+v", row)
	}
	if got := readBackfilledBlob(t, idx, 300); got.TxIndex == nil || *got.TxIndex != liveIndex {
		t.Fatalf("tx_index = %v, want the live %d", got.TxIndex, liveIndex)
	}
}
