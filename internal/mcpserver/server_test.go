package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
)

// Fixture credentials are composed from a repeated character rather than
// written as realistic literals, so secret scanners never see anything that
// resembles a real credential in test data.
func fakeSecret(label string) string {
	return strings.Repeat("k", config.MinMCPKeyLength) + "-" + label
}

var (
	testKey      = fakeSecret("community")
	testAdminKey = fakeSecret("pricing-only")
)

// fakeBackend imitates the REST layer: it records every request and answers
// from a per-path table of envelopes, defaulting to a success envelope that
// echoes the path and query so tests can assert the dispatch.
type fakeBackend struct {
	mu       sync.Mutex
	requests []*http.Request
	respond  map[string]func(w http.ResponseWriter, r *http.Request)
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{respond: make(map[string]func(http.ResponseWriter, *http.Request))}
}

func (b *fakeBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	b.requests = append(b.requests, r)
	fn := b.respond[r.URL.Path]
	b.mu.Unlock()
	if fn != nil {
		fn(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"data":    map[string]any{"path": r.URL.EscapedPath(), "query": r.URL.Query()},
	})
}

func (b *fakeBackend) last() *http.Request {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.requests) == 0 {
		return nil
	}
	return b.requests[len(b.requests)-1]
}

func (b *fakeBackend) paths() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.requests))
	for _, r := range b.requests {
		out = append(out, r.URL.Path)
	}
	return out
}

func writeError(status int, code, msg string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": msg, "error_code": code})
	}
}

func testConfig() config.MCPConfig {
	return config.MCPConfig{
		Enabled:        true,
		Path:           "/mcp",
		RateLimitRPS:   100,
		RateLimitBurst: 100,
		Keys: []config.MCPKeyConfig{
			{Name: "community", Key: testKey},
			{Name: "pricing-only", Key: testAdminKey, Tools: []string{"get_blob_pricing", "list_networks"}},
		},
	}
}

type authTransport struct {
	header, value string
}

