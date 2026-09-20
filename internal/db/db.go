package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file" // file source driver for migrations
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"

	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
)

// DB is a wrapper around sqlx.DB.
// ExecContext, SelectContext, and GetContext are promoted from the embedded *sqlx.DB.
type DB struct {
	*sqlx.DB
}

// sessionSettings is applied to every connection the pool opens.
//
// JIT is off because nothing this service runs is a query JIT pays for.
// Postgres compiles a query's expressions once its estimated cost passes
// jit_above_cost (100000 by default), and the builder and chart aggregates
// over a day or more of blobs clear that easily — but the compile is paid
// again on every execution, and on the production database it measured
// 1.8-2.7 seconds of a 5-second aggregate budget (87 functions, inlined and
// optimized) to save a few hundred milliseconds of expression evaluation.
// The API's timeouts make any query long enough to amortize that a failed
// request already; the indexer's writes and backfill reads never reach the
// threshold. Set per session rather than in the server configuration so
// every deployment gets it, not only the ones whose Postgres is tuned.
var sessionSettings = []string{"SET jit = off"}

// sessionConnector wraps the driver's connector and applies sessionSettings
// to each new connection before the pool hands it out, so the settings hold
// for the connection's whole life without touching every query site.
type sessionConnector struct {
	driver.Connector
	settings []string
}

func (c sessionConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	execer, ok := conn.(driver.ExecerContext)
	if !ok {
		_ = conn.Close()
		return nil, errors.New("database driver does not support ExecContext; cannot apply session settings")
	}
	for _, stmt := range c.settings {
		if _, err := execer.ExecContext(ctx, stmt, nil); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("failed to apply session setting %q: %w", stmt, err)
		}
	}
	return conn, nil
}

// Connect establishes a connection to the database with pool configuration
func Connect(ctx context.Context, dbCfg config.DatabaseConfig) (*DB, error) {
	connector, err := pq.NewConnector(dbCfg.URL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse database URL: %w", err)
	}
	db := sqlx.NewDb(sql.OpenDB(sessionConnector{Connector: connector, settings: sessionSettings}), "postgres")

	// Configure connection pool from config (defaults match previous hardcoded values)
	db.SetMaxOpenConns(dbCfg.MaxOpenConns)
	db.SetMaxIdleConns(dbCfg.MaxIdleConns)
	db.SetConnMaxLifetime(dbCfg.ConnMaxLifetime)
	db.SetConnMaxIdleTime(dbCfg.ConnMaxIdleTime)

	// Test the connection
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	return &DB{DB: db}, nil
}

// RunMigrations runs database migrations
func RunMigrations(dbURL string) error {
	db, err := sqlx.Connect("postgres", dbURL)
	if err != nil {
		return fmt.Errorf("failed to connect to database for migrations: %w", err)
	}
	defer db.Close()

	migrationsPath, err := migrationsDir()
	if err != nil {
		return err
	}

	// Create a new migrate instance
	migrateDriver, err := postgres.WithInstance(db.DB, &postgres.Config{})
	if err != nil {
		return fmt.Errorf("failed to create migration driver: %w", err)
	}

	m, err := migrate.NewWithDatabaseInstance(
		fmt.Sprintf("file://%s", migrationsPath),
		"postgres",
		migrateDriver,
	)
	if err != nil {
		return fmt.Errorf("failed to create migration instance: %w", err)
	}

	// Run migrations. A dirty schema (a previous run died mid-migration, e.g.
	// the migration Job was deleted by an Argo CD sync retry) is recovered
	// automatically when verifiably safe; see recoverDirtySchema.
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		var dirty migrate.ErrDirty
		if !errors.As(err, &dirty) {
			return fmt.Errorf("failed to run migrations: %w", err)
		}
		if recErr := recoverDirtySchema(m, db, migrationsPath, dirty.Version); recErr != nil {
			return fmt.Errorf("failed to run migrations: %w (automatic dirty-schema recovery refused: %w)", err, recErr)
		}
		if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return fmt.Errorf("failed to run migrations after dirty-schema recovery: %w", err)
		}
	}

	log.Println("Database migrations completed successfully")
	return nil
}

