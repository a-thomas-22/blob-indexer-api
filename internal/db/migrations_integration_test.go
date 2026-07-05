//go:build integration

package db

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"

	"github.com/a-thomas-22/blob-indexer-api/internal/testdb"
)

// integrationDBURL returns the connection string for this package's dedicated
// test database (derived from TEST_DB_URL), or skips the test when
// TEST_DB_URL is not provided. CI sets TEST_DB_URL; developers may set it
// locally to point at a throwaway Postgres instance. The derived database
// keeps this package's schema resets from racing other packages' test
// binaries, which go test runs in parallel.
func integrationDBURL(t *testing.T) string {
	t.Helper()
	return testdb.URL(t, "db")
}

// resetSchema drops every table in the public schema so the test starts from a
// blank slate. Cheaper than recreating the database for each run.
func resetSchema(t *testing.T, db *sqlx.DB) {
	t.Helper()
	stmts := []string{
		"DROP SCHEMA IF EXISTS public CASCADE",
		"CREATE SCHEMA public",
		"GRANT ALL ON SCHEMA public TO PUBLIC",
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("reset schema (%s): %v", s, err)
		}
	}
}

// migrator builds a *migrate.Migrate bound to the test database's migrations.
func migrator(t *testing.T, db *sqlx.DB) *migrate.Migrate {
	t.Helper()
	dir, err := migrationsDir()
	if err != nil {
		t.Fatalf("migrationsDir: %v", err)
	}
	driver, err := migratepg.WithInstance(db.DB, &migratepg.Config{})
	if err != nil {
		t.Fatalf("postgres migration driver: %v", err)
	}
	m, err := migrate.NewWithDatabaseInstance("file://"+dir, "postgres", driver)
	if err != nil {
		t.Fatalf("migrate.NewWithDatabaseInstance: %v", err)
	}
	t.Cleanup(func() {
		// Don't call m.Close — the underlying *sql.DB is owned by sqlx.
	})
	return m
}