func (t authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.header != "" {
		r.Header.Set(t.header, t.value)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// harness serves the MCP handler over a real HTTP listener and connects an
// SDK client with the given credential header.
type harness struct {
	t       *testing.T
	backend *fakeBackend
	server  *Server
	ts      *httptest.Server
}

func newHarness(t *testing.T, cfg config.MCPConfig) *harness {
	t.Helper()
	backend := newFakeBackend()
	server, err := New(cfg, backend, "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	return &harness{t: t, backend: backend, server: server, ts: ts}
}

func (h *harness) connect(key string) *mcp.ClientSession {
	h.t.Helper()
	transport := &mcp.StreamableClientTransport{
		Endpoint:   h.ts.URL,
		HTTPClient: &http.Client{Transport: authTransport{header: "Authorization", value: "Bearer " + key}},
		MaxRetries: -1,
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil).Connect(context.Background(), transport, nil)
	if err != nil {
		h.t.Fatalf("connect: %v", err)
	}
	h.t.Cleanup(func() { _ = session.Close() })
	return session
}

func callTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	return result
}

func resultText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if len(result.Content) != 1 {
		t.Fatalf("expected one content block, got %d", len(result.Content))
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected text content, got %T", result.Content[0])
	}
	return text.Text
}

func decodePayload(t *testing.T, result *mcp.CallToolResult) toolPayload {
	t.Helper()
	if result.IsError {
		t.Fatalf("unexpected tool error: %s", resultText(t, result))
	}
	var payload toolPayload
	if err := json.Unmarshal([]byte(resultText(t, result)), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return payload
}

func TestNew_RejectsBadInputs(t *testing.T) {
	if _, err := New(testConfig(), nil, "v"); err == nil {
		t.Fatal("expected error for nil backend")
	}
	cfg := testConfig()
	cfg.Keys[1].Tools = []string{"get_blob_pricing", "launch_rockets"}
	_, err := New(cfg, newFakeBackend(), "v")
	if err == nil || !strings.Contains(err.Error(), "launch_rockets") {
		t.Fatalf("expected unknown-tool error, got %v", err)
	}

	// Structural invariants are re-checked here so a programmatic caller
	// cannot cross-wire principals (servers are keyed by name).
	cases := map[string]func(*config.MCPConfig){
		"duplicate name":   func(c *config.MCPConfig) { c.Keys[1].Name = c.Keys[0].Name },
		"duplicate secret": func(c *config.MCPConfig) { c.Keys[1].Key = c.Keys[0].Key },
		"empty name":       func(c *config.MCPConfig) { c.Keys[0].Name = "" },
		"empty secret":     func(c *config.MCPConfig) { c.Keys[0].Key = "" },
	}
	for name, mutate := range cases {
		c := testConfig()
		mutate(&c)
		if _, err := New(c, newFakeBackend(), "v"); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

func TestValidateConfig(t *testing.T) {
	disabled := config.MCPConfig{Keys: []config.MCPKeyConfig{{Name: "x", Key: "y", Tools: []string{"nope"}}}}
	if err := ValidateConfig(disabled); err != nil {
		t.Fatalf("disabled config must not be validated for tools: %v", err)
	}
	if err := ValidateConfig(testConfig()); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	names := ToolNames()
	if len(names) != len(toolSpecs) {
		t.Fatalf("ToolNames returned %d names for %d specs", len(names), len(toolSpecs))
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Fatalf("ToolNames not sorted/unique at %d: %q >= %q", i, names[i-1], names[i])
		}
	}
	if !strings.Contains(strings.Join(names, ","), "get_blob_market_overview") {
		t.Fatal("overview tool missing from catalog")
	}
}

func TestNew_DefaultsVersion(t *testing.T) {
	h := newHarness(t, func() config.MCPConfig { c := testConfig(); return c }())
	if h.server.Path() != "/mcp" {
		t.Fatalf("Path() = %q", h.server.Path())
	}
	server, err := New(testConfig(), newFakeBackend(), "")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	transport := &mcp.StreamableClientTransport{Endpoint: ts.URL, HTTPClient: &http.Client{Transport: authTransport{"Authorization", "Bearer " + testKey}}}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil).Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if got := session.InitializeResult().ServerInfo.Version; got != "dev" {
		t.Fatalf("expected default version dev, got %q", got)
	}
	if session.InitializeResult().Instructions == "" {
		t.Fatal("expected server instructions to be advertised")
	}
}

func TestAuthentication(t *testing.T) {
	h := newHarness(t, testConfig())

	type postResult struct {
		status       int
		authenticate string
		body         map[string]any
	}
	post := func(header, value string) postResult {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, h.ts.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if header != "" {
			req.Header.Set(header, value)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		result := postResult{status: resp.StatusCode, authenticate: resp.Header.Get("WWW-Authenticate")}
		if resp.StatusCode == http.StatusUnauthorized {
			if err := json.NewDecoder(resp.Body).Decode(&result.body); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}

	cases := []struct {
		name, header, value string
		want                int
	}{
		{"missing", "", "", http.StatusUnauthorized},
		{"wrong bearer", "Authorization", "Bearer nope-nope-nope-nope-nope", http.StatusUnauthorized},
		{"empty bearer", "Authorization", "Bearer ", http.StatusUnauthorized},
		{"basic scheme", "Authorization", "Basic " + testKey, http.StatusUnauthorized},
		{"bearer ok", "Authorization", "Bearer " + testKey, http.StatusOK},
		{"bearer case-insensitive", "Authorization", "BEARER " + testAdminKey, http.StatusOK},
		{"x-api-key ok", "X-API-Key", testKey, http.StatusOK},
		{"x-api-key wrong", "X-API-Key", "short", http.StatusUnauthorized},
		{"same length wrong", "Authorization", "Bearer " + strings.Repeat("x", len(testKey)), http.StatusUnauthorized},
		{"prefix of key", "Authorization", "Bearer " + testKey[:len(testKey)-1], http.StatusUnauthorized},
		{"key plus suffix", "Authorization", "Bearer " + testKey + "x", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := post(tc.header, tc.value)
			if resp.status != tc.want {
				t.Fatalf("status = %d, want %d", resp.status, tc.want)
			}
			if tc.want == http.StatusUnauthorized {
				if !strings.HasPrefix(resp.authenticate, "Bearer") {
					t.Fatalf("WWW-Authenticate = %q", resp.authenticate)
				}
				if resp.body["error_code"] != "unauthorized" || resp.body["success"] != false {
					t.Fatalf("body = %v", resp.body)
				}
			}
		})
	}
	if len(h.backend.requests) != 0 {
		t.Fatalf("ping must not reach the backend, saw %v", h.backend.paths())
	}
}

func TestToolListingHonoursAllowlist(t *testing.T) {
	h := newHarness(t, testConfig())

	all := h.connect(testKey)
	tools, err := all.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != len(toolSpecs) {
		t.Fatalf("unrestricted key sees %d tools, want %d", len(tools.Tools), len(toolSpecs))
	}
	for _, tool := range tools.Tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Fatalf("tool %s must be marked read-only", tool.Name)
		}
		if tool.Description == "" || tool.Title == "" {
			t.Fatalf("tool %s is missing description or title", tool.Name)
		}
	}

	restricted := h.connect(testAdminKey)
	tools, err = restricted.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(tools.Tools))
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	// The SDK lists tools sorted by name.
	if strings.Join(names, ",") != "get_blob_pricing,list_networks" {
		t.Fatalf("restricted key sees %v", names)
	}

	// Calling a tool outside the allowlist is a protocol error (unknown tool),
	// never a backend fetch.
	if _, err := restricted.CallTool(context.Background(), &mcp.CallToolParams{Name: "search", Arguments: map[string]any{"query": "base"}}); err == nil {
		t.Fatal("expected error calling a tool outside the allowlist")
	}
	if len(h.backend.paths()) != 0 {
		t.Fatalf("disallowed tool reached backend: %v", h.backend.paths())
	}
}