// UpsertNetworks syncs configured networks into the networks table. The rest of
// the schema references networks by chain_id (the canonical network key), so
// this must run before indexed rows are written when foreign keys are enabled.
func (db *DB) UpsertNetworks(ctx context.Context, networks []config.NetworkConfig) error {
	query := `
		INSERT INTO networks (chain_id, name, start_block, is_enabled, updated_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (chain_id) DO UPDATE SET
			name = EXCLUDED.name,
			start_block = EXCLUDED.start_block,
			is_enabled = EXCLUDED.is_enabled,
			updated_at = NOW()
	`
	for _, network := range networks {
		if _, err := db.ExecContext(ctx, query, network.ChainID, network.Name, network.StartBlock, network.Enabled); err != nil {
			return fmt.Errorf("failed to upsert network %s (%d): %w", network.Name, network.ChainID, err)
		}
	}
	return nil
}

// GetMetadata retrieves a metadata value by key
func (db *DB) GetMetadata(ctx context.Context, key string) (string, error) {
	var value string
	query := "SELECT value FROM indexer_metadata WHERE key = $1"
	err := db.GetContext(ctx, &value, query, key)
	if err != nil {
		return "", fmt.Errorf("failed to get metadata for key %s: %w", key, err)
	}
	return value, nil
}

// GetNetworkMetadata retrieves a metadata value by key and network ID
func (db *DB) GetNetworkMetadata(ctx context.Context, networkID int, key string) (string, error) {
	var value string
	query := "SELECT value FROM indexer_metadata WHERE chain_id = $1 AND key = $2"
	err := db.GetContext(ctx, &value, query, networkID, key)
	if err != nil {
		return "", fmt.Errorf("failed to get metadata for key %s and network %d: %w", key, networkID, err)
	}
	return value, nil
}

// SetMetadata sets a metadata value
func (db *DB) SetMetadata(ctx context.Context, key, value string) error {
	query := `
		INSERT INTO indexer_metadata (key, value)
		VALUES ($1, $2)
		ON CONFLICT (key) WHERE chain_id IS NULL
		DO UPDATE SET value = EXCLUDED.value
	`
	_, err := db.ExecContext(ctx, query, key, value)
	if err != nil {
		return fmt.Errorf("failed to set metadata for key %s: %w", key, err)
	}
	return nil
}

// SetNetworkMetadata sets a metadata value for a specific network
func (db *DB) SetNetworkMetadata(ctx context.Context, networkID int, key, value string) error {
	query := `
		INSERT INTO indexer_metadata (chain_id, key, value)
		VALUES ($1, $2, $3)
		ON CONFLICT (chain_id, key) DO UPDATE SET value = $3
	`
	_, err := db.ExecContext(ctx, query, networkID, key, value)
	if err != nil {
		return fmt.Errorf("failed to set metadata for key %s and network %d: %w", key, networkID, err)
	}
	return nil
}

// MetadataKV is one key/value entry for SetNetworkMetadataBatch.
type MetadataKV struct {
	Key   string
	Value string
}

// DeleteNetworkMetadata removes one metadata key for a network and reports
// whether a row existed. A key that is already absent is not an error, so
// callers can retire a key on every start without checking first.
func (db *DB) DeleteNetworkMetadata(ctx context.Context, networkID int, key string) (bool, error) {
	result, err := db.ExecContext(ctx,
		"DELETE FROM indexer_metadata WHERE chain_id = $1 AND key = $2", networkID, key)
	if err != nil {
		return false, fmt.Errorf("failed to delete metadata for key %s and network %d: %w", key, networkID, err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to read metadata delete count for key %s and network %d: %w", key, networkID, err)
	}
	return deleted > 0, nil
}

// SetNetworkMetadataBatch upserts multiple metadata values for a network in a
// single statement, avoiding one round-trip per key. Keys must be distinct
// within a call: a duplicate key would make ON CONFLICT DO UPDATE affect the
// same row twice, which Postgres rejects.
func (db *DB) SetNetworkMetadataBatch(ctx context.Context, networkID int, entries []MetadataKV) error {
	if len(entries) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(entries))
	var values strings.Builder
	args := make([]interface{}, 0, 1+len(entries)*2)
	args = append(args, networkID)
	for i, entry := range entries {
		if _, dup := seen[entry.Key]; dup {
			return fmt.Errorf("duplicate metadata key %q in batch for network %d", entry.Key, networkID)
		}
		seen[entry.Key] = struct{}{}
		if i > 0 {
			values.WriteString(", ")
		}
		fmt.Fprintf(&values, "($1, $%d, $%d)", len(args)+1, len(args)+2)
		args = append(args, entry.Key, entry.Value)
	}

	query := `
		INSERT INTO indexer_metadata (chain_id, key, value)
		VALUES ` + values.String() + `
		ON CONFLICT (chain_id, key) DO UPDATE SET value = EXCLUDED.value
	`
	if _, err := db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("failed to set metadata batch (%d keys) for network %d: %w", len(entries), networkID, err)
	}
	return nil
}

