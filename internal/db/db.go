package db

import (
	"context"
	"errors"
	"fmt"
	"log"
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

	// Run migrations
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("failed to run migrations: %w", err)
	}

	log.Println("Database migrations completed successfully")
	return nil
}

// UpsertNetworks syncs configured networks into the networks table. The rest of
// the schema uses chain_id as network_id, so this must run before indexed rows
// are written when foreign keys are enabled.
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
	query := "SELECT value FROM indexer_metadata WHERE network_id = $1 AND key = $2"
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
		ON CONFLICT (key) WHERE network_id IS NULL
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
		INSERT INTO indexer_metadata (network_id, key, value)
		VALUES ($1, $2, $3)
		ON CONFLICT (network_id, key) DO UPDATE SET value = $3
	`
	_, err := db.ExecContext(ctx, query, networkID, key, value)
	if err != nil {
		return fmt.Errorf("failed to set metadata for key %s and network %d: %w", key, networkID, err)
	}
	return nil
}

// GetIndexedBlockHash returns the stored block hash for a given block number.
// Returns sql.ErrNoRows if the block hasn't been indexed.
func (db *DB) GetIndexedBlockHash(ctx context.Context, networkID int, blockNumber uint64) (string, error) {
	var hash string
	query := "SELECT block_hash FROM indexed_blocks WHERE network_id = $1 AND block_number = $2"
	err := db.GetContext(ctx, &hash, query, networkID, blockNumber)
	if err != nil {
		return "", fmt.Errorf("failed to get indexed block hash for network %d block %d: %w", networkID, blockNumber, err)
	}
	return hash, nil
}

// DeleteBlobsFromBlock deletes all blobs at or above the given block number for a network.
func (db *DB) DeleteBlobsFromBlock(ctx context.Context, networkID int, fromBlock int64) error {
	query := "DELETE FROM blobs WHERE network_id = $1 AND block_number >= $2"
	_, err := db.ExecContext(ctx, query, networkID, fromBlock)
	return err
}

// DeleteIndexedBlocksFromBlock deletes indexed block records at or above the given block number.
func (db *DB) DeleteIndexedBlocksFromBlock(ctx context.Context, networkID int, fromBlock uint64) error {
	query := "DELETE FROM indexed_blocks WHERE network_id = $1 AND block_number >= $2"
	_, err := db.ExecContext(ctx, query, networkID, fromBlock)
	return err
}

// DeleteBlockMetricsFromBlock deletes block metrics at or above the given block number for a network.
func (db *DB) DeleteBlockMetricsFromBlock(ctx context.Context, networkID int, fromBlock int64) error {
	query := "DELETE FROM block_metrics WHERE network_id = $1 AND block_number >= $2"
	_, err := db.ExecContext(ctx, query, networkID, fromBlock)
	return err
}

// DeleteStalePendingBlobs removes pending blobs older than the given cutoff time.
func (db *DB) DeleteStalePendingBlobs(ctx context.Context, networkID int, cutoff time.Time) (int64, error) {
	query := "DELETE FROM blobs WHERE network_id = $1 AND block_number < 0 AND timestamp < $2"
	res, err := db.ExecContext(ctx, query, networkID, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
