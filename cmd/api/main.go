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
// @BasePath /api/v1
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

	"go.uber.org/zap"

	_ "github.com/a-thomas-22/blob-indexer-api/docs"
	"github.com/a-thomas-22/blob-indexer-api/internal/api"
	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	logger.Initialize()
	defer func() { _ = logger.Sync() }()

	logger.Info("Starting blob-indexer API server", zap.String("version", version))

	cfg, err := config.LoadForAPI()
	if err != nil {
		logger.Fatal("Failed to load configuration", zap.Error(err))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	database, err := db.Connect(ctx, cfg.Database)
	if err != nil {
		logger.Fatal("Failed to connect to database", zap.Error(err))
	}

	if err := db.RunMigrations(cfg.Database.URL); err != nil {
		logger.Fatal("Failed to run database migrations", zap.Error(err))
	}

	router := api.NewRouter(database, cfg)
	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)

	go func() {
		logger.Info("API server listening", zap.Int("port", cfg.Server.Port))
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("Server error", zap.Error(err))
			cancel()
		}
	}()

	select {
	case <-shutdown:
		logger.Info("Shutdown signal received")
	case <-ctx.Done():
		logger.Info("Context canceled")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("Server shutdown error", zap.Error(err))
	}
	logger.Info("HTTP server shutdown complete")

	if err := database.Close(); err != nil {
		logger.Warn("Database close returned error", zap.Error(err))
	}
	logger.Info("API server shutdown complete")
}
