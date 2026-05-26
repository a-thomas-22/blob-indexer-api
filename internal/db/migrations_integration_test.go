//go:build integration

package db

import (
	"database/sql"
	"errors"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
)

// integrationDBURL returns the connection string for the test database, or
// skips the test when it is not provided. CI sets TEST_DB_URL; developers may
// set it locally to point at a throwaway Postgres instance.
func integrationDBURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TEST_DB_URL")
	if url == "" {
		t.Skip("TEST_DB_URL not set; skipping integration test")
	}
	return url
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
func TestBackfillPerBlobRows(t *testing.T) {
	const gasPerBlob = 131072

	db, err := sqlx.Connect("postgres", integrationDBURL(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer db.Close()

	resetSchema(t, db)

	m := migrator(t, db)
	// Migrate up to (but not including) the backfill migration. Versions are
	// the numeric prefix of the migration filename. 000007 = blob attribution
	// claims; 000008 = pending index change; 000009 = backfill.
	if err := m.Migrate(7); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to 7: %v", err)
	}

	// Seed: ensure the network referenced by the foreign key exists.
	if _, err := db.Exec(`
		INSERT INTO networks (chain_id, name, start_block, is_enabled)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (chain_id) DO NOTHING
	`, 1, "mainnet-test", "0", true); err != nil {
		t.Fatalf("seed network: %v", err)
	}

	// Seed three confirmed blocks of legacy data:
	//   block 100 — one 5-blob tx (collapsed to 1 row, blob_gas_used = 5*131072)
	//   block 101 — one 5-blob tx + one 1-blob tx (2 rows total)
	//   block 102 — two multi-blob txs interleaved: txDD at txIndex 1 with 3
	//               blobs, txEE at txIndex 2 with 2 blobs. After backfill the
	//               per-blob order must be [txDD blob0..2, txEE blob0..1] so
	//               consumers ordering by blob_index see DD's blobs before
	//               EE's — matching the runtime indexer. legacy_blob_index
	//               (the tx's position in the block) drives that order.
	type seed struct {
		blockNumber int64
		blobIndex   int
		txHash      string
		blobCount   int // blobs encoded in this collapsed row
	}
	seeds := []seed{
		{100, 0, "0xaa", 5},
		{101, 0, "0xbb", 5},
		{101, 1, "0xcc", 1},
		{102, 1, "0xdd", 3},
		{102, 2, "0xee", 2},
	}
	for _, s := range seeds {
		if _, err := db.Exec(`
			INSERT INTO blobs (
				network_id, block_number, blob_index, tx_hash, from_address, user_attribution,
				blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_eth,
				timestamp, confirmed, indexer_version, max_fee_per_blob_gas, blob_gas_used
			) VALUES (
				1, $1, $2, $3, '0xfrom', '',
				$4, 7, 1, $5,
				NOW(), true, 'pre-fix',
				9, $6
			)
		`,
			s.blockNumber, s.blobIndex, s.txHash,
			gasPerBlob*s.blobCount,   // collapsed blob_size_bytes
			7*gasPerBlob*s.blobCount, // collapsed total_cost_eth (baseFee * total gas)
			gasPerBlob*s.blobCount,   // collapsed blob_gas_used
		); err != nil {
			t.Fatalf("seed blob: %v", err)
		}
	}

	// Seed block_metrics rows with the legacy wrong blob_count (tx count, not blob count).
	if _, err := db.Exec(`
		INSERT INTO block_metrics (
			network_id, block_number, block_timestamp, blob_count,
			blob_gas_used, blob_gas_target, blob_gas_limit,
			excess_blob_gas, blob_base_fee, utilization_ratio,
			blob_params_target, blob_params_max, update_fraction
		) VALUES
			(1, 100, NOW(), 1, $1, 0, 0, 0, 7, 0, 3, 6, 0),
			(1, 101, NOW(), 2, $2, 0, 0, 0, 7, 0, 3, 6, 0),
			(1, 102, NOW(), 2, $3, 0, 0, 0, 7, 0, 3, 6, 0)
	`, gasPerBlob*5, gasPerBlob*6, gasPerBlob*5); err != nil {
		t.Fatalf("seed block_metrics: %v", err)
	}

	// Run the remaining migrations (8 and 9).
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to head: %v", err)
	}

	// Expectations:
	// block 100: 5 blob rows
	// block 101: 6 blob rows (5+1)
	// block 102: 5 blob rows (3+2)
	type blockCheck struct {
		blockNumber int64
		wantRows    int
		wantGas     int64
	}
	for _, c := range []blockCheck{
		{100, 5, 5 * gasPerBlob},
		{101, 6, 6 * gasPerBlob},
		{102, 5, 5 * gasPerBlob},
	} {
		var got int
		if err := db.QueryRow(`SELECT COUNT(*) FROM blobs WHERE network_id = 1 AND block_number = $1`, c.blockNumber).Scan(&got); err != nil {
			t.Fatalf("count blobs for block %d: %v", c.blockNumber, err)
		}
		if got != c.wantRows {
			t.Errorf("block %d: got %d blob rows, want %d", c.blockNumber, got, c.wantRows)
		}

		// Every row in the block must have blob_gas_used == 131072.
		var gasSum sql.NullInt64
		if err := db.QueryRow(`SELECT COALESCE(SUM(blob_gas_used), 0) FROM blobs WHERE network_id = 1 AND block_number = $1`, c.blockNumber).Scan(&gasSum); err != nil {
			t.Fatalf("sum blob_gas_used for block %d: %v", c.blockNumber, err)
		}
		if gasSum.Int64 != c.wantGas {
			t.Errorf("block %d: blob_gas_used sum = %d, want %d", c.blockNumber, gasSum.Int64, c.wantGas)
		}

		var maxGas sql.NullInt64
		if err := db.QueryRow(`SELECT MAX(blob_gas_used) FROM blobs WHERE network_id = 1 AND block_number = $1`, c.blockNumber).Scan(&maxGas); err != nil {
			t.Fatalf("max blob_gas_used for block %d: %v", c.blockNumber, err)
		}
		if maxGas.Int64 != gasPerBlob {
			t.Errorf("block %d: max blob_gas_used = %d, want %d (one blob's worth)", c.blockNumber, maxGas.Int64, gasPerBlob)
		}

		// blob_index values must be contiguous 0..N-1 (not merely unique).
		// Consumers order by (block_number ASC, blob_index ASC) and expect
		// no gaps and no out-of-order ordinals. Compare MIN/MAX/COUNT/
		// COUNT(DISTINCT) so we catch both gaps and accidental duplicates.
		var minIdx, maxIdx, distinct, total int
		if err := db.QueryRow(`
			SELECT MIN(blob_index), MAX(blob_index), COUNT(DISTINCT blob_index), COUNT(*)
			FROM blobs
			WHERE network_id = 1 AND block_number = $1
		`, c.blockNumber).Scan(&minIdx, &maxIdx, &distinct, &total); err != nil {
			t.Fatalf("blob_index shape for block %d: %v", c.blockNumber, err)
		}
		if minIdx != 0 || maxIdx != c.wantRows-1 || distinct != c.wantRows || total != c.wantRows {
			t.Errorf("block %d: blob_index expected contiguous 0..%d (rows=%d), got min=%d max=%d distinct=%d total=%d",
				c.blockNumber, c.wantRows-1, c.wantRows, minIdx, maxIdx, distinct, total)
		}

		// block_metrics.blob_count must match the actual blob count.
		var bmCount int
		if err := db.QueryRow(`SELECT blob_count FROM block_metrics WHERE network_id = 1 AND block_number = $1`, c.blockNumber).Scan(&bmCount); err != nil {
			t.Fatalf("block_metrics for block %d: %v", c.blockNumber, err)
		}
		if bmCount != c.wantRows {
			t.Errorf("block %d: block_metrics.blob_count = %d, want %d", c.blockNumber, bmCount, c.wantRows)
		}
	}

	// total_cost_eth on every blob row should equal base_fee_per_blob_gas * 131072.
	var bad int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM blobs
		WHERE confirmed = true
		  AND total_cost_eth <> base_fee_per_blob_gas * 131072
	`).Scan(&bad); err != nil {
		t.Fatalf("per-blob cost check: %v", err)
	}
	if bad != 0 {
		t.Fatalf("%d blob rows have total_cost_eth != base_fee * 131072", bad)
	}

	// Per-tx blob count should match what we seeded.
	wantByTx := map[string]int{"0xaa": 5, "0xbb": 5, "0xcc": 1, "0xdd": 3, "0xee": 2}
	for txHash, want := range wantByTx {
		var got int
		if err := db.QueryRow(`SELECT COUNT(*) FROM blobs WHERE tx_hash = $1`, txHash).Scan(&got); err != nil {
			t.Fatalf("count by tx %s: %v", txHash, err)
		}
		if got != want {
			t.Errorf("tx %s: %d rows, want %d", txHash, got, want)
		}
	}

	// Per-block tx ordering invariant: in block 102 the legacy tx ordering
	// was [txDD at txIndex 1, txEE at txIndex 2], so the per-blob ordering
	// must be txDD's three blobs (indices 0..2) before txEE's two blobs
	// (indices 3..4). This catches backfills that interleave blobs across
	// txs or place extra blobs after MAX(blob_index) instead of grouping
	// per tx.
	type ordered struct {
		blobIndex int
		txHash    string
	}
	rowsOrd, err := db.Query(`
		SELECT blob_index, tx_hash FROM blobs
		WHERE network_id = 1 AND block_number = 102
		ORDER BY blob_index ASC
	`)
	if err != nil {
		t.Fatalf("ordering query: %v", err)
	}
	var ord []ordered
	for rowsOrd.Next() {
		var o ordered
		if err := rowsOrd.Scan(&o.blobIndex, &o.txHash); err != nil {
			rowsOrd.Close()
			t.Fatalf("scan ordering row: %v", err)
		}
		ord = append(ord, o)
	}
	rowsOrd.Close()
	wantOrd := []ordered{
		{0, "0xdd"}, {1, "0xdd"}, {2, "0xdd"},
		{3, "0xee"}, {4, "0xee"},
	}
	if len(ord) != len(wantOrd) {
		t.Fatalf("block 102 ordering: got %d rows, want %d", len(ord), len(wantOrd))
	}
	for i, w := range wantOrd {
		if ord[i] != w {
			t.Errorf("block 102 row %d: got (idx=%d, tx=%s), want (idx=%d, tx=%s)",
				i, ord[i].blobIndex, ord[i].txHash, w.blobIndex, w.txHash)
		}
	}

	// Sanity: schema_migrations should be at the latest version.
	latest, err := LatestMigrationVersion()
	if err != nil {
		t.Fatalf("LatestMigrationVersion: %v", err)
	}
	var v uint
	if err := db.QueryRow(`SELECT version FROM schema_migrations LIMIT 1`).Scan(&v); err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	if v != latest {
		t.Fatalf("schema_migrations version = %d, want %d", v, latest)
	}
}

// TestMigrationsDownThenUp validates that reversible migrations round-trip.
// Migration 9 is intentionally irreversible (its down is a no-op), so we only
// step back to the most recent reversible boundary.
func TestMigrationsDownThenUp(t *testing.T) {
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
	// Step back to 7 (before the per-blob migrations). 8.down restores the
	// unique pending index; 9.down is a no-op marker.
	if err := m.Migrate(7); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate down to 7: %v", err)
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
	if v < 9 {
		t.Fatalf("expected version >= 9, got %d", v)
	}
}
