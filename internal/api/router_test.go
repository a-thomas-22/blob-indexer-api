package api

import (
	"context"
	"testing"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	_ "github.com/a-thomas-22/blob-indexer-api/internal/testutil"
)

func TestNewRouter_ReturnsHandler(t *testing.T) {
	cfg := &config.Config{
		Server:  config.ServerConfig{Port: 8080, DevMode: true},
		Indexer: config.IndexerConfig{Version: "test"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler := NewRouter(ctx, nil, cfg)
	if handler == nil {
		t.Fatal("expected non-nil handler")
	}
}

func TestNewRouter_DevModeDisabled(t *testing.T) {
	cfg := &config.Config{
		Server:  config.ServerConfig{Port: 8080, DevMode: false},
		Indexer: config.IndexerConfig{Version: "test"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler := NewRouter(ctx, nil, cfg)
	if handler == nil {
		t.Fatal("expected non-nil handler")
	}
}
