//go:build integration

package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
)

// recordsTestChainID is the network the streak fixtures use.
const recordsTestChainID = 1

// streakRun mirrors one blob_block_streaks row for assertions.
type streakRun struct {
	StartBlock int64 `db:"start_block"`
	EndBlock   int64 `db:"end_block"`
	Length     int64 `db:"length"`
}

func (r streakRun) String() string {
	return fmt.Sprintf("[%d-%d len=%d]", r.StartBlock, r.EndBlock, r.Length)
}

// newRecordsTestDB brings up a migrated schema with the default network row.
func newRecordsTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Connect("postgres", integrationDBURL(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	resetSchema(t, db)
	m := migrator(t, db)
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up: %v", err)
	}
	return db
}

// seedBlocks writes one block_metrics row per (block, blob count) pair under a
// 3-target / 6-max blob schedule, going through the same upsert shape the
// indexer uses so the streak triggers fire exactly as they do in production.
func seedBlocks(t *testing.T, db *sqlx.DB, blocks map[int64]int) {
	t.Helper()
	for block, blobs := range blocks {
		seedBlock(t, db, block, blobs)
	}
}

func seedBlock(t *testing.T, db *sqlx.DB, block int64, blobs int) {
	t.Helper()
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(block) * 12 * time.Second)
	_, err := db.Exec(`
		INSERT INTO block_metrics (
			chain_id, block_number, block_timestamp, blob_count,
			blob_gas_used, blob_gas_target, blob_gas_limit,
			excess_blob_gas, blob_base_fee, base_fee_wei, utilization_ratio,
			blob_params_target, blob_params_max, update_fraction
		) VALUES ($1, $2, $3, $4, $5, 393216, 786432, 0, $6, 500, 0.5, 3, 6, 3338477)
		ON CONFLICT (chain_id, block_number) DO UPDATE SET
			blob_count = EXCLUDED.blob_count,
			blob_gas_used = EXCLUDED.blob_gas_used
	`, recordsTestChainID, block, ts, blobs, int64(blobs)*131072, 1000000+block)
	if err != nil {
		t.Fatalf("seed block %d: %v", block, err)
	}
}

func streaks(t *testing.T, db *sqlx.DB, kind string) []streakRun {
	t.Helper()
	var runs []streakRun
	err := db.Select(&runs, `
		SELECT start_block, end_block, length FROM blob_block_streaks
		WHERE chain_id = $1 AND kind = $2
		ORDER BY start_block
	`, recordsTestChainID, kind)
	if err != nil {
		t.Fatalf("read %s streaks: %v", kind, err)
	}
	return runs
}

func assertStreaks(t *testing.T, db *sqlx.DB, kind string, want ...streakRun) {
	t.Helper()
	got := streaks(t, db, kind)
	if len(got) != len(want) {
		t.Fatalf("%s streaks: got %v, want %v", kind, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s streaks: got %v, want %v", kind, got, want)
		}
	}
}

// assertMatchesFromScratch recomputes the runs directly from block_metrics and
// compares them to what the triggers maintained. This is the real invariant:
// no sequence of inserts, updates, and deletes may leave the summary table
// disagreeing with the raw history it summarizes.
func assertMatchesFromScratch(t *testing.T, db *sqlx.DB, kind string) {
	t.Helper()
	predicate := map[string]string{
		"full":         "blob_count >= 6",
		"above_target": "blob_count > 3",
	}[kind]
	if predicate == "" {
		t.Fatalf("unknown streak kind %q", kind)
	}

	var want []streakRun
	err := db.Select(&want, `
		SELECT MIN(block_number) AS start_block, MAX(block_number) AS end_block, COUNT(*) AS length
		FROM (
			SELECT block_number, block_number - ROW_NUMBER() OVER (ORDER BY block_number) AS run_key
			FROM block_metrics
			WHERE chain_id = $1 AND `+predicate+`
		) q
		GROUP BY run_key
		ORDER BY 1
	`, recordsTestChainID)
	if err != nil {
		t.Fatalf("recompute %s streaks from scratch: %v", kind, err)
	}
	assertStreaks(t, db, kind, want...)
}

