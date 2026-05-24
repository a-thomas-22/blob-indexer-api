package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5/middleware"

	_ "github.com/a-thomas-22/blob-indexer-api/internal/testutil"
)

func TestRateLimiter_Allow_NewIP(t *testing.T) {
	rl := &RateLimiter{
		visitors: make(map[string]*visitor),
		rate:     10,
		burst:    20,
	}

	if !rl.allow("1.2.3.4") {
		t.Fatal("expected first request from new IP to be allowed")
	}

	// Visitor should be created with burst-1 tokens
	v := rl.visitors["1.2.3.4"]
	if v == nil {
		t.Fatal("visitor was not stored")
	}
	if v.tokens != 19 { // burst(20) - 1
		t.Errorf("expected 19 tokens, got %f", v.tokens)
	}
}

func TestRateLimiter_Allow_ExhaustedTokens(t *testing.T) {
	rl := &RateLimiter{
		visitors: make(map[string]*visitor),
		rate:     10,
		burst:    5,
	}

	ip := "10.0.0.1"
	// Exhaust all tokens (burst=5, so 5 requests should succeed)
	for i := 0; i < 5; i++ {
		if !rl.allow(ip) {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}

	// Next request should be denied
	if rl.allow(ip) {
		t.Fatal("request should be denied after token exhaustion")
	}
}

func TestRateLimiter_Allow_TokenReplenishment(t *testing.T) {
	rl := &RateLimiter{
		visitors: make(map[string]*visitor),
		rate:     100,
		burst:    100,
	}

	ip := "10.0.0.2"
	// Exhaust all tokens
	for i := 0; i < 100; i++ {
		rl.allow(ip)
	}

	// Simulate time passing by adjusting lastSeen backwards
	rl.mu.Lock()
	v := rl.visitors[ip]
	v.lastSeen = v.lastSeen.Add(-2 * 1e9) // 2 seconds ago → 200 tokens replenished
	rl.mu.Unlock()

	if !rl.allow(ip) {
		t.Fatal("request should be allowed after token replenishment")
	}
}

func TestRateLimiter_TokensCappedAtBurst(t *testing.T) {
	rl := &RateLimiter{
		visitors: make(map[string]*visitor),
		rate:     1000,
		burst:    10,
	}

	ip := "10.0.0.3"
	rl.allow(ip) // creates visitor

	// Simulate lots of time passing
	rl.mu.Lock()
	v := rl.visitors[ip]
	v.lastSeen = v.lastSeen.Add(-1e12) // very long ago
	rl.mu.Unlock()

	rl.allow(ip) // triggers replenishment

	rl.mu.Lock()
	if v.tokens > rl.burst {
		t.Errorf("tokens %f should not exceed burst %f", v.tokens, rl.burst)
	}
	rl.mu.Unlock()
}

func TestRateLimitMiddleware_Returns429(t *testing.T) {
	rl := &RateLimiter{
		visitors: make(map[string]*visitor),
		rate:     10,
		burst:    1, // only 1 request allowed
	}

	handler := RateLimitMiddleware(rl)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// First request should succeed
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.RemoteAddr = "5.5.5.5:1234"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Second request should be rate limited
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", w.Code)
	}

	// Check response body
	var resp Response
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Success {
		t.Error("expected Success=false")
	}
	if resp.Error != "Rate limit exceeded" {
		t.Errorf("expected 'Rate limit exceeded', got %q", resp.Error)
	}
	if w.Header().Get("Retry-After") != "1" {
		t.Errorf("expected Retry-After=1, got %q", w.Header().Get("Retry-After"))
	}
}

func TestRateLimitMiddleware_IgnoresXRealIP(t *testing.T) {
	rl := &RateLimiter{
		visitors: make(map[string]*visitor),
		rate:     10,
		burst:    1,
	}

	handler := RateLimitMiddleware(rl)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Request with a spoofed X-Real-Ip header.
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Real-Ip", "8.8.8.8")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if _, ok := rl.visitors["8.8.8.8"]; ok {
		t.Error("visitor should not be tracked by X-Real-Ip header")
	}
	if _, ok := rl.visitors["127.0.0.1"]; !ok {
		t.Error("visitor should be tracked by normalized remote address")
	}
}

func TestRateLimitMiddleware_UsesClientIPContext(t *testing.T) {
	rl := &RateLimiter{
		visitors: make(map[string]*visitor),
		rate:     10,
		burst:    1,
	}

	handler := middleware.ClientIPFromRemoteAddr(RateLimitMiddleware(rl)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.RemoteAddr = "192.0.2.10:1234"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if _, ok := rl.visitors["192.0.2.10"]; !ok {
		t.Error("visitor should be tracked by chi client IP context")
	}
	if _, ok := rl.visitors["192.0.2.10:1234"]; ok {
		t.Error("visitor should not include the remote address port")
	}
}