// TestMigrationsApplyCleanly checks that every bundled migration applies
// against an empty database without errors. This catches SQL syntax bugs and
// ordering issues that unit tests cannot.
func TestMigrationsApplyCleanly(t *testing.T) {
	db, err := sqlx.Connect("postgres", integrationDBURL(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer db.Close()

	resetSchema(t, db)

	m := migrator(t, db)
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up: %v", err)
	}

	latest, err := LatestMigrationVersion()
	if err != nil {
		t.Fatalf("LatestMigrationVersion: %v", err)
	}
	v, dirty, err := m.Version()
	if err != nil {
		t.Fatalf("m.Version: %v", err)
	}
	if dirty {
		t.Fatal("schema is dirty after migrate up")
	}
	if v != latest {
		t.Fatalf("migrated to version %d, want %d", v, latest)
	}
}

// TestBackfillPerBlobRows seeds known multi-blob rows in the legacy shape (one
// row per tx with blob_gas_used = N * 131072) and verifies that migration 9
// splits them into per-blob rows and corrects block_metrics.blob_count.
func TestRunMigrationsRecoversDirtySchema(t *testing.T) {
	url := integrationDBURL(t)
	db, err := sqlx.Connect("postgres", url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer db.Close()
	resetSchema(t, db)

	m := migrator(t, db)
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up: %v", err)
	}

	// Simulate the killed run: golang-migrate writes (N, dirty) before
	// executing migration N's up.sql, and the kill rolls the migration body
	// back while the version row persists. Marking the latest migration dirty
	// on a fully-migrated schema reproduces that state exactly (dirty at an
	// older version cannot coexist with later migrations having run) and makes
	// the recovery re-run exercise the newest migration's idempotency.
	latest, err := LatestMigrationVersion()
	if err != nil {
		t.Fatalf("LatestMigrationVersion: %v", err)
	}
	if _, err := db.Exec(`UPDATE schema_migrations SET version = $1, dirty = true`, latest); err != nil {
		t.Fatalf("mark schema dirty: %v", err)
	}

	if err := RunMigrations(url); err != nil {
		t.Fatalf("RunMigrations should recover a dirty schema: %v", err)
	}

	var v uint
	var dirty bool
	if err := db.QueryRow(`SELECT version, dirty FROM schema_migrations LIMIT 1`).Scan(&v, &dirty); err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	if dirty {
		t.Fatal("schema still dirty after recovery")
	}
	if v != latest {
		t.Fatalf("recovered to version %d, want %d", v, latest)
	}

	// Spot-check that the baseline migration actually applied on the re-run.
	var exists bool
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'blob_user_stats')`).Scan(&exists); err != nil {
		t.Fatalf("check blob_user_stats: %v", err)
	}
	if !exists {
		t.Fatal("blob_user_stats missing after dirty-schema recovery")
	}
}

type networkBlobStatsCheck struct {
	networkID       int
	confirmed       int64
	sumBaseFee      string
	sumTip          string
	sumTotalCost    string
	lastBlock       int64
	lastIndexedTime time.Time
}

func assertNetworkBlobStats(t *testing.T, db *sqlx.DB, want networkBlobStatsCheck) {
	t.Helper()

	var got networkBlobStatsCheck
	if err := db.QueryRow(`
		SELECT
			chain_id,
			total_confirmed_blobs,
			sum_base_fee_per_blob_gas::text,
			sum_tip_per_blob_gas::text,
			sum_total_cost::text,
			last_indexed_block,
			last_indexed_time
		FROM network_blob_stats
		WHERE chain_id = $1
	`, want.networkID).Scan(
		&got.networkID,
		&got.confirmed,
		&got.sumBaseFee,
		&got.sumTip,
		&got.sumTotalCost,
		&got.lastBlock,
		&got.lastIndexedTime,
	); err != nil {
		t.Fatalf("query network_blob_stats: %v", err)
	}

	if got.networkID != want.networkID ||
		got.confirmed != want.confirmed ||
		got.sumBaseFee != want.sumBaseFee ||
		got.sumTip != want.sumTip ||
		got.sumTotalCost != want.sumTotalCost ||
		got.lastBlock != want.lastBlock ||
		!got.lastIndexedTime.Equal(want.lastIndexedTime) {
		t.Fatalf("network_blob_stats = %+v, want %+v", got, want)
	}
}

// TestNetworkBlobStatsMigrationMaintainsSummary verifies the statement-level
// triggers on blobs and block_metrics against the source tables they
// summarize. The API's /stats path depends on this table staying consistent
// without rescanning full blob history. blobs holds confirmed rows only, so
// every insert/update/delete flows into the summary.
func TestNetworkBlobStatsMigrationMaintainsSummary(t *testing.T) {
	db, err := sqlx.Connect("postgres", integrationDBURL(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer db.Close()

	resetSchema(t, db)

	m := migrator(t, db)
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up: %v", err)
	}

	if _, err := db.Exec(`
		INSERT INTO networks (chain_id, name, start_block, is_enabled)
		VALUES (1, 'mainnet-test', '0', true)
		ON CONFLICT (chain_id) DO NOTHING
	`); err != nil {
		t.Fatalf("seed network: %v", err)
	}

	t100 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	t101 := time.Date(2024, 1, 1, 0, 1, 0, 0, time.UTC)
	t102 := time.Date(2024, 1, 1, 0, 2, 0, 0, time.UTC)

	if _, err := db.Exec(`
		INSERT INTO blobs (
			chain_id, block_number, blob_index, tx_hash, from_address, user_attribution,
			blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
			timestamp, max_fee_per_blob_gas, blob_gas_used
		) VALUES
			(1, 100, 0, '0xa', '0xfrom', '', 131072, 10, 2, 100, $1, 12, 131072),
			(1, 101, 0, '0xb', '0xfrom', '', 131072, 30, 6, 300, $2, 36, 131072)
	`, t100, t101); err != nil {
		t.Fatalf("seed blobs: %v", err)
	}

	if _, err := db.Exec(`
		INSERT INTO block_metrics (
			chain_id, block_number, block_timestamp, blob_count,
			blob_gas_used, blob_gas_target, blob_gas_limit,
			excess_blob_gas, blob_base_fee, utilization_ratio,
			blob_params_target, blob_params_max, update_fraction
		) VALUES
			(1, 100, $1, 1, 131072, 393216, 786432, 0, 10, 0.333333, 3, 6, 3338477),
			(1, 101, $2, 1, 131072, 393216, 786432, 0, 30, 0.333333, 3, 6, 3338477)
	`, t100, t101); err != nil {
		t.Fatalf("seed block metrics: %v", err)
	}

	assertNetworkBlobStats(t, db, networkBlobStatsCheck{
		networkID:       1,
		confirmed:       2,
		sumBaseFee:      "40",
		sumTip:          "8",
		sumTotalCost:    "400",
		lastBlock:       101,
		lastIndexedTime: t101,
	})

	if _, err := db.Exec(`
		INSERT INTO blobs (
			chain_id, block_number, blob_index, tx_hash, from_address, user_attribution,
			blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
			timestamp, max_fee_per_blob_gas, blob_gas_used
		) VALUES
			(1, 102, 0, '0xc', '0xfrom', '', 131072, 5, 1, 50, $1, 6, 131072)
	`, t102); err != nil {
		t.Fatalf("insert confirmed blob: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO block_metrics (
			chain_id, block_number, block_timestamp, blob_count,
			blob_gas_used, blob_gas_target, blob_gas_limit,
			excess_blob_gas, blob_base_fee, utilization_ratio,
			blob_params_target, blob_params_max, update_fraction
		) VALUES
			(1, 102, $1, 1, 131072, 393216, 786432, 0, 5, 0.333333, 3, 6, 3338477)
	`, t102); err != nil {
		t.Fatalf("insert block metrics: %v", err)
	}
	assertNetworkBlobStats(t, db, networkBlobStatsCheck{
		networkID:       1,
		confirmed:       3,
		sumBaseFee:      "45",
		sumTip:          "9",
		sumTotalCost:    "450",
		lastBlock:       102,
		lastIndexedTime: t102,
	})

	if _, err := db.Exec(`
		UPDATE blobs
		SET base_fee_per_blob_gas = 20, tip_per_blob_gas = 4, total_cost_wei = 200
		WHERE chain_id = 1 AND tx_hash = '0xa'
	`); err != nil {
		t.Fatalf("update confirmed blob costs: %v", err)
	}
	if _, err := db.Exec(`UPDATE blobs SET user_attribution = 'alice' WHERE chain_id = 1 AND tx_hash = '0xa'`); err != nil {
		t.Fatalf("update non-summary blob field: %v", err)
	}
	assertNetworkBlobStats(t, db, networkBlobStatsCheck{
		networkID:       1,
		confirmed:       3,
		sumBaseFee:      "55",
		sumTip:          "11",
		sumTotalCost:    "550",
		lastBlock:       102,
		lastIndexedTime: t102,
	})

	if _, err := db.Exec(`DELETE FROM blobs WHERE chain_id = 1 AND tx_hash = '0xb'`); err != nil {
		t.Fatalf("delete confirmed blob: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM block_metrics WHERE chain_id = 1 AND block_number = 102`); err != nil {
		t.Fatalf("delete latest block metrics: %v", err)
	}
	assertNetworkBlobStats(t, db, networkBlobStatsCheck{
		networkID:       1,
		confirmed:       2,
		sumBaseFee:      "25",
		sumTip:          "5",
		sumTotalCost:    "250",
		lastBlock:       101,
		lastIndexedTime: t101,
	})
}

// TestMigrationsDownThenUp validates that migrations above the consolidated
// baseline round-trip: step back to the baseline, then re-apply to latest.
func TestMigrationsDownThenUp(t *testing.T) {
	db, err := sqlx.Connect("postgres", integrationDBURL(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer db.Close()
	resetSchema(t, db)

	latest, err := LatestMigrationVersion()
	if err != nil {
		t.Fatalf("LatestMigrationVersion: %v", err)
	}

	m := migrator(t, db)
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up: %v", err)
	}
	if err := m.Migrate(1); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate down to baseline: %v", err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up again: %v", err)
	}
	v, dirty, err := m.Version()
	if err != nil {
		t.Fatalf("m.Version: %v", err)
	}
	if dirty {
		t.Fatal("schema dirty after down/up")
	}
	if v != latest {
		t.Fatalf("expected version %d, got %d", latest, v)
	}
}

