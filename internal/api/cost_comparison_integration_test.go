//go:build integration

package api

// Integration coverage for the cost-comparison chart queries against a real
// Postgres (TEST_DB_URL). The unit tests exercise the handler through sqlmock,
// which never parses SQL, so this is the only check that the query text is
// valid and that the execution-fee pricing math and its blob-fee proxy
// fallback behave as documented:
//   * buckets whose blocks carry a recorded base_fee_wei price the calldata
//     equivalent at the bucket's average execution base fee, so savings track
//     the market instead of collapsing to the constant 93.75% proxy ratio;
//   * buckets with no recorded execution base fee (legacy rows hold 0) keep
//     the blob-fee proxy and omit average_execution_base_fee_wei;
//   * the rollup-served paths (hourly source buckets, fine 60s source
//     buckets, and fine buckets recomputed by the startup backfill) return
//     exactly what the raw path returns.

import (
	"context"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db"
	"github.com/a-thomas-22/blob-indexer-api/internal/testdb"
)

func TestCostComparisonQueriesAgainstRealPostgres(t *testing.T) {
	// This test resets its schema, so it runs on this package's dedicated
	// database rather than TEST_DB_URL itself.
	url := testdb.URL(t, "api")
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

	// Recent timestamps keep the fine (60s) rollup triggers active: they skip
	// rows older than the 48h retention window. Hour truncation aligns the
	// range with the 3600s display buckets used below.
	start := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Hour)
	end := start.Add(2 * time.Hour)

	// Bucket A (hour 1): two blocks with recorded execution base fees of
	// 2000000 and 4000000 wei, so the bucket average is 3000000. Bucket B
	// (hour 2): one legacy block with base_fee_wei = 0 (nothing recorded).
	if _, err := sqlxDB.Exec(`
		INSERT INTO block_metrics (chain_id, block_number, block_timestamp, base_fee_wei) VALUES
			(1, 100, $1, 2000000),
			(1, 101, $2, 4000000),
			(1, 200, $3, 0)
	`, start, start.Add(12*time.Second), start.Add(time.Hour)); err != nil {
		t.Fatalf("seed block metrics: %v", err)
	}

	// Bucket A carries two 131072-byte blobs (262144 bytes total), each with
	// total_cost_wei chosen so the bucket's blob cost is exactly half of the
	// execution-priced calldata equivalent:
	//   262144 bytes * 16 gas/byte * 3000000 wei = 12582912000000 wei
	// Bucket B's blob prices with the proxy (blob base fee 25):
	//   131072 * 16 * 25 = 52428800 wei, and 3276800 / 52428800 = 6.25%,
	// reproducing the constant 93.75% savings of the proxy model.
	if _, err := sqlxDB.Exec(`
		INSERT INTO blobs (
			chain_id, block_number, blob_index, tx_hash, from_address, user_attribution,
			blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
			timestamp, max_fee_per_blob_gas, blob_gas_used, versioned_hash
		) VALUES
			(1, 100, 0, '0xcosta', '0xfrom', 'Rollup', 131072, 10, 2, 3145728000000, $1, 12, 131072, '0xvhcosta'),
			(1, 101, 0, '0xcostb', '0xfrom', 'Rollup', 131072, 20, 2, 3145728000000, $2, 24, 131072, '0xvhcostb'),
			(1, 200, 0, '0xcostc', '0xfrom', 'Rollup', 131072, 25, 2, 3276800, $3, 30, 131072, '0xvhcostc')
	`, start, start.Add(12*time.Second), start.Add(time.Hour)); err != nil {
		t.Fatalf("seed blobs: %v", err)
	}

	fetch := func(query string, args ...interface{}) []costComparisonChartRow {
		t.Helper()
		var rows []costComparisonChartRow
		if err := sqlxDB.SelectContext(ctx, &rows, query, args...); err != nil {
			t.Fatalf("query failed: %v", err)
		}
		return rows
	}

	raw := fetch(queryCostComparisonTimeChart, 1, "24h", start, end, int64(3600), "hour", calldataGasPerByte)
	if len(raw) != 2 {
		t.Fatalf("expected 2 raw buckets, got %d: %+v", len(raw), raw)
	}

	execBucket := raw[0]
	if execBucket.BlobCount != 2 || execBucket.BlobBytes != 262144 {
		t.Fatalf("unexpected execution bucket shape: %+v", execBucket)
	}
	if execBucket.CalldataEquivalentCostWei != "12582912000000" {
		t.Fatalf("execution bucket calldata cost = %s, want 12582912000000", execBucket.CalldataEquivalentCostWei)
	}
	if !execBucket.AverageExecutionBaseFeeWei.Valid || execBucket.AverageExecutionBaseFeeWei.String != "3000000" {
		t.Fatalf("execution bucket average base fee = %+v, want 3000000", execBucket.AverageExecutionBaseFeeWei)
	}
	if execBucket.SavingsWei != "6291456000000" || execBucket.SavingsPercent != 50 {
		t.Fatalf("execution bucket savings = %s / %v, want 6291456000000 / 50", execBucket.SavingsWei, execBucket.SavingsPercent)
	}

	proxyBucket := raw[1]
	if proxyBucket.CalldataEquivalentCostWei != "52428800" {
		t.Fatalf("proxy bucket calldata cost = %s, want 52428800", proxyBucket.CalldataEquivalentCostWei)
	}
	if proxyBucket.AverageExecutionBaseFeeWei.Valid {
		t.Fatalf("proxy bucket must not report an execution base fee, got %+v", proxyBucket.AverageExecutionBaseFeeWei)
	}
	if proxyBucket.SavingsPercent != 93.75 {
		t.Fatalf("proxy bucket savings percent = %v, want the proxy ratio 93.75", proxyBucket.SavingsPercent)
	}

	// Summary spans both pricing modes and must sum the per-bucket values.
	if execBucket.SummaryBlobCostWei != "6291459276800" ||
		execBucket.SummaryCalldataEquivalentCostWei != "12582964428800" ||
		execBucket.SummarySavingsWei != "6291505152000" {
		t.Fatalf("unexpected summary: %+v", execBucket)
	}
	if math.Abs(execBucket.SummarySavingsPercent-50.000182) > 1e-5 {
		t.Fatalf("summary savings percent = %v, want ~50.000182", execBucket.SummarySavingsPercent)
	}

	// The rollup-served paths must reproduce the raw rows exactly, both from
	// the hourly source buckets and from the trigger-maintained fine buckets.
	hourly := fetch(queryCostComparisonTimeChartRollup, 1, "24h", start, end, int64(3600), calldataGasPerByte, int64(3600))
	if !reflect.DeepEqual(hourly, raw) {
		t.Fatalf("hourly rollup rows diverge from raw:\nraw:    %+v\nrollup: %+v", raw, hourly)
	}
	fine := fetch(queryCostComparisonTimeChartRollup, 1, "24h", start, end, int64(3600), calldataGasPerByte, int64(60))
	if !reflect.DeepEqual(fine, raw) {
		t.Fatalf("fine rollup rows diverge from raw:\nraw:    %+v\nrollup: %+v", raw, fine)
	}

	// Rebuild the fine buckets through the startup backfill path and compare
	// again: the backfill statement must aggregate the base fee columns the
	// same way the trigger refresh does.
	if _, err := sqlxDB.Exec(`DELETE FROM blob_chart_rollups WHERE bucket_seconds = 60`); err != nil {
		t.Fatalf("clear fine blob rollups: %v", err)
	}
	if _, err := sqlxDB.Exec(`DELETE FROM block_metrics_rollups WHERE bucket_seconds = 60`); err != nil {
		t.Fatalf("clear fine block rollups: %v", err)
	}
	database, err := db.Connect(ctx, config.DatabaseConfig{URL: url, MaxOpenConns: 5, MaxIdleConns: 2})
	if err != nil {
		t.Fatalf("db.Connect: %v", err)
	}
	defer database.Close()
	if err := database.BackfillFineChartRollupsChunk(ctx, 1, start, end); err != nil {
		t.Fatalf("BackfillFineChartRollupsChunk: %v", err)
	}
	backfilled := fetch(queryCostComparisonTimeChartRollup, 1, "24h", start, end, int64(3600), calldataGasPerByte, int64(60))
	if !reflect.DeepEqual(backfilled, raw) {
		t.Fatalf("backfilled fine rollup rows diverge from raw:\nraw:    %+v\nrollup: %+v", raw, backfilled)
	}

	// Block granularity prices each block at its own recorded fee and falls
	// back per block when none was recorded.
	blocks := fetch(queryCostComparisonBlockChart, 1, start, end, calldataGasPerByte)
	if len(blocks) != 3 {
		t.Fatalf("expected 3 block rows, got %d: %+v", len(blocks), blocks)
	}
	wantBlocks := []struct {
		calldata string
		avgFee   string
		percent  float64
	}{
		{calldata: "4194304000000", avgFee: "2000000", percent: 25},
		{calldata: "8388608000000", avgFee: "4000000", percent: 62.5},
		{calldata: "52428800", avgFee: "", percent: 93.75},
	}
	for i, want := range wantBlocks {
		got := blocks[i]
		if got.CalldataEquivalentCostWei != want.calldata || got.SavingsPercent != want.percent {
			t.Fatalf("block row %d = %s / %v, want %s / %v", i, got.CalldataEquivalentCostWei, got.SavingsPercent, want.calldata, want.percent)
		}
		if want.avgFee == "" {
			if got.AverageExecutionBaseFeeWei.Valid {
				t.Fatalf("block row %d must not report an execution base fee, got %+v", i, got.AverageExecutionBaseFeeWei)
			}
		} else if !got.AverageExecutionBaseFeeWei.Valid || got.AverageExecutionBaseFeeWei.String != want.avgFee {
			t.Fatalf("block row %d average base fee = %+v, want %s", i, got.AverageExecutionBaseFeeWei, want.avgFee)
		}
	}
	if blocks[0].SummaryCalldataEquivalentCostWei != execBucket.SummaryCalldataEquivalentCostWei ||
		blocks[0].SummaryBlobCostWei != execBucket.SummaryBlobCostWei {
		t.Fatalf("block summary diverges from time summary: %+v vs %+v", blocks[0], execBucket)
	}

	// The hourly rollup rows themselves must carry the migration 000012
	// aggregates: the trigger refresh records the fee sum and the count of
	// blocks with a recorded fee, and zero-fee legacy blocks contribute to
	// neither.
	var feeAgg []struct {
		BucketStart       time.Time `db:"bucket_start"`
		SumBaseFeeWei     string    `db:"sum_base_fee_wei"`
		BaseFeeBlockCount int64     `db:"base_fee_block_count"`
	}
	if err := sqlxDB.SelectContext(ctx, &feeAgg, `
		SELECT bucket_start, sum_base_fee_wei::text AS sum_base_fee_wei, base_fee_block_count
		FROM block_metrics_rollups
		WHERE chain_id = 1 AND bucket_seconds = 3600
		ORDER BY bucket_start ASC
	`); err != nil {
		t.Fatalf("read hourly rollup aggregates: %v", err)
	}
	if len(feeAgg) != 2 {
		t.Fatalf("expected 2 hourly rollup rows, got %d: %+v", len(feeAgg), feeAgg)
	}
	if feeAgg[0].SumBaseFeeWei != "6000000" || feeAgg[0].BaseFeeBlockCount != 2 {
		t.Fatalf("execution bucket rollup aggregates = %+v, want sum 6000000 count 2", feeAgg[0])
	}
	if feeAgg[1].SumBaseFeeWei != "0" || feeAgg[1].BaseFeeBlockCount != 0 {
		t.Fatalf("legacy bucket rollup aggregates = %+v, want sum 0 count 0", feeAgg[1])
	}
}
