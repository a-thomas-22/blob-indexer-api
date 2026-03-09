package bootstrap

import (
	"errors"
	"testing"
	"time"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
)

func TestInitializeApp_ConfigLoadError(t *testing.T) {
	loadErr := errors.New("config load failed")
	loadFn := func() (*config.Config, error) {
		return nil, loadErr
	}

	_, err := InitializeApp(loadFn)
	if err == nil {
		t.Fatal("expected error from InitializeApp when config load fails")
	}
	if !errors.Is(err, loadErr) {
		t.Fatalf("expected config load error wrapped in result, got: %v", err)
	}
}

func TestInitializeApp_DBConnectError(t *testing.T) {
	loadFn := func() (*config.Config, error) {
		return &config.Config{
			Database: config.DatabaseConfig{
				URL:             "postgres://invalid:invalid@127.0.0.1:1/invalid?sslmode=disable&connect_timeout=1",
				MaxOpenConns:    1,
				MaxIdleConns:    1,
				ConnMaxLifetime: time.Second,
				ConnMaxIdleTime: time.Second,
			},
			Networks: []config.NetworkConfig{
				{Name: "test", ChainID: 1, Enabled: true},
			},
		}, nil
	}

	_, err := InitializeApp(loadFn)
	if err == nil {
		t.Fatal("expected error from InitializeApp when DB connect fails")
	}
}
