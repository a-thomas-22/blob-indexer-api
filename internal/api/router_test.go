package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestNewRouters_DevRoutesOnPublicRouterByDefault(t *testing.T) {
	cfg := &config.Config{
		Server:  config.ServerConfig{Port: 8080, DevMode: true},
		Indexer: config.IndexerConfig{Version: "test"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	publicRouter, devRouter := NewRouters(ctx, nil, cfg)
	if publicRouter == nil {
		t.Fatal("expected non-nil public router")
	}
	if devRouter != nil {
		t.Fatal("expected nil dev router when dev_port is unset")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/dev/metrics", http.NoBody)
	w := httptest.NewRecorder()
	publicRouter.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected public dev metrics to return 200, got %d", w.Code)
	}
}

func TestNewRouters_DedicatedDevPortSplitsRoutes(t *testing.T) {
	cfg := &config.Config{
		Server:  config.ServerConfig{Port: 8080, DevPort: 8081, DevMode: true},
		Indexer: config.IndexerConfig{Version: "test"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	publicRouter, devRouter := NewRouters(ctx, nil, cfg)
	if publicRouter == nil {
		t.Fatal("expected non-nil public router")
	}
	if devRouter == nil {
		t.Fatal("expected non-nil dev router when dev_port is set")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/dev/metrics", http.NoBody)
	publicW := httptest.NewRecorder()
	publicRouter.ServeHTTP(publicW, req)
	if publicW.Code != http.StatusNotFound {
		t.Fatalf("expected public dev metrics to return 404, got %d", publicW.Code)
	}

	devW := httptest.NewRecorder()
	devRouter.ServeHTTP(devW, req)
	if devW.Code != http.StatusOK {
		t.Fatalf("expected dedicated dev metrics to return 200, got %d", devW.Code)
	}

	statusReq := httptest.NewRequest(http.MethodGet, "/api/v1/status", http.NoBody)
	statusW := httptest.NewRecorder()
	devRouter.ServeHTTP(statusW, statusReq)
	if statusW.Code != http.StatusNotFound {
		t.Fatalf("expected public status to be absent from dev router, got %d", statusW.Code)
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

func TestAsyncAPISpecEndpoint(t *testing.T) {
	cfg := &config.Config{
		Server:  config.ServerConfig{Port: 8080},
		Indexer: config.IndexerConfig{Version: "test"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler := NewRouter(ctx, nil, cfg)

	req := httptest.NewRequest(http.MethodGet, "/asyncapi.yaml", http.NoBody)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected asyncapi endpoint to return 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/yaml") {
		t.Fatalf("expected application/yaml content type, got %q", ct)
	}
	body, err := io.ReadAll(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "asyncapi:") || !strings.Contains(text, "new_block") {
		t.Fatalf("asyncapi response missing expected websocket schema content")
	}
}