func TestBlobBlockStreaks_MaintainedAcrossBlockLifecycle(t *testing.T) {
	db := newRecordsTestDB(t)

	// A run of length 1 counts, gaps between qualifying blocks do not merge,
	// and the two predicates are tracked independently.
	seedBlocks(t, db, map[int64]int{
		100: 1, 101: 4, 102: 6, 103: 6, 104: 6,
		105: 2, 106: 5, 107: 6, 108: 0, 109: 3,
	})
	assertStreaks(t, db, "full",
		streakRun{StartBlock: 102, EndBlock: 104, Length: 3},
		streakRun{StartBlock: 107, EndBlock: 107, Length: 1},
	)
	assertStreaks(t, db, "above_target",
		streakRun{StartBlock: 101, EndBlock: 104, Length: 4},
		streakRun{StartBlock: 106, EndBlock: 107, Length: 2},
	)

	// Blocks arriving out of order leave a hole in indexed history, which
	// breaks the run until the hole is filled.
	seedBlocks(t, db, map[int64]int{112: 6, 113: 6})
	assertStreaks(t, db, "full",
		streakRun{StartBlock: 102, EndBlock: 104, Length: 3},
		streakRun{StartBlock: 107, EndBlock: 107, Length: 1},
		streakRun{StartBlock: 112, EndBlock: 113, Length: 2},
	)

	// Filling the hole merges the neighbours, including across two existing
	// runs when the arriving block bridges them.
	seedBlock(t, db, 111, 6)
	assertStreaks(t, db, "full",
		streakRun{StartBlock: 102, EndBlock: 104, Length: 3},
		streakRun{StartBlock: 107, EndBlock: 107, Length: 1},
		streakRun{StartBlock: 111, EndBlock: 113, Length: 3},
	)
	seedBlock(t, db, 110, 6)
	assertStreaks(t, db, "full",
		streakRun{StartBlock: 102, EndBlock: 104, Length: 3},
		streakRun{StartBlock: 107, EndBlock: 107, Length: 1},
		streakRun{StartBlock: 110, EndBlock: 113, Length: 4},
	)

	// A reindex that rewrites a block out of the predicate splits its run.
	seedBlock(t, db, 111, 2)
	assertStreaks(t, db, "full",
		streakRun{StartBlock: 102, EndBlock: 104, Length: 3},
		streakRun{StartBlock: 107, EndBlock: 107, Length: 1},
		streakRun{StartBlock: 110, EndBlock: 110, Length: 1},
		streakRun{StartBlock: 112, EndBlock: 113, Length: 2},
	)
	assertMatchesFromScratch(t, db, "full")
	assertMatchesFromScratch(t, db, "above_target")
}

// A reorg deletes a block range from block_metrics; the blocks it removes must
// break the runs they were part of, exactly like never-indexed blocks.
func TestBlobBlockStreaks_ReorgDeleteBreaksRuns(t *testing.T) {
	db := newRecordsTestDB(t)

	seedBlocks(t, db, map[int64]int{
		200: 6, 201: 6, 202: 6, 203: 6, 204: 6, 205: 6,
	})
	assertStreaks(t, db, "full", streakRun{StartBlock: 200, EndBlock: 205, Length: 6})

	// Rewind the tip, the way handleReorg does.
	if _, err := db.Exec(
		"DELETE FROM block_metrics WHERE chain_id = $1 AND block_number >= $2",
		recordsTestChainID, 203); err != nil {
		t.Fatalf("reorg delete: %v", err)
	}
	assertStreaks(t, db, "full", streakRun{StartBlock: 200, EndBlock: 202, Length: 3})

	// Deleting from the middle splits rather than truncates.
	seedBlocks(t, db, map[int64]int{203: 6, 204: 6, 205: 6})
	if _, err := db.Exec(
		"DELETE FROM block_metrics WHERE chain_id = $1 AND block_number = $2",
		recordsTestChainID, 202); err != nil {
		t.Fatalf("delete middle block: %v", err)
	}
	assertStreaks(t, db, "full",
		streakRun{StartBlock: 200, EndBlock: 201, Length: 2},
		streakRun{StartBlock: 203, EndBlock: 205, Length: 3},
	)
	assertMatchesFromScratch(t, db, "full")
}

