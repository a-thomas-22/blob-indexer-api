package api

import (
	"regexp"
	"strings"
	"testing"
)

// builderQueries is every SQL string the block-builder endpoints run, plus
// the fragments they are assembled from, keyed by the name a failure should
// point at.
func builderQueries() map[string]string {
	return map[string]string{
		"queryBuilderAggregates":               queryBuilderAggregates,
		"queryBuilderUsers":                    queryBuilderUsers,
		"queryBuilderSkipped":                  queryBuilderSkipped,
		"queryBuilderSkippedDetailFrom":        queryBuilderSkippedDetailFrom,
		"queryBuilderRecentBlocks":             queryBuilderRecentBlocks,
		"queryBuilderShareTimeChart":           queryBuilderShareTimeChart,
		"queryBuilderShareBlockChart":          queryBuilderShareBlockChart,
		"queryBlockBuilderForBlock":            queryBlockBuilderForBlock,
		"queryBlockBuildersByBlockNumbers":     queryBlockBuildersByBlockNumbers,
		"queryBlobInclusionCandidatesForBlock": queryBlobInclusionCandidatesForBlock,
		"builderTxSourceSQL":                   builderTxSourceSQL("$1", "$2", "$3"),
		"builderEntityKeyedTxsSQL":             builderEntityKeyedTxsSQL("$1", "$2", "$3"),
		"builderMetricsWindowSQL":              builderMetricsWindowSQL,
		"builderAddressAttributionSQL":         builderAddressAttributionSQL("src"),
		"builderShareSeriesSQL":                builderShareSeriesSQL("$5"),
		"builderShareSelectSQL":                builderShareSelectSQL,
	}
}

// boundsCTERef matches a reference to a CTE named exactly `bounds`.
var boundsCTERef = regexp.MustCompile(`(?i)\bbounds\b`)

// TestBuilderQueriesCarryNoBoundsCTE keeps HL-38 from coming back. Holding
// the requested window in a one-row `bounds` CTE reads well, but every
// builder query references it from more than one place, and Postgres
// materializes a CTE with more than one reference. A materialized CTE is an
// optimization fence: the planner cannot see the timestamps, falls back to
// its default range selectivity, and picks a scan of the whole blobs table
// with the window demoted to a join filter — 86s per /builders request in
// production. The window belongs in the predicates as the bare parameter
// placeholders, which no planning rule can hide.
func TestBuilderQueriesCarryNoBoundsCTE(t *testing.T) {
	for name, query := range builderQueries() {
		for _, forbidden := range []string{"WITH bounds AS", "FROM bounds", "JOIN bounds"} {
			if strings.Contains(query, forbidden) {
				t.Errorf("%s contains %q: the range window must be spliced into each predicate as the parameter placeholders, not held in a bounds CTE (HL-38)", name, forbidden)
			}
		}
		if loc := boundsCTERef.FindStringIndex(query); loc != nil {
			t.Errorf("%s references a CTE named `bounds` at offset %d: %q (HL-38)", name, loc[0], snippet(query, loc[0]))
		}
	}
}

// TestBuilderQueriesBindTheirWindow is the other half of the guard: having
// removed the CTE, each query must still constrain both of its range-scoped
// tables by the placeholders the handler passes.
func TestBuilderQueriesBindTheirWindow(t *testing.T) {
	cases := map[string][]string{
		"queryBuilderAggregates": {
			"bb.block_timestamp >= $2::timestamp",
			"bb.block_timestamp < $3::timestamp",
			"bm.block_timestamp >= $2::timestamp",
			"bm.block_timestamp < $3::timestamp",
			"bl.timestamp >= $2::timestamp",
			"bl.timestamp < $3::timestamp",
		},
		"queryBuilderUsers": {
			"bb.block_timestamp >= $2::timestamp",
			"bb.block_timestamp < $3::timestamp",
			"bl.timestamp >= $2::timestamp",
			"bl.timestamp < $3::timestamp",
		},
		"queryBuilderSkipped": {
			"bb.block_timestamp >= $2::timestamp",
			"bb.block_timestamp < $3::timestamp",
			"c.block_timestamp >= $2::timestamp",
			"c.block_timestamp < $3::timestamp",
		},
		"queryBuilderShareTimeChart": {
			"bb.block_timestamp >= $2::timestamp",
			"bb.block_timestamp < $3::timestamp",
			"bm.block_timestamp >= $2::timestamp",
			"bm.block_timestamp < $3::timestamp",
		},
		"queryBuilderShareBlockChart": {
			"bb.block_timestamp >= $2::timestamp",
			"bb.block_timestamp < $3::timestamp",
			"bm.block_timestamp >= $2::timestamp",
			"bm.block_timestamp < $3::timestamp",
		},
	}
	queries := builderQueries()
	for name, predicates := range cases {
		query, ok := queries[name]
		if !ok {
			t.Fatalf("%s is not in builderQueries()", name)
		}
		for _, predicate := range predicates {
			if !strings.Contains(query, predicate) {
				t.Errorf("%s is missing the window predicate %q", name, predicate)
			}
		}
	}
}

func snippet(query string, at int) string {
	start := at - 40
	if start < 0 {
		start = 0
	}
	end := at + 40
	if end > len(query) {
		end = len(query)
	}
	return query[start:end]
}