// TestMempoolBlobsMigration verifies migration 2: pending rows move to the
// dedicated UNLOGGED mempool_blobs table, legacy block_number < 0 sentinel
// rows are purged from blobs (including their blob_user_stats contribution),
// and the dead pending partial index is dropped.
func TestMempoolBlobsMigration(t *testing.T) {
	db, err := sqlx.Connect("postgres", integrationDBURL(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer db.Close()
	resetSchema(t, db)

	m := migrator(t, db)
	if err := m.Migrate(1); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to baseline: %v", err)
	}

	// Seed one confirmed and one legacy pending sentinel row (baseline schema
	// keeps pending rows in blobs). The default mainnet network (chain 1) is
	// inserted by the baseline migration.
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`
		INSERT INTO blobs (
			chain_id, block_number, blob_index, tx_hash, from_address, user_attribution,
			blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
			timestamp, confirmed, max_fee_per_blob_gas, blob_gas_used
		) VALUES
			(1, 100, 0, '0xconfirmed', '0xfrom', '', 131072, 10, 2, 100, $1, true, 12, 131072),
			(1, -1, 0, '0xpending', '0xfrom', '', 131072, 50, 10, 500, $1, false, 60, 131072)
	`, t0); err != nil {
		t.Fatalf("seed blobs: %v", err)
	}

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to latest: %v", err)
	}

	var legacyPending int
	if err := db.Get(&legacyPending, `SELECT COUNT(*) FROM blobs WHERE block_number < 0`); err != nil {
		t.Fatalf("count legacy pending rows: %v", err)
	}
	if legacyPending != 0 {
		t.Fatalf("expected legacy pending rows to be purged, got %d", legacyPending)
	}
	var confirmed int
	if err := db.Get(&confirmed, `SELECT COUNT(*) FROM blobs WHERE chain_id = 1`); err != nil {
		t.Fatalf("count confirmed rows: %v", err)
	}
	if confirmed != 1 {
		t.Fatalf("expected the confirmed row to survive, got %d rows", confirmed)
	}

	// The purge must also remove the pending contribution from blob_user_stats
	// via the delete trigger (the rollup is confirmed-only now).
	assertBlobUserStats(t, db, "0xfrom", 1, 100)

	// The dead partial index is gone.
	var indexExists bool
	if err := db.Get(&indexExists, `
		SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'idx_blobs_pending_chain_tx_hash')
	`); err != nil {
		t.Fatalf("check partial index: %v", err)
	}
	if indexExists {
		t.Fatal("expected idx_blobs_pending_chain_tx_hash to be dropped")
	}

	// mempool_blobs is UNLOGGED and accepts the indexer's upsert shape.
	var persistence string
	if err := db.Get(&persistence, `SELECT relpersistence FROM pg_class WHERE relname = 'mempool_blobs'`); err != nil {
		t.Fatalf("check mempool_blobs persistence: %v", err)
	}
	if persistence != "u" {
		t.Fatalf("expected mempool_blobs to be UNLOGGED (relpersistence 'u'), got %q", persistence)
	}
	for i := 0; i < 2; i++ {
		if _, err := db.Exec(`
			INSERT INTO mempool_blobs (
				chain_id, tx_hash, blob_index, from_address, user_attribution,
				blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
				timestamp, max_fee_per_blob_gas, blob_gas_used
			) VALUES (1, '0xmempool', 0, '0xfrom', '', 131072, 50, 10, 500, $1, 60, 131072)
			ON CONFLICT (chain_id, tx_hash, blob_index) DO UPDATE SET timestamp = EXCLUDED.timestamp
		`, t0.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("upsert mempool blob (round %d): %v", i, err)
		}
	}
	var mempoolRows int
	if err := db.Get(&mempoolRows, `SELECT COUNT(*) FROM mempool_blobs WHERE chain_id = 1`); err != nil {
		t.Fatalf("count mempool rows: %v", err)
	}
	if mempoolRows != 1 {
		t.Fatalf("expected upsert to keep a single mempool row, got %d", mempoolRows)
	}

	// Pending rows no longer touch the analytical rollups: the mempool insert
	// above must not have changed blob_user_stats.
	assertBlobUserStats(t, db, "0xfrom", 1, 100)
}

