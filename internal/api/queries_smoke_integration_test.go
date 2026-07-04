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
	"os"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"

	"github.com/a-thomas-22/blob-indexer-api/internal/db"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
)

func TestMempoolQueriesAgainstRealPostgres(t *testing.T) {
	url := os.Getenv("TEST_DB_URL")
	if url == "" {
		t.Skip("TEST_DB_URL not set; skipping integration test")
	}
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
			timestamp, confirmed, max_fee_per_blob_gas, blob_gas_used
		) VALUES (1, 100, 0, '0xconfirmed', '0xfrom', 'Rollup', 131072, 10, 2, 100, $1, true, 12, 131072)
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

	t.Run("queryTopBlobUsersAll", func(t *testing.T) {
		var users []models.BlobUserStats
		if err := sqlxDB.SelectContext(ctx, &users, queryTopBlobUsersAll, 1, 10, 0, "all", "count"); err != nil {
			t.Fatalf("queryTopBlobUsersAll: %v", err)
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