func TestToolDispatch(t *testing.T) {
	h := newHarness(t, testConfig())
	session := h.connect(testKey)

	cases := []struct {
		tool      string
		args      map[string]any
		wantPath  string
		wantQuery url.Values
	}{
		{"list_networks", nil, "/api/v1/networks", url.Values{}},
		{"get_indexer_status", map[string]any{"network": "mainnet"}, "/api/v1/status", url.Values{"network": {"mainnet"}}},
		{"get_blob_pricing", map[string]any{"network": "1", "blocks": 50}, "/api/v1/blob/pricing", url.Values{"network": {"1"}, "blocks": {"50"}}},
		{"get_blob_pricing", map[string]any{}, "/api/v1/blob/pricing", url.Values{}},
		{"get_mempool_pressure", map[string]any{"network": "sepolia"}, "/api/v1/blob/mempool/pressure", url.Values{"network": {"sepolia"}}},
		{"get_blob_stats", nil, "/api/v1/stats", url.Values{}},
		{"get_rolling_stats", map[string]any{"windows": []string{"5m", "1h"}}, "/api/v1/stats/windows", url.Values{"windows": {"5m,1h"}}},
		{"get_rolling_stats", nil, "/api/v1/stats/windows", url.Values{}},
		{"get_blob_market_chart", map[string]any{"range": "7d", "granularity": "hour", "limit": 100}, "/api/v1/charts/blob-market", url.Values{"range": {"7d"}, "granularity": {"hour"}, "limit": {"100"}}},
		{"get_attribution_usage_chart", map[string]any{"range": "all", "limit": 3}, "/api/v1/charts/attribution-usage", url.Values{"range": {"all"}, "limit": {"3"}}},
		{"get_cost_comparison_chart", map[string]any{"range": "24h"}, "/api/v1/charts/cost-comparison", url.Values{"range": {"24h"}}},
		{"get_blob_tips_chart", map[string]any{"granularity": "day"}, "/api/v1/charts/blob-tips", url.Values{"granularity": {"day"}}},
		{"get_top_blob_users", map[string]any{"limit": 5, "offset": 10, "sort": "spend", "range": "24h", "group": "entity"}, "/api/v1/users", url.Values{"limit": {"5"}, "offset": {"10"}, "sort": {"spend"}, "range": {"24h"}, "group": {"entity"}}},
		{"get_top_blob_users", map[string]any{"unattributed_only": true}, "/api/v1/users/unattributed", url.Values{}},
		{"get_entity", map[string]any{"key": "base", "range": "7d"}, "/api/v1/entities/base", url.Values{"range": {"7d"}}},
		{"get_entity", map[string]any{"key": "Some Rollup/Name"}, "/api/v1/entities/Some%20Rollup%2FName", url.Values{}},
		{"get_blob_records", map[string]any{"limit": 3}, "/api/v1/records", url.Values{"limit": {"3"}}},
		{"get_latest_blobs", map[string]any{"limit": 2, "entity": "base"}, "/api/v1/blob/latest", url.Values{"limit": {"2"}, "entity": {"base"}}},
		{"get_mempool_blobs", map[string]any{"from": "0xabc", "offset": 4}, "/api/v1/blob/mempool", url.Values{"from": {"0xabc"}, "offset": {"4"}}},
		{"get_blob_replacements", map[string]any{"tx_hash": "0x01", "limit": 7, "offset": 1}, "/api/v1/blob/replacements", url.Values{"tx_hash": {"0x01"}, "limit": {"7"}, "offset": {"1"}}},
		{"get_block", map[string]any{"number": 123456, "network": "mainnet"}, "/api/v1/block/123456", url.Values{"network": {"mainnet"}}},
		{"get_blob_by_tx_hash", map[string]any{"tx_hash": "0xdead"}, "/api/v1/blob/0xdead", url.Values{}},
		{"get_blob_by_versioned_hash", map[string]any{"versioned_hash": "0x01beef"}, "/api/v1/blob/by-hash/0x01beef", url.Values{}},
		{"search", map[string]any{"query": "arbitrum", "network": "mainnet"}, "/api/v1/search", url.Values{"q": {"arbitrum"}, "network": {"mainnet"}}},
	}
	for _, tc := range cases {
		t.Run(tc.tool+"/"+fmt.Sprint(tc.args), func(t *testing.T) {
			result := callTool(t, session, tc.tool, tc.args)
			payload := decodePayload(t, result)
			var data struct {
				Path  string     `json:"path"`
				Query url.Values `json:"query"`
			}
			if err := json.Unmarshal(payload.Data, &data); err != nil {
				t.Fatal(err)
			}
			if data.Path != tc.wantPath {
				t.Fatalf("path = %q, want %q", data.Path, tc.wantPath)
			}
			if fmt.Sprint(data.Query) != fmt.Sprint(tc.wantQuery) {
				t.Fatalf("query = %v, want %v", data.Query, tc.wantQuery)
			}
			last := h.backend.last()
			if last.Method != http.MethodGet || last.Header.Get("Accept") != "application/json" || last.RemoteAddr != "127.0.0.1:0" {
				t.Fatalf("unexpected backend request shape: %s %s %q", last.Method, last.Header.Get("Accept"), last.RemoteAddr)
			}
			if payload.Meta != nil {
				t.Fatalf("expected no meta, got %s", payload.Meta)
			}
		})
	}
}

