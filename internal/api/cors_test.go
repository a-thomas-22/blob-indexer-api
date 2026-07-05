package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
)

func TestCORSMiddleware_AllowsConfiguredOriginOnSuccess(t *testing.T) {
	handler := CORSMiddleware(testCORSConfig())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats?network=mainnet", http.NoBody)
	req.Header.Set("Origin", "http://localhost:3000")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Fatalf("expected origin echo, got %q", got)
	}
	if got := w.Header().Get("Access-Control-Expose-Headers"); got != "Content-Length, ETag" {
		t.Fatalf("expected exposed headers, got %q", got)
	}
	// A single-literal-origin policy pins the header set, so the response no
	// longer varies by requester and Vary: Origin is intentionally absent.
	if got := w.Header().Get("Vary"); got != "" {
		t.Fatalf("expected no Vary for pinned single-origin policy, got %q", got)
	}
}

func TestCORSMiddleware_PreflightShortCircuitsWithNoContent(t *testing.T) {
	nextCalled := false
	handler := CORSMiddleware(testCORSConfig())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/stats?network=mainnet", http.NoBody)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "content-type")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if nextCalled {
		t.Fatal("expected preflight to short-circuit before next handler")
	}
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Fatalf("expected origin echo, got %q", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Methods"); got != "GET, OPTIONS" {
		t.Fatalf("expected allowed methods, got %q", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Headers"); got != "Accept, Content-Type, Authorization" {
		t.Fatalf("expected allowed headers, got %q", got)
	}
	if got := w.Header().Get("Access-Control-Max-Age"); got != "86400" {
		t.Fatalf("expected max age, got %q", got)
	}
	if got := w.Header().Get("Vary"); got != "Origin, Access-Control-Request-Method, Access-Control-Request-Headers" {
		t.Fatalf("expected preflight Vary, got %q", got)
	}
}

func TestCORSMiddleware_AddsHeadersToErrorResponses(t *testing.T) {
	handler := CORSMiddleware(testCORSConfig())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats?network=mainnet", http.NoBody)
	req.Header.Set("Origin", "http://localhost:3000")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Fatalf("expected CORS header on error response, got %q", got)
	}
}

func TestCORSMiddleware_UsesOriginEchoWithCredentials(t *testing.T) {
	cfg := testCORSConfig()
	cfg.AllowAllOrigins = true
	cfg.AllowCredentials = true
	cfg.AllowedOrigins = nil

	handler := CORSMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/status?network=mainnet", http.NoBody)
	req.Header.Set("Origin", "https://preview.example.com")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://preview.example.com" {
		t.Fatalf("expected origin echo instead of wildcard, got %q", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("expected credentials header, got %q", got)
	}
}

func TestCORSMiddleware_AllowsOriginPattern(t *testing.T) {
	cfg := testCORSConfig()
	cfg.AllowedOrigins = nil
	cfg.AllowedOriginPatterns = []string{"https://*.vercel.app"}

	handler := CORSMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/status?network=mainnet", http.NoBody)
	req.Header.Set("Origin", "https://branch-name.vercel.app")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://branch-name.vercel.app" {
		t.Fatalf("expected pattern origin to be allowed, got %q", got)
	}
}

func TestCORSMiddleware_BlocksDisallowedOrigin(t *testing.T) {
	// Use a multi-origin config so reflection (not pinning) is exercised: a
	// disallowed Origin must not be echoed back.
	cfg := testCORSConfig()
	cfg.AllowedOrigins = append(cfg.AllowedOrigins, "https://second.example")
	handler := CORSMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/status?network=mainnet", http.NoBody)
	req.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("expected disallowed origin to be omitted, got %q", got)
	}

	// Pinned single-origin mode answers every requester with the constant
	// allowed origin; the disallowed caller is still blocked by its browser
	// because the pinned value names a different origin.
	pinned := CORSMiddleware(testCORSConfig())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	w2 := httptest.NewRecorder()
	pinned.ServeHTTP(w2, req)
	if got := w2.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Fatalf("expected pinned origin for single-origin policy, got %q", got)
	}
}

func TestCORSMiddleware_DisabledPassesThrough(t *testing.T) {
	cfg := testCORSConfig()
	cfg.Enabled = false

	handler := CORSMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/status?network=mainnet", http.NoBody)
	req.Header.Set("Origin", "http://localhost:3000")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("expected disabled CORS to omit origin header, got %q", got)
	}
}

func TestCORSMiddleware_PreflightAllowsWildcardConfig(t *testing.T) {
	cfg := testCORSConfig()
	cfg.AllowedOrigins = []string{"*"}
	cfg.AllowedMethods = []string{"*"}
	cfg.AllowedHeaders = []string{"*"}

	handler := CORSMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/stats?network=mainnet", http.NoBody)
	req.Header.Set("Origin", "https://preview.example.com")
	req.Header.Set("Access-Control-Request-Method", "PATCH")
	req.Header.Set("Access-Control-Request-Headers", "x-preview-token, content-type")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://preview.example.com" {
		t.Fatalf("expected origin echo, got %q", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Methods"); got != "*" {
		t.Fatalf("expected wildcard methods, got %q", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Headers"); got != "X-Preview-Token, Content-Type" {
		t.Fatalf("expected requested headers to be echoed, got %q", got)
	}
}

func TestCORSMiddleware_PreflightRejectsDisallowedRequest(t *testing.T) {
	handler := CORSMiddleware(testCORSConfig())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/stats?network=mainnet", http.NoBody)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "x-not-allowed")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("expected disallowed preflight to omit origin header, got %q", got)
	}
	if got := w.Header().Get("Vary"); got != "Origin, Access-Control-Request-Method, Access-Control-Request-Headers" {
		t.Fatalf("expected preflight Vary, got %q", got)
	}
}

