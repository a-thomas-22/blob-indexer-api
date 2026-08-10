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
			timestamp, max_fee_per_blob_gas, blob_gas_used, versioned_hash, slot
		) VALUES (1, 100, 0, '0xconfirmed', '0xfrom', 'Rollup', 131072, 10, 2, 100, $1, 12, 131072, '0xvhconfirmed', 12345678)
	`, now); err != nil {
		t.Fatalf("seed confirmed blob: %v", err)
	}
	if _, err := sqlxDB.Exec(`
		INSERT INTO mempool_blobs (
			chain_id, tx_hash, blob_index, from_address, user_attribution,
			blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
			timestamp, max_fee_per_blob_gas, blob_gas_used, versioned_hash
		) VALUES (1, '0xpendingtx', 0, '0xfrom', 'Rollup', 131072, 50, 10, 500, $1, 60, 131072, '0xvhpending')
	`, now); err != nil {
		t.Fatalf("seed mempool blob: %v", err)
	}
	if _, err := sqlxDB.Exec(`
		INSERT INTO block_metrics (chain_id, block_number, block_timestamp)
		VALUES (1, 100, $1)
	`, now); err != nil {
		t.Fatalf("seed block metrics: %v", err)
	}
	// Known-rollup row for a sender with no blob history: exercises the /search
	// rollup-name arm without disturbing the blob_user_stats-derived
	// assertions above.
	if _, err := sqlxDB.Exec(`
		INSERT INTO blob_users (chain_id, address, name, description, category, first_seen, last_seen)
		VALUES (1, '0xbase', 'Base', '', 'rollup', $1, $1)
	`, now); err != nil {
		t.Fatalf("seed blob user: %v", err)
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
	if _, err := sqlxDB.Exec(`
		INSERT INTO blob_replacements (chain_id, replaced_tx_hash, replacement_tx_hash, from_address, nonce, replaced_at)
		VALUES (1, '0xreplaced', '0xpendingtx', '0xfrom', 7, $1)
	`, now); err != nil {
		t.Fatalf("seed blob replacement: %v", err)
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

	t.Run("queryBlobReplacements", func(t *testing.T) {
		var events []models.BlobReplacement
		if err := sqlxDB.SelectContext(ctx, &events, queryBlobReplacements, 1, 10, 0); err != nil {
			t.Fatalf("queryBlobReplacements: %v", err)
		}
		if len(events) != 1 || events[0].ReplacedTxHash != "0xreplaced" || events[0].ReplacementTxHash != "0xpendingtx" || events[0].Nonce != 7 {
			t.Fatalf("unexpected replacement events: %+v", events)
		}
	})

	t.Run("queryBlobReplacementsByTxHash", func(t *testing.T) {
		// Matches on either side of the replacement; a hash the log never saw
		// returns empty rather than erroring.
		for _, hash := range []string{"0xreplaced", "0xpendingtx"} {
			var events []models.BlobReplacement
			if err := sqlxDB.SelectContext(ctx, &events, queryBlobReplacementsByTxHash, 1, hash, 10, 0); err != nil {
				t.Fatalf("queryBlobReplacementsByTxHash(%s): %v", hash, err)
			}
			if len(events) != 1 {
				t.Fatalf("expected 1 event for %s, got %+v", hash, events)
			}
		}
		var events []models.BlobReplacement
		if err := sqlxDB.SelectContext(ctx, &events, queryBlobReplacementsByTxHash, 1, "0xunknown", 10, 0); err != nil {
			t.Fatalf("queryBlobReplacementsByTxHash(unknown): %v", err)
		}
		if len(events) != 0 {
			t.Fatalf("expected no events for unknown hash, got %+v", events)
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
		if blob.Slot == nil || *blob.Slot != 12345678 {
			t.Fatalf("expected stored slot 12345678 on confirmed blob, got %+v", blob.Slot)
		}
		if err := sqlxDB.GetContext(ctx, &blob, queryBlobByTxHash, "0xpendingtx", 1); err != nil {
			t.Fatalf("queryBlobByTxHash pending: %v", err)
		}
		if blob.Confirmed || blob.BlockNumber != models.PendingBlockNumber {
			t.Fatalf("expected pending blob, got %+v", blob)
		}
		if blob.Slot != nil {
			t.Fatalf("expected NULL slot on the mempool projection, got %d", *blob.Slot)
		}
		if err := sqlxDB.GetContext(ctx, &blob, queryBlobByTxHash, "0xmissing", 1); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("expected sql.ErrNoRows for missing tx, got %v", err)
		}
	})

	t.Run("queryBlobByVersionedHash", func(t *testing.T) {
		// Temp rows scoped to this subtest: a second blob on the confirmed tx
		// (multi-blob hash-list assembly) and a pending re-post of the
		// confirmed blob's content (identical content produces the identical
		// versioned hash, so the lookup must prefer the confirmed row). Both
		// are removed on exit so later subtests keep their seeded counts; the
		// blobs delete also rolls its trigger-maintained stats back.
		if _, err := sqlxDB.Exec(`
			INSERT INTO blobs (
				chain_id, block_number, blob_index, tx_hash, from_address, user_attribution,
				blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
				timestamp, max_fee_per_blob_gas, blob_gas_used, versioned_hash
			) VALUES (1, 100, 1, '0xconfirmed', '0xfrom', 'Rollup', 131072, 10, 2, 100, $1, 12, 131072, '0xvhconfirmed2')
		`, now); err != nil {
			t.Fatalf("seed second confirmed blob: %v", err)
		}
		if _, err := sqlxDB.Exec(`
			INSERT INTO mempool_blobs (
				chain_id, tx_hash, blob_index, from_address, user_attribution,
				blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
				timestamp, max_fee_per_blob_gas, blob_gas_used, versioned_hash
			) VALUES (1, '0xpendingdup', 0, '0xfrom', 'Rollup', 131072, 50, 10, 500, $1, 60, 131072, '0xvhconfirmed')
		`, now); err != nil {
			t.Fatalf("seed duplicate-content pending blob: %v", err)
		}
		defer func() {
			if _, err := sqlxDB.Exec(`DELETE FROM blobs WHERE chain_id = 1 AND tx_hash = '0xconfirmed' AND blob_index = 1`); err != nil {
				t.Fatalf("clean up second confirmed blob: %v", err)
			}
			if _, err := sqlxDB.Exec(`DELETE FROM mempool_blobs WHERE chain_id = 1 AND tx_hash = '0xpendingdup'`); err != nil {
				t.Fatalf("clean up duplicate-content pending blob: %v", err)
			}
		}()

		// A hash carried by both a confirmed and a pending transaction resolves
		// to the confirmed row, and the matched row is the blob itself.
		var blob models.Blob
		if err := sqlxDB.GetContext(ctx, &blob, queryBlobByVersionedHash, "0xvhconfirmed", 1); err != nil {
			t.Fatalf("queryBlobByVersionedHash confirmed: %v", err)
		}
		if !blob.Confirmed || blob.BlockNumber != 100 || blob.TxHash != "0xconfirmed" || blob.BlobIndex != 0 {
			t.Fatalf("expected confirmed blob at index 0, got %+v", blob)
		}
		// The response carries the transaction's full ordered hash list,
		// assembled from the sibling rows' scalar hashes.
		want := pq.StringArray{"0xvhconfirmed", "0xvhconfirmed2"}
		if len(blob.VersionedHashes) != 2 || blob.VersionedHashes[0] != want[0] || blob.VersionedHashes[1] != want[1] {
			t.Fatalf("expected versioned hash list %v, got %v", want, blob.VersionedHashes)
		}

		// Matching the second blob of the tx returns that blob's own row.
		if err := sqlxDB.GetContext(ctx, &blob, queryBlobByVersionedHash, "0xvhconfirmed2", 1); err != nil {
			t.Fatalf("queryBlobByVersionedHash second blob: %v", err)
		}
		if blob.TxHash != "0xconfirmed" || blob.BlobIndex != 1 {
			t.Fatalf("expected second blob of confirmed tx, got %+v", blob)
		}

		// A pending-only hash falls through to the mempool arm.
		if err := sqlxDB.GetContext(ctx, &blob, queryBlobByVersionedHash, "0xvhpending", 1); err != nil {
			t.Fatalf("queryBlobByVersionedHash pending: %v", err)
		}
		if blob.Confirmed || blob.BlockNumber != models.PendingBlockNumber || blob.TxHash != "0xpendingtx" {
			t.Fatalf("expected pending blob, got %+v", blob)
		}

		if err := sqlxDB.GetContext(ctx, &blob, queryBlobByVersionedHash, "0xvhmissing", 1); !errors.Is(err, sql.ErrNoRows) {
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

	t.Run("querySearchBlockByNumber", func(t *testing.T) {
		var blockNumber int64
		if err := sqlxDB.GetContext(ctx, &blockNumber, querySearchBlockByNumber, 1, 100); err != nil {
			t.Fatalf("querySearchBlockByNumber: %v", err)
		}
		if blockNumber != 100 {
			t.Fatalf("expected block 100, got %d", blockNumber)
		}
		if err := sqlxDB.GetContext(ctx, &blockNumber, querySearchBlockByNumber, 1, 999); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("expected sql.ErrNoRows for unindexed block, got %v", err)
		}
	})

	t.Run("querySearchTxByHash", func(t *testing.T) {
		var tx searchTxRow
		if err := sqlxDB.GetContext(ctx, &tx, querySearchTxByHash, 1, "0xconfirmed"); err != nil {
			t.Fatalf("querySearchTxByHash confirmed: %v", err)
		}
		if tx.TxHash != "0xconfirmed" || tx.BlockNumber != 100 {
			t.Fatalf("unexpected confirmed tx match: %+v", tx)
		}
		if err := sqlxDB.GetContext(ctx, &tx, querySearchTxByHash, 1, "0xpendingtx"); err != nil {
			t.Fatalf("querySearchTxByHash pending: %v", err)
		}
		if tx.TxHash != "0xpendingtx" || tx.BlockNumber != models.PendingBlockNumber {
			t.Fatalf("unexpected pending tx match: %+v", tx)
		}
		if err := sqlxDB.GetContext(ctx, &tx, querySearchTxByHash, 1, "0xmissing"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("expected sql.ErrNoRows for missing tx, got %v", err)
		}
	})

	t.Run("querySearchBlobByVersionedHash", func(t *testing.T) {
		var blob searchBlobRow
		if err := sqlxDB.GetContext(ctx, &blob, querySearchBlobByVersionedHash, 1, "0xvhconfirmed"); err != nil {
			t.Fatalf("querySearchBlobByVersionedHash confirmed: %v", err)
		}
		if blob.VersionedHash != "0xvhconfirmed" || blob.TxHash != "0xconfirmed" || blob.BlockNumber != 100 {
			t.Fatalf("unexpected confirmed blob match: %+v", blob)
		}
		if err := sqlxDB.GetContext(ctx, &blob, querySearchBlobByVersionedHash, 1, "0xvhpending"); err != nil {
			t.Fatalf("querySearchBlobByVersionedHash pending: %v", err)
		}
		if blob.VersionedHash != "0xvhpending" || blob.TxHash != "0xpendingtx" || blob.BlockNumber != models.PendingBlockNumber {
			t.Fatalf("unexpected pending blob match: %+v", blob)
		}
		if err := sqlxDB.GetContext(ctx, &blob, querySearchBlobByVersionedHash, 1, "0xmissing"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("expected sql.ErrNoRows for missing versioned hash, got %v", err)
		}
	})

	t.Run("querySearchSenderByAddress", func(t *testing.T) {
		var sender searchSenderRow
		if err := sqlxDB.GetContext(ctx, &sender, querySearchSenderByAddress, 1, "0xfrom"); err != nil {
			t.Fatalf("querySearchSenderByAddress: %v", err)
		}
		if sender.FromAddress != "0xfrom" || sender.UserAttribution != "Rollup" {
			t.Fatalf("unexpected sender match: %+v", sender)
		}
		if err := sqlxDB.GetContext(ctx, &sender, querySearchSenderByAddress, 1, "0xnothere"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("expected sql.ErrNoRows for unknown sender, got %v", err)
		}
	})

	t.Run("querySearchRollupsByName", func(t *testing.T) {
		var rollups []searchRollupRow
		if err := sqlxDB.SelectContext(ctx, &rollups, querySearchRollupsByName, 1, escapeLikePattern("ba")+"%", maxSearchRollupMatches); err != nil {
			t.Fatalf("querySearchRollupsByName: %v", err)
		}
		if len(rollups) != 1 || rollups[0].Name != "Base" || len(rollups[0].Addresses) != 1 || rollups[0].Addresses[0] != "0xbase" {
			t.Fatalf("unexpected rollup matches: %+v", rollups)
		}
		// LIKE metacharacters in user input must match literally, not as
		// wildcards: "_ase" would otherwise match "Base".
		rollups = nil
		if err := sqlxDB.SelectContext(ctx, &rollups, querySearchRollupsByName, 1, escapeLikePattern("_ase")+"%", maxSearchRollupMatches); err != nil {
			t.Fatalf("querySearchRollupsByName escaped: %v", err)
		}
		if len(rollups) != 0 {
			t.Fatalf("expected no matches for escaped pattern, got %+v", rollups)
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

// TestUserGroupQueriesAgainstRealPostgres validates the entity-grouped /users
// query text against a real schema: attributed addresses merging into one row
// per entity slug (including case-insensitive attribution variants),
// unattributed addresses surviving as individual rows, exact aggregate sums,
// shares computed over the same denominators as the per-address queries, and
// — the point of the grouped mode — ordering and LIMIT applied after
// grouping. The non-UTC session mirrors TestUserWindowQueriesAgainstRealPostgres
// so the grouped windowed query's own UTC window bound is validated too.
func TestUserGroupQueriesAgainstRealPostgres(t *testing.T) {
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

	sqlxDB.SetMaxOpenConns(1)
	if _, err := sqlxDB.Exec("SET TIME ZONE 'Asia/Tokyo'"); err != nil {
		t.Fatalf("set session time zone: %v", err)
	}

	ctx := context.Background()
	recent := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
	recentLater := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)

	// Scroll posts from two addresses (attribution differing only in case, so
	// the slug must merge them): 0xaaa and 0xccc with 2 blobs / 200 wei each.
	// 0xbbb is unattributed with 3 blobs / 600 wei — more blobs than any
	// single Scroll address but fewer than the merged entity, which is what
	// separates per-address from post-grouping ordering.
	if _, err := sqlxDB.Exec(`
		INSERT INTO blobs (
			chain_id, block_number, blob_index, tx_hash, from_address, user_attribution,
			blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
			timestamp, max_fee_per_blob_gas, blob_gas_used
		) VALUES
			(1, 200, 0, '0xa1', '0xaaa', 'Scroll', 131072, 10, 2, 100, $1, 12, 131072),
			(1, 200, 1, '0xa2', '0xaaa', 'Scroll', 131072, 10, 2, 100, $1, 12, 131072),
			(1, 201, 0, '0xc1', '0xccc', 'scroll', 131072, 10, 2, 100, $1, 12, 131072),
			(1, 201, 1, '0xc2', '0xccc', 'scroll', 131072, 10, 2, 100, $2, 12, 131072),
			(1, 202, 0, '0xb1', '0xbbb', '', 131072, 10, 2, 200, $1, 12, 131072),
			(1, 202, 1, '0xb2', '0xbbb', '', 131072, 10, 2, 200, $1, 12, 131072),
			(1, 202, 2, '0xb3', '0xbbb', '', 131072, 10, 2, 200, $1, 12, 131072)
	`, recent, recentLater); err != nil {
		t.Fatalf("seed blobs: %v", err)
	}
	// Category source for the Scroll entity: only one member has a known-user
	// row, so the grouped category must still resolve to 'rollup'.
	if _, err := sqlxDB.Exec(`
		INSERT INTO blob_users (chain_id, address, name, description, category, first_seen, last_seen)
		VALUES (1, '0xaaa', 'Scroll', '', 'rollup', $1, $1)
	`, recent); err != nil {
		t.Fatalf("seed blob user: %v", err)
	}
	// A pending blob participates in the all-history share denominator
	// (matching the per-address all-history queries) but not in windowed
	// shares, which cover confirmed blobs only.
	if _, err := sqlxDB.Exec(`
		INSERT INTO mempool_blobs (
			chain_id, tx_hash, blob_index, from_address, user_attribution,
			blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
			timestamp, max_fee_per_blob_gas, blob_gas_used
		) VALUES (1, '0xpending', 0, '0xddd', '', 131072, 50, 10, 500, $1, 60, 131072)
	`, recent); err != nil {
		t.Fatalf("seed mempool blob: %v", err)
	}

	for _, window := range []string{"1h", "24h"} {
		t.Run("queryTopBlobUserGroupsWithOptions "+window, func(t *testing.T) {
			var groups []models.BlobUserGroupStats
			if err := sqlxDB.SelectContext(ctx, &groups, queryTopBlobUserGroupsWithOptions, 1, 10, 0, window, "count"); err != nil {
				t.Fatalf("queryTopBlobUserGroupsWithOptions %s: %v", window, err)
			}
			if len(groups) != 2 {
				t.Fatalf("expected 2 grouped rows, got %+v", groups)
			}
			scroll, unattributed := groups[0], groups[1]
			if scroll.Key != "scroll" || scroll.Name != "Scroll" || scroll.Category != "rollup" {
				t.Fatalf("unexpected entity identity: %+v", scroll)
			}
			// Members tie on count and spend, so the address tie-breaker fixes
			// busiest-first ordering.
			if len(scroll.Addresses) != 2 || scroll.Addresses[0] != "0xaaa" || scroll.Addresses[1] != "0xccc" {
				t.Fatalf("unexpected entity members: %+v", scroll.Addresses)
			}
			if scroll.BlobCount != 4 || scroll.TotalCostWei != "400" {
				t.Fatalf("unexpected entity aggregates: %+v", scroll)
			}
			// Shares over the full 7-blob / 1000-wei window: computed from the
			// grouped sums, equal to the members' per-address shares combined.
			if scroll.BlobSharePercent != 57.142857 || scroll.SpendSharePercent != 40 {
				t.Fatalf("unexpected entity shares: %+v", scroll)
			}
			if unattributed.Key != "0xbbb" || unattributed.Name != "" || unattributed.Category != "unknown" {
				t.Fatalf("unexpected unattributed row identity: %+v", unattributed)
			}
			if len(unattributed.Addresses) != 1 || unattributed.Addresses[0] != "0xbbb" {
				t.Fatalf("unexpected unattributed members: %+v", unattributed.Addresses)
			}
			if unattributed.BlobCount != 3 || unattributed.TotalCostWei != "600" {
				t.Fatalf("unexpected unattributed aggregates: %+v", unattributed)
			}
			if unattributed.BlobSharePercent != 42.857143 || unattributed.SpendSharePercent != 60 {
				t.Fatalf("unexpected unattributed shares: %+v", unattributed)
			}
			if sum := scroll.BlobSharePercent + unattributed.BlobSharePercent; sum < 99.999999 || sum > 100.000001 {
				t.Fatalf("grouped blob shares should cover the window, got %v", sum)
			}
			if sum := scroll.SpendSharePercent + unattributed.SpendSharePercent; sum < 99.999999 || sum > 100.000001 {
				t.Fatalf("grouped spend shares should cover the window, got %v", sum)
			}

			// The entity's last_timestamp is the max across members — compare
			// against the per-address rows from the same session so timestamp
			// session/parsing conventions cancel out.
			var users []models.BlobUserStats
			if err := sqlxDB.SelectContext(ctx, &users, queryTopBlobUsersWithOptions, 1, 10, 0, window, "count"); err != nil {
				t.Fatalf("queryTopBlobUsersWithOptions %s: %v", window, err)
			}
			var wantLast time.Time
			for _, user := range users {
				if (user.Address == "0xaaa" || user.Address == "0xccc") && user.LastTimestamp.After(wantLast) {
					wantLast = user.LastTimestamp
				}
			}
			if !scroll.LastTimestamp.Equal(wantLast) {
				t.Fatalf("expected entity last_timestamp %v (max across members), got %v", wantLast, scroll.LastTimestamp)
			}
		})
	}

	t.Run("limit applies after grouping", func(t *testing.T) {
		// Per-address, 0xbbb leads on count (3 vs 2/2) …
		var users []models.BlobUserStats
		if err := sqlxDB.SelectContext(ctx, &users, queryTopBlobUsersWithOptions, 1, 1, 0, "1h", "count"); err != nil {
			t.Fatalf("queryTopBlobUsersWithOptions limit 1: %v", err)
		}
		if len(users) != 1 || users[0].Address != "0xbbb" {
			t.Fatalf("expected per-address leader 0xbbb, got %+v", users)
		}
		// … but grouped, the merged Scroll entity (4 blobs) takes the single
		// slot with its combined totals: the limit ran after grouping.
		var groups []models.BlobUserGroupStats
		if err := sqlxDB.SelectContext(ctx, &groups, queryTopBlobUserGroupsWithOptions, 1, 1, 0, "1h", "count"); err != nil {
			t.Fatalf("queryTopBlobUserGroupsWithOptions limit 1: %v", err)
		}
		if len(groups) != 1 || groups[0].Key != "scroll" || groups[0].BlobCount != 4 {
			t.Fatalf("expected grouped leader scroll with combined count, got %+v", groups)
		}
	})

	t.Run("spend sort and offset after grouping", func(t *testing.T) {
		var groups []models.BlobUserGroupStats
		if err := sqlxDB.SelectContext(ctx, &groups, queryTopBlobUserGroupsWithOptions, 1, 10, 0, "1h", "spend"); err != nil {
			t.Fatalf("queryTopBlobUserGroupsWithOptions spend: %v", err)
		}
		if len(groups) != 2 || groups[0].Key != "0xbbb" || groups[1].Key != "scroll" {
			t.Fatalf("expected spend order [0xbbb scroll], got %+v", groups)
		}
		groups = nil
		if err := sqlxDB.SelectContext(ctx, &groups, queryTopBlobUserGroupsWithOptions, 1, 10, 1, "1h", "count"); err != nil {
			t.Fatalf("queryTopBlobUserGroupsWithOptions offset 1: %v", err)
		}
		if len(groups) != 1 || groups[0].Key != "0xbbb" {
			t.Fatalf("expected offset to skip the grouped leader, got %+v", groups)
		}
	})

	t.Run("degenerate slug keeps name and address key", func(t *testing.T) {
		// Chain 2 keeps this case out of the chain-1 share denominators. A
		// fully non-ASCII attribution slugs to '', so the two senders must
		// stay separate address-keyed rows — but with their attribution name
		// preserved, not blanked into pseudo-unattributed rows.
		if _, err := sqlxDB.Exec(`
			INSERT INTO networks (chain_id, name, start_block, is_enabled)
			VALUES (2, 'groupedtest', '0', true)
			ON CONFLICT (chain_id) DO NOTHING
		`); err != nil {
			t.Fatalf("seed network: %v", err)
		}
		if _, err := sqlxDB.Exec(`
			INSERT INTO blobs (
				chain_id, block_number, blob_index, tx_hash, from_address, user_attribution,
				blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
				timestamp, max_fee_per_blob_gas, blob_gas_used
			) VALUES
				(2, 300, 0, '0xe1', '0xeee', '東京', 131072, 10, 2, 100, $1, 12, 131072),
				(2, 300, 1, '0xf1', '0xfff', '東京', 131072, 10, 2, 100, $1, 12, 131072)
		`, recent); err != nil {
			t.Fatalf("seed degenerate-name blobs: %v", err)
		}

		for name, query := range map[string]string{
			"windowed": queryTopBlobUserGroupsWithOptions,
			"all":      queryTopBlobUserGroupsAll,
		} {
			window := "1h"
			if name == "all" {
				window = "all"
			}
			var groups []models.BlobUserGroupStats
			if err := sqlxDB.SelectContext(ctx, &groups, query, 2, 10, 0, window, "count"); err != nil {
				t.Fatalf("%s grouped query on degenerate names: %v", name, err)
			}
			if len(groups) != 2 {
				t.Fatalf("%s: expected 2 address-keyed rows for degenerate slugs, got %+v", name, groups)
			}
			for _, row := range groups {
				if row.Key != row.Addresses[0] || len(row.Addresses) != 1 {
					t.Fatalf("%s: expected address-keyed single-member row, got %+v", name, row)
				}
				if row.Name != "東京" {
					t.Fatalf("%s: expected preserved attribution name, got %+v", name, row)
				}
			}
		}
	})

	t.Run("queryTopBlobUserGroupsAll", func(t *testing.T) {
		var groups []models.BlobUserGroupStats
		if err := sqlxDB.SelectContext(ctx, &groups, queryTopBlobUserGroupsAll, 1, 10, 0, "all", "count"); err != nil {
			t.Fatalf("queryTopBlobUserGroupsAll: %v", err)
		}
		if len(groups) != 2 || groups[0].Key != "scroll" || groups[1].Key != "0xbbb" {
			t.Fatalf("expected all-history order [scroll 0xbbb], got %+v", groups)
		}
		scroll, unattributed := groups[0], groups[1]
		if scroll.BlobCount != 4 || scroll.TotalCostWei != "400" || len(scroll.Addresses) != 2 {
			t.Fatalf("unexpected all-history entity aggregates: %+v", scroll)
		}
		// The all-history denominator folds in the pending set (8 blobs /
		// 1500 wei), matching the per-address all-history queries.
		if scroll.BlobSharePercent != 50 || scroll.SpendSharePercent != 26.666667 {
			t.Fatalf("unexpected all-history entity shares: %+v", scroll)
		}
		if unattributed.BlobCount != 3 || unattributed.BlobSharePercent != 37.5 || unattributed.SpendSharePercent != 40 {
			t.Fatalf("unexpected all-history unattributed row: %+v", unattributed)
		}

		groups = nil
		if err := sqlxDB.SelectContext(ctx, &groups, queryTopBlobUserGroupsAll, 1, 10, 0, "all", "spend"); err != nil {
			t.Fatalf("queryTopBlobUserGroupsAll spend: %v", err)
		}
		if len(groups) != 2 || groups[0].Key != "0xbbb" {
			t.Fatalf("expected all-history spend leader 0xbbb, got %+v", groups)
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
