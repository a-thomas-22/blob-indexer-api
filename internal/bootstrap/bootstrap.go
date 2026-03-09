package bootstrap

import (
	"context"
	"fmt"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db"
)

// AppResources contains shared startup resources for binaries.
type AppResources struct {
	Ctx      context.Context
	Cancel   context.CancelFunc
	Config   *config.Config
	Database *db.DB
}

// InitializeApp loads config, initializes context, connects DB, and runs migrations.
func InitializeApp(loadConfig func() (*config.Config, error)) (*AppResources, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load configuration: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	database, err := db.Connect(ctx, cfg.Database)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	if err := db.RunMigrations(cfg.Database.URL); err != nil {
		_ = database.Close()
		cancel()
		return nil, fmt.Errorf("failed to run database migrations: %w", err)
	}

	return &AppResources{
		Ctx:      ctx,
		Cancel:   cancel,
		Config:   cfg,
		Database: database,
	}, nil
}