// GetIndexedBlockHash returns the stored block hash for a given block number.
// Returns sql.ErrNoRows if the block hasn't been indexed.
func (db *DB) GetIndexedBlockHash(ctx context.Context, networkID int, blockNumber uint64) (string, error) {
	var hash string
	query := "SELECT block_hash FROM indexed_blocks WHERE chain_id = $1 AND block_number = $2"
	err := db.GetContext(ctx, &hash, query, networkID, blockNumber)
	if err != nil {
		return "", fmt.Errorf("failed to get indexed block hash for network %d block %d: %w", networkID, blockNumber, err)
	}
	return hash, nil
}

// GetFirstUnindexedBlock returns the first missing indexed_blocks row in the
// inclusive range. If the range is fully indexed, it returns targetBlock + 1.
func (db *DB) GetFirstUnindexedBlock(ctx context.Context, networkID int, startBlock, targetBlock uint64) (uint64, error) {
	if startBlock > targetBlock {
		return targetBlock + 1, nil
	}

	var blockNumber uint64
	query := `
		WITH indexed AS (
			SELECT
				block_number,
				LEAD(block_number) OVER (ORDER BY block_number) AS next_block
			FROM indexed_blocks
			WHERE chain_id = $1
				AND block_number >= $2
				AND block_number <= $3
		),
		candidates AS (
			SELECT $2::bigint AS block_number
			WHERE NOT EXISTS (
				SELECT 1 FROM indexed_blocks
				WHERE chain_id = $1 AND block_number = $2
			)
			UNION ALL
			SELECT block_number + 1
			FROM indexed
			WHERE next_block IS NOT NULL
				AND next_block > block_number + 1
			UNION ALL
			SELECT MAX(block_number) + 1
			FROM indexed
			HAVING MAX(block_number) IS NOT NULL
				AND MAX(block_number) < $3
		)
		SELECT COALESCE(MIN(block_number), $3::bigint + 1) FROM candidates
	`
	if err := db.GetContext(ctx, &blockNumber, query, networkID, startBlock, targetBlock); err != nil {
		return 0, fmt.Errorf("failed to get first unindexed block for network %d range %d-%d: %w", networkID, startBlock, targetBlock, err)
	}

	return blockNumber, nil
}

// GetUnindexedBlocksInRange returns the block numbers in the inclusive range
// that have no indexed_blocks row, capped at limit. When
// floorAtEarliestIndexed is true, blocks below the network's earliest indexed
// row are never reported and a network with no indexed rows yields nothing —
// for callers that cannot resolve where coverage was meant to begin (LATEST
// start blocks), so a never-indexed prefix is not misread as a gap. Callers
// that know the intended start pass false and clamp startBlock to it, since
// a missing prefix above that start is a real gap (a crash can persist a
// watermark before any lower block commits).
func (db *DB) GetUnindexedBlocksInRange(ctx context.Context, networkID int, startBlock, endBlock uint64, limit int, floorAtEarliestIndexed bool) ([]uint64, error) {
	if startBlock > endBlock || limit <= 0 {
		return nil, nil
	}

	query := `
		SELECT gs.block_number
		FROM generate_series($2::bigint, $3::bigint) AS gs(block_number)
		WHERE NOT EXISTS (
			SELECT 1 FROM indexed_blocks
			WHERE chain_id = $1 AND block_number = gs.block_number
		)
		ORDER BY gs.block_number
		LIMIT $4
	`
	if floorAtEarliestIndexed {
		query = `
			WITH bounds AS (
				SELECT MIN(block_number) AS min_indexed
				FROM indexed_blocks
				WHERE chain_id = $1
			)
			SELECT gs.block_number
			FROM bounds,
				generate_series(GREATEST($2::bigint, bounds.min_indexed), $3::bigint) AS gs(block_number)
			WHERE bounds.min_indexed IS NOT NULL
				AND NOT EXISTS (
					SELECT 1 FROM indexed_blocks
					WHERE chain_id = $1 AND block_number = gs.block_number
				)
			ORDER BY gs.block_number
			LIMIT $4
		`
	}

	var blocks []uint64
	if err := db.SelectContext(ctx, &blocks, query, networkID, startBlock, endBlock, limit); err != nil {
		return nil, fmt.Errorf("failed to get unindexed blocks for network %d range %d-%d: %w", networkID, startBlock, endBlock, err)
	}

	return blocks, nil
}

