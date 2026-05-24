package main

import (
	"os"
	"time"

	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/db"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

const (
	migrationAttempts = 60
	migrationDelay    = 2 * time.Second
)

func main() {
	os.Exit(run())
}

func run() int {
	logger.Initialize()
	defer func() { _ = logger.Sync() }()

	dbURL := os.Getenv("DB_URL")
	if dbURL == "" {
		logger.Fatal("DB_URL is required")
	}

	for attempt := 1; attempt <= migrationAttempts; attempt++ {
		if err := db.RunMigrations(dbURL); err == nil {
			logger.Info("Database migrations completed")
			return 0
		} else if attempt == migrationAttempts {
			logger.Fatal("Failed to run database migrations", zap.Error(err), zap.Int("attempt", attempt))
		} else {
			logger.Error("Failed to run database migrations; retrying",
				zap.Error(err),
				zap.Int("attempt", attempt),
				zap.Int("max_attempts", migrationAttempts),
				zap.Duration("retry_delay", migrationDelay))
			time.Sleep(migrationDelay)
		}
	}

	return 0
}
