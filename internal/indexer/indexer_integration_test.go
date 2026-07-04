//go:build integration

package indexer

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
)

// These tests exercise the multi-row INSERT/UPDATE statements the indexer
// generates at runtime against a real Postgres, something sqlmock cannot
// validate: SQL syntax (placeholder generation, VALUES casts) and the
// statement-level stats triggers aggregating multi-row transition tables.

const integrationChainID = 424242

// newIntegrationIndexer resets the TEST_DB_URL database, applies migrations,
// registers a test network, and returns an Indexer wired to the real DB.
func newIntegrationIndexer(t *testing.T) (*Indexer, *db.DB) {
	t.Helper()
	url := os.Getenv("TEST_DB_URL")
	if url == "" {
		t.Skip("TEST_DB_URL not set; skipping integration test")
	}

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

	if err := idx.insertBlockData(blobs, indexedBlock, nil); err != nil {
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
	if err := idx.insertBlockData(blobs, indexedBlock, nil); err != nil {
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

func TestIntegrationInsertPendingBlobsMultiRow(t *testing.T) {
	idx, database := newIntegrationIndexer(t)
	ctx := context.Background()

	pending := []models.Blob{
		integrationBlob(-1, 0, "0xpending", "0xccc", false),
		integrationBlob(-1, 0, "0xpending", "0xccc", false),
	}

	// First poll: no existing rows → mismatch path → one multi-row INSERT.
	if err := idx.insertPendingBlobs(pending); err != nil {
		t.Fatalf("insertPendingBlobs() first poll error = %v", err)
	}

	var indices []int
	if err := database.SelectContext(ctx, &indices,
		"SELECT blob_index FROM blobs WHERE chain_id = $1 AND block_number < 0 ORDER BY blob_index", integrationChainID); err != nil {
		t.Fatalf("read pending indices: %v", err)
	}
	if len(indices) != 2 || indices[0] != 0 || indices[1] != 1 {
		t.Fatalf("expected pending blob_index [0 1], got %v", indices)
	}

	// Second poll with changed fee data: steady-state path → one
	// UPDATE ... FROM (VALUES ...). Indices must stay stable and the new
	// values must land.
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
		"SELECT blob_index, tip_per_blob_gas FROM blobs WHERE chain_id = $1 AND block_number < 0 ORDER BY blob_index", integrationChainID); err != nil {
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

	// Pending rows are unconfirmed and must not count into network stats.
	var confirmed int64
	if err := database.GetContext(ctx, &confirmed,
		"SELECT COALESCE((SELECT total_confirmed_blobs FROM network_blob_stats WHERE chain_id = $1), 0)", integrationChainID); err != nil {
		t.Fatalf("read network_blob_stats: %v", err)
	}
	if confirmed != 0 {
		t.Fatalf("expected 0 confirmed blobs from pending rows, got %d", confirmed)
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