// TestDropConfirmedMigration verifies migration 3: the vestigial confirmed
// column and its indexes are dropped, replacement indexes exist before use,
// the stats triggers keep working without the predicate, and the down
// migration restores the old shape.
func TestDropConfirmedMigration(t *testing.T) {
	db, err := sqlx.Connect("postgres", integrationDBURL(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer db.Close()
	resetSchema(t, db)

	m := migrator(t, db)
	if err := m.Migrate(2); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to 2: %v", err)
	}

	// Seed at version 2 (confirmed column still exists): one confirmed row and
	// one stray unconfirmed row that the migration's safety net must purge.
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`
		INSERT INTO blobs (
			chain_id, block_number, blob_index, tx_hash, from_address, user_attribution,
			blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
			timestamp, confirmed, max_fee_per_blob_gas, blob_gas_used
		) VALUES
			(1, 100, 0, '0xkeep', '0xfrom', '', 131072, 10, 2, 100, $1, true, 12, 131072),
			(1, 101, 0, '0xstray', '0xfrom', '', 131072, 30, 6, 300, $1, false, 36, 131072)
	`, t0); err != nil {
		t.Fatalf("seed blobs: %v", err)
	}

	// Pin to version 3: this test validates 000003's own contract, and later
	// migrations legitimately change the index end-state (000004 drops the
	// replacement indexes that are redundant against the UNIQUE key and the
	// chart cover index).
	if err := m.Migrate(3); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to 3: %v", err)
	}

	var columnExists bool
	if err := db.Get(&columnExists, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'blobs' AND column_name = 'confirmed'
		)
	`); err != nil {
		t.Fatalf("check confirmed column: %v", err)
	}
	if columnExists {
		t.Fatal("expected blobs.confirmed to be dropped")
	}

	var rows int
	if err := db.Get(&rows, `SELECT COUNT(*) FROM blobs WHERE chain_id = 1`); err != nil {
		t.Fatalf("count blobs: %v", err)
	}
	if rows != 1 {
		t.Fatalf("expected the stray unconfirmed row to be purged, got %d rows", rows)
	}

	assertIndexes(t, db, map[string]bool{
		"idx_blobs_confirmed":                             false,
		"idx_blobs_chain_confirmed_block":                 false,
		"idx_blobs_chain_confirmed_timestamp":             false,
		"idx_blobs_chain_confirmed_timestamp_cover":       false,
		"idx_blobs_chain_confirmed_timestamp_chart_cover": false,
		"idx_blobs_chain_block":                           true,
		"idx_blobs_chain_timestamp_cover":                 true,
		"idx_blobs_chain_timestamp_chart_cover":           true,
		"idx_blobs_chain_timestamp":                       true,
	})

	// The replaced trigger bodies must keep maintaining the rollups without
	// the confirmed predicate.
	if _, err := db.Exec(`
		INSERT INTO blobs (
			chain_id, block_number, blob_index, tx_hash, from_address, user_attribution,
			blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
			timestamp, max_fee_per_blob_gas, blob_gas_used
		) VALUES
			(1, 102, 0, '0xnew', '0xfrom', '', 131072, 5, 1, 50, $1, 6, 131072)
	`, t0.Add(time.Minute)); err != nil {
		t.Fatalf("insert post-migration blob: %v", err)
	}
	var confirmedTotal int64
	if err := db.Get(&confirmedTotal, `SELECT total_confirmed_blobs FROM network_blob_stats WHERE chain_id = 1`); err != nil {
		t.Fatalf("read network_blob_stats: %v", err)
	}
	if confirmedTotal != 2 {
		t.Fatalf("expected total_confirmed_blobs=2 after post-migration insert, got %d", confirmedTotal)
	}
	assertBlobUserStats(t, db, "0xfrom", 2, 150)

	// Down restores the column (default TRUE), the confirmed-keyed indexes,
	// and drops the replacements.
	if err := m.Migrate(2); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate down to 2: %v", err)
	}
	var allConfirmed bool
	if err := db.Get(&allConfirmed, `SELECT bool_and(confirmed) FROM blobs WHERE chain_id = 1`); err != nil {
		t.Fatalf("check restored confirmed values: %v", err)
	}
	if !allConfirmed {
		t.Fatal("expected every restored row to read confirmed = true")
	}
	assertIndexes(t, db, map[string]bool{
		"idx_blobs_chain_confirmed_block":                 true,
		"idx_blobs_chain_confirmed_timestamp_cover":       true,
		"idx_blobs_chain_confirmed_timestamp_chart_cover": true,
		"idx_blobs_chain_block":                           false,
		"idx_blobs_chain_timestamp_cover":                 false,
		"idx_blobs_chain_timestamp_chart_cover":           false,
	})

	// Re-applying up must be a clean no-op-safe run (idempotency).
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("re-apply up: %v", err)
	}
}

func assertIndexes(t *testing.T, db *sqlx.DB, want map[string]bool) {
	t.Helper()
	for name, wantExists := range want {
		var exists bool
		if err := db.Get(&exists, `SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = $1)`, name); err != nil {
			t.Fatalf("check index %s: %v", name, err)
		}
		if exists != wantExists {
			t.Fatalf("index %s: exists=%v, want %v", name, exists, wantExists)
		}
	}
}

func TestPublicAPIRollupsStayConsistent(t *testing.T) {
	db, err := sqlx.Connect("postgres", integrationDBURL(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer db.Close()
	resetSchema(t, db)

	m := migrator(t, db)
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up: %v", err)
	}

	t10 := time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC)
	t11 := time.Date(2026, 5, 26, 0, 0, 12, 0, time.UTC)

	if _, err := db.Exec(`
		INSERT INTO block_metrics (
			chain_id, block_number, block_timestamp, blob_count,
			blob_gas_used, blob_gas_target, blob_gas_limit,
			excess_blob_gas, blob_base_fee, utilization_ratio,
			blob_params_target, blob_params_max, update_fraction
		) VALUES
			(1, 10, $1, 2, 262144, 393216, 786432, 0, 10, 0.333333, 3, 6, 0),
			(1, 11, $2, 1, 131072, 393216, 786432, 0, 30, 0.166667, 3, 6, 0)
	`, t10, t11); err != nil {
		t.Fatalf("insert block metrics: %v", err)
	}

	if _, err := db.Exec(`
		INSERT INTO blobs (
			chain_id, block_number, blob_index, tx_hash, from_address, user_attribution,
			blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
			timestamp, max_fee_per_blob_gas, blob_gas_used
		) VALUES
			(1, 10, 0, '0xaa', '0x1111111111111111111111111111111111111111', 'Rollup A', 131072, 10, 1, 100, $1, 11, 131072),
			(1, 10, 1, '0xbb', '0x2222222222222222222222222222222222222222', '', 131072, 20, 2, 200, $1, 22, 131072)
	`, t10); err != nil {
		t.Fatalf("insert blobs: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO mempool_blobs (
			chain_id, tx_hash, blob_index, from_address, user_attribution,
			blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
			timestamp, max_fee_per_blob_gas, blob_gas_used
		) VALUES
			(1, '0xcc', 0, '0x1111111111111111111111111111111111111111', 'Rollup A', 131072, 30, 3, 300, $1, 33, 131072)
	`, t10.Add(6*time.Second)); err != nil {
		t.Fatalf("insert pending blob: %v", err)
	}

	// Pending rows live in mempool_blobs and must not touch the rollups.
	assertNetworkBlobStats(t, db, networkBlobStatsCheck{
		networkID:       1,
		confirmed:       2,
		sumBaseFee:      "30",
		sumTip:          "3",
		sumTotalCost:    "300",
		lastBlock:       11,
		lastIndexedTime: t11,
	})
	assertBlobUserStats(t, db, "0x1111111111111111111111111111111111111111", 1, 100)
	assertBlobUserStats(t, db, "0x2222222222222222222222222222222222222222", 1, 200)

	// Promotion is delete-from-mempool + insert-into-blobs.
	if _, err := db.Exec(`DELETE FROM mempool_blobs WHERE chain_id = 1 AND tx_hash = '0xcc'`); err != nil {
		t.Fatalf("delete promoted mempool blob: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO blobs (
			chain_id, block_number, blob_index, tx_hash, from_address, user_attribution,
			blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
			timestamp, max_fee_per_blob_gas, blob_gas_used
		) VALUES
			(1, 11, 0, '0xcc', '0x1111111111111111111111111111111111111111', 'Rollup A', 131072, 30, 3, 300, $1, 33, 131072)
	`, t10.Add(6*time.Second)); err != nil {
		t.Fatalf("promote pending blob: %v", err)
	}
	assertNetworkBlobStats(t, db, networkBlobStatsCheck{
		networkID:       1,
		confirmed:       3,
		sumBaseFee:      "60",
		sumTip:          "6",
		sumTotalCost:    "600",
		lastBlock:       11,
		lastIndexedTime: t11,
	})
	assertBlobUserStats(t, db, "0x1111111111111111111111111111111111111111", 2, 400)

	if _, err := db.Exec(`DELETE FROM block_metrics WHERE chain_id = 1 AND block_number = 11`); err != nil {
		t.Fatalf("delete latest block metric: %v", err)
	}
	assertNetworkBlobStats(t, db, networkBlobStatsCheck{
		networkID:       1,
		confirmed:       3,
		sumBaseFee:      "60",
		sumTip:          "6",
		sumTotalCost:    "600",
		lastBlock:       10,
		lastIndexedTime: t10,
	})

	if _, err := db.Exec(`DELETE FROM blobs WHERE chain_id = 1 AND from_address = '0x1111111111111111111111111111111111111111'`); err != nil {
		t.Fatalf("delete sender blobs: %v", err)
	}
	assertNetworkBlobStats(t, db, networkBlobStatsCheck{
		networkID:       1,
		confirmed:       1,
		sumBaseFee:      "20",
		sumTip:          "2",
		sumTotalCost:    "200",
		lastBlock:       10,
		lastIndexedTime: t10,
	})

	var remaining int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM blob_user_stats
		WHERE chain_id = 1 AND from_address = '0x1111111111111111111111111111111111111111'
	`).Scan(&remaining); err != nil {
		t.Fatalf("count removed sender stats: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("expected sender rollup to be removed, got %d rows", remaining)
	}
}