// DeleteBlobsFromBlock deletes all blobs at or above the given block number for a network.
func (db *DB) DeleteBlobsFromBlock(ctx context.Context, networkID int, fromBlock int64) error {
	query := "DELETE FROM blobs WHERE chain_id = $1 AND block_number >= $2"
	_, err := db.ExecContext(ctx, query, networkID, fromBlock)
	return err
}

// DeleteIndexedBlocksFromBlock deletes indexed block records at or above the given block number.
func (db *DB) DeleteIndexedBlocksFromBlock(ctx context.Context, networkID int, fromBlock uint64) error {
	query := "DELETE FROM indexed_blocks WHERE chain_id = $1 AND block_number >= $2"
	_, err := db.ExecContext(ctx, query, networkID, fromBlock)
	return err
}

// DeleteBlockMetricsFromBlock deletes block metrics at or above the given block number for a network.
func (db *DB) DeleteBlockMetricsFromBlock(ctx context.Context, networkID int, fromBlock int64) error {
	query := "DELETE FROM block_metrics WHERE chain_id = $1 AND block_number >= $2"
	_, err := db.ExecContext(ctx, query, networkID, fromBlock)
	return err
}

// DeleteBlockBuildersFromBlock deletes block builder rows at or above the
// given block number for a network. Builder rows are per-block facts derived
// from the block header, so a reorg invalidates them exactly like the block
// metrics they sit next to.
func (db *DB) DeleteBlockBuildersFromBlock(ctx context.Context, networkID int, fromBlock int64) error {
	query := "DELETE FROM block_builders WHERE chain_id = $1 AND block_number >= $2"
	_, err := db.ExecContext(ctx, query, networkID, fromBlock)
	return err
}

// DeleteBlobInclusionCandidatesFromBlock deletes the per-transaction
// candidate detail at or above the given block number for a network. The
// rows describe a block that no longer exists, and the replacement block
// writes its own snapshot.
func (db *DB) DeleteBlobInclusionCandidatesFromBlock(ctx context.Context, networkID int, fromBlock int64) error {
	query := "DELETE FROM blob_inclusion_candidates WHERE chain_id = $1 AND block_number >= $2"
	_, err := db.ExecContext(ctx, query, networkID, fromBlock)
	return err
}

