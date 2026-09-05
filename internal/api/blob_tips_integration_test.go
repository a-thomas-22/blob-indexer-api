//go:build integration

package api

// End-to-end check of the blob-tips chart queries against a real Postgres
// (TEST_DB_URL). The unit tests drive the handler through a mock that never
// parses SQL, so this is the only check that the two query constants are
// valid, that their columns unify with blobTipsChartRow, and that legacy
// rows without a recorded priority fee are counted but kept out of the fee
// statistics.

import (
	"context"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"

	"github.com/a-thomas-22/blob-indexer-api/internal/db"
	"github.com/a-thomas-22/blob-indexer-api/internal/testdb"
)

func TestBlobTipsQueriesAgainstRealPostgres(t *testing.T) {
	url := testdb.URL(t, "api_blob_tips")
	sqlxDB, err := sqlx.Connect("postgres", url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sqlxDB.Close()

	for _, stmt := range []string{
		"DROP SCHEMA IF EXISTS public CASCADE",
		"CREATE SCHEMA public",
		"GRANT ALL ON SCHEMA public TO PUBLIC",
	} {
		if _, err := sqlxDB.Exec(stmt); err != nil {
			t.Fatalf("reset schema (%s): %v", stmt, err)
		}
	}
	if err := db.RunMigrations(url); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ctx := context.Background()
	start := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)

	if _, err := sqlxDB.Exec(`
		INSERT INTO networks (chain_id, name, start_block) VALUES (1, 'mainnet', '0')
		ON CONFLICT (chain_id) DO NOTHING
	`); err != nil {
		t.Fatalf("seed network: %v", err)
	}
	if _, err := sqlxDB.Exec(`
		INSERT INTO blob_users (chain_id, address, name, category, first_seen, last_seen) VALUES
			(1, '0xOP', 'Optimism', 'rollup', $1, $1),
			(1, '0xARB', 'Arbitrum', 'rollup', $1, $1)
	`, start); err != nil {
		t.Fatalf("seed users: %v", err)
	}
	if _, err := sqlxDB.Exec(`
		INSERT INTO block_metrics (chain_id, block_number, block_timestamp, blob_count) VALUES
			(1, 100, $1, 3),
			(1, 101, $2, 1),
			(1, 200, $3, 2)
	`, start, start.Add(12*time.Second), start.Add(time.Hour)); err != nil {
		t.Fatalf("seed block metrics: %v", err)
	}

	// Hour 1: Optimism pays 5 and 3 gwei across two blobs, Arbitrum 1 gwei,
	// and an unattributed sender 1 gwei in the next block. Hour 2: one
	// Arbitrum blob at 1 gwei plus a legacy row with no recorded fee.
	if _, err := sqlxDB.Exec(`
		INSERT INTO blobs (
			chain_id, block_number, blob_index, tx_hash, from_address, user_attribution,
			blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
			timestamp, max_fee_per_blob_gas, blob_gas_used, versioned_hash,
			max_priority_fee_per_gas, max_fee_per_gas, priority_fee_per_gas
		) VALUES
			(1, 100, 0, '0xop1', '0xop', 'Optimism', 131072, 10, 2, 1310720, $1, 12, 131072, '0xvh1', 5000000000, 60000000000, 5000000000),
			(1, 100, 1, '0xop1', '0xop', 'Optimism', 131072, 10, 2, 1310720, $1, 12, 131072, '0xvh2', 5000000000, 60000000000, 5000000000),
			(1, 100, 2, '0xarb1', '0xarb', 'Arbitrum', 131072, 10, 2, 1310720, $1, 12, 131072, '0xvh3', 1000000000, 60000000000, 1000000000),
			(1, 101, 0, '0xanon', '0xnobody', '', 131072, 10, 2, 1310720, $2, 12, 131072, '0xvh4', 1000000000, 60000000000, 1000000000),
			(1, 200, 0, '0xarb2', '0xarb', 'Arbitrum', 131072, 10, 2, 1310720, $3, 12, 131072, '0xvh5', 1000000000, 60000000000, 1000000000),
			(1, 200, 1, '0xlegacy', '0xarb', 'Arbitrum', 131072, 10, 2, 1310720, $3, 12, 131072, '0xvh6', NULL, NULL, NULL)
	`, start, start.Add(12*time.Second), start.Add(time.Hour)); err != nil {
		t.Fatalf("seed blobs: %v", err)
	}
	// Optimism's second blob in block 100 is really 3 gwei; set it after the
	// bulk insert so the VALUES list above stays aligned.
	if _, err := sqlxDB.Exec(`UPDATE blobs SET priority_fee_per_gas = 3000000000 WHERE chain_id = 1 AND block_number = 100 AND blob_index = 1`); err != nil {
		t.Fatalf("adjust blob: %v", err)
	}

	fetch := func(query string, args ...interface{}) []blobTipsChartRow {
		t.Helper()
		var rows []blobTipsChartRow
		if err := sqlxDB.SelectContext(ctx, &rows, query, args...); err != nil {
			t.Fatalf("query failed: %v", err)
		}
		return rows
	}

	chart := chartRequest{Range: "24h", Granularity: "hour", BucketSeconds: 3600, StartTime: start, EndTime: end}
	rows := fetch(queryBlobTipsTimeChart, 1, start, end, int64(3600), 5)
	resp := buildBlobTipsChartResponse(1, "mainnet", chart, rows)

	if resp.Summary.TotalBlobs != 6 || resp.Summary.PricedBlobs != 5 {
		t.Fatalf("summary counts = %d/%d, want 6/5", resp.Summary.TotalBlobs, resp.Summary.PricedBlobs)
	}
	// Priced fees: 5, 3, 1, 1, 1 gwei => mean 2.2, median 1, p95 5, max 5.
	if resp.Summary.AveragePriorityFeeGwei != "2.2" || resp.Summary.MedianPriorityFeeGwei != "1" || resp.Summary.P95PriorityFeeGwei != "5" || resp.Summary.MaxPriorityFeeGwei != "5" {
		t.Fatalf("unexpected summary fees: %+v", resp.Summary)
	}
	if len(resp.Points) != 2 {
		t.Fatalf("expected 2 hourly buckets, got %d: %+v", len(resp.Points), resp.Points)
	}
	hour1 := resp.Points[0]
	if hour1.BlobCount != 4 || hour1.AveragePriorityFeeGwei != "2.5" || hour1.MedianPriorityFeeGwei != "1" || hour1.MaxPriorityFeeGwei != "5" {
		t.Fatalf("unexpected hour 1 stats: %+v", hour1)
	}
	if got := hour1.Values["optimism"]; got.BlobCount != 2 || got.AveragePriorityFeeGwei != "4" || got.MaxPriorityFeeGwei != "5" {
		t.Fatalf("unexpected optimism hour 1 value: %+v", got)
	}
	if got := hour1.Values["unknown"]; got.BlobCount != 1 || got.AveragePriorityFeeGwei != "1" {
		t.Fatalf("unexpected unknown hour 1 value: %+v", got)
	}
	hour2 := resp.Points[1]
	// The legacy row is counted in total_blobs but not here.
	if hour2.BlobCount != 1 || hour2.MaxPriorityFeeGwei != "1" {
		t.Fatalf("unexpected hour 2 stats: %+v", hour2)
	}
	if got := hour2.Values["optimism"]; got.BlobCount != 0 || got.AveragePriorityFeeGwei != "0" {
		t.Fatalf("expected zero-filled optimism hour 2 value, got %+v", got)
	}

	// Optimism and Arbitrum tie on priced blobs, so the higher bid leads.
	if len(resp.Series) != 3 || resp.Series[0].Key != "optimism" || resp.Series[1].Key != "arbitrum" || resp.Series[2].Key != "unknown" {
		t.Fatalf("unexpected series: %+v", resp.Series)
	}
	if resp.Series[0].Name != "Optimism" || resp.Series[0].Category != "rollup" || resp.Series[0].Address != "0xop" {
		t.Fatalf("unexpected optimism series: %+v", resp.Series[0])
	}
	shares := resp.Summary.Shares
	if shares[0].BlobCount != 2 || shares[0].AveragePriorityFeeGwei != "4" || shares[0].MaxPriorityFeeGwei != "5" || shares[0].BlobSharePercent != 40 {
		t.Fatalf("unexpected optimism share: %+v", shares[0])
	}
	if shares[1].BlobCount != 2 || shares[1].AveragePriorityFeeGwei != "1" || shares[1].BlobSharePercent != 40 {
		t.Fatalf("unexpected arbitrum share: %+v", shares[1])
	}

	// Long-tail grouping: with a series limit of 1 only Optimism keeps its
	// identity; Arbitrum folds into "other" while unknown stays separate.
	limited := buildBlobTipsChartResponse(1, "mainnet", chart, fetch(queryBlobTipsTimeChart, 1, start, end, int64(3600), 1))
	if len(limited.Series) != 3 || limited.Series[0].Key != "optimism" || limited.Series[1].Key != "other" || limited.Series[2].Key != "unknown" {
		t.Fatalf("unexpected limited series: %+v", limited.Series)
	}
	if got := limited.Points[0].Values["other"]; got.BlobCount != 1 || got.MaxPriorityFeeGwei != "1" {
		t.Fatalf("unexpected other value: %+v", got)
	}
	if got := limited.Points[1].Values["other"]; got.BlobCount != 1 || got.AveragePriorityFeeGwei != "1" {
		t.Fatalf("unexpected other hour 2 value: %+v", got)
	}

	blockChart := chartRequest{Range: "1h", Granularity: "block", StartTime: start, EndTime: end}
	blockResp := buildBlobTipsChartResponse(1, "mainnet", blockChart, fetch(queryBlobTipsBlockChart, 1, start, end, 5))
	if len(blockResp.Points) != 3 {
		t.Fatalf("expected 3 block points, got %d: %+v", len(blockResp.Points), blockResp.Points)
	}
	block100 := blockResp.Points[0]
	if block100.StartBlock == nil || *block100.StartBlock != 100 || block100.BlobCount != 3 || block100.MaxPriorityFeeGwei != "5" || block100.AveragePriorityFeeGwei != "3" {
		t.Fatalf("unexpected block 100 point: %+v", block100)
	}
	block200 := blockResp.Points[2]
	if block200.StartBlock == nil || *block200.StartBlock != 200 || block200.BlobCount != 1 {
		t.Fatalf("unexpected block 200 point: %+v", block200)
	}
	if blockResp.Summary.TotalBlobs != 6 || blockResp.Summary.PricedBlobs != 5 {
		t.Fatalf("unexpected block summary: %+v", blockResp.Summary)
	}
}
