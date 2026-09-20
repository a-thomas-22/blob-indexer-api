package api

import (
	"regexp"
	"strings"
	"testing"
)

// chartQueries is every SQL string the chart endpoints run, plus the
// fragments they are assembled from, keyed by the name a failure should
// point at.
func chartQueries() map[string]string {
	return map[string]string{
		"queryBlobTipsTimeChart":               queryBlobTipsTimeChart,
		"queryBlobTipsBlockChart":              queryBlobTipsBlockChart,
		"queryBlobMarketTimeChart":             queryBlobMarketTimeChart,
		"queryBlobMarketBlockChart":            queryBlobMarketBlockChart,
		"queryBlobMarketTimeChartRollup":       queryBlobMarketTimeChartRollup,
		"queryCostComparisonTimeChart":         queryCostComparisonTimeChart,
		"queryCostComparisonBlockChart":        queryCostComparisonBlockChart,
		"queryCostComparisonTimeChartRollup":   queryCostComparisonTimeChartRollup,
		"queryAttributionUsageTimeChart":       queryAttributionUsageTimeChart,
		"queryAttributionUsageBlockChart":      queryAttributionUsageBlockChart,
		"queryAttributionUsageTimeChartRollup": queryAttributionUsageTimeChartRollup,
		"rawChartRangeStartSQL":                rawChartRangeStartSQL,
		"rollupChartRangeStartSQL":             rollupChartRangeStartSQL,
		"blobTipsSeriesSQL":                    blobTipsSeriesSQL("$5"),
		"blobTipsSelectSQL":                    blobTipsSelectSQL,
		"attributionEntityBaseSQL":             attributionEntityBaseSQL("$7"),
	}
}

// chartBoundsCTERef matches a reference to a CTE named exactly `bounds`.
var chartBoundsCTERef = regexp.MustCompile(`(?i)\bbounds\b`)

// TestChartQueriesCarryNoBoundsCTE keeps the HL-38 planner fence out of the
// chart queries. Holding the requested window in a one-row `bounds` CTE
// reads well, but every chart query referenced it from more than one place,
// and Postgres materializes a CTE with more than one reference. A
// materialized CTE is an optimization fence: the planner cannot see the
// timestamps, falls back to its default range selectivity, and can pick a
// scan of the whole blobs table with the window demoted to a join filter,
// which is what made /builders take 86s in production. /charts/blob-tips
// reads blobs directly (no rollup carries priority fees), so it is exposed
// to exactly that. The window belongs in the predicates as the bare
// parameter placeholders, which no planning rule can hide.
func TestChartQueriesCarryNoBoundsCTE(t *testing.T) {
	for name, query := range chartQueries() {
		for _, forbidden := range []string{"WITH bounds AS", "FROM bounds", "JOIN bounds"} {
			if strings.Contains(query, forbidden) {
				t.Errorf("%s contains %q: the range window must be spliced into each predicate as the parameter placeholders, not held in a bounds CTE (HL-38)", name, forbidden)
			}
		}
		if loc := chartBoundsCTERef.FindStringIndex(query); loc != nil {
			lo, hi := loc[0]-40, loc[1]+40
			if lo < 0 {
				lo = 0
			}
			if hi > len(query) {
				hi = len(query)
			}
			t.Errorf("%s references a CTE named `bounds` at offset %d: %q (HL-38)", name, loc[0], query[lo:hi])
		}
	}
}