// The predicates must agree with is_full / is_above_target on /blob/pricing,
// including the fallback from blob params to the gas columns that
// blobSpaceLimit() applies. A block with neither recorded qualifies for
// neither predicate rather than defaulting to "full".
func TestBlobBlockStreaks_PredicatesMatchPricingSemantics(t *testing.T) {
	db := newRecordsTestDB(t)

	ts := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	// 300: params only. 301: gas columns only (2 target / 4 max blobs).
	// 302: neither, which is unclassifiable.
	if _, err := db.Exec(`
		INSERT INTO block_metrics (
			chain_id, block_number, block_timestamp, blob_count,
			blob_gas_used, blob_gas_target, blob_gas_limit,
			excess_blob_gas, blob_base_fee, base_fee_wei, utilization_ratio,
			blob_params_target, blob_params_max, update_fraction
		) VALUES
			($1, 300, $2, 6, 786432, 0, 0, 0, 10, 500, 0.5, 3, 6, 3338477),
			($1, 301, $3, 4, 524288, 262144, 524288, 0, 10, 500, 0.5, 0, 0, 3338477),
			($1, 302, $4, 9, 1179648, 0, 0, 0, 10, 500, 0.5, 0, 0, 3338477)
	`, recordsTestChainID, ts, ts.Add(12*time.Second), ts.Add(24*time.Second)); err != nil {
		t.Fatalf("seed mixed-schedule blocks: %v", err)
	}

	// 300 is full via blob_params_max, 301 is full via blob_gas_limit, and 302
	// has no schedule at all, so the run covers 300-301 only.
	assertStreaks(t, db, "full", streakRun{StartBlock: 300, EndBlock: 301, Length: 2})
	assertStreaks(t, db, "above_target", streakRun{StartBlock: 300, EndBlock: 301, Length: 2})

	// A block exactly at target is not above it; a block at max is full.
	seedBlock(t, db, 400, 3)
	assertStreaks(t, db, "above_target", streakRun{StartBlock: 300, EndBlock: 301, Length: 2})
	seedBlock(t, db, 401, 6)
	assertStreaks(t, db, "full",
		streakRun{StartBlock: 300, EndBlock: 301, Length: 2},
		streakRun{StartBlock: 401, EndBlock: 401, Length: 1},
	)
}

// The chunked backfill is what populates history on first deploy. Rebuilding
// from an empty table in arbitrary chunks must reproduce exactly what the
// triggers built incrementally, including runs that straddle a chunk boundary.
func TestBackfillBlobBlockStreaksChunk_RebuildsHistoryAcrossChunkBoundaries(t *testing.T) {
	db := newRecordsTestDB(t)

	blocks := map[int64]int{}
	for block := int64(500); block <= 560; block++ {
		// Full from 505 through 535, straddling the chunk boundary at 520.
		if block >= 505 && block <= 535 {
			blocks[block] = 6
		} else {
			blocks[block] = 1
		}
	}
	seedBlocks(t, db, blocks)
	incremental := streaks(t, db, "full")

	if _, err := db.Exec("DELETE FROM blob_block_streaks WHERE chain_id = $1", recordsTestChainID); err != nil {
		t.Fatalf("clear streaks: %v", err)
	}

	wrapped := &DB{DB: db}
	ctx := context.Background()
	for _, chunk := range [][2]int64{{490, 519}, {520, 549}, {550, 579}} {
		if err := wrapped.BackfillBlobBlockStreaksChunk(ctx, recordsTestChainID, chunk[0], chunk[1]); err != nil {
			t.Fatalf("backfill chunk %v: %v", chunk, err)
		}
	}

	rebuilt := streaks(t, db, "full")
	if len(rebuilt) != len(incremental) {
		t.Fatalf("backfill produced %v, triggers produced %v", rebuilt, incremental)
	}
	for i := range rebuilt {
		if rebuilt[i] != incremental[i] {
			t.Fatalf("backfill produced %v, triggers produced %v", rebuilt, incremental)
		}
	}
	assertMatchesFromScratch(t, db, "full")

	// Re-running a chunk must be a no-op, since a restarted backfill redoes
	// the chunk containing its checkpoint.
	if err := wrapped.BackfillBlobBlockStreaksChunk(ctx, recordsTestChainID, 520, 549); err != nil {
		t.Fatalf("re-run chunk: %v", err)
	}
	assertMatchesFromScratch(t, db, "full")
}

