package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

const (
	DefaultSchemaWaitTimeout  = 2 * time.Minute
	DefaultSchemaPollInterval = 2 * time.Second

	currentSchemaVersionQuery = "SELECT version, dirty FROM schema_migrations LIMIT 1"
)

var (
	ErrSchemaNotReady = errors.New("database schema is not ready")
	ErrSchemaTooNew   = errors.New("database schema is newer than this binary supports")
)

// SchemaStatus captures the local and database migration versions.
type SchemaStatus struct {
	ExpectedVersion uint
	CurrentVersion  uint
	Dirty           bool
	Ready           bool
	Message         string
}

// LatestMigrationVersion returns the highest bundled local migration version.
func LatestMigrationVersion() (uint, error) {
	migrationsPath, err := migrationsDir()
	if err != nil {
		return 0, err
	}
	return latestMigrationVersionFromDir(migrationsPath)
}

// CheckSchemaVersion verifies that the database migration state matches the
// migrations bundled with this binary.
func (db *DB) CheckSchemaVersion(ctx context.Context) (SchemaStatus, error) {
	expected, err := LatestMigrationVersion()
	if err != nil {
		return SchemaStatus{}, err
	}

	status := SchemaStatus{ExpectedVersion: expected}

	var row struct {
		Version uint `db:"version"`
		Dirty   bool `db:"dirty"`
	}
	if err := db.GetContext(ctx, &row, currentSchemaVersionQuery); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			status.Message = "schema_migrations has no version row"
		} else {
			status.Message = fmt.Sprintf("schema version unavailable: %v", err)
		}
		return status, fmt.Errorf("%w: %s", ErrSchemaNotReady, status.Message)
	}

	status.CurrentVersion = row.Version
	status.Dirty = row.Dirty

	switch {
	case row.Dirty:
		status.Message = "migration is dirty or still in progress"
		return status, fmt.Errorf("%w: %s", ErrSchemaNotReady, status.Message)
	case row.Version < expected:
		status.Message = "database migrations are behind this binary"
		return status, fmt.Errorf("%w: %s", ErrSchemaNotReady, status.Message)
	case row.Version > expected:
		status.Message = "database migrations are ahead of this binary"
		return status, fmt.Errorf("%w: current=%d expected=%d", ErrSchemaTooNew, row.Version, expected)
	default:
		status.Ready = true
		status.Message = "database schema is ready"
		return status, nil
	}
}

// WaitForSchema waits until migrations are applied to the version expected by
// this binary. A newer database version fails immediately because waiting cannot
// make an old binary compatible.
func (db *DB) WaitForSchema(ctx context.Context, timeout, pollInterval time.Duration) error {
	timeout, pollInterval = normalizeSchemaWait(timeout, pollInterval)

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var lastErr error
	for {
		status, err := db.CheckSchemaVersion(waitCtx)
		if err == nil {
			logger.Info("Database schema is ready",
				zap.Uint("version", status.CurrentVersion))
			return nil
		}
		if errors.Is(err, ErrSchemaTooNew) {
			return err
		}
		lastErr = err

		logger.Info("Waiting for database schema migrations",
			zap.Uint("current_version", status.CurrentVersion),
			zap.Uint("expected_version", status.ExpectedVersion),
			zap.Bool("dirty", status.Dirty),
			zap.String("reason", status.Message),
			zap.Duration("retry_delay", pollInterval))

		select {
		case <-waitCtx.Done():
			if lastErr == nil {
				lastErr = waitCtx.Err()
			}
			return fmt.Errorf("timed out waiting for database schema after %s: %w", timeout, lastErr)
		case <-ticker.C:
		}
	}
}

func normalizeSchemaWait(timeout, pollInterval time.Duration) (waitTimeout, waitPollInterval time.Duration) {
	waitTimeout = timeout
	waitPollInterval = pollInterval
	if waitTimeout <= 0 {
		waitTimeout = DefaultSchemaWaitTimeout
	}
	if waitPollInterval <= 0 {
		waitPollInterval = DefaultSchemaPollInterval
	}
	return waitTimeout, waitPollInterval
}

func migrationsDir() (string, error) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("failed to get caller information")
	}
	return filepath.Join(filepath.Dir(filename), "migrations"), nil
}

func latestMigrationVersionFromDir(dir string) (uint, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("failed to read migrations directory: %w", err)
	}

	var latest uint64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".up.sql") {
			continue
		}

		versionText, _, ok := strings.Cut(entry.Name(), "_")
		if !ok {
			return 0, fmt.Errorf("migration file %q does not start with a version prefix", entry.Name())
		}

		version, err := strconv.ParseUint(versionText, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("failed to parse migration version from %q: %w", entry.Name(), err)
		}
		if version > latest {
			latest = version
		}
	}
	if latest == 0 {
		return 0, fmt.Errorf("no up migrations found in %s", dir)
	}

	return uint(latest), nil
}