func TestToolInputValidationErrors(t *testing.T) {
	h := newHarness(t, testConfig())
	session := h.connect(testKey)

	cases := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"get_entity", map[string]any{"key": "  "}, "key is required"},
		{"get_blob_by_tx_hash", map[string]any{"tx_hash": ""}, "tx_hash is required"},
		{"get_blob_by_versioned_hash", map[string]any{"versioned_hash": " "}, "versioned_hash is required"},
		{"search", map[string]any{"query": ""}, "query is required"},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			result := callTool(t, session, tc.tool, tc.args)
			if !result.IsError {
				t.Fatalf("expected tool error, got %s", resultText(t, result))
			}
			if got := resultText(t, result); !strings.Contains(got, tc.want) {
				t.Fatalf("error = %q, want substring %q", got, tc.want)
			}
		})
	}
	if len(h.backend.paths()) != 0 {
		t.Fatalf("invalid inputs must not reach backend: %v", h.backend.paths())
	}

	// Schema violations (wrong types, unknown properties) are rejected by the
	// SDK before the handler runs and reported as tool errors the model can
	// read.
	result := callTool(t, session, "get_block", map[string]any{"number": "not-a-number"})
	if !result.IsError || !strings.Contains(resultText(t, result), `want "integer"`) {
		t.Fatalf("expected schema validation tool error, got %v %q", result.IsError, resultText(t, result))
	}
	result = callTool(t, session, "get_blob_stats", map[string]any{"bogus": true})
	if !result.IsError || !strings.Contains(resultText(t, result), "unexpected additional properties") {
		t.Fatalf("expected additional-properties tool error, got %v %q", result.IsError, resultText(t, result))
	}
	if len(h.backend.paths()) != 0 {
		t.Fatalf("schema violations must not reach backend: %v", h.backend.paths())
	}
}

