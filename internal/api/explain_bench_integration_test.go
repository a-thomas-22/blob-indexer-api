//go:build integration

package api

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
)

// benchDB connects to TEST_DB_URL and skips unless the database holds seeded
// mainnet (chain 1) data with fine rollups. These are measurement harnesses
// for a production-scale seeded database, run manually and alone. Unlike the
// schema-resetting integration tests, which run on per-package derived
// databases (internal/testdb), this deliberately reads TEST_DB_URL itself:
// that is where make seed-data puts the data, and nothing resets it anymore.
func benchDB(t *testing.T) *sqlx.DB {
	t.Helper()
	url := os.Getenv("TEST_DB_URL")
	if url == "" {
		t.Skip("TEST_DB_URL not set; skipping seeded-database benchmark")
	}
	db, err := sqlx.Connect("postgres", url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var fineBuckets int
	if err := db.Get(&fineBuckets, `
		SELECT COUNT(*) FROM block_metrics_rollups WHERE chain_id = 1 AND bucket_seconds = 60
	`); err != nil || fineBuckets == 0 {
		t.Skipf("TEST_DB_URL database is not seeded with fine rollups (count err=%v, rows=%d); seed before benchmarking", err, fineBuckets)
	}
	return db
}

// TestExplainRollingStatsAndChartQueries runs EXPLAIN ANALYZE for the raw and
// rollup-backed variants of the hot queries against a seeded database. It is
// a measurement harness, not an assertion suite: it prints plans and timings.
func TestExplainRollingStatsAndChartQueries(t *testing.T) {
	db := benchDB(t)

	explain := func(name, query string, args ...interface{}) {
		t.Helper()
		// Warm once, then measure.
		for i := 0; i < 2; i++ {
			rows, err := db.Query("EXPLAIN (ANALYZE, BUFFERS) "+query, args...)
			if err != nil {
				t.Fatalf("%s: explain: %v", name, err)
			}
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
			if i == 1 {
				t.Logf("===== %s =====\n%s", name, strings.Join(plan, "\n"))
			}
		}
	}

	generatedAt := time.Now().UTC()
	labels := []string{"5m", "1h", "24h"}
	durations := []int64{300, 3600, 86400}

	explain("rolling-stats RAW (5m,1h,24h)", queryRollingStatsWindows,
		1, pq.Array(labels), pq.Array(durations), generatedAt)
	explain("rolling-stats FINE ROLLUP (5m,1h,24h)", queryRollingStatsWindowsFine,
		1, pq.Array(labels), pq.Array(durations), generatedAt)

	// Default blob-market chart: range=24h, auto granularity = 300s buckets.
	end := alignChartEnd(generatedAt, 300)
	start := end.Add(-24 * time.Hour)
	explain("blob-market chart 24h@300s RAW", queryBlobMarketTimeChart,
		1, start, end, int64(300))
	explain("blob-market chart 24h@300s FINE ROLLUP", queryBlobMarketTimeChartRollup,
		1, start, end, int64(300), int64(60))

	// 1h range at minute granularity.
	end1h := alignChartEnd(generatedAt, 60)
	start1h := end1h.Add(-time.Hour)
	explain("blob-market chart 1h@60s RAW", queryBlobMarketTimeChart,
		1, start1h, end1h, int64(60))
	explain("blob-market chart 1h@60s FINE ROLLUP", queryBlobMarketTimeChartRollup,
		1, start1h, end1h, int64(60), int64(60))
}

// TestFineRollingStatsMatchesRaw cross-checks the fine rollup path against the
// raw path on the same seeded data. With generated_at aligned to a minute both
// queries cover identical window bounds, so exact metrics (sums, counts,
// unique senders, averages) must agree; median and p95 are estimated on the
// fine path and only checked loosely.
func TestFineRollingStatsMatchesRaw(t *testing.T) {
	db := benchDB(t)

	// Anchor to the newest seeded block so the short windows are never empty
	// regardless of how long ago the seed ran, and align to a minute so the
	// raw window [end-d, end) equals the fine window exactly.
	var newest time.Time
	if err := db.Get(&newest, `SELECT MAX(block_timestamp) FROM block_metrics WHERE chain_id = 1`); err != nil {
		t.Fatalf("newest block timestamp: %v", err)
	}
	generatedAt := newest.UTC().Truncate(time.Minute)
	labels := []string{"5m", "1h", "24h"}
	durations := []int64{300, 3600, 86400}

	fetch := func(query string) map[string]rollingStatsWindowRow {
		t.Helper()
		var rows []rollingStatsWindowRow
		if err := db.Select(&rows, query, 1, pq.Array(labels), pq.Array(durations), generatedAt); err != nil {
			t.Fatalf("query failed: %v", err)
		}
		byLabel := make(map[string]rollingStatsWindowRow, len(rows))
		for _, row := range rows {
			byLabel[row.Window] = row
		}
		return byLabel
	}

	raw := fetch(queryRollingStatsWindows)
	fine := fetch(queryRollingStatsWindowsFine)

	parse := func(label, field, value string) float64 {
		t.Helper()
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			t.Fatalf("%s: parse %s %q: %v", label, field, value, err)
		}
		return f
	}

	for _, label := range labels {
		r, ok := raw[label]
		if !ok {
			t.Fatalf("raw path returned no %s window", label)
		}
		f, ok := fine[label]
		if !ok {
			t.Fatalf("fine path returned no %s window", label)
		}

		if r.TotalBlobs == 0 {
			t.Fatalf("%s: seeded database returned zero blobs; seed before running", label)
		}
		if f.TotalBlobs != r.TotalBlobs || f.TotalBlobGasUsed != r.TotalBlobGasUsed ||
			f.UniqueSenders != r.UniqueSenders || f.TotalBlocks != r.TotalBlocks ||
			f.BlocksAboveTarget != r.BlocksAboveTarget || f.BlocksAtMax != r.BlocksAtMax {
			t.Fatalf("%s: exact metrics diverge:\nraw:  %+v\nfine: %+v", label, r, f)
		}
		if parse(label, "total_cost", f.TotalCostWei) != parse(label, "total_cost", r.TotalCostWei) {
			t.Fatalf("%s: total cost diverges: raw %s fine %s", label, r.TotalCostWei, f.TotalCostWei)
		}
		for field, pair := range map[string][2]string{
			"average_blob_base_fee": {r.AverageBlobBaseFee, f.AverageBlobBaseFee},
			"average_utilization":   {r.AverageUtilization, f.AverageUtilization},
		} {
			rv, fv := parse(label, field, pair[0]), parse(label, field, pair[1])
			if diff := (fv - rv) / rv; diff > 1e-9 || diff < -1e-9 {
				t.Fatalf("%s: %s diverges: raw %s fine %s", label, field, pair[0], pair[1])
			}
		}
		// Median/p95 are estimated from per-minute medians on the fine path.
		// The seeded fee curve sweeps its full range within every 24h window,
		// so a lax estimator (e.g. median of per-minute p95s, which collapses
		// toward the median) would miss by ~20% here — keep these tight.
		for field, pair := range map[string][2]string{
			"median": {r.MedianBlobBaseFee, f.MedianBlobBaseFee},
			"p95":    {r.P95BlobBaseFee, f.P95BlobBaseFee},
		} {
			rv, fv := parse(label, field, pair[0]), parse(label, field, pair[1])
			if diff := (fv - rv) / rv; diff > 0.05 || diff < -0.05 {
				t.Fatalf("%s: %s estimate off by more than 5%%: raw %s fine %s", label, field, pair[0], pair[1])
			}
		}
	}
}
