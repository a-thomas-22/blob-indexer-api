//go:build integration

package api

// Smoke test for the mempool-related query constants against a real Postgres
// (TEST_DB_URL). The unit tests exercise these paths through sqlmock, which
// never parses SQL, so this is the only check that the query text and its
// type unification (e.g. the blobs/mempool_blobs UNION in queryBlobByTxHash)
// are valid.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"

	"github.com/a-thomas-22/blob-indexer-api/internal/db"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	"github.com/a-thomas-22/blob-indexer-api/internal/testdb"
)

func TestMempoolQueriesAgainstRealPostgres(t *testing.T) {
	// This test resets its schema, so it runs on this package's dedicated
	// database rather than TEST_DB_URL itself — parallel test binaries from
	// other packages must never see the reset.
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
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	// EIP-4844 versioned blob hashes. The "shared" hash appears on both the
	// confirmed and the pending transaction (identical blob content produces
	// identical versioned hashes), so the by-hash lookup must prefer the
	// confirmed row.
	confirmedOnlyHash := "0x01" + strings.Repeat("aa", 31)
	pendingOnlyHash := "0x01" + strings.Repeat("bb", 31)
	sharedHash := "0x01" + strings.Repeat("cc", 31)
	missingHash := "0x01" + strings.Repeat("dd", 31)

	if _, err := sqlxDB.Exec(`
		INSERT INTO blobs (
			chain_id, block_number, blob_index, tx_hash, from_address, user_attribution,
			blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
			timestamp, max_fee_per_blob_gas, blob_gas_used, versioned_hashes
		) VALUES (1, 100, 0, '0xconfirmed', '0xfrom', 'Rollup', 131072, 10, 2, 100, $1, 12, 131072, $2)
	`, now, pq.StringArray{confirmedOnlyHash, sharedHash}); err != nil {
		t.Fatalf("seed confirmed blob: %v", err)
	}
	if _, err := sqlxDB.Exec(`
		INSERT INTO mempool_blobs (
			chain_id, tx_hash, blob_index, from_address, user_attribution,
			blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
			timestamp, max_fee_per_blob_gas, blob_gas_used, versioned_hashes
		) VALUES (1, '0xpendingtx', 0, '0xfrom', 'Rollup', 131072, 50, 10, 500, $1, 60, 131072, $2)
	`, now, pq.StringArray{pendingOnlyHash, sharedHash}); err != nil {
		t.Fatalf("seed mempool blob: %v", err)
	}
	// Block 99 is deliberately missing: the coverage bounds are documented as
	// sparse extremes, so an interior gap (a failed block awaiting retry) must
	// not shrink the reported range.
	if _, err := sqlxDB.Exec(`
		INSERT INTO indexed_blocks (chain_id, block_number, block_hash, parent_hash)
		VALUES (1, 98, '0xhash98', '0xhash97'), (1, 100, '0xhash100', '0xhash99')
	`); err != nil {
		t.Fatalf("seed indexed blocks: %v", err)
	}

	t.Run("queryMempoolBlobs", func(t *testing.T) {
		var blobs []models.Blob
		if err := sqlxDB.SelectContext(ctx, &blobs, queryMempoolBlobs, 1, 10, 0); err != nil {
			t.Fatalf("queryMempoolBlobs: %v", err)
		}
		if len(blobs) != 1 || blobs[0].BlockNumber != models.PendingBlockNumber || blobs[0].Confirmed {
			t.Fatalf("unexpected mempool blobs: %+v", blobs)
		}
	})

	t.Run("queryMempoolBlobsByAddress", func(t *testing.T) {
		var blobs []models.Blob
		if err := sqlxDB.SelectContext(ctx, &blobs, queryMempoolBlobsByAddress, 1, "0xfrom", 10, 0); err != nil {
			t.Fatalf("queryMempoolBlobsByAddress: %v", err)
		}
		if len(blobs) != 1 {
			t.Fatalf("expected 1 pending blob for sender, got %d", len(blobs))
		}
	})

	t.Run("queryMempoolPressure", func(t *testing.T) {
		var pressure mempoolPressureAggregate
		if err := sqlxDB.GetContext(ctx, &pressure, queryMempoolPressure, 1, 101, 100, "40"); err != nil {
			t.Fatalf("queryMempoolPressure: %v", err)
		}
		if pressure.PendingBlobCount != 1 || pressure.LikelyIncludable != 1 {
			t.Fatalf("unexpected pressure aggregate: %+v", pressure)
		}
	})

	t.Run("queryBlobByTxHash", func(t *testing.T) {
		var blob models.Blob
		if err := sqlxDB.GetContext(ctx, &blob, queryBlobByTxHash, "0xconfirmed", 1); err != nil {
			t.Fatalf("queryBlobByTxHash confirmed: %v", err)
		}
		if !blob.Confirmed || blob.BlockNumber != 100 {
			t.Fatalf("expected confirmed blob, got %+v", blob)
		}
		if err := sqlxDB.GetContext(ctx, &blob, queryBlobByTxHash, "0xpendingtx", 1); err != nil {
			t.Fatalf("queryBlobByTxHash pending: %v", err)
		}
		if blob.Confirmed || blob.BlockNumber != models.PendingBlockNumber {
			t.Fatalf("expected pending blob, got %+v", blob)
		}
		if err := sqlxDB.GetContext(ctx, &blob, queryBlobByTxHash, "0xmissing", 1); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("expected sql.ErrNoRows for missing tx, got %v", err)
		}
	})

	t.Run("queryBlobByVersionedHash", func(t *testing.T) {
		var blob models.Blob
		if err := sqlxDB.GetContext(ctx, &blob, queryBlobByVersionedHash, confirmedOnlyHash, 1); err != nil {
			t.Fatalf("queryBlobByVersionedHash confirmed: %v", err)
		}
		if !blob.Confirmed || blob.BlockNumber != 100 || blob.TxHash != "0xconfirmed" {
			t.Fatalf("expected confirmed blob, got %+v", blob)
		}
		if len(blob.VersionedHashes) != 2 || blob.VersionedHashes[0] != confirmedOnlyHash {
			t.Fatalf("expected full versioned hash list on result, got %v", blob.VersionedHashes)
		}
		if err := sqlxDB.GetContext(ctx, &blob, queryBlobByVersionedHash, pendingOnlyHash, 1); err != nil {
			t.Fatalf("queryBlobByVersionedHash pending: %v", err)
		}
		if blob.Confirmed || blob.BlockNumber != models.PendingBlockNumber || blob.TxHash != "0xpendingtx" {
			t.Fatalf("expected pending blob, got %+v", blob)
		}
		if err := sqlxDB.GetContext(ctx, &blob, queryBlobByVersionedHash, sharedHash, 1); err != nil {
			t.Fatalf("queryBlobByVersionedHash shared: %v", err)
		}
		if !blob.Confirmed || blob.TxHash != "0xconfirmed" {
			t.Fatalf("expected confirmed row to win for a hash carried by both, got %+v", blob)
		}
		if err := sqlxDB.GetContext(ctx, &blob, queryBlobByVersionedHash, missingHash, 1); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("expected sql.ErrNoRows for missing versioned hash, got %v", err)
		}
	})

	t.Run("queryBlobStats", func(t *testing.T) {
		var stats models.BlobStatsAggregate
		if err := sqlxDB.GetContext(ctx, &stats, queryBlobStats, 1); err != nil {
			t.Fatalf("queryBlobStats: %v", err)
		}
		if stats.TotalConfirmedBlobs != 1 || stats.TotalPendingBlobs != 1 || stats.TotalBlobs != 2 {
			t.Fatalf("unexpected stats: %+v", stats)
		}
	})

	t.Run("queryTopBlobUsersAllByCount", func(t *testing.T) {
		var users []models.BlobUserStats
		if err := sqlxDB.SelectContext(ctx, &users, queryTopBlobUsersAllByCount, 1, 10, 0, "all"); err != nil {
			t.Fatalf("queryTopBlobUsersAllByCount: %v", err)
		}
		// blob_user_stats is confirmed-only; totals fold in the pending set.
		if len(users) != 1 || users[0].BlobCount != 1 || users[0].BlobSharePercent != 50 {
			t.Fatalf("unexpected users: %+v", users)
		}
	})

	t.Run("queryUserByAddress", func(t *testing.T) {
		var user models.BlobUserStats
		if err := sqlxDB.GetContext(ctx, &user, queryUserByAddress, 1, "0xfrom"); err != nil {
			t.Fatalf("queryUserByAddress: %v", err)
		}
		if user.BlobCount != 1 {
			t.Fatalf("unexpected user: %+v", user)
		}
	})

	t.Run("queryBlobUserCategoryBreakdownAll", func(t *testing.T) {
		var shares []models.BlobUserCategoryShare
		if err := sqlxDB.SelectContext(ctx, &shares, queryBlobUserCategoryBreakdownAll, 1, "all"); err != nil {
			t.Fatalf("queryBlobUserCategoryBreakdownAll: %v", err)
		}
		if len(shares) != 1 || shares[0].BlobCount != 1 {
			t.Fatalf("unexpected category shares: %+v", shares)
		}
	})

	t.Run("queryIndexedBlockCoverage", func(t *testing.T) {
		var coverage indexedBlockCoverage
		if err := sqlxDB.GetContext(ctx, &coverage, queryIndexedBlockCoverage, 1); err != nil {
			t.Fatalf("queryIndexedBlockCoverage: %v", err)
		}
		// The seeded rows skip block 99: bounds span the interior gap.
		if coverage.EarliestIndexedBlock == nil || *coverage.EarliestIndexedBlock != 98 {
			t.Fatalf("expected earliest indexed block 98, got %+v", coverage)
		}
		if coverage.LatestIndexedBlock == nil || *coverage.LatestIndexedBlock != 100 {
			t.Fatalf("expected latest indexed block 100, got %+v", coverage)
		}
		// A network with no indexed blocks scans NULL aggregates into nil
		// bounds rather than erroring.
		coverage = indexedBlockCoverage{}
		if err := sqlxDB.GetContext(ctx, &coverage, queryIndexedBlockCoverage, 424242); err != nil {
			t.Fatalf("queryIndexedBlockCoverage empty network: %v", err)
		}
		if coverage.EarliestIndexedBlock != nil || coverage.LatestIndexedBlock != nil {
			t.Fatalf("expected nil coverage bounds for empty network, got %+v", coverage)
		}
	})

	t.Run("queryDevIndexerCounts", func(t *testing.T) {
		var counts models.BlobCountTotals
		if err := sqlxDB.GetContext(ctx, &counts, queryDevIndexerCounts, 1); err != nil {
			t.Fatalf("queryDevIndexerCounts: %v", err)
		}
		if counts.Confirmed != 1 || counts.Pending != 1 {
			t.Fatalf("unexpected counts: %+v", counts)
		}
	})
}