func TestBackendErrorsBecomeToolErrors(t *testing.T) {
	h := newHarness(t, testConfig())
	h.backend.respond["/api/v1/blob/pricing"] = writeError(http.StatusBadRequest, "network_not_found", "Network not found")
	h.backend.respond["/api/v1/stats"] = writeError(http.StatusServiceUnavailable, "", "Failed to get blob statistics")
	h.backend.respond["/api/v1/status"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("404 page not found"))
	}
	h.backend.respond["/api/v1/records"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":false}`))
	}
	h.backend.respond["/api/v1/users"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"data":[{"address":"0x1"}],"meta":{"range":"24h"}}`))
	}
	session := h.connect(testKey)

	result := callTool(t, session, "get_blob_pricing", map[string]any{"network": "nope"})
	if !result.IsError || resultText(t, result) != "network_not_found: Network not found (HTTP 400)" {
		t.Fatalf("got %v %q", result.IsError, resultText(t, result))
	}

	result = callTool(t, session, "get_blob_stats", nil)
	if !result.IsError || resultText(t, result) != "Service Unavailable: Failed to get blob statistics (HTTP 503)" {
		t.Fatalf("got %v %q", result.IsError, resultText(t, result))
	}

	result = callTool(t, session, "get_indexer_status", nil)
	if !result.IsError || !strings.Contains(resultText(t, result), "invalid_backend_response") || !strings.Contains(resultText(t, result), "404 page not found") {
		t.Fatalf("got %v %q", result.IsError, resultText(t, result))
	}

	result = callTool(t, session, "get_blob_records", nil)
	if !result.IsError || resultText(t, result) != "OK: OK (HTTP 200)" {
		t.Fatalf("got %v %q", result.IsError, resultText(t, result))
	}

	payload := decodePayload(t, callTool(t, session, "get_top_blob_users", map[string]any{"range": "24h"}))
	if string(payload.Meta) != `{"range":"24h"}` || string(payload.Data) != `[{"address":"0x1"}]` {
		t.Fatalf("payload = %s / %s", payload.Data, payload.Meta)
	}
}

func TestBackendErrorTruncatesLongBodies(t *testing.T) {
	h := newHarness(t, testConfig())
	h.backend.respond["/api/v1/status"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(strings.Repeat("x", maxErrorBodyBytes*2)))
	}
	_, err := h.server.fetch(context.Background(), "/status", url.Values{})
	var backendErr *backendError
	if !errors.As(err, &backendErr) {
		t.Fatalf("expected backendError, got %v", err)
	}
	if backendErr.Status != http.StatusBadGateway || !strings.HasSuffix(backendErr.Message, "…") || len(backendErr.Message) > maxErrorBodyBytes+100 {
		t.Fatalf("unexpected error: status=%d len=%d", backendErr.Status, len(backendErr.Message))
	}
}

func TestFetchRejectsUnbuildableRequest(t *testing.T) {
	h := newHarness(t, testConfig())
	if _, err := h.server.fetch(context.Background(), "/\x7f", url.Values{}); err == nil {
		t.Fatal("expected request build error for invalid path")
	}
}

func TestResponseRecorderDefaults(t *testing.T) {
	rec := newResponseRecorder()
	rec.Header().Set("X-Test", "1")
	if _, err := rec.Write([]byte("body")); err != nil {
		t.Fatal(err)
	}
	rec.WriteHeader(http.StatusTeapot) // ignored: status already committed by Write
	if rec.status != http.StatusOK || rec.body.String() != "body" || rec.Header().Get("X-Test") != "1" {
		t.Fatalf("recorder state: %d %q", rec.status, rec.body.String())
	}
	// A handler that writes neither status nor body still reads as 200.
	h := newHarness(t, testConfig())
	h.backend.respond["/api/v1/stats"] = func(http.ResponseWriter, *http.Request) {}
	_, err := h.server.fetch(context.Background(), "/stats", url.Values{})
	var backendErr *backendError
	if !errors.As(err, &backendErr) || backendErr.Status != http.StatusOK {
		t.Fatalf("expected 200 invalid-body error, got %v", err)
	}
}

func TestJSONResultEncodingError(t *testing.T) {
	if _, err := jsonResult(make(chan int)); err == nil {
		t.Fatal("expected encoding error")
	}
}

func TestOverview(t *testing.T) {
	h := newHarness(t, testConfig())
	h.backend.respond["/api/v1/blob/pricing"] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("blocks") != "20" || r.URL.Query().Get("network") != "mainnet" {
			writeError(http.StatusBadRequest, "bad_query", r.URL.RawQuery)(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"success":true,"data":{"current_base_fee":"1"}}`))
	}
	h.backend.respond["/api/v1/blob/mempool/pressure"] = writeError(http.StatusServiceUnavailable, "db_overloaded", "Database overloaded")
	session := h.connect(testKey)

	result := callTool(t, session, "get_blob_market_overview", map[string]any{"network": "mainnet"})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	var overview overviewResult
	if err := json.Unmarshal([]byte(resultText(t, result)), &overview); err != nil {
		t.Fatal(err)
	}
	if overview.Network != "mainnet" || overview.Guide == "" {
		t.Fatalf("overview header: %+v", overview)
	}
	if string(overview.Sections["pricing"]) != `{"current_base_fee":"1"}` {
		t.Fatalf("pricing section = %s", overview.Sections["pricing"])
	}
	for _, key := range []string{"indexer_status", "rolling_stats"} {
		if _, ok := overview.Sections[key]; !ok {
			t.Fatalf("missing section %s", key)
		}
	}
	if _, ok := overview.Sections["mempool_pressure"]; ok {
		t.Fatal("failed section must not appear under sections")
	}
	if got := overview.Errors["mempool_pressure"]; got != "db_overloaded: Database overloaded (HTTP 503)" {
		t.Fatalf("errors = %v", overview.Errors)
	}
	if len(h.backend.paths()) != len(overviewSections) {
		t.Fatalf("expected %d backend fetches, got %v", len(overviewSections), h.backend.paths())
	}
}