func TestCORSPolicy_RejectsEmptyOriginAndMethod(t *testing.T) {
	policy := newCORSPolicy(testCORSConfig())

	if policy.isOriginAllowed("") {
		t.Fatal("expected empty origin to be rejected")
	}
	if policy.isMethodAllowed("") {
		t.Fatal("expected empty method to be rejected")
	}
}

func TestCORSPolicy_HeaderValidation(t *testing.T) {
	policy := newCORSPolicy(testCORSConfig())

	if !policy.areHeadersAllowed(nil) {
		t.Fatal("expected empty requested headers to be allowed")
	}
	if policy.areHeadersAllowed([]string{"X-Not-Allowed"}) {
		t.Fatal("expected unknown requested header to be rejected")
	}
}

func TestAddVary_DeduplicatesExistingValues(t *testing.T) {
	headers := http.Header{}
	headers.Add("Vary", "Accept-Encoding, Origin")
	headers.Add("Vary", "Origin")

	addVary(headers, "Access-Control-Request-Method", "Origin", "Access-Control-Request-Headers")

	if got := headers.Get("Vary"); got != "Accept-Encoding, Origin, Access-Control-Request-Method, Access-Control-Request-Headers" {
		t.Fatalf("unexpected Vary header %q", got)
	}
}

func TestWildcardMatch(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		value   string
		want    bool
	}{
		{name: "all", pattern: "*", value: "https://preview.example.com", want: true},
		{name: "exact match", pattern: "https://app.example.com", value: "https://app.example.com", want: true},
		{name: "exact mismatch", pattern: "https://app.example.com", value: "https://api.example.com", want: false},
		{name: "single segment", pattern: "https://*.vercel.app", value: "https://branch.vercel.app", want: true},
		{name: "multi segment", pattern: "https://*-blob-*.vercel.app", value: "https://main-blob-flow.vercel.app", want: true},
		{name: "bad prefix", pattern: "https://*.vercel.app", value: "http://branch.vercel.app", want: false},
		{name: "missing middle", pattern: "https://*-blob-*.vercel.app", value: "https://main-flow.vercel.app", want: false},
		{name: "bad suffix", pattern: "https://*.vercel.app", value: "https://branch.example.com", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := wildcardMatch(tt.pattern, tt.value); got != tt.want {
				t.Fatalf("wildcardMatch(%q, %q) = %v, want %v", tt.pattern, tt.value, got, tt.want)
			}
		})
	}
}

func testCORSConfig() config.CORSConfig {
	return config.CORSConfig{
		Enabled:          true,
		AllowedOrigins:   []string{"http://localhost:3000"},
		AllowedMethods:   []string{"GET", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Content-Type", "Authorization"},
		ExposedHeaders:   []string{"Content-Length", "ETag"},
		MaxAgeSeconds:    86400,
		AllowCredentials: false,
	}
}

// TestCORSMiddleware_PinnedSingleOrigin verifies that a single-literal-origin
// policy emits a constant Access-Control-Allow-Origin (no Vary: Origin) for
// every requester, so edge-cached copies always carry a header valid for the
// real frontend regardless of who populated the cache.
func TestCORSMiddleware_PinnedSingleOrigin(t *testing.T) {
	mw := CORSMiddleware(config.CORSConfig{
		Enabled:        true,
		AllowedOrigins: []string{"https://blob-flow.example"},
		AllowedMethods: []string{"GET", "OPTIONS"},
		ExposedHeaders: []string{"Content-Length", "ETag"},
	})
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	cases := []struct {
		name   string
		origin string
	}{
		{"no origin header", ""},
		{"allowed origin", "https://blob-flow.example"},
		{"other origin", "https://evil.example"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://blob-flow.example" {
				t.Errorf("ACAO = %q, want pinned origin", got)
			}
			if got := w.Header().Get("Vary"); got != "" {
				t.Errorf("Vary = %q, want none for constant headers", got)
			}
			if got := w.Header().Get("Access-Control-Expose-Headers"); got != "Content-Length, ETag" {
				t.Errorf("Expose-Headers = %q", got)
			}
		})
	}
}

// TestCORSMiddleware_NoPinWithMultipleOrigins verifies reflection semantics are
// preserved when more than one origin is allowed.
func TestCORSMiddleware_NoPinWithMultipleOrigins(t *testing.T) {
	mw := CORSMiddleware(config.CORSConfig{
		Enabled:        true,
		AllowedOrigins: []string{"https://a.example", "https://b.example"},
		AllowedMethods: []string{"GET"},
	})
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// No Origin header → no ACAO.
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", http.NoBody))
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("ACAO = %q, want empty without Origin", got)
	}

	// Allowed Origin → reflected.
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.Header.Set("Origin", "https://b.example")
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req)
	if got := w2.Header().Get("Access-Control-Allow-Origin"); got != "https://b.example" {
		t.Errorf("ACAO = %q, want reflected origin", got)
	}
	if got := w2.Header().Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q, want Origin", got)
	}
}

// TestCORSMiddleware_NoPinWithPatterns verifies wildcard patterns disable
// pinning even with a single literal origin.
func TestCORSMiddleware_NoPinWithPatterns(t *testing.T) {
	policy := newCORSPolicy(config.CORSConfig{
		Enabled:               true,
		AllowedOrigins:        []string{"https://a.example"},
		AllowedOriginPatterns: []string{"https://*.preview.example"},
		AllowedMethods:        []string{"GET"},
	})
	if policy.pinnedOrigin != "" {
		t.Fatalf("pinnedOrigin = %q, want empty with patterns configured", policy.pinnedOrigin)
	}
}
