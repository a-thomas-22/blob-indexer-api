// @title Blob Indexer API
// @version 1.0
// @description API for indexing and querying blob transactions on Ethereum across multiple networks
// @termsOfService http://swagger.io/terms/

// @contact.name API Support
// @contact.url https://github.com/a-thomas-22/blob-indexer-api
// @contact.email support@example.com

// @license.name MIT
// @license.url https://opensource.org/licenses/MIT

// @host localhost:8080
// @BasePath /api
// @schemes http https

// @tag.name networks
// @tag.description Network-related endpoints

// @tag.name blobs
// @tag.description Blob transaction endpoints

// @tag.name users
// @tag.description User attribution endpoints

// @tag.name stats
// @tag.description Statistics endpoints

// @tag.name status
// @tag.description Indexer status endpoints

// @tag.name dev
// @tag.description Development endpoints (only available in dev mode)
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/a-thomas-22/blob-indexer-api/docs"

	"github.com/a-thomas-22/blob-indexer-api/internal/api"
	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db"
	"github.com/a-thomas-22/blob-indexer-api/internal/ethereum"
	"github.com/a-thomas-22/blob-indexer-api/internal/indexer"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
	"go.uber.org/zap"
)

func main() {
	// Initialize logger
	logger.Initialize()
	defer logger.Sync()

	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		// Log what file was attempted to be loaded
		logger.Error("Failed to load configuration",
			zap.String("config_path", os.Getenv("CONFIG_PATH")),
			zap.String("config_file", "railway-config.yaml"),
			zap.Error(err))
		// Print current working directory for debugging
		cwd, _ := os.Getwd()
		logger.Error("Current working directory", zap.String("cwd", cwd))
		// Print environment variables for debugging
		for _, env := range os.Environ() {
			logger.Debug("Environment variable", zap.String("env", env))
		}

		logger.Fatal("Failed to load configuration", zap.Error(err))
	}

	// Set up context with cancellation for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize database connection
	database, err := db.Connect(ctx, cfg.Database.URL)
	if err != nil {
		logger.Fatal("Failed to connect to database", zap.Error(err))
	}
	defer database.DB.Close()

	// Run database migrations
	if err := db.RunMigrations(cfg.Database.URL); err != nil {
		logger.Fatal("Failed to run database migrations", zap.Error(err))
	}

	// Get enabled networks
	enabledNetworks := cfg.GetEnabledNetworks()
	if len(enabledNetworks) == 0 {
		logger.Fatal("No enabled networks found in configuration")
	}

	// Create indexers for each enabled network
	indexers := make(map[int]*indexer.Indexer)
	for _, network := range enabledNetworks {
		// Initialize Ethereum client for this network
		ethClient, err := ethereum.NewClient(network.RpcURL)
		if err != nil {
			logger.Fatal("Failed to initialize Ethereum client",
				zap.String("network", network.Name),
				zap.Error(err))
		}

		// Create indexer for this network
		idx := indexer.New(ctx, database, ethClient, cfg, network)
		indexers[network.ChainID] = idx

		// Start indexing in background
		go func(networkName string, idx *indexer.Indexer) {
			logger.Info("Starting indexer", zap.String("network", networkName))
			if err := idx.Start(); err != nil {
				logger.Error("Indexer error",
					zap.String("network", networkName),
					zap.Error(err))
				cancel() // Cancel context on indexer error
			}
		}(network.Name, idx)
	}

	// Initialize API server
	router := api.NewRouter(database, indexers, cfg)
	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Server.Port),
		Handler: router,
	}

	// Channel to listen for shutdown signals
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)

	// Start server in a goroutine
	go func() {
		logger.Info("API server listening", zap.Int("port", cfg.Server.Port))
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("Server error", zap.Error(err))
			cancel() // Cancel context on server error
		}
	}()

	// Wait for shutdown signal or context cancellation
	select {
	case <-shutdown:
		logger.Info("Shutdown signal received")
	case <-ctx.Done():
		logger.Info("Context cancelled")
	}

	// Create a timeout context for graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	// Stop all indexers
	for _, idx := range indexers {
		idx.Stop()
	}

	// Shutdown the server
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("Server shutdown error", zap.Error(err))
	}

	logger.Info("Server shutdown complete")
}