// TestChartQueriesBindTheirWindow is the other half of the guard: having
// removed the CTE, each query must still constrain its range-scoped tables
// by the placeholders the handler passes, in the positions the handler's
// argument builder fills (chartBlobMarketTimeArgs / chartBlockArgs put the
// window at $2/$3; chartTimeArgs and chartRollupTimeArgs at $3/$4 behind
// the range label).
func TestChartQueriesBindTheirWindow(t *testing.T) {
	rawAllStart := "bl.timestamp >= " + rawChartRangeStartSQL
	rollupAllStart := "r.bucket_start >= " + rollupChartRangeStartSQL
	cases := map[string][]string{
		"queryBlobTipsTimeChart": {
			"bl.timestamp >= $2::timestamp",
			"bl.timestamp < $3::timestamp",
			"bl.priority_fee_per_gas IS NOT NULL",
			"bm.block_timestamp >= $2::timestamp",
			"bm.block_timestamp < $3::timestamp",
		},
		"queryBlobTipsBlockChart": {
			"bm.block_timestamp >= $2::timestamp",
			"bm.block_timestamp < $3::timestamp",
			"bl.block_number = bu.block_number",
		},
		"queryBlobMarketTimeChart": {
			"bl.timestamp >= $2::timestamp",
			"bl.timestamp < $3::timestamp",
			"bm.block_timestamp >= $2::timestamp",
			"bm.block_timestamp < $3::timestamp",
		},
		"queryBlobMarketBlockChart": {
			"bl.timestamp >= $2::timestamp",
			"bl.timestamp < $3::timestamp",
			"bm.block_timestamp >= $2::timestamp",
			"bm.block_timestamp < $3::timestamp",
			"$2::timestamp AS range_start",
			"$3::timestamp AS range_end",
		},
		"queryBlobMarketTimeChartRollup": {
			"r.bucket_start >= $2::timestamp",
			"r.bucket_start < $3::timestamp",
		},
		"queryCostComparisonTimeChart": {
			rawAllStart,
			"bl.timestamp < $4::timestamp",
			"bm.block_timestamp >= " + rawChartRangeStartSQL,
			"bm.block_timestamp < $4::timestamp",
		},
		"queryCostComparisonBlockChart": {
			"bl.timestamp >= $2::timestamp",
			"bl.timestamp < $3::timestamp",
			"bm.block_timestamp >= $2::timestamp",
			"bm.block_timestamp < $3::timestamp",
			"$2::timestamp AS range_start",
			"$3::timestamp AS range_end",
		},
		"queryCostComparisonTimeChartRollup": {
			rollupAllStart,
			"r.bucket_start < $4::timestamp",
		},
		"queryAttributionUsageTimeChart": {
			rawAllStart,
			"bl.timestamp < $4::timestamp",
		},
		"queryAttributionUsageBlockChart": {
			"bm.block_timestamp >= $2::timestamp",
			"bm.block_timestamp < $3::timestamp",
			"bl.block_number = bu.block_number",
		},
		"queryAttributionUsageTimeChartRollup": {
			rollupAllStart,
			"r.bucket_start < $4::timestamp",
		},
	}
	queries := chartQueries()
	for name, predicates := range cases {
		query, ok := queries[name]
		if !ok {
			t.Fatalf("%s is not in chartQueries()", name)
		}
		for _, predicate := range predicates {
			if !strings.Contains(query, predicate) {
				t.Errorf("%s is missing the window predicate %q", name, predicate)
			}
		}
	}
}

// The range=all start expressions must resolve to the bare start placeholder
// for a bounded range and only consult the tables for range=all, so a
// bounded request never pays for the earliest-row lookup.
func TestChartRangeStartExpressionsFoldForBoundedRanges(t *testing.T) {
	for name, expr := range map[string]string{
		"rawChartRangeStartSQL":    rawChartRangeStartSQL,
		"rollupChartRangeStartSQL": rollupChartRangeStartSQL,
	} {
		for _, want := range []string{"WHEN $2::text = 'all'", "ELSE $3::timestamp", "$4::timestamp"} {
			if !strings.Contains(expr, want) {
				t.Errorf("%s is missing %q", name, want)
			}
		}
	}
	if !strings.Contains(rawChartRangeStartSQL, "date_trunc($6::text") || !strings.Contains(rawChartRangeStartSQL, "FROM blobs") {
		t.Errorf("rawChartRangeStartSQL must truncate the earliest blob timestamp to $6: %s", rawChartRangeStartSQL)
	}
	if !strings.Contains(rollupChartRangeStartSQL, "r.bucket_seconds = $7::int") || !strings.Contains(rollupChartRangeStartSQL, "FROM blob_chart_rollups") {
		t.Errorf("rollupChartRangeStartSQL must read the earliest $7-second rollup bucket: %s", rollupChartRangeStartSQL)
	}
}
