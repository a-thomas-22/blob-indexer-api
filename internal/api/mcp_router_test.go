package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
)

// Composed rather than written as a realistic literal so secret scanners do
// not flag test data as a leaked credential.
var testMCPKey = strings.Repeat("k", config.MinMCPKeyLength) + "-router-test"

func mcpTestConfig(enabled bool) *config.Config {
	return &config.Config{
		Server:  config.ServerConfig{Port: 8080},
		Indexer: config.IndexerConfig{Version: "test-v1"},
		MCP: config.MCPConfig{
			Enabled:        enabled,
			Path:           "/mcp",
			RateLimitRPS:   100,
			RateLimitBurst: 100,
			Keys:           []config.MCPKeyConfig{{Name: "community", Key: testMCPKey}},
		},
		Networks: []config.NetworkConfig{{Name: "testnet", ChainID: 42, Enabled: true}},
	}
}

type bearerTransport struct{ key string }

func (t bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set("Authorization", "Bearer "+t.key)
	return http.DefaultTransport.RoundTrip(r)
}

func TestRouter_MCPDisabledByDefault(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler := NewRouter(ctx, &mockDB{}, mcpTestConfig(false))

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when MCP is disabled, got %d", w.Code)
	}
}

func TestRouter_MCPRequiresKeyAndServesTools(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	publicRouter, devRouter := NewRouters(ctx, &mockDB{}, mcpTestConfig(true))
	if devRouter != nil {
		t.Fatal("expected no dev router")
	}

	// Unauthenticated requests are rejected before the transport runs.
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	publicRouter.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without key, got %d: %s", w.Code, w.Body.String())
	}

	// An authenticated client can list tools and call one; the call loops
	// back into the REST handlers (here backed by the mock DB).
	ts := httptest.NewServer(publicRouter)
	defer ts.Close()
	transport := &mcp.StreamableClientTransport{
		Endpoint:   ts.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearerTransport{key: testMCPKey}},
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()
	if got := session.InitializeResult().ServerInfo.Version; got != "test-v1" {
		t.Fatalf("expected server version from config, got %q", got)
	}

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) == 0 {
		t.Fatal("expected tools to be listed")
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_networks", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %s", result.Content[0].(*mcp.TextContent).Text)
	}
	var payload struct {
		Data []NetworkResponse `json:"data"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].(*mcp.TextContent).Text), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Data) != 1 || payload.Data[0].Name != "testnet" || payload.Data[0].ChainID != 42 {
		t.Fatalf("unexpected networks payload: %+v", payload.Data)
	}

	// REST-layer validation errors surface as tool errors the model can read.
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "get_blob_pricing", Arguments: map[string]any{"network": "nope"}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "network_not_found") {
		t.Fatalf("expected network_not_found tool error, got %+v", result.Content)
	}

	// The MCP collector is registered on the metrics endpoint.
	metricsReq := httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody)
	metricsW := httptest.NewRecorder()
	publicRouter.ServeHTTP(metricsW, metricsReq)
	if !strings.Contains(metricsW.Body.String(), `blob_indexer_mcp_tool_calls_total{outcome="ok",principal="community",tool="list_networks"} 1`) {
		t.Fatalf("expected MCP metrics on /metrics, got:\n%s", metricsW.Body.String())
	}
}

func TestRouter_MCPNotOnDedicatedDevRouter(t *testing.T) {
	cfg := mcpTestConfig(true)
	cfg.Server.DevPort = 8081
	cfg.Server.DevMode = true
	cfg.Server.DevAPIKey = "dev-key"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	publicRouter, devRouter := NewRouters(ctx, &mockDB{}, cfg)
	if devRouter == nil {
		t.Fatal("expected dev router")
	}
	for name, router := range map[string]http.Handler{"public": publicRouter, "dev": devRouter} {
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		want := http.StatusUnauthorized
		if name == "dev" {
			want = http.StatusNotFound
		}
		if w.Code != want {
			t.Fatalf("%s router: expected %d, got %d", name, want, w.Code)
		}
	}
}

func TestNewAPI_InvalidMCPConfigLeavesEndpointUnmounted(t *testing.T) {
	cfg := mcpTestConfig(true)
	cfg.MCP.Keys[0].Tools = []string{"not_a_tool"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler := NewRouter(ctx, &mockDB{}, cfg)

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testMCPKey)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (fail closed) for invalid MCP config, got %d", w.Code)
	}
}

func TestLoopbackHandler_ServesPublicRoutesWithoutEdgeMiddleware(t *testing.T) {
	api := newTestAPI()
	handler := api.loopbackHandler()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/networks", http.NoBody)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from loopback, got %d: %s", w.Code, w.Body.String())
	}
	var resp Response
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Success {
		t.Fatalf("expected success envelope, got %s", w.Body.String())
	}

	// Aggregate endpoints are not rate limited on the loopback path.
	for i := 0; i < 50; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", http.NoBody)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("loopback must not apply the aggregate rate limiter (call %d)", i)
		}
	}

	// A request whose context carries another router's chi state (as MCP tool
	// contexts do) must still be routed from scratch, not as a sub-mount.
	stale := chi.NewRouteContext()
	stale.RouteMethod = http.MethodPost
	stale.RoutePath = "/mcp"
	req = httptest.NewRequest(http.MethodGet, "/api/v1/networks", http.NoBody)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, stale))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected loopback to ignore inherited chi route context, got %d", w.Code)
	}

	// Unknown paths fall through to a plain 404, which the MCP layer reports.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/nope", http.NoBody)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown loopback path, got %d", w.Code)
	}
}
