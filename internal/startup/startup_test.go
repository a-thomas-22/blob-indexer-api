package startup

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	_ "github.com/a-thomas-22/blob-indexer-api/internal/testutil"
)

// repoRoot returns the path to the repository root relative to this test file.
func repoRoot() string {
	_, filename, _, _ := runtime.Caller(0)
	// startup_test.go is at internal/startup/startup_test.go
	return filepath.Join(filepath.Dir(filename), "..", "..")
}

func TestInitialize_ConfigLoadError(t *testing.T) {
	// Use requireRPC=true without any environment vars set so config.Load() fails
	// due to missing network configuration or missing RPC URL.
	t.Setenv("DB_URL", "postgres://invalid:invalid@127.0.0.1:1/invalid?sslmode=disable&connect_timeout=1")
	t.Setenv("CONFIG_PATH", "/nonexistent/path/config.yaml")

	ctx := context.Background()
	_, err := Initialize(ctx, true)
	if err == nil {
		t.Fatal("expected Initialize to fail when config is invalid")
	}
}

func TestInitialize_DBConnectError(t *testing.T) {
	// Set a bad DB URL so that after config loads successfully, DB connect fails.
	t.Setenv("DB_URL", "postgres://invalid:invalid@127.0.0.1:1/invalid?sslmode=disable&connect_timeout=1")
	t.Setenv("CONFIG_PATH", filepath.Join(repoRoot(), "config.yaml"))

	ctx := context.Background()
	// requireRPC=false uses config.LoadForAPI() which doesn't require RPC URLs,
	// so config will load but DB connect should fail.
	_, err := Initialize(ctx, false)
	if err == nil {
		t.Fatal("expected Initialize to fail when DB cannot be reached")
	}
}

func TestApp_Shutdown_NilDB(t *testing.T) {
	// Shutdown should not panic when DB is nil.
	app := &App{DB: nil}
	app.Shutdown() // should not panic
}
