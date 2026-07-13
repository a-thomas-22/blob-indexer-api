package db

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file" // file source driver for migrations
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq" // PostgreSQL driver

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
)

// DB is a wrapper around sqlx.DB.
// ExecContext, SelectContext, and GetContext are promoted from the embedded *sqlx.DB.
type DB struct {
	*sqlx.DB
}

// Connect establishes a connection to the database with pool configuration
func Connect(ctx context.Context, dbCfg config.DatabaseConfig) (*DB, error) {
	db, err := sqlx.ConnectContext(ctx, "postgres", dbCfg.URL)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

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
	driver, err := postgres.WithInstance(db.DB, &postgres.Config{})
	if err != nil {
		return fmt.Errorf("failed to create migration driver: %w", err)
	}

	m, err := migrate.NewWithDatabaseInstance(
		fmt.Sprintf("file://%s", migrationsPath),
		"postgres",
		driver,
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
