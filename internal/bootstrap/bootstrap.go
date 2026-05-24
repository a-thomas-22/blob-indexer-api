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

// InitializeApp loads config, initializes context, connects DB, optionally runs
// migrations, and syncs configured networks into the database.
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

	if cfg.Database.RunMigrations {
		if err := db.RunMigrations(cfg.Database.URL); err != nil {
			_ = database.Close()
			cancel()
			return nil, fmt.Errorf("failed to run database migrations: %w", err)
		}
	}

	if err := database.WaitForSchema(ctx, cfg.Database.SchemaWaitTimeout, cfg.Database.SchemaPollInterval); err != nil {
		_ = database.Close()
		cancel()
		return nil, fmt.Errorf("database schema is not ready: %w", err)
	}

	if err := database.UpsertNetworks(ctx, cfg.Networks); err != nil {
		_ = database.Close()
		cancel()
		return nil, fmt.Errorf("failed to sync configured networks: %w", err)
	}

	return &AppResources{
		Ctx:      ctx,
		Cancel:   cancel,
		Config:   cfg,
		Database: database,
	}, nil
}