func TestIndexedBlockBounds_Integration(t *testing.T) {
	db := newRecordsTestDB(t)
	wrapped := &DB{DB: db}
	ctx := context.Background()

	bounds, err := wrapped.IndexedBlockBounds(ctx, recordsTestChainID)
	if err != nil {
		t.Fatalf("bounds on empty history: %v", err)
	}
	if bounds.HasBlocks {
		t.Fatalf("expected no indexed blocks, got %+v", bounds)
	}

	seedBlocks(t, db, map[int64]int{700: 1, 705: 2, 999: 3})
	bounds, err = wrapped.IndexedBlockBounds(ctx, recordsTestChainID)
	if err != nil {
		t.Fatalf("bounds: %v", err)
	}
	if !bounds.HasBlocks || bounds.Min != 700 || bounds.Max != 999 {
		t.Fatalf("unexpected bounds: %+v", bounds)
	}
}

// The /records reads must be served by the indexes migration 000013 adds, not
// by a sort over the whole table: that is the difference between the endpoint
// scaling with history and not.
func TestRecordsReadsUseIndexes(t *testing.T) {
	db := newRecordsTestDB(t)

	seedBlocks(t, db, map[int64]int{800: 6, 801: 6, 802: 1})

	cases := []struct {
		name  string
		query string
		args  []interface{}
		index string
	}{
		{
			name: "base fee peaks",
			query: `SELECT block_number, block_timestamp, blob_base_fee, blob_count FROM block_metrics
				WHERE chain_id = $1 ORDER BY blob_base_fee DESC, block_number DESC LIMIT 10`,
			args:  []interface{}{recordsTestChainID},
			index: "idx_block_metrics_chain_blob_base_fee",
		},
		{
			name: "top streaks",
			query: `SELECT start_block, end_block, length FROM blob_block_streaks
				WHERE chain_id = $1 AND kind = $2 ORDER BY length DESC, end_block DESC LIMIT 10`,
			args:  []interface{}{recordsTestChainID, "full"},
			index: "idx_blob_block_streaks_chain_kind_length",
		},
		{
			name: "busiest hours",
			query: `SELECT bucket_start, sum_blob_count FROM block_metrics_rollups
				WHERE chain_id = $1 AND bucket_seconds = 3600 AND sum_blob_count > 0
				ORDER BY sum_blob_count DESC, bucket_start DESC LIMIT 10`,
			args:  []interface{}{recordsTestChainID},
			index: "idx_block_metrics_rollups_hourly_blob_count",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Fixtures are far too small for the planner to prefer an index on
			// cost, so sequential scans are disabled to assert that the index
			// is capable of serving the query at all.
			if _, err := db.Exec("SET enable_seqscan = off"); err != nil {
				t.Fatalf("disable seqscan: %v", err)
			}
			defer func() { _, _ = db.Exec("RESET enable_seqscan") }()

			var lines []string
			if err := db.Select(&lines, "EXPLAIN "+tc.query, tc.args...); err != nil {
				t.Fatalf("explain: %v", err)
			}
			plan := strings.Join(lines, "\n")
			if !strings.Contains(plan, tc.index) {
				t.Fatalf("expected %s in plan:\n%s", tc.index, plan)
			}
		})
	}
}