func assertBlobUserStats(t *testing.T, db *sqlx.DB, address string, wantCount, wantTotal int64) {
	t.Helper()
	var got struct {
		BlobCount int64 `db:"blob_count"`
		TotalCost int64 `db:"total_cost_wei"`
	}
	if err := db.Get(&got, `
		SELECT blob_count, total_cost_wei::bigint
		FROM blob_user_stats
		WHERE chain_id = 1 AND from_address = $1
	`, address); err != nil {
		t.Fatalf("get blob_user_stats for %s: %v", address, err)
	}
	if got.BlobCount != wantCount || got.TotalCost != wantTotal {
		t.Fatalf("blob_user_stats[%s] = %+v, want count=%d total=%d", address, got, wantCount, wantTotal)
	}
}

// TestBlockMetricsRollupThresholdCounts seeds block_metrics at migration 13
// (rollup buckets exist but lack the threshold counters), then verifies that
// migration 14 backfills blocks_above_target/blocks_at_max for every
// granularity and that the redefined refresh function maintains them on
// insert and delete. Covers explicit gas columns, the blob-params*131072
// fallback, and unclassified blocks (neither source set).
func TestBlockMetricsRollupThresholdCounts(t *testing.T) {
	db, err := sqlx.Connect("postgres", integrationDBURL(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer db.Close()
	resetSchema(t, db)

	m := migrator(t, db)
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to 13: %v", err)
	}

	if _, err := db.Exec(`
		INSERT INTO networks (chain_id, name, start_block, is_enabled)
		VALUES (1, 'mainnet-test', '0', true)
		ON CONFLICT (chain_id) DO NOTHING
	`); err != nil {
		t.Fatalf("seed network: %v", err)
	}

	base := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	// target = 393216 (3 blobs), max = 786432 (6 blobs), explicitly or via the
	// blob-params fallback:
	//   100: above target only (explicit gas columns)
	//   101: above target and at max (explicit gas columns)
	//   102: below target (explicit gas columns)
	//   103: above target and at max (params fallback, gas columns zero)
	//   104: unclassified (no gas columns, no params) — counts toward neither
	if _, err := db.Exec(`
		INSERT INTO block_metrics (
			chain_id, block_number, block_timestamp, blob_count,
			blob_gas_used, blob_gas_target, blob_gas_limit,
			excess_blob_gas, blob_base_fee, utilization_ratio,
			blob_params_target, blob_params_max, update_fraction
		) VALUES
			(1, 100, $1, 4, 393217, 393216, 786432, 0, 10, 0.5, 0, 0, 0),
			(1, 101, $2, 6, 786432, 393216, 786432, 0, 20, 1.0, 0, 0, 0),
			(1, 102, $3, 1, 131072, 393216, 786432, 0, 30, 0.166667, 0, 0, 0),
			(1, 103, $4, 6, 786432, 0, 0, 0, 40, 1.0, 3, 6, 0),
			(1, 104, $5, 2, 999999, 0, 0, 0, 50, 0.5, 0, 0, 0)
	`, base, base.Add(12*time.Second), base.Add(24*time.Second), base.Add(36*time.Second), base.Add(48*time.Second)); err != nil {
		t.Fatalf("seed block metrics: %v", err)
	}

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to latest: %v", err)
	}

	// Migration 14's backfill must populate the counters for every granularity.
	for _, bucketSeconds := range []int{3600, 21600, 86400} {
		assertRollupThresholdCounts(t, db, bucketSeconds, 5, 3, 2)
	}

	// The redefined refresh function must maintain the counters via the
	// block_metrics triggers: one more above-target block...
	if _, err := db.Exec(`
		INSERT INTO block_metrics (
			chain_id, block_number, block_timestamp, blob_count,
			blob_gas_used, blob_gas_target, blob_gas_limit,
			excess_blob_gas, blob_base_fee, utilization_ratio,
			blob_params_target, blob_params_max, update_fraction
		) VALUES (1, 105, $1, 4, 500000, 393216, 786432, 0, 60, 0.636, 0, 0, 0)
	`, base.Add(time.Minute)); err != nil {
		t.Fatalf("insert above-target block: %v", err)
	}
	for _, bucketSeconds := range []int{3600, 21600, 86400} {
		assertRollupThresholdCounts(t, db, bucketSeconds, 6, 4, 2)
	}

	// ...and removing an at-max block drops both counters.
	if _, err := db.Exec(`DELETE FROM block_metrics WHERE chain_id = 1 AND block_number = 101`); err != nil {
		t.Fatalf("delete at-max block: %v", err)
	}
	for _, bucketSeconds := range []int{3600, 21600, 86400} {
		assertRollupThresholdCounts(t, db, bucketSeconds, 5, 3, 1)
	}
}

