//go:build integration

package api

// EXPLAIN checks for the chart queries against a real Postgres
// (TEST_DB_URL): with a few hundred thousand blob rows behind a 24h window,
// every blobs-backed chart query must reach blobs through its
// (chain_id, timestamp) covering index, and no query may carry the one-row
// `bounds` CTE whose materialization hid the window from the planner
// (HL-38). The no-DB guard in chart_queries_test.go keeps the CTE out of the
// SQL text; this is the check that the plans actually benefit.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
)

// Fixture shape. blobsPerBlock is what makes blobs big enough to matter: 60
// days of history at 40 blobs a block is ~288k rows, past the point where a
// sequential scan is cheap, so a query that hides the window from the
// planner really does plan as `Seq Scan on blobs`. Every eighth blob row has
// no recorded priority fee, so the partial priced index is a real subset of
// the table rather than a copy of it.
const (
	chartPlanDays           = 60
	chartPlanBlocksPerDay   = 120
	chartPlanTotalBlocks    = chartPlanDays * chartPlanBlocksPerDay
	chartPlanSecondsPerStep = 86400 / chartPlanBlocksPerDay
	chartPlanBlobsPerBlock  = 39
)

// seedChartPlanFixture lays 60 days of blocks and blobs down with one
// INSERT ... SELECT generate_series per table: the statement-level triggers
// on blobs (rollups) and block_metrics (streaks) make anything row-by-row far
// too slow at this size. The rollup tables fill through those triggers, so
// the rollup-backed queries are planned against real rows too.
func seedChartPlanFixture(t *testing.T, sqlxDB *sqlx.DB, now time.Time) {
	t.Helper()
	oldest := now.Add(-chartPlanDays * 24 * time.Hour)

	if _, err := sqlxDB.Exec(`
		INSERT INTO block_metrics (chain_id, block_number, block_timestamp, blob_count, blob_params_max, base_fee_wei)
		SELECT 1, g, $1::timestamp + (g * $2 * INTERVAL '1 second'), 3, 6, 20000000000
		FROM generate_series(1, $3) AS g
	`, oldest, chartPlanSecondsPerStep, chartPlanTotalBlocks); err != nil {
		t.Fatalf("seed block_metrics: %v", err)
	}
	if _, err := sqlxDB.Exec(`
		INSERT INTO blobs (
			chain_id, block_number, blob_index, tx_hash, from_address, user_attribution,
			blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
			timestamp, max_fee_per_blob_gas, blob_gas_used,
			max_priority_fee_per_gas, max_fee_per_gas, priority_fee_per_gas, first_seen_at, tx_index
		)
		SELECT 1, g, i, '0xtx' || g || '_' || i, '0xsender' || (g % 7), '', 131072, 10, 2, 1310720,
			$1::timestamp + (g * $2 * INTERVAL '1 second'), 12, 131072,
			1000000000, 60000000000,
			CASE WHEN (g * 1000 + i) % 8 = 0 THEN NULL ELSE 1000000000 + (i * 1000000) END,
			$1::timestamp + (g * $2 * INTERVAL '1 second') - INTERVAL '3 seconds', i
		FROM generate_series(1, $3) AS g
		CROSS JOIN generate_series(0, $4) AS i
		-- Scramble the physical order so ANALYZE records a near-zero
		-- correlation between blobs.timestamp and the heap, which is what
		-- production looks like after in-place backfills have rewritten
		-- rows. With a perfectly correlated heap the planner can still
		-- reach a hidden window through a bitmap scan and the plan
		-- assertions stop discriminating. md5 keeps it deterministic.
		ORDER BY md5((g * 1000 + i)::text)
	`, oldest, chartPlanSecondsPerStep, chartPlanTotalBlocks, chartPlanBlobsPerBlock); err != nil {
		t.Fatalf("seed blobs: %v", err)
	}
	if _, err := sqlxDB.Exec("ANALYZE"); err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

// TestChartQueryPlansStayOnRangeIndexes seeds enough history that a 24h
// window is a small fraction of it, then asserts every chart query reaches
// blobs through the (chain_id, timestamp) indexes and that none of them
// materializes a bounds CTE.
func TestChartQueryPlansStayOnRangeIndexes(t *testing.T) {
	sqlxDB, _ := resetBuilderSchema(t, "api_chart_explain")
	now := time.Now().UTC().Truncate(time.Hour)
	seedChartPlanFixture(t, sqlxDB, now)

	end := now
	start := end.Add(-24 * time.Hour)
	const hour = int64(3600)

	explain := func(name, query string, args ...interface{}) string {
		t.Helper()
		rows, err := sqlxDB.Query("EXPLAIN "+query, args...)
		if err != nil {
			t.Fatalf("%s: explain: %v", name, err)
		}
		defer rows.Close()
		var plan []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("%s: scan: %v", name, err)
			}
			plan = append(plan, line)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: rows: %v", name, err)
		}
		text := strings.Join(plan, "\n")
		t.Logf("===== %s =====\n%s", name, text)
		return text
	}

	// A sequential scan of blobs means the window bound is not being pushed
	// into an index, which is the whole reason range=all is rejected for
	// the blobs-backed charts. block_metrics has its own
	// (chain_id, block_timestamp) covering index, so it is held to the same
	// standard wherever the window is what reaches it. The two block
	// charts that re-join block_metrics by block number after the window
	// has been applied are exempt: on a fixture this small the planner
	// hash-joins the whole 7200-row table rather than probing its primary
	// key 120 times, which is a size call, not a hidden bound.
	assertNoSeqScan := func(name, plan string, tables ...string) {
		t.Helper()
		for _, table := range tables {
			if strings.Contains(plan, "Seq Scan on "+table) {
				t.Errorf("%s: plan sequentially scans %s:\n%s", name, table, plan)
			}
		}
	}

	// HL-38: the one-row `bounds` CTE was referenced from more than one
	// place in every chart query, so Postgres materialized it, and a
	// materialized CTE hides the window from the planner. The window is
	// spliced in as the bare parameters now, so no plan may contain a
	// bounds CTE at all.
	assertNoBoundsCTE := func(name, plan string) {
		t.Helper()
		for _, node := range []string{"CTE bounds", "CTE Scan on bounds"} {
			if strings.Contains(plan, node) {
				t.Errorf("%s: plan still materializes the bounds CTE (%q):\n%s", name, node, plan)
			}
		}
	}

	assertPlan := func(name, plan string, tables ...string) {
		t.Helper()
		assertNoSeqScan(name, plan, tables...)
		assertNoBoundsCTE(name, plan)
	}

	// Raw, blobs-backed queries: the ones exposed to the /builders pathology.
	plan := explain("blob-tips 24h hourly", queryBlobTipsTimeChart, 1, start, end, hour, defaultAttributionSeriesLimit)
	assertPlan("blob-tips 24h hourly", plan, "blobs", "block_metrics")
	if !strings.Contains(plan, "idx_blobs_chain_timestamp_priced_cover") {
		t.Errorf("blob-tips 24h hourly: expected the priced partial covering index in the plan:\n%s", plan)
	}

	plan = explain("blob-tips by block", queryBlobTipsBlockChart, 1, start, end, defaultAttributionSeriesLimit)
	assertPlan("blob-tips by block", plan, "blobs")

	plan = explain("blob-market raw 24h", queryBlobMarketTimeChart, 1, start, end, hour)
	assertPlan("blob-market raw 24h", plan, "blobs", "block_metrics")

	plan = explain("blob-market by block", queryBlobMarketBlockChart, 1, start, end)
	assertPlan("blob-market by block", plan, "blobs", "block_metrics")

	plan = explain("cost-comparison raw 24h", queryCostComparisonTimeChart, 1, "24h", start, end, hour, "hour", calldataGasPerByte)
	assertPlan("cost-comparison raw 24h", plan, "blobs", "block_metrics")

	plan = explain("cost-comparison by block", queryCostComparisonBlockChart, 1, start, end, calldataGasPerByte)
	assertPlan("cost-comparison by block", plan, "blobs")

	plan = explain("attribution-usage raw 24h", queryAttributionUsageTimeChart, 1, "24h", start, end, hour, "hour", defaultAttributionSeriesLimit)
	assertPlan("attribution-usage raw 24h", plan, "blobs", "block_metrics")

	plan = explain("attribution-usage by block", queryAttributionUsageBlockChart, 1, start, end, defaultAttributionSeriesLimit)
	assertPlan("attribution-usage by block", plan, "blobs", "block_metrics")

	// Rollup-backed queries carried the same fence; their tables are keyed
	// by (chain_id, bucket_seconds, bucket_start), so a visible window is
	// a primary-key range scan.
	plan = explain("blob-market rollup 24h", queryBlobMarketTimeChartRollup, 1, start, end, hour, hour)
	assertPlan("blob-market rollup 24h", plan, "block_metrics_rollups", "blob_chart_rollups")

	plan = explain("cost-comparison rollup 24h", queryCostComparisonTimeChartRollup, 1, "24h", start, end, hour, calldataGasPerByte, hour)
	assertPlan("cost-comparison rollup 24h", plan, "block_metrics_rollups", "blob_chart_rollups")

	plan = explain("attribution-usage rollup 24h", queryAttributionUsageTimeChartRollup, 1, "24h", start, end, hour, defaultAttributionSeriesLimit, hour)
	assertPlan("attribution-usage rollup 24h", plan, "block_metrics_rollups", "blob_chart_rollups")

	// range=all is the one shape where the start really is data-dependent:
	// it must still plan (the MIN sub-select becomes an InitPlan) and still
	// carry no bounds CTE. A whole-table scan is the correct plan there.
	plan = explain("cost-comparison raw all", queryCostComparisonTimeChart, 1, "all", start, end, int64(86400), "day", calldataGasPerByte)
	assertNoBoundsCTE("cost-comparison raw all", plan)
	plan = explain("attribution-usage rollup all", queryAttributionUsageTimeChartRollup, 1, "all", start, end, int64(86400), defaultAttributionSeriesLimit, int64(86400))
	assertNoBoundsCTE("attribution-usage rollup all", plan)

	// Sanity: the seeded window really is a small slice of the table, so the
	// plans above were a meaningful test of selectivity.
	var inWindow, total int
	if err := sqlxDB.Get(&inWindow, `SELECT COUNT(*) FROM blobs WHERE chain_id = 1 AND timestamp >= $1 AND timestamp < $2`, start, end); err != nil {
		t.Fatalf("count window: %v", err)
	}
	if err := sqlxDB.Get(&total, `SELECT COUNT(*) FROM blobs WHERE chain_id = 1`); err != nil {
		t.Fatalf("count total: %v", err)
	}
	if inWindow == 0 || total < inWindow*10 {
		t.Fatalf("seeded history is not selective enough: %d of %d blob rows in the window", inWindow, total)
	}
	fmt.Printf("chart EXPLAIN fixture: %d of %d blob rows in the 24h window\n", inWindow, total)
}
