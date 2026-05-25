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
	"github.com/a-thomas-22/blob-indexer-api/internal/bootstrap"
	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	logger.Initialize()
	defer func() { _ = logger.Sync() }()

	logger.Info("Starting blob-indexer API server", zap.String("version", version))

	resources, err := bootstrap.InitializeApp(config.LoadForAPI)
	if err != nil {
		logger.Fatal("Failed to initialize application", zap.Error(err))
	}
	cfg := resources.Config
	database := resources.Database
	ctx := resources.Ctx
	cancel := resources.Cancel
	logger.InitializeWithConfig(cfg.Logging.Level, cfg.Logging.Format)
	defer cancel()

	publicRouter, devRouter := api.NewRouters(ctx, database, cfg)
	servers := []namedServer{
		{
			name:   "api",
			server: newHTTPServer(cfg.Server.Port, publicRouter, cfg),
		},
	}
	if devRouter != nil {
		servers = append(servers, namedServer{
			name:   "dev-api",
			server: newHTTPServer(cfg.Server.DevPort, devRouter, cfg),
		})
	}

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)

	serverErrors := make(chan serverError, len(servers))
	for _, srv := range servers {
		go func() {
			logger.Info("API server listening",
				zap.String("server", srv.name),
				zap.String("addr", srv.server.Addr))
			if err := srv.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				serverErrors <- serverError{name: srv.name, err: err}
			}
		}()
	}

	select {
	case <-shutdown:
		logger.Info("Shutdown signal received")
	case <-ctx.Done():
		logger.Info("Context canceled")
	case err := <-serverErrors:
		logger.Error("Server error", zap.String("server", err.name), zap.Error(err.err))
		cancel()
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer shutdownCancel()

	for _, srv := range servers {
		if err := srv.server.Shutdown(shutdownCtx); err != nil {
			logger.Error("Server shutdown error", zap.String("server", srv.name), zap.Error(err))
		}
	}
	logger.Info("HTTP servers shutdown complete")

	if err := database.Close(); err != nil {
		logger.Warn("Database close returned error", zap.Error(err))
	}
	logger.Info("API server shutdown complete")
}

type namedServer struct {
	name   string
	server *http.Server
}

type serverError struct {
	name string
	err  error
}

func newHTTPServer(port int, handler http.Handler, cfg *config.Config) *http.Server {
	return &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           handler,
		ReadTimeout:       cfg.Server.ReadTimeout,
		WriteTimeout:      cfg.Server.WriteTimeout,
		IdleTimeout:       cfg.Server.IdleTimeout,
		ReadHeaderTimeout: 10 * time.Second,
	}
}
