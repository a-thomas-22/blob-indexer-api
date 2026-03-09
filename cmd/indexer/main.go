package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db"
	"github.com/a-thomas-22/blob-indexer-api/internal/ethereum"
	"github.com/a-thomas-22/blob-indexer-api/internal/indexer"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	logger.Initialize()
	defer func() { _ = logger.Sync() }()

	logger.Info("Starting blob-indexer", zap.String("version", version))

	cfg, err := config.Load()
	if err != nil {
		logger.Fatal("Failed to load configuration", zap.Error(err))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	database, err := db.Connect(ctx, cfg.Database)
	if err != nil {
		logger.Fatal("Failed to connect to database", zap.Error(err))
	}
	defer func() {
		if err := database.Close(); err != nil {
			logger.Warn("Database close returned error", zap.Error(err))
		}
	}()

	if err := db.RunMigrations(cfg.Database.URL); err != nil {
		logger.Fatal("Failed to run database migrations", zap.Error(err))
	}

	enabledNetworks := cfg.GetEnabledNetworks()
	if len(enabledNetworks) == 0 {
		logger.Fatal("No enabled networks found in configuration")
	}

	indexers := make([]*indexer.Indexer, 0, len(enabledNetworks))
	for _, network := range enabledNetworks {
		// Always enable 429 retry handling; proactive rate limiting is
		// controlled by cfg.Indexer.RPCRateLimit (0 = disabled).
		ethClient, err := ethereum.NewClient(network.RpcURL,
			ethereum.WithRateLimit(ethereum.RateLimitConfig{
				RequestsPerSecond: cfg.Indexer.RPCRateLimit,
				MaxRetries:        3,
				InitialBackoff:    time.Second,
			}))
		if err != nil {
			logger.Fatal("Failed to initialize Ethereum client",
				zap.String("network", network.Name),
				zap.Error(err))
		}

		idx := indexer.New(ctx, database, ethClient, cfg, network)
		indexers = append(indexers, idx)

		go func(networkName string, idx *indexer.Indexer) {
			logger.Info("Starting indexer", zap.String("network", networkName))
			if err := idx.Start(); err != nil {
				logger.Error("Indexer error",
					zap.String("network", networkName),
					zap.Error(err))
			}
		}(network.Name, idx)
	}

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)

	select {
	case <-shutdown:
		logger.Info("Shutdown signal received")
	case <-ctx.Done():
		logger.Info("Context canceled")
	}

	for _, idx := range indexers {
		idx.Stop()
	}

	logger.Info("Indexer shutdown complete")
}
