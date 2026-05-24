package main

import (
	"os"

	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/db"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

func main() {
	os.Exit(run())
}

func run() int {
	logger.Initialize()
	defer func() { _ = logger.Sync() }()

	logger.InitializeWithConfig(os.Getenv("LOG_LEVEL"), os.Getenv("LOG_FORMAT"))

	dbURL := os.Getenv("DB_URL")
	if dbURL == "" {
		logger.Fatal("DB_URL is required")
	}

	if err := db.RunMigrations(dbURL); err != nil {
		logger.Fatal("Failed to run database migrations", zap.Error(err))
	}

	logger.Info("Database migrations completed")
	return 0
}