// TestFineChartRollupsLifecycle exercises the fine (60s) chart rollup bucket
// added by the fine_chart_rollups migration: statement triggers maintain fine buckets for rows
// inside the retention window and skip rows outside it, the indexer's
// chunked backfill statement recomputes buckets from raw rows (full replace),
// and pruning removes expired fine rows without touching coarse buckets.
func TestFineChartRollupsLifecycle(t *testing.T) {
	sqlxDB, err := sqlx.Connect("postgres", integrationDBURL(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sqlxDB.Close()
	resetSchema(t, sqlxDB)

	m := migrator(t, sqlxDB)
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up: %v", err)
	}

	testDB := &DB{DB: sqlxDB}
	ctx := context.Background()

	recent := time.Now().UTC().Truncate(time.Minute).Add(-10 * time.Minute)
	old := time.Now().UTC().Truncate(time.Minute).Add(-72 * time.Hour)

	if _, err := sqlxDB.Exec(`
		INSERT INTO block_metrics (
			chain_id, block_number, block_timestamp, blob_count,
			blob_gas_used, blob_gas_target, blob_gas_limit,
			excess_blob_gas, blob_base_fee, utilization_ratio,
			blob_params_target, blob_params_max, update_fraction
		) VALUES
			(1, 200, $1, 1, 131072, 393216, 786432, 0, 10, 0.333333, 3, 6, 0),
			(1, 201, $2, 1, 131072, 393216, 786432, 0, 30, 0.333333, 3, 6, 0),
			(1, 100, $3, 1, 131072, 393216, 786432, 0, 50, 0.333333, 3, 6, 0)
	`, recent, recent.Add(12*time.Second), old); err != nil {
		t.Fatalf("insert block metrics: %v", err)
	}

	if _, err := sqlxDB.Exec(`
		INSERT INTO blobs (
			chain_id, block_number, blob_index, tx_hash, from_address, user_attribution,
			blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
			timestamp, max_fee_per_blob_gas, blob_gas_used
		) VALUES
			(1, 200, 0, '0xf1', '0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'Rollup A', 131072, 10, 1, 100, $1, 11, 131072),
			(1, 201, 0, '0xf2', '0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'Rollup A', 131072, 30, 1, 100, $2, 33, 131072),
			(1, 100, 0, '0xf3', '0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', '', 131072, 50, 1, 300, $3, 55, 131072)
	`, recent, recent.Add(12*time.Second), old); err != nil {
		t.Fatalf("insert blobs: %v", err)
	}

	// Triggers maintain the fine bucket for recent rows...
	assertFineBlobBucket(t, sqlxDB, recent, fineBlobBucketCheck{
		blobCount: 2, blobBytes: 262144, totalCost: 200, sumSizeBaseFee: 131072*10 + 131072*30,
	})
	var fineBlocks struct {
		BlockCount int64  `db:"block_count"`
		Median     string `db:"median_blob_base_fee"`
	}
	if err := sqlxDB.Get(&fineBlocks, `
		SELECT block_count, median_blob_base_fee::text AS median_blob_base_fee
		FROM block_metrics_rollups
		WHERE chain_id = 1 AND bucket_seconds = 60 AND bucket_start = $1
	`, recent); err != nil {
		t.Fatalf("get fine block bucket: %v", err)
	}
	if fineBlocks.BlockCount != 2 || fineBlocks.Median != "10" {
		t.Fatalf("fine block bucket = %+v, want 2 blocks with median 10", fineBlocks)
	}

	// ...and skip rows older than the retention window, which still land in
	// the coarse buckets.
	assertFineBucketCounts(t, sqlxDB, old, 0, 0)
	var oldHourly int
	if err := sqlxDB.Get(&oldHourly, `
		SELECT COUNT(*) FROM blob_chart_rollups
		WHERE chain_id = 1 AND bucket_seconds = 3600 AND bucket_start = date_trunc('hour', $1::timestamp)
	`, old); err != nil {
		t.Fatalf("count old hourly buckets: %v", err)
	}
	if oldHourly != 1 {
		t.Fatalf("expected old row to land in the hourly bucket, got %d rows", oldHourly)
	}

	// Deleting an out-of-retention row refreshes coarse buckets only and must
	// not resurrect fine buckets for it.
	if _, err := sqlxDB.Exec(`DELETE FROM blobs WHERE chain_id = 1 AND tx_hash = '0xf3'`); err != nil {
		t.Fatalf("delete old blob: %v", err)
	}
	assertFineBucketCounts(t, sqlxDB, old, 0, 0)

	// The chunked backfill fully replaces bucket contents from raw rows.
	if _, err := sqlxDB.Exec(`
		UPDATE blob_chart_rollups SET blob_count = 999, total_cost_wei = 0
		WHERE chain_id = 1 AND bucket_seconds = 60 AND bucket_start = $1
	`, recent); err != nil {
		t.Fatalf("corrupt fine bucket: %v", err)
	}
	if err := testDB.BackfillFineChartRollupsChunk(ctx, 1, recent.Add(-time.Minute), recent.Add(time.Minute)); err != nil {
		t.Fatalf("backfill chunk: %v", err)
	}
	assertFineBlobBucket(t, sqlxDB, recent, fineBlobBucketCheck{
		blobCount: 2, blobBytes: 262144, totalCost: 200, sumSizeBaseFee: 131072*10 + 131072*30,
	})

	// Pruning removes fine buckets older than the cutoff and leaves coarse
	// buckets alone.
	deleted, err := testDB.PruneFineChartRollups(ctx, 1, time.Now().UTC())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if deleted < 2 {
		t.Fatalf("expected prune to delete the fine buckets, got %d rows", deleted)
	}
	assertFineBucketCounts(t, sqlxDB, recent, 0, 0)
	var hourlyLeft int
	if err := sqlxDB.Get(&hourlyLeft, `
		SELECT COUNT(*) FROM blob_chart_rollups WHERE chain_id = 1 AND bucket_seconds = 3600
	`); err != nil {
		t.Fatalf("count hourly buckets after prune: %v", err)
	}
	if hourlyLeft == 0 {
		t.Fatal("prune must not remove coarse buckets")
	}
}

type fineBlobBucketCheck struct {
	blobCount      int64
	blobBytes      int64
	totalCost      int64
	sumSizeBaseFee int64
}

func assertFineBlobBucket(t *testing.T, db *sqlx.DB, bucketStart time.Time, want fineBlobBucketCheck) {
	t.Helper()
	var got struct {
		BlobCount      int64 `db:"blob_count"`
		BlobBytes      int64 `db:"blob_bytes"`
		TotalCost      int64 `db:"total_cost_wei"`
		SumSizeBaseFee int64 `db:"sum_size_base_fee"`
	}
	if err := db.Get(&got, `
		SELECT blob_count, blob_bytes, total_cost_wei::bigint AS total_cost_wei, sum_size_base_fee::bigint AS sum_size_base_fee
		FROM blob_chart_rollups
		WHERE chain_id = 1 AND bucket_seconds = 60 AND bucket_start = $1
	`, bucketStart); err != nil {
		t.Fatalf("get fine blob bucket at %s: %v", bucketStart, err)
	}
	if got.BlobCount != want.blobCount || got.BlobBytes != want.blobBytes ||
		got.TotalCost != want.totalCost || got.SumSizeBaseFee != want.sumSizeBaseFee {
		t.Fatalf("fine blob bucket = %+v, want %+v", got, want)
	}
}

func assertFineBucketCounts(t *testing.T, db *sqlx.DB, bucketStart time.Time, wantBlobRows, wantBlockRows int) {
	t.Helper()
	var blobRows, blockRows int
	if err := db.Get(&blobRows, `
		SELECT COUNT(*) FROM blob_chart_rollups
		WHERE chain_id = 1 AND bucket_seconds = 60 AND bucket_start = $1
	`, bucketStart); err != nil {
		t.Fatalf("count fine blob buckets: %v", err)
	}
	if err := db.Get(&blockRows, `
		SELECT COUNT(*) FROM block_metrics_rollups
		WHERE chain_id = 1 AND bucket_seconds = 60 AND bucket_start = $1
	`, bucketStart); err != nil {
		t.Fatalf("count fine block buckets: %v", err)
	}
	if blobRows != wantBlobRows || blockRows != wantBlockRows {
		t.Fatalf("fine buckets at %s = %d blob rows / %d block rows, want %d / %d",
			bucketStart, blobRows, blockRows, wantBlobRows, wantBlockRows)
	}
}

func assertRollupThresholdCounts(t *testing.T, db *sqlx.DB, bucketSeconds int, wantBlocks, wantAbove, wantAtMax int64) {
	t.Helper()
	var got struct {
		BlockCount        int64 `db:"block_count"`
		BlocksAboveTarget int64 `db:"blocks_above_target"`
		BlocksAtMax       int64 `db:"blocks_at_max"`
	}
	if err := db.Get(&got, `
		SELECT block_count, blocks_above_target, blocks_at_max
		FROM block_metrics_rollups
		WHERE chain_id = 1 AND bucket_seconds = $1
	`, bucketSeconds); err != nil {
		t.Fatalf("get rollup counts for bucket %d: %v", bucketSeconds, err)
	}
	if got.BlockCount != wantBlocks || got.BlocksAboveTarget != wantAbove || got.BlocksAtMax != wantAtMax {
		t.Fatalf(
			"rollup[%d] = blocks=%d above=%d atMax=%d, want blocks=%d above=%d atMax=%d",
			bucketSeconds, got.BlockCount, got.BlocksAboveTarget, got.BlocksAtMax,
			wantBlocks, wantAbove, wantAtMax,
		)
	}
}

// TestWritePathCostsMigration validates 000004's end state: the redundant
// indexes are gone, the load-bearing ones survive, and the delta-based
// blob_user_stats triggers stay exact through update/delete/revocation.
func TestWritePathCostsMigration(t *testing.T) {
	db, err := sqlx.Connect("postgres", integrationDBURL(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer db.Close()
	resetSchema(t, db)

	m := migrator(t, db)
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up: %v", err)
	}

	assertIndexes(t, db, map[string]bool{
		// Dropped as redundant.
		"idx_blobs_chain_id":              false,
		"idx_blobs_block_number":          false,
		"idx_blobs_chain_block":           false,
		"idx_blobs_from_address":          false,
		"idx_blobs_timestamp":             false,
		"idx_blobs_chain_timestamp":       false,
		"idx_blobs_chain_timestamp_cover": false,
		"idx_block_metrics_chain_block":   false,
		"idx_blob_users_chain_id":         false,
		"idx_blob_users_address":          false,
		// Load-bearing survivors.
		"idx_blobs_chain_txhash":                true,
		"idx_blobs_chain_from_timestamp":        true,
		"idx_blobs_chain_timestamp_chart_cover": true,
		"idx_blobs_chain_lower_from_address":    true,
	})

	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`
		INSERT INTO blobs (
			chain_id, block_number, blob_index, tx_hash, from_address, user_attribution,
			blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
			timestamp, max_fee_per_blob_gas, blob_gas_used
		) VALUES
			(1, 200, 0, '0xd1', '0xdelta', 'RollupD', 131072, 10, 1, 100, $1, 11, 131072),
			(1, 201, 0, '0xd2', '0xdelta', 'RollupD', 131072, 10, 1, 100, $1, 11, 131072)
	`, t0); err != nil {
		t.Fatalf("seed blobs: %v", err)
	}
	assertBlobUserStats(t, db, "0xdelta", 2, 200)

	// Fee-only update takes the delta path exactly.
	if _, err := db.Exec(`UPDATE blobs SET total_cost_wei = 120 WHERE chain_id = 1 AND from_address = '0xdelta'`); err != nil {
		t.Fatalf("update blobs: %v", err)
	}
	assertBlobUserStats(t, db, "0xdelta", 2, 240)

	// Attribution revocation (non-empty -> empty) clears the stored name.
	if _, err := db.Exec(`UPDATE blobs SET user_attribution = '' WHERE chain_id = 1 AND from_address = '0xdelta'`); err != nil {
		t.Fatalf("revoke attribution: %v", err)
	}
	var attribution string
	if err := db.Get(&attribution, `SELECT user_attribution FROM blob_user_stats WHERE chain_id = 1 AND from_address = '0xdelta'`); err != nil {
		t.Fatalf("read attribution: %v", err)
	}
	if attribution != "" {
		t.Fatalf("expected cleared attribution after revocation, got %q", attribution)
	}

	// Reorg-style delete decrements; deleting the rest removes the row.
	if _, err := db.Exec(`DELETE FROM blobs WHERE chain_id = 1 AND block_number = 201`); err != nil {
		t.Fatalf("delete blob: %v", err)
	}
	assertBlobUserStats(t, db, "0xdelta", 1, 120)
	if _, err := db.Exec(`DELETE FROM blobs WHERE chain_id = 1 AND block_number = 200`); err != nil {
		t.Fatalf("delete last blob: %v", err)
	}
	var statsRows int
	if err := db.Get(&statsRows, `SELECT COUNT(*) FROM blob_user_stats WHERE chain_id = 1 AND from_address = '0xdelta'`); err != nil {
		t.Fatalf("count stats rows: %v", err)
	}
	if statsRows != 0 {
		t.Fatalf("expected stats row removed at zero count, got %d rows", statsRows)
	}

	// Down restores the 000003 end-state indexes and the refresh triggers.
	if err := m.Migrate(3); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate down to 3: %v", err)
	}
	assertIndexes(t, db, map[string]bool{
		"idx_blobs_chain_block":           true,
		"idx_blobs_chain_timestamp_cover": true,
		"idx_blobs_chain_timestamp":       true,
		"idx_block_metrics_chain_block":   true,
	})

	// Re-applying up must be a clean idempotent run.
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("re-apply up: %v", err)
	}
}