func TestOverviewAllSectionsFail(t *testing.T) {
	h := newHarness(t, testConfig())
	for _, section := range overviewSections {
		h.backend.respond[apiBasePath+section.path] = writeError(http.StatusBadRequest, "network_not_found", "Network not found")
	}
	session := h.connect(testKey)
	result := callTool(t, session, "get_blob_market_overview", map[string]any{"network": "nope"})
	if !result.IsError {
		t.Fatalf("expected tool error, got %s", resultText(t, result))
	}
	if got := resultText(t, result); got != "every overview section failed: network_not_found: Network not found (HTTP 400)" {
		t.Fatalf("error = %q", got)
	}
}

func TestOverviewNormalizesNetworkSelector(t *testing.T) {
	// A blank selector resolves to the default network, so the echoed header
	// must omit it rather than claim a network was chosen.
	h := newHarness(t, testConfig())
	session := h.connect(testKey)
	text := resultText(t, callTool(t, session, "get_blob_market_overview", map[string]any{"network": "   "}))
	if strings.Contains(text, `"network"`) {
		t.Fatalf("blank network must be omitted from the header: %s", text)
	}
	for _, r := range h.backend.requests {
		if _, ok := r.URL.Query()["network"]; ok {
			t.Fatalf("blank network must not be sent to the backend: %s", r.URL.RawQuery)
		}
	}

	// A padded selector is echoed and queried in its trimmed form.
	h = newHarness(t, testConfig())
	session = h.connect(testKey)
	text = resultText(t, callTool(t, session, "get_blob_market_overview", map[string]any{"network": " mainnet "}))
	if !strings.Contains(text, `"network":"mainnet"`) {
		t.Fatalf("expected trimmed network in header: %s", text)
	}
	for _, r := range h.backend.requests {
		if got := r.URL.Query().Get("network"); got != "mainnet" {
			t.Fatalf("backend network = %q, want mainnet", got)
		}
	}
}