// DeleteStaleBlobInclusionCandidates removes per-transaction candidate detail
// for blocks older than the given cutoff. The aggregates the rows were
// summarized into live on block_builders and are not touched.
func (db *DB) DeleteStaleBlobInclusionCandidates(ctx context.Context, networkID int, cutoff time.Time) (int64, error) {
	query := "DELETE FROM blob_inclusion_candidates WHERE chain_id = $1 AND block_timestamp < $2"
	res, err := db.ExecContext(ctx, query, networkID, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RepairIncludedBlobInclusionCandidates deletes candidate rows whose
// transaction could no longer be included at the block that recorded it as
// pending — the transaction itself, or the same-sender same-nonce fee bump
// that replaced it (blob_replacements), was confirmed in a lower block — and
// recomputes the affected block_builders aggregates from the rows that
// remain. Such a row is impossible by definition and only exists because
// the recording block's snapshot ran before the lower block committed (see
// indexer/commit_order.go). One replacement hop is enough: a hash evicted
// by a pending fee bump leaves the pool at that moment, not at a block, so
// only the hash a confirming block superseded can be caught in a later
// snapshot.
//
// The aggregates are recomputed only where the block still carries its
// detail: a row past the retention prune has no rows to recompute from and
// keeps its stored values. Rows that remain keep their reason; a
// 'nonce_gap' row whose lower-nonce sibling was just removed stays a
// nonce_gap, which undercounts the builder's eligible skips rather than
// blaming it, and is left alone.
//
// It returns the number of rows removed and the distinct blocks they
// belonged to.
func (db *DB) RepairIncludedBlobInclusionCandidates(ctx context.Context, networkID int) (removed int64, blocks []int64, err error) {
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return 0, nil, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	rows, err := tx.QueryContext(ctx, `
		DELETE FROM blob_inclusion_candidates c
		WHERE c.chain_id = $1
			AND (
				EXISTS (
					SELECT 1 FROM blobs b
					WHERE b.chain_id = c.chain_id
						AND b.tx_hash = c.tx_hash
						AND b.block_number < c.block_number
				)
				OR EXISTS (
					SELECT 1 FROM blob_replacements r
					JOIN blobs b ON b.chain_id = r.chain_id AND b.tx_hash = r.replacement_tx_hash
					WHERE r.chain_id = c.chain_id
						AND r.replaced_tx_hash = c.tx_hash
						AND b.block_number < c.block_number
				)
			)
		RETURNING c.block_number
	`, networkID)
	if err != nil {
		return 0, nil, err
	}
	seen := make(map[int64]struct{})
	for rows.Next() {
		var blockNumber int64
		if err = rows.Scan(&blockNumber); err != nil {
			rows.Close()
			return 0, nil, err
		}
		removed++
		if _, dup := seen[blockNumber]; !dup {
			seen[blockNumber] = struct{}{}
			blocks = append(blocks, blockNumber)
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, nil, err
	}
	rows.Close()

	if removed > 0 {
		_, err = tx.ExecContext(ctx, `
			UPDATE block_builders AS bb SET
				pending_candidate_txs = r.pending,
				eligible_skipped_txs = r.skipped_txs,
				eligible_skipped_blobs = r.skipped_blobs,
				eligible_skipped_max_tip = r.max_tip
			FROM (
				SELECT a.block_number,
					COUNT(c.tx_hash)::int AS pending,
					COUNT(c.tx_hash) FILTER (WHERE c.reason = $3)::int AS skipped_txs,
					COALESCE(SUM(c.blob_count) FILTER (WHERE c.reason = $3), 0)::int AS skipped_blobs,
					MAX(c.max_priority_fee_per_gas) FILTER (WHERE c.reason = $3) AS max_tip
				FROM unnest($2::bigint[]) AS a(block_number)
				LEFT JOIN blob_inclusion_candidates c
					ON c.chain_id = $1 AND c.block_number = a.block_number
				GROUP BY a.block_number
			) AS r
			WHERE bb.chain_id = $1 AND bb.block_number = r.block_number AND bb.candidate_snapshot
		`, networkID, pq.Array(blocks), models.CandidateEligible)
		if err != nil {
			return 0, nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, nil, err
	}
	return removed, blocks, nil
}

// DeleteStalePendingBlobs removes pending blobs whose liveness watermark is
// older than the given cutoff time. last_seen is bumped whenever the node
// still reports the tx as pending, so this reaps txs the node stopped
// reporting, not txs that are merely old; NULL last_seen (rows written by a
// pre-last_seen binary) falls back to the first-seen timestamp, which is the
// pre-watermark behavior.
func (db *DB) DeleteStalePendingBlobs(ctx context.Context, networkID int, cutoff time.Time) (int64, error) {
	query := "DELETE FROM mempool_blobs WHERE chain_id = $1 AND COALESCE(last_seen, timestamp) < $2"
	res, err := db.ExecContext(ctx, query, networkID, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteStaleBlobReplacements removes replacement records older than the given
// cutoff time.
func (db *DB) DeleteStaleBlobReplacements(ctx context.Context, networkID int, cutoff time.Time) (int64, error) {
	query := "DELETE FROM blob_replacements WHERE chain_id = $1 AND replaced_at < $2"
	res, err := db.ExecContext(ctx, query, networkID, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