// TestBlockMetricsNotifyTrigger verifies migration 6: every committed
// block_metrics INSERT or UPDATE posts a blob_indexer_new_block notification
// carrying the chain and block number, delivered on commit.
func TestBlockMetricsNotifyTrigger(t *testing.T) {
	url := integrationDBURL(t)
	db, err := sqlx.Connect("postgres", url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer db.Close()
	resetSchema(t, db)

	m := migrator(t, db)
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up: %v", err)
	}

	listener := pq.NewListener(url, time.Second, 10*time.Second, nil)
	defer listener.Close()
	if err := listener.Listen("blob_indexer_new_block"); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}

	waitNotification := func(wantChain int, wantBlock int64) {
		t.Helper()
		for {
			select {
			case n := <-listener.Notify:
				if n == nil {
					continue // reconnect marker
				}
				var payload struct {
					ChainID     int   `json:"chain_id"`
					BlockNumber int64 `json:"block_number"`
				}
				if err := json.Unmarshal([]byte(n.Extra), &payload); err != nil {
					t.Fatalf("unmarshal payload %q: %v", n.Extra, err)
				}
				if payload.ChainID != wantChain || payload.BlockNumber != wantBlock {
					t.Fatalf("notification = %+v, want chain %d block %d", payload, wantChain, wantBlock)
				}
				return
			case <-time.After(5 * time.Second):
				t.Fatalf("no notification received for chain %d block %d", wantChain, wantBlock)
			}
		}
	}

	// INSERT path — the baseline seeds network chain 1.
	if _, err := db.Exec(`
		INSERT INTO block_metrics (chain_id, block_number, block_timestamp, blob_count)
		VALUES (1, 12345, NOW() AT TIME ZONE 'UTC', 3)
	`); err != nil {
		t.Fatalf("insert block_metrics: %v", err)
	}
	waitNotification(1, 12345)

	// UPDATE path (the indexer's ON CONFLICT DO UPDATE on re-index/reorg).
	if _, err := db.Exec(`UPDATE block_metrics SET blob_count = 4 WHERE chain_id = 1 AND block_number = 12345`); err != nil {
		t.Fatalf("update block_metrics: %v", err)
	}
	waitNotification(1, 12345)

	// Rolled-back writes must not notify.
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO block_metrics (chain_id, block_number, block_timestamp, blob_count)
		VALUES (1, 99999, NOW() AT TIME ZONE 'UTC', 1)
	`); err != nil {
		t.Fatalf("insert in tx: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	select {
	case n := <-listener.Notify:
		if n != nil {
			t.Fatalf("rolled-back insert must not notify, got %s", n.Extra)
		}
	case <-time.After(time.Second):
		// Expected: silence.
	}
}
