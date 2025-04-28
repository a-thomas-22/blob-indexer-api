package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"runtime"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
)

// DB is a wrapper around sqlx.DB
type DB struct {
	*sqlx.DB
}

// ExecContext executes a query without returning any rows
func (db *DB) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	return db.DB.ExecContext(ctx, query, args...)
}

// SelectContext executes a query and scans the results into dest
func (db *DB) SelectContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
	return db.DB.SelectContext(ctx, dest, query, args...)
}

// GetContext executes a query and scans the result into dest
func (db *DB) GetContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
	return db.DB.GetContext(ctx, dest, query, args...)
}

// Connect establishes a connection to the database
func Connect(ctx context.Context, dbURL string) (*DB, error) {
	db, err := sqlx.ConnectContext(ctx, "postgres", dbURL)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

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

	// Get the path to the migrations directory
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		return errors.New("failed to get caller information")
	}
	migrationsPath := filepath.Join(filepath.Dir(filename), "migrations")

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
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("failed to run migrations: %w", err)
	}

	log.Println("Database migrations completed successfully")
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

// SetMetadata sets a metadata value
func (db *DB) SetMetadata(ctx context.Context, key, value string) error {
	query := `
		INSERT INTO indexer_metadata (key, value)
		VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = $2
	`
	_, err := db.ExecContext(ctx, query, key, value)
	if err != nil {
		return fmt.Errorf("failed to set metadata for key %s: %w", key, err)
	}
	return nil
}