// TestUserWindowQueriesAgainstRealPostgres validates the windowed /users query
// text against a real schema: the 1h tier reading trigger-maintained fine
// (60s) rollup buckets, the hourly tier's 24h/30d bounds, and the
// deterministic ordering of count ties. It runs on a deliberately non-UTC
// session so a window bound computed in session-local time (instead of UTC
// wall time, matching the naive bucket_start timestamps) shifts by the UTC
// offset and fails the bounds assertions.
func TestUserWindowQueriesAgainstRealPostgres(t *testing.T) {
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

	// Pin the pool to one connection so the session TimeZone below applies to
	// every query in this test. Tokyo is UTC+9: far enough that any
	// session-local window bound moves by hours in the direction that changes
	// which seeded blobs fall inside each window.
	sqlxDB.SetMaxOpenConns(1)
	if _, err := sqlxDB.Exec("SET TIME ZONE 'Asia/Tokyo'"); err != nil {
		t.Fatalf("set session time zone: %v", err)
	}

	ctx := context.Background()
	recent := time.Now().UTC().Add(-2 * time.Minute)
	older := time.Now().UTC().Add(-3 * 24 * time.Hour)

	// Sender A is attributed and active both minutes and days ago; sender B is
	// unattributed and only recent, with higher spend so the recent-window
	// count tie must break by cost. The insert triggers populate both the fine
	// (60s) and hourly rollup buckets for the recent rows; the older row is
	// outside fine retention and only gets hourly buckets.
	if _, err := sqlxDB.Exec(`
		INSERT INTO blobs (
			chain_id, block_number, blob_index, tx_hash, from_address, user_attribution,
			blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
			timestamp, max_fee_per_blob_gas, blob_gas_used
		) VALUES
			(1, 200, 0, '0xrecenta', '0xaaa', 'RollupA', 131072, 10, 2, 100, $1, 12, 131072),
			(1, 200, 1, '0xrecentb', '0xbbb', '', 131072, 10, 2, 200, $1, 12, 131072),
			(1, 100, 0, '0xoldera', '0xaaa', 'RollupA', 131072, 10, 2, 300, $2, 12, 131072)
	`, recent, older); err != nil {
		t.Fatalf("seed blobs: %v", err)
	}

	for _, window := range []string{"1h", "24h"} {
		t.Run("queryTopBlobUsersWithOptions "+window, func(t *testing.T) {
			var users []models.BlobUserStats
			if err := sqlxDB.SelectContext(ctx, &users, queryTopBlobUsersWithOptions, 1, 10, 0, window, "count"); err != nil {
				t.Fatalf("queryTopBlobUsersWithOptions %s: %v", window, err)
			}
			// Only the two recent blobs are inside the window; the count tie
			// breaks by spend, so B (200 wei) precedes A (100 wei).
			if len(users) != 2 || users[0].Address != "0xbbb" || users[1].Address != "0xaaa" {
				t.Fatalf("unexpected %s users: %+v", window, users)
			}
			if users[0].BlobCount != 1 || users[0].BlobSharePercent != 50 || users[0].TotalCostWei != "200" {
				t.Fatalf("unexpected %s leader aggregates: %+v", window, users[0])
			}
		})
	}

	t.Run("queryTopBlobUsersWithOptions 30d", func(t *testing.T) {
		var users []models.BlobUserStats
		if err := sqlxDB.SelectContext(ctx, &users, queryTopBlobUsersWithOptions, 1, 10, 0, "30d", "count"); err != nil {
			t.Fatalf("queryTopBlobUsersWithOptions 30d: %v", err)
		}
		// The 30d window also covers the older blob, so A leads on count.
		if len(users) != 2 || users[0].Address != "0xaaa" || users[0].BlobCount != 2 || users[1].BlobCount != 1 {
			t.Fatalf("unexpected 30d users: %+v", users)
		}
		if users[0].TotalCostWei != "400" || users[0].Name != "RollupA" {
			t.Fatalf("unexpected 30d leader aggregates: %+v", users[0])
		}
	})

	t.Run("queryTopUnattributedBlobUsersWithOptions 1h", func(t *testing.T) {
		var users []models.BlobUserStats
		if err := sqlxDB.SelectContext(ctx, &users, queryTopUnattributedBlobUsersWithOptions, 1, 10, 0, "1h", "count"); err != nil {
			t.Fatalf("queryTopUnattributedBlobUsersWithOptions 1h: %v", err)
		}
		if len(users) != 1 || users[0].Address != "0xbbb" {
			t.Fatalf("unexpected unattributed users: %+v", users)
		}
	})

	t.Run("queryBlobUserCategoryBreakdown windows", func(t *testing.T) {
		var shares []models.BlobUserCategoryShare
		if err := sqlxDB.SelectContext(ctx, &shares, queryBlobUserCategoryBreakdown, 1, "1h"); err != nil {
			t.Fatalf("queryBlobUserCategoryBreakdown 1h: %v", err)
		}
		if len(shares) != 1 || shares[0].Category != "unknown" || shares[0].BlobCount != 2 {
			t.Fatalf("unexpected 1h category shares: %+v", shares)
		}
		if err := sqlxDB.SelectContext(ctx, &shares, queryBlobUserCategoryBreakdown, 1, "30d"); err != nil {
			t.Fatalf("queryBlobUserCategoryBreakdown 30d: %v", err)
		}
		if len(shares) != 1 || shares[0].BlobCount != 3 {
			t.Fatalf("unexpected 30d category shares: %+v", shares)
		}
	})
}

