//go:build integration

package indexer

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	"github.com/a-thomas-22/blob-indexer-api/internal/testdb"
)

// These tests exercise the multi-row INSERT/UPDATE statements the indexer
// generates at runtime against a real Postgres, something sqlmock cannot
// validate: SQL syntax (placeholder generation, VALUES casts) and the
// statement-level stats triggers aggregating multi-row transition tables.

const integrationChainID = 424242

// newIntegrationIndexer resets this package's dedicated test database
// (derived from TEST_DB_URL), applies migrations, registers a test network,
// and returns an Indexer wired to the real DB.
func newIntegrationIndexer(t *testing.T) (*Indexer, *db.DB) {
	t.Helper()
	url := testdb.URL(t, "indexer")

	raw, err := sqlx.Connect("postgres", url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	for _, s := range []string{
		"DROP SCHEMA IF EXISTS public CASCADE",
		"CREATE SCHEMA public",
		"GRANT ALL ON SCHEMA public TO PUBLIC",
	} {
		if _, err := raw.Exec(s); err != nil {
			raw.Close()
			t.Fatalf("reset schema (%s): %v", s, err)
		}
	}
	raw.Close()

	if err := db.RunMigrations(url); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	database, err := db.Connect(context.Background(), config.DatabaseConfig{URL: url, MaxOpenConns: 5, MaxIdleConns: 2})
	if err != nil {
		t.Fatalf("db.Connect: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	network := config.NetworkConfig{Name: "integration-test", ChainID: integrationChainID, StartBlock: "0", Enabled: true}
	if err := database.UpsertNetworks(context.Background(), []config.NetworkConfig{network}); err != nil {
		t.Fatalf("UpsertNetworks: %v", err)
	}

	return NewForTest(database, &config.Config{}, network, 0), database
}

func integrationBlob(blockNumber int64, index int, txHash, sender string, confirmed bool) models.Blob {
	maxFee := "12"
	gasUsed := int64(131072)
	return models.Blob{
		ChainID:           integrationChainID,
		BlockNumber:       blockNumber,
		BlobIndex:         index,
		TxHash:            txHash,
		FromAddress:       sender,
		UserAttribution:   "",
		BlobSizeBytes:     131072,
		BaseFeePerBlobGas: "10",
		TipPerBlobGas:     "2",
		TotalCostWei:      "1000",
		Timestamp:         time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
		Confirmed:         confirmed,
		MaxFeePerBlobGas:  &maxFee,
		BlobGasUsed:       &gasUsed,
	}
}

func TestIntegrationInsertBlockDataMultiRow(t *testing.T) {
	idx, database := newIntegrationIndexer(t)
	ctx := context.Background()

	blobs := []models.Blob{
		integrationBlob(100, 0, "0xtx1", "0xaaa", true),
		integrationBlob(100, 1, "0xtx1", "0xaaa", true),
		integrationBlob(100, 2, "0xtx2", "0xbbb", true),
	}
	indexedBlock := models.IndexedBlock{ChainID: integrationChainID, BlockNumber: 100, BlockHash: "0xhash", ParentHash: "0xparent"}

	if err := idx.insertBlockData(blobs, indexedBlock, nil, 0); err != nil {
		t.Fatalf("insertBlockData() error = %v", err)
	}

	var blobCount int
	if err := database.GetContext(ctx, &blobCount, "SELECT COUNT(*) FROM blobs WHERE chain_id = $1", integrationChainID); err != nil {
		t.Fatalf("count blobs: %v", err)
	}
	if blobCount != 3 {
		t.Fatalf("expected 3 blobs, got %d", blobCount)
	}

	// The single statement must have fired the statement-level triggers with
	// the full 3-row transition table.
	var stats struct {
		TotalConfirmedBlobs int64  `db:"total_confirmed_blobs"`
		SumTotalCost        string `db:"sum_total_cost"`
	}
	if err := database.GetContext(ctx, &stats,
		"SELECT total_confirmed_blobs, sum_total_cost FROM network_blob_stats WHERE chain_id = $1", integrationChainID); err != nil {
		t.Fatalf("read network_blob_stats: %v", err)
	}
	if stats.TotalConfirmedBlobs != 3 {
		t.Fatalf("expected total_confirmed_blobs=3, got %d", stats.TotalConfirmedBlobs)
	}
	if cost, err := strconv.ParseFloat(stats.SumTotalCost, 64); err != nil || cost != 3000 {
		t.Fatalf("expected sum_total_cost=3000, got %q (parse err %v)", stats.SumTotalCost, err)
	}

	var userCount int64
	if err := database.GetContext(ctx, &userCount,
		"SELECT blob_count FROM blob_user_stats WHERE chain_id = $1 AND from_address = $2", integrationChainID, "0xaaa"); err != nil {
		t.Fatalf("read blob_user_stats: %v", err)
	}
	if userCount != 2 {
		t.Fatalf("expected blob_count=2 for 0xaaa, got %d", userCount)
	}

	// Re-inserting the same block takes the ON CONFLICT DO UPDATE path; the
	// update trigger's net delta must be zero.
	if err := idx.insertBlockData(blobs, indexedBlock, nil, 0); err != nil {
		t.Fatalf("insertBlockData() reinsert error = %v", err)
	}
	if err := database.GetContext(ctx, &stats.TotalConfirmedBlobs,
		"SELECT total_confirmed_blobs FROM network_blob_stats WHERE chain_id = $1", integrationChainID); err != nil {
		t.Fatalf("re-read network_blob_stats: %v", err)
	}
	if stats.TotalConfirmedBlobs != 3 {
		t.Fatalf("expected total_confirmed_blobs=3 after reinsert, got %d", stats.TotalConfirmedBlobs)
	}
}

// Regression test for the reorg fetch/cleanup race fallout: a stale-fork
// version of a block can land after handleReorg's cleanup with MORE blobs than
// the canonical block. The canonical reprocess upserts blob_index 0..n-1, so
// without the in-transaction trim the surplus rows would survive forever and
// the statement-level stats triggers would keep counting them.
func TestIntegrationInsertBlockDataTrimsStaleBlobRows(t *testing.T) {
	idx, database := newIntegrationIndexer(t)
	ctx := context.Background()

	// Stale-fork block 100 landed with three blobs from 0xaaa.
	stale := []models.Blob{
		integrationBlob(100, 0, "0xstale", "0xaaa", true),
		integrationBlob(100, 1, "0xstale", "0xaaa", true),
		integrationBlob(100, 2, "0xstale", "0xaaa", true),
	}
	staleIndexed := models.IndexedBlock{ChainID: integrationChainID, BlockNumber: 100, BlockHash: "0xstalehash", ParentHash: "0xstaleparent"}
	if err := idx.insertBlockData(stale, staleIndexed, nil, 0); err != nil {
		t.Fatalf("insertBlockData() stale insert error = %v", err)
	}

	// The canonical reprocess of block 100 carries a single blob from 0xbbb.
	canonical := []models.Blob{integrationBlob(100, 0, "0xcanon", "0xbbb", true)}
	canonicalIndexed := models.IndexedBlock{ChainID: integrationChainID, BlockNumber: 100, BlockHash: "0xcanonhash", ParentHash: "0xcanonparent"}
	if err := idx.insertBlockData(canonical, canonicalIndexed, nil, 0); err != nil {
		t.Fatalf("insertBlockData() canonical reprocess error = %v", err)
	}

	// Only the canonical row set may survive.
	var rows []struct {
		BlobIndex int    `db:"blob_index"`
		TxHash    string `db:"tx_hash"`
	}
	if err := database.SelectContext(ctx, &rows,
		"SELECT blob_index, tx_hash FROM blobs WHERE chain_id = $1 AND block_number = 100 ORDER BY blob_index", integrationChainID); err != nil {
		t.Fatalf("read blobs: %v", err)
	}
	if len(rows) != 1 || rows[0].BlobIndex != 0 || rows[0].TxHash != "0xcanon" {
		t.Fatalf("expected single canonical blob row [0 0xcanon], got %+v", rows)
	}

	// The trim ran inside the same transaction, so the stats triggers must
	// have net-counted it: one confirmed blob total, the stale sender zeroed.
	var totalConfirmed int64
	if err := database.GetContext(ctx, &totalConfirmed,
		"SELECT COALESCE((SELECT total_confirmed_blobs FROM network_blob_stats WHERE chain_id = $1), 0)", integrationChainID); err != nil {
		t.Fatalf("read network_blob_stats: %v", err)
	}
	if totalConfirmed != 1 {
		t.Fatalf("expected total_confirmed_blobs=1 after trim, got %d", totalConfirmed)
	}
	var staleSenderBlobs int64
	if err := database.GetContext(ctx, &staleSenderBlobs,
		"SELECT COALESCE(SUM(blob_count), 0) FROM blob_user_stats WHERE chain_id = $1 AND from_address = $2", integrationChainID, "0xaaa"); err != nil {
		t.Fatalf("read blob_user_stats for stale sender: %v", err)
	}
	if staleSenderBlobs != 0 {
		t.Fatalf("expected stale sender 0xaaa to hold 0 blobs after trim, got %d", staleSenderBlobs)
	}

	// A canonical block with no blobs at all must clear every stale row too —
	// the trim has to run even when the insert loop is skipped entirely.
	emptyIndexed := models.IndexedBlock{ChainID: integrationChainID, BlockNumber: 100, BlockHash: "0xemptyhash", ParentHash: "0xemptyparent"}
	if err := idx.insertBlockData(nil, emptyIndexed, nil, 0); err != nil {
		t.Fatalf("insertBlockData() empty reprocess error = %v", err)
	}
	var blobCount int
	if err := database.GetContext(ctx, &blobCount,
		"SELECT COUNT(*) FROM blobs WHERE chain_id = $1 AND block_number = 100", integrationChainID); err != nil {
		t.Fatalf("count blobs after empty reprocess: %v", err)
	}
	if blobCount != 0 {
		t.Fatalf("expected 0 blob rows after zero-blob reprocess, got %d", blobCount)
	}
	if err := database.GetContext(ctx, &totalConfirmed,
		"SELECT COALESCE((SELECT total_confirmed_blobs FROM network_blob_stats WHERE chain_id = $1), 0)", integrationChainID); err != nil {
		t.Fatalf("re-read network_blob_stats: %v", err)
	}
	if totalConfirmed != 0 {
		t.Fatalf("expected total_confirmed_blobs=0 after zero-blob reprocess, got %d", totalConfirmed)
	}
}

func TestIntegrationInsertPendingBlobsUpsert(t *testing.T) {
	idx, database := newIntegrationIndexer(t)
	ctx := context.Background()

	pending := []models.Blob{
		integrationBlob(models.PendingBlockNumber, 0, "0xpending", "0xccc", false),
		integrationBlob(models.PendingBlockNumber, 0, "0xpending", "0xccc", false),
	}

	// First poll: one multi-row upsert into mempool_blobs at the per-tx
	// blob ordinals 0..N-1.
	if err := idx.insertPendingBlobs(pending); err != nil {
		t.Fatalf("insertPendingBlobs() first poll error = %v", err)
	}

	var indices []int
	if err := database.SelectContext(ctx, &indices,
		"SELECT blob_index FROM mempool_blobs WHERE chain_id = $1 ORDER BY blob_index", integrationChainID); err != nil {
		t.Fatalf("read pending indices: %v", err)
	}
	if len(indices) != 2 || indices[0] != 0 || indices[1] != 1 {
		t.Fatalf("expected pending blob_index [0 1], got %v", indices)
	}

	// Second poll with changed fee data hits the ON CONFLICT DO UPDATE arm:
	// same rows, stable ordinals, new values.
	pending[0].TipPerBlobGas = "7"
	pending[1].TipPerBlobGas = "7"
	if err := idx.insertPendingBlobs(pending); err != nil {
		t.Fatalf("insertPendingBlobs() second poll error = %v", err)
	}

	var rows []struct {
		BlobIndex int    `db:"blob_index"`
		Tip       string `db:"tip_per_blob_gas"`
	}
	if err := database.SelectContext(ctx, &rows,
		"SELECT blob_index, tip_per_blob_gas FROM mempool_blobs WHERE chain_id = $1 ORDER BY blob_index", integrationChainID); err != nil {
		t.Fatalf("read pending rows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 pending rows after re-poll, got %d", len(rows))
	}
	for _, r := range rows {
		if tip, err := strconv.ParseFloat(r.Tip, 64); err != nil || tip != 7 {
			t.Fatalf("expected tip_per_blob_gas=7 on blob_index %d, got %q (parse err %v)", r.BlobIndex, r.Tip, err)
		}
	}
	if rows[0].BlobIndex != 0 || rows[1].BlobIndex != 1 {
		t.Fatalf("expected stable blob_index [0 1], got [%d %d]", rows[0].BlobIndex, rows[1].BlobIndex)
	}

	// A poll that sees fewer blobs for the tx trims the surplus ordinals.
	if err := idx.insertPendingBlobs(pending[:1]); err != nil {
		t.Fatalf("insertPendingBlobs() shrink poll error = %v", err)
	}
	if err := database.SelectContext(ctx, &indices,
		"SELECT blob_index FROM mempool_blobs WHERE chain_id = $1 ORDER BY blob_index", integrationChainID); err != nil {
		t.Fatalf("read trimmed indices: %v", err)
	}
	if len(indices) != 1 || indices[0] != 0 {
		t.Fatalf("expected trimmed blob_index [0], got %v", indices)
	}

	// Pending rows live outside blobs and must not count into network stats
	// or the per-sender rollups.
	var confirmed int64
	if err := database.GetContext(ctx, &confirmed,
		"SELECT COALESCE((SELECT total_confirmed_blobs FROM network_blob_stats WHERE chain_id = $1), 0)", integrationChainID); err != nil {
		t.Fatalf("read network_blob_stats: %v", err)
	}
	if confirmed != 0 {
		t.Fatalf("expected 0 confirmed blobs from pending rows, got %d", confirmed)
	}
	var senderRows int
	if err := database.GetContext(ctx, &senderRows,
		"SELECT COUNT(*) FROM blob_user_stats WHERE chain_id = $1", integrationChainID); err != nil {
		t.Fatalf("read blob_user_stats: %v", err)
	}
	if senderRows != 0 {
		t.Fatalf("expected no blob_user_stats rows from pending blobs, got %d", senderRows)
	}

	// Once the tx is confirmed in blobs, further polls are a no-op and do not
	// resurrect pending rows.
	confirmedBlob := integrationBlob(200, 0, "0xpending", "0xccc", true)
	indexedBlock := models.IndexedBlock{ChainID: integrationChainID, BlockNumber: 200, BlockHash: "0xhash200", ParentHash: "0xparent200"}
	if err := idx.insertBlockData([]models.Blob{confirmedBlob}, indexedBlock, nil, 0); err != nil {
		t.Fatalf("insertBlockData() promote error = %v", err)
	}
	var pendingLeft int
	if err := database.GetContext(ctx, &pendingLeft,
		"SELECT COUNT(*) FROM mempool_blobs WHERE chain_id = $1", integrationChainID); err != nil {
		t.Fatalf("count pending after promotion: %v", err)
	}
	if pendingLeft != 0 {
		t.Fatalf("expected promotion to clear mempool_blobs, got %d rows", pendingLeft)
	}
	if err := idx.insertPendingBlobs(pending[:1]); err != nil {
		t.Fatalf("insertPendingBlobs() post-confirm poll error = %v", err)
	}
	if err := database.GetContext(ctx, &pendingLeft,
		"SELECT COUNT(*) FROM mempool_blobs WHERE chain_id = $1", integrationChainID); err != nil {
		t.Fatalf("count pending after post-confirm poll: %v", err)
	}
	if pendingLeft != 0 {
		t.Fatalf("expected confirmed tx to suppress pending rows, got %d rows", pendingLeft)
	}
}

// A fee-bumped replacement reuses the sender's nonce under a new hash, so the
// replaced hash never confirms and — before the nonce column — only the TTL
// sweep removed its pending rows. Verify both cleanup sites against real
// Postgres: seeing the replacement pending (insertPendingBlobs) and confirming
// a same-(sender, nonce) tx in a block (insertBlockData), plus NULL-nonce
// legacy rows staying untouched by either.
func TestIntegrationMempoolReplacementCleanup(t *testing.T) {
	idx, database := newIntegrationIndexer(t)
	ctx := context.Background()

	pendingTxHashes := func() []string {
		t.Helper()
		var hashes []string
		if err := database.SelectContext(ctx, &hashes,
			"SELECT DISTINCT tx_hash FROM mempool_blobs WHERE chain_id = $1 ORDER BY tx_hash", integrationChainID); err != nil {
			t.Fatalf("read pending tx hashes: %v", err)
		}
		return hashes
	}
	insertPending := func(nonce uint64, txHash string, blobCount int) {
		t.Helper()
		blobs := make([]models.Blob, 0, blobCount)
		for i := 0; i < blobCount; i++ {
			b := integrationBlob(models.PendingBlockNumber, i, txHash, "0xsender", false)
			b.Nonce = nonce
			blobs = append(blobs, b)
		}
		if err := idx.insertPendingBlobs(blobs); err != nil {
			t.Fatalf("insertPendingBlobs(%s) error = %v", txHash, err)
		}
	}

	// Sender S has tx 0xorig pending at nonce 7 (two blobs), plus an
	// unrelated pending tx at nonce 8 that must survive every cleanup below.
	insertPending(7, "0xorig", 2)
	insertPending(8, "0xother", 1)

	// A fee bump at the same (sender, nonce) under a new hash: seeing it
	// pending must drop both of 0xorig's rows and only those.
	insertPending(7, "0xbump", 1)
	if got := pendingTxHashes(); len(got) != 2 || got[0] != "0xbump" || got[1] != "0xother" {
		t.Fatalf("expected pending [0xbump 0xother] after replacement, got %v", got)
	}

	// A second bump confirms in a block without ever being seen pending.
	// Storing the block must clear 0xbump via (sender, nonce) even though
	// 0xbump's hash appears nowhere in the block.
	final := integrationBlob(400, 0, "0xfinal", "0xsender", true)
	final.Nonce = 7
	indexedBlock := models.IndexedBlock{ChainID: integrationChainID, BlockNumber: 400, BlockHash: "0xhash400", ParentHash: "0xparent400"}
	if err := idx.insertBlockData([]models.Blob{final}, indexedBlock, nil, 0); err != nil {
		t.Fatalf("insertBlockData() confirm error = %v", err)
	}
	if got := pendingTxHashes(); len(got) != 1 || got[0] != "0xother" {
		t.Fatalf("expected pending [0xother] after confirmation, got %v", got)
	}

	// A row written by a pre-nonce binary holds NULL: no (sender, nonce)
	// cleanup may ever match it — NULL never equals — so it survives until
	// the TTL sweep.
	if _, err := database.ExecContext(ctx, `
		INSERT INTO mempool_blobs (
			chain_id, tx_hash, blob_index, from_address, user_attribution,
			blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
			timestamp
		) VALUES ($1, '0xlegacy', 0, '0xsender', '', 131072, 10, 2, 1000, $2)
	`, integrationChainID, time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("seed legacy NULL-nonce row: %v", err)
	}
	insertPending(9, "0xnine", 1)
	if got := pendingTxHashes(); len(got) != 3 || got[0] != "0xlegacy" || got[1] != "0xnine" || got[2] != "0xother" {
		t.Fatalf("expected pending [0xlegacy 0xnine 0xother], got %v", got)
	}
}

func TestIntegrationSetNetworkMetadataBatch(t *testing.T) {
	_, database := newIntegrationIndexer(t)
	ctx := context.Background()

	entries := []db.MetadataKV{
		{Key: "last_indexed_block", Value: "123"},
		{Key: "last_indexed_at", Value: "2026-07-01T12:00:00Z"},
	}
	if err := database.SetNetworkMetadataBatch(ctx, integrationChainID, entries); err != nil {
		t.Fatalf("SetNetworkMetadataBatch() insert error = %v", err)
	}

	// Second write upserts over the first.
	entries[0].Value = "124"
	if err := database.SetNetworkMetadataBatch(ctx, integrationChainID, entries); err != nil {
		t.Fatalf("SetNetworkMetadataBatch() upsert error = %v", err)
	}

	got, err := database.GetNetworkMetadata(ctx, integrationChainID, "last_indexed_block")
	if err != nil {
		t.Fatalf("GetNetworkMetadata: %v", err)
	}
	if got != "124" {
		t.Fatalf("expected last_indexed_block=124, got %q", got)
	}
}

// TestIntegrationStartupGapRecovery exercises the generate_series anti-join
// behind GetUnindexedBlocksInRange against real Postgres — sqlmock cannot
// validate that SQL. It simulates the post-crash state where parallel workers
// committed out of order: the watermark reached 106 while 103 and 105 never
// committed.
func TestIntegrationStartupGapRecovery(t *testing.T) {
	idx, database := newIntegrationIndexer(t)
	ctx := context.Background()

	// Empty table: with the earliest-indexed floor, min_indexed is NULL and
	// the scan must yield nothing rather than flagging the whole window as
	// missing; without the floor, every block in the range is missing.
	empty, err := database.GetUnindexedBlocksInRange(ctx, integrationChainID, 57, 106, 50, true)
	if err != nil {
		t.Fatalf("GetUnindexedBlocksInRange floored on empty table: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("expected no floored gaps for a network with no indexed rows, got %v", empty)
	}
	all, err := database.GetUnindexedBlocksInRange(ctx, integrationChainID, 57, 106, 50, false)
	if err != nil {
		t.Fatalf("GetUnindexedBlocksInRange unfloored on empty table: %v", err)
	}
	if len(all) != 50 || all[0] != 57 || all[49] != 106 {
		t.Fatalf("expected the full 57-106 range unfloored, got %d blocks %v", len(all), all)
	}

	for _, blockNumber := range []uint64{100, 101, 102, 104, 106} {
		if _, err := database.ExecContext(ctx,
			"INSERT INTO indexed_blocks (chain_id, block_number, block_hash, parent_hash) VALUES ($1, $2, $3, $4)",
			integrationChainID, blockNumber, "0xhash", "0xparent"); err != nil {
			t.Fatalf("insert indexed block %d: %v", blockNumber, err)
		}
	}

	// Floored (LATEST-style callers): the never-indexed prefix 57-99 is
	// suppressed; only the interior gaps surface.
	floored, err := database.GetUnindexedBlocksInRange(ctx, integrationChainID, 57, 106, 50, true)
	if err != nil {
		t.Fatalf("GetUnindexedBlocksInRange floored: %v", err)
	}
	if len(floored) != 2 || floored[0] != 103 || floored[1] != 105 {
		t.Fatalf("expected floored gaps [103 105], got %v", floored)
	}

	// Unfloored (knowable start): the missing prefix is a real gap — the
	// bootstrap-crash case where only the highest queued block committed.
	unfloored, err := database.GetUnindexedBlocksInRange(ctx, integrationChainID, 57, 106, 50, false)
	if err != nil {
		t.Fatalf("GetUnindexedBlocksInRange unfloored: %v", err)
	}
	if len(unfloored) != 45 || unfloored[0] != 57 || unfloored[42] != 99 || unfloored[43] != 103 || unfloored[44] != 105 {
		t.Fatalf("expected 57-99 plus 103 and 105 unfloored, got %d blocks %v", len(unfloored), unfloored)
	}

	// Indexer path with a numeric configured start: the window clamps to the
	// start and seeds exactly the crash-orphaned interior blocks.
	idx.network.StartBlock = "100"
	idx.startupGapScanBlocks = 50
	idx.seedStartupGapRecovery(106)

	idx.failedBlocksMu.Lock()
	defer idx.failedBlocksMu.Unlock()
	if len(idx.failedBlocks) != 2 || idx.failedBlocks[103] != 1 || idx.failedBlocks[105] != 1 {
		t.Fatalf("expected exactly blocks 103 and 105 seeded, got %v", idx.failedBlocks)
	}
}

// TestIntegrationReorgRecoveryMarkerLifecycle walks the persisted reorg
// recovery marker through its full life against real Postgres: handleReorg
// merges it into the deletion transaction, a fresh indexer (simulating the
// post-crash process) recovers the range from it, and the completion check
// keeps it while gaps remain and clears it once indexed_blocks covers the
// range again. This validates the marker upsert/read/delete SQL and the
// GetFirstUnindexedBlock verification, which sqlmock cannot.
func TestIntegrationReorgRecoveryMarkerLifecycle(t *testing.T) {
	idx, database := newIntegrationIndexer(t)
	ctx := context.Background()

	idx.maxReorgDepth = 64
	idx.ethClient, _ = newMockEthClient(t, 10)

	// Blocks 1-4 match the canonical chain; 5-8 carry stale-fork hashes. Block
	// 6 holds a blob so the reorg deletion has data to sweep.
	for blockNumber := uint64(1); blockNumber <= 8; blockNumber++ {
		hash := fmt.Sprintf("0xstale%d", blockNumber)
		parent := fmt.Sprintf("0xstale%d", blockNumber-1)
		if blockNumber <= 4 {
			canonical, err := idx.ethClient.GetBlockByNumber(ctx, blockNumber)
			if err != nil {
				t.Fatalf("get canonical block %d: %v", blockNumber, err)
			}
			hash = canonical.Hash().Hex()
			parent = canonical.ParentHash().Hex()
		}
		var blobs []models.Blob
		if blockNumber == 6 {
			blobs = []models.Blob{integrationBlob(int64(blockNumber), 0, "0xreorgtx", "0xddd", true)}
		}
		indexed := models.IndexedBlock{ChainID: integrationChainID, BlockNumber: int64(blockNumber), BlockHash: hash, ParentHash: parent}
		if err := idx.insertBlockData(blobs, indexed, nil, 0); err != nil {
			t.Fatalf("insertBlockData(%d): %v", blockNumber, err)
		}
	}

	// A prior reorg left an unrecovered marker [6, 9]; the new reorg's range
	// [5, 8] must merge with it, not clobber it.
	if err := database.SetNetworkMetadataBatch(ctx, integrationChainID, []db.MetadataKV{
		{Key: models.MetadataReorgRewindFrom, Value: "6"},
		{Key: models.MetadataReorgInvalidatedThrough, Value: "9"},
	}); err != nil {
		t.Fatalf("seed prior marker: %v", err)
	}

	// Block 5's parent mismatches: handleReorg walks back, confirms 4 as the
	// fork point, and deletes everything above it.
	if err := idx.handleReorg(5); !errors.Is(err, errReorgDetected) {
		t.Fatalf("expected errReorgDetected from handleReorg, got %v", err)
	}

	// No recovery signal was raised in this process before the reorg, so the
	// prior marker's range may never have been queued: the live signal must
	// cover the merged range, not just the fresh [5, 8].
	if from, through := idx.consumeReorgReset(); from != 5 || through != 9 {
		t.Fatalf("expected merged live signal [5 9], got [%d %d]", from, through)
	}

	var surviving []int64
	if err := database.SelectContext(ctx, &surviving,
		"SELECT block_number FROM indexed_blocks WHERE chain_id = $1 ORDER BY block_number", integrationChainID); err != nil {
		t.Fatalf("read surviving indexed blocks: %v", err)
	}
	if len(surviving) != 4 || surviving[0] != 1 || surviving[3] != 4 {
		t.Fatalf("expected only blocks 1-4 to survive the reorg, got %v", surviving)
	}
	var blobCount int
	if err := database.GetContext(ctx, &blobCount,
		"SELECT COUNT(*) FROM blobs WHERE chain_id = $1", integrationChainID); err != nil {
		t.Fatalf("count blobs after reorg: %v", err)
	}
	if blobCount != 0 {
		t.Fatalf("expected reorged blobs deleted, got %d rows", blobCount)
	}
	watermark, err := database.GetNetworkMetadata(ctx, integrationChainID, models.MetadataLastIndexedBlock)
	if err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	if watermark != "4" {
		t.Fatalf("expected watermark rewound to 4, got %q", watermark)
	}
	assertMarker := func(wantFrom, wantThrough string) {
		t.Helper()
		gotFrom, err := database.GetNetworkMetadata(ctx, integrationChainID, models.MetadataReorgRewindFrom)
		if err != nil {
			t.Fatalf("read marker rewind_from: %v", err)
		}
		gotThrough, err := database.GetNetworkMetadata(ctx, integrationChainID, models.MetadataReorgInvalidatedThrough)
		if err != nil {
			t.Fatalf("read marker invalidated_through: %v", err)
		}
		if gotFrom != wantFrom || gotThrough != wantThrough {
			t.Fatalf("expected marker [%s %s], got [%s %s]", wantFrom, wantThrough, gotFrom, gotThrough)
		}
	}
	assertMarker("5", "9")

	// Simulate the crash: a fresh indexer instance must recover the merged
	// range from the marker alone.
	recovered := NewForTest(database, &config.Config{}, idx.GetNetworkInfo(), 4)
	recovered.seedReorgRecoveryFromMarker()
	from, through := recovered.consumeReorgReset()
	if from != 5 || through != 9 {
		t.Fatalf("expected recovered range [5 9], got [%d %d]", from, through)
	}

	// The range is still missing: the completion check must keep the marker.
	recovered.maybeCompleteReorgRecovery()
	assertMarker("5", "9")

	// Re-index the invalidated range (canonical hashes this time).
	for blockNumber := uint64(5); blockNumber <= 9; blockNumber++ {
		canonical, err := idx.ethClient.GetBlockByNumber(ctx, blockNumber)
		if err != nil {
			t.Fatalf("get canonical block %d: %v", blockNumber, err)
		}
		indexed := models.IndexedBlock{
			ChainID:     integrationChainID,
			BlockNumber: int64(blockNumber),
			BlockHash:   canonical.Hash().Hex(),
			ParentHash:  canonical.ParentHash().Hex(),
		}
		if err := recovered.insertBlockData(nil, indexed, nil, 0); err != nil {
			t.Fatalf("reindex insertBlockData(%d): %v", blockNumber, err)
		}
	}

	// Coverage is verifiable now: the completion check must clear the marker.
	recovered.maybeCompleteReorgRecovery()
	var markerRows int
	if err := database.GetContext(ctx, &markerRows,
		"SELECT COUNT(*) FROM indexer_metadata WHERE chain_id = $1 AND key IN ($2, $3)",
		integrationChainID, models.MetadataReorgRewindFrom, models.MetadataReorgInvalidatedThrough); err != nil {
		t.Fatalf("count marker rows: %v", err)
	}
	if markerRows != 0 {
		t.Fatalf("expected marker cleared after full re-index, got %d rows", markerRows)
	}
}