func TestOverviewOmitsErrorsWhenClean(t *testing.T) {
	h := newHarness(t, testConfig())
	session := h.connect(testKey)
	text := resultText(t, callTool(t, session, "get_blob_market_overview", nil))
	if strings.Contains(text, `"errors"`) {
		t.Fatalf("clean overview must omit errors: %s", text)
	}
	if strings.Contains(text, `"network"`) {
		t.Fatalf("unset network must be omitted: %s", text)
	}
}

func TestRateLimitPerKey(t *testing.T) {
	cfg := testConfig()
	cfg.RateLimitRPS = 0.001
	cfg.RateLimitBurst = 2
	h := newHarness(t, cfg)
	community := h.connect(testKey)
	other := h.connect(testAdminKey)

	for i := 0; i < 2; i++ {
		if result := callTool(t, community, "get_blob_stats", nil); result.IsError {
			t.Fatalf("call %d unexpectedly limited: %s", i, resultText(t, result))
		}
	}
	limited := callTool(t, community, "get_blob_stats", nil)
	if !limited.IsError || !strings.Contains(resultText(t, limited), "rate limit exceeded") {
		t.Fatalf("expected rate-limit tool error, got %v %q", limited.IsError, resultText(t, limited))
	}
	// Listing is never rate limited, and other keys have their own bucket.
	if _, err := community.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("ListTools under rate limit: %v", err)
	}
	if result := callTool(t, other, "get_blob_pricing", nil); result.IsError {
		t.Fatalf("other key should not be limited: %s", resultText(t, result))
	}
	if got := len(h.backend.paths()); got != 3 {
		t.Fatalf("expected 3 backend fetches (limited call must not fetch), got %d", got)
	}

	// Metrics reflect every outcome, labeled by principal.
	reg := prometheus.NewRegistry()
	reg.MustRegister(h.server.Collector())
	expected := `
# HELP blob_indexer_mcp_tool_calls_total MCP tool calls by API key name, tool, and outcome.
# TYPE blob_indexer_mcp_tool_calls_total counter
blob_indexer_mcp_tool_calls_total{outcome="ok",principal="community",tool="get_blob_stats"} 2
blob_indexer_mcp_tool_calls_total{outcome="ok",principal="pricing-only",tool="get_blob_pricing"} 1
blob_indexer_mcp_tool_calls_total{outcome="rate_limited",principal="community",tool="get_blob_stats"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "blob_indexer_mcp_tool_calls_total"); err != nil {
		t.Fatal(err)
	}
}

func TestMetricsRecordToolAndProtocolErrors(t *testing.T) {
	h := newHarness(t, testConfig())
	h.backend.respond["/api/v1/stats"] = writeError(http.StatusBadRequest, "bad", "bad")
	session := h.connect(testKey)
	callTool(t, session, "get_blob_stats", nil)
	callTool(t, session, "get_block", map[string]any{"number": "x"})

	count := testutil.ToFloat64(h.server.metrics.calls.WithLabelValues("community", "get_blob_stats", outcomeToolError))
	if count != 1 {
		t.Fatalf("tool_error count = %v", count)
	}
	// Schema violations are tool errors too (the SDK reports them in-band).
	count = testutil.ToFloat64(h.server.metrics.calls.WithLabelValues("community", "get_block", outcomeToolError))
	if count != 1 {
		t.Fatalf("schema violation tool_error count = %v", count)
	}

	// Protocol-level failures from the next handler are counted separately;
	// drive the middleware directly with an injectable clock.
	h.server.now = func() time.Time { return time.Unix(0, 0) }
	p := h.server.principals[0]
	handler := h.server.observe(p)(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return nil, errors.New("boom")
	})
	req := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{Params: &mcp.CallToolParamsRaw{Name: "get_block"}}
	if _, err := handler(context.Background(), "tools/call", req); err == nil || err.Error() != "boom" {
		t.Fatalf("expected next error to propagate, got %v", err)
	}
	count = testutil.ToFloat64(h.server.metrics.calls.WithLabelValues("community", "get_block", outcomeProtocolError))
	if count != 1 {
		t.Fatalf("protocol_error count = %v", count)
	}
	// Client-invented tool names never become metric labels or log fields;
	// they collapse to a constant so cardinality stays bounded. The SDK client
	// closes its session after an unknown-tool error, so each probe uses a
	// fresh session.
	for i := 0; i < 5; i++ {
		probe := h.connect(testKey)
		if _, err := probe.CallTool(context.Background(), &mcp.CallToolParams{Name: fmt.Sprintf("invented-%d", i), Arguments: map[string]any{}}); err == nil {
			t.Fatalf("call %d: expected unknown-tool error", i)
		}
	}
	if got := testutil.ToFloat64(h.server.metrics.calls.WithLabelValues("community", unknownToolLabel, outcomeProtocolError)); got != 5 {
		t.Fatalf("unknown tool calls should share one label, got %v", got)
	}
	if n := testutil.CollectAndCount(h.server.Collector()); n > 5 {
		t.Fatalf("expected bounded series count, got %d", n)
	}

	// Non tools/call methods bypass the hook entirely.
	passthrough := h.server.observe(p)(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return &mcp.ListToolsResult{}, nil
	})
	if _, err := passthrough(context.Background(), "tools/list", &mcp.ServerRequest[*mcp.ListToolsParams]{Params: &mcp.ListToolsParams{}}); err != nil {
		t.Fatal(err)
	}
}

func TestObserveHandlesNonToolParams(t *testing.T) {
	h := newHarness(t, testConfig())
	p := h.server.principals[0]
	called := false
	handler := h.server.observe(p)(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		called = true
		return &mcp.CallToolResult{}, nil
	})
	// A tools/call whose params are not CallToolParamsRaw still flows through
	// with an empty tool label.
	if _, err := handler(context.Background(), "tools/call", &mcp.ServerRequest[*mcp.ListToolsParams]{Params: &mcp.ListToolsParams{}}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("next handler not invoked")
	}
	if got := testutil.ToFloat64(h.server.metrics.calls.WithLabelValues("community", unknownToolLabel, outcomeOK)); got != 1 {
		t.Fatalf("expected unknown-tool ok count 1, got %v", got)
	}
}

func TestPrompt(t *testing.T) {
	h := newHarness(t, testConfig())
	session := h.connect(testAdminKey)

	prompts, err := session.ListPrompts(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts.Prompts) != 1 || prompts.Prompts[0].Name != explainPromptName {
		t.Fatalf("prompts = %+v", prompts.Prompts)
	}

	result, err := session.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: explainPromptName, Arguments: map[string]string{"network": "sepolia", "audience": "a rollup operator"}})
	if err != nil {
		t.Fatal(err)
	}
	text := result.Messages[0].Content.(*mcp.TextContent).Text
	if !strings.Contains(text, `network "sepolia"`) || !strings.Contains(text, "a rollup operator") || !strings.Contains(text, "get_blob_market_overview") {
		t.Fatalf("prompt text = %s", text)
	}

	result, err = session.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: explainPromptName})
	if err != nil {
		t.Fatal(err)
	}
	text = result.Messages[0].Content.(*mcp.TextContent).Text
	if !strings.Contains(text, "the default network") || !strings.Contains(text, "someone new to Ethereum blobs") {
		t.Fatalf("default prompt text = %s", text)
	}
}

func TestHandlerWithoutPrincipalReturnsNilServer(t *testing.T) {
	// Defensive path: the transport's getServer sees no principal only if the
	// auth middleware were bypassed. Exercise it directly.
	h := newHarness(t, testConfig())
	if _, ok := principalFromContext(context.Background()); ok {
		t.Fatal("empty context must carry no principal")
	}
	if _, ok := h.server.lookup(""); ok {
		t.Fatal("empty secret must not authenticate")
	}
	// Stateless transport rejects GET streams with 405 even when authenticated.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, h.ts.URL, http.NoBody)
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d", resp.StatusCode)
	}
}