// TestWSPollerQueriesAgainstRealPostgres validates the WebSocket poller's and
// snapshot builder's query text against a real schema, including type
// unification of block_number scans into uint64 slices. The /block/{number}
// endpoint's single-block lookup shares the seeded data.
func TestWSPollerQueriesAgainstRealPostgres(t *testing.T) {
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
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	if _, err := sqlxDB.Exec(`
		INSERT INTO block_metrics (chain_id, block_number, block_timestamp, blob_count)
		VALUES (1, 100, $1, 1), (1, 101, $1, 0)
	`, now); err != nil {
		t.Fatalf("seed block_metrics: %v", err)
	}
	if _, err := sqlxDB.Exec(`
		INSERT INTO blobs (
			chain_id, block_number, blob_index, tx_hash, from_address, user_attribution,
			blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
			timestamp, max_fee_per_blob_gas, blob_gas_used
		) VALUES (1, 100, 0, '0xws', '0xfrom', 'Rollup', 131072, 10, 2, 100, $1, 12, 131072)
	`, now); err != nil {
		t.Fatalf("seed blob: %v", err)
	}

	t.Run("queryRecentBlockMetricsNumbers", func(t *testing.T) {
		var numbers []uint64
		if err := sqlxDB.SelectContext(ctx, &numbers, queryRecentBlockMetricsNumbers, 1, 32); err != nil {
			t.Fatalf("queryRecentBlockMetricsNumbers: %v", err)
		}
		if len(numbers) != 2 || numbers[0] != 101 || numbers[1] != 100 {
			t.Fatalf("got %v, want [101 100]", numbers)
		}
		// Empty network yields an empty (non-error) result.
		numbers = nil
		if err := sqlxDB.SelectContext(ctx, &numbers, queryRecentBlockMetricsNumbers, 424242, 32); err != nil {
			t.Fatalf("queryRecentBlockMetricsNumbers empty: %v", err)
		}
		if len(numbers) != 0 {
			t.Fatalf("got %v for empty network, want none", numbers)
		}
	})

	t.Run("queryBlockMetricsNumbersSince", func(t *testing.T) {
		var numbers []uint64
		if err := sqlxDB.SelectContext(ctx, &numbers, queryBlockMetricsNumbersSince, 1, uint64(99), 10); err != nil {
			t.Fatalf("queryBlockMetricsNumbersSince: %v", err)
		}
		if len(numbers) != 2 || numbers[0] != 100 || numbers[1] != 101 {
			t.Fatalf("got %v, want [100 101]", numbers)
		}
	})

	t.Run("queryBlockMetricsForBlock", func(t *testing.T) {
		var metric models.BlockMetrics
		if err := sqlxDB.GetContext(ctx, &metric, queryBlockMetricsForBlock, 1, int64(100)); err != nil {
			t.Fatalf("queryBlockMetricsForBlock: %v", err)
		}
		if metric.BlockNumber != 100 || metric.BlobCount != 1 {
			t.Fatalf("unexpected metric: %+v", metric)
		}
		if err := sqlxDB.GetContext(ctx, &metric, queryBlockMetricsForBlock, 1, int64(999)); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("expected sql.ErrNoRows for unindexed block, got %v", err)
		}
	})

	t.Run("queryBlobsByBlockNumber", func(t *testing.T) {
		var blobs []models.Blob
		if err := sqlxDB.SelectContext(ctx, &blobs, queryBlobsByBlockNumber, 1, uint64(100)); err != nil {
			t.Fatalf("queryBlobsByBlockNumber: %v", err)
		}
		if len(blobs) != 1 || blobs[0].TxHash != "0xws" {
			t.Fatalf("unexpected blobs: %+v", blobs)
		}
	})

	t.Run("queryBlobsByBlockNumbers", func(t *testing.T) {
		var blobs []models.Blob
		if err := sqlxDB.SelectContext(ctx, &blobs, queryBlobsByBlockNumbers, 1, pq.Array([]int64{100, 101})); err != nil {
			t.Fatalf("queryBlobsByBlockNumbers: %v", err)
		}
		if len(blobs) != 1 || blobs[0].BlockNumber != 100 {
			t.Fatalf("unexpected blobs: %+v", blobs)
		}
	})
}
