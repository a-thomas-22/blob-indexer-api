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

	if _, err := sqlxDB.Exec(`
		INSERT INTO blobs (
			chain_id, block_number, blob_index, tx_hash, from_address, user_attribution,
			blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
			timestamp, max_fee_per_blob_gas, blob_gas_used
		) VALUES (1, 100, 0, '0xconfirmed', '0xfrom', 'Rollup', 131072, 10, 2, 100, $1, 12, 131072)
	`, now); err != nil {
		t.Fatalf("seed confirmed blob: %v", err)
	}
	if _, err := sqlxDB.Exec(`
		INSERT INTO mempool_blobs (
			chain_id, tx_hash, blob_index, from_address, user_attribution,
			blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
			timestamp, max_fee_per_blob_gas, blob_gas_used
		) VALUES (1, '0xpendingtx', 0, '0xfrom', 'Rollup', 131072, 50, 10, 500, $1, 60, 131072)
	`, now); err != nil {
		t.Fatalf("seed mempool blob: %v", err)
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

// TestWSPollerQueriesAgainstRealPostgres validates the WebSocket poller's and
// snapshot builder's query text against a real schema, including type
// unification of block_number scans into uint64 slices.
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

	t.Run("queryMaxBlockMetricsNumber", func(t *testing.T) {
		var maxBlock uint64
		if err := sqlxDB.GetContext(ctx, &maxBlock, queryMaxBlockMetricsNumber, 1); err != nil {
			t.Fatalf("queryMaxBlockMetricsNumber: %v", err)
		}
		if maxBlock != 101 {
			t.Fatalf("got max %d, want 101", maxBlock)
		}
		// Empty network must COALESCE to zero, not error.
		if err := sqlxDB.GetContext(ctx, &maxBlock, queryMaxBlockMetricsNumber, 424242); err != nil {
			t.Fatalf("queryMaxBlockMetricsNumber empty: %v", err)
		}
		if maxBlock != 0 {
			t.Fatalf("got max %d for empty network, want 0", maxBlock)
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
