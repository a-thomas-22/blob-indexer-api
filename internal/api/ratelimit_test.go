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

func TestRateLimitMiddleware_UsesTrustedCFConnectingIP(t *testing.T) {
	rl := &RateLimiter{
		visitors: make(map[string]*visitor),
		rate:     10,
		burst:    1,
	}
	resolver := newClientIPResolver([]string{"CF-Connecting-IP"})

	handler := RateLimitMiddlewareWithResolver(rl, resolver)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.RemoteAddr = "203.0.113.10:1234"
	req.Header.Set("CF-Connecting-IP", "198.51.100.25")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if _, ok := rl.visitors["198.51.100.25"]; !ok {
		t.Error("visitor should be tracked by trusted CF-Connecting-IP header")
	}
	if _, ok := rl.visitors["203.0.113.10"]; ok {
		t.Error("visitor should not fall back to remote address when trusted header is valid")
	}
}

func TestRateLimitMiddleware_IgnoresCFConnectingIPByDefault(t *testing.T) {
	rl := &RateLimiter{
		visitors: make(map[string]*visitor),
		rate:     10,
		burst:    1,
	}

	handler := RateLimitMiddleware(rl)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.RemoteAddr = "203.0.113.10:1234"
	req.Header.Set("CF-Connecting-IP", "198.51.100.25")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if _, ok := rl.visitors["198.51.100.25"]; ok {
		t.Error("visitor should not be tracked by untrusted CF-Connecting-IP header")
	}
	if _, ok := rl.visitors["203.0.113.10"]; !ok {
		t.Error("visitor should be tracked by normalized remote address")
	}
}

func TestClientIPResolver_UsesFirstForwardedForAddress(t *testing.T) {
	resolver := newClientIPResolver([]string{"X-Forwarded-For"})
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.RemoteAddr = "203.0.113.10:1234"
	req.Header.Set("X-Forwarded-For", "198.51.100.25, 198.51.100.26")

	if got := resolver.IP(req); got != "198.51.100.25" {
		t.Fatalf("expected first X-Forwarded-For address, got %q", got)
	}
}

func TestClientIPResolver_InvalidTrustedHeaderFallsBack(t *testing.T) {
	resolver := newClientIPResolver([]string{"CF-Connecting-IP"})
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.RemoteAddr = "203.0.113.10:1234"
	req.Header.Set("CF-Connecting-IP", "not-an-ip")

	if got := resolver.IP(req); got != "203.0.113.10" {
		t.Fatalf("expected fallback remote address, got %q", got)
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

func TestNormalizeIPAddress(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
		ok    bool
	}{
		{"empty", "", "", false},
		{"whitespace", "   ", "", false},
		{"unknown literal", "unknown", "", false},
		{"unknown mixed case", "Unknown", "", false},
		{"quoted ipv4", `"198.51.100.1"`, "198.51.100.1", true},
		{"plain ipv4", "198.51.100.1", "198.51.100.1", true},
		{"ipv4 with port", "198.51.100.1:1234", "198.51.100.1", true},
		{"bracketed ipv6", "[2001:db8::1]", "2001:db8::1", true},
		{"bracketed ipv6 with port", "[2001:db8::1]:443", "2001:db8::1", true},
		{"bare ipv6", "2001:db8::1", "2001:db8::1", true},
		{"garbage", "not-an-ip", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := normalizeIPAddress(tc.input)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("normalizeIPAddress(%q) = (%q, %v), want (%q, %v)", tc.input, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestNormalizeRemoteAddress(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"hostport with ipv4", "198.51.100.1:1234", "198.51.100.1"},
		{"hostport with ipv6", "[2001:db8::1]:443", "2001:db8::1"},
		{"hostport with non-ip host", "example.com:8080", "example.com"},
		{"bare ipv4", "198.51.100.1", "198.51.100.1"},
		{"bare ipv6", "2001:db8::1", "2001:db8::1"},
		{"garbage", "not-an-ip", "not-an-ip"},
		{"empty", "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeRemoteAddress(tc.input); got != tc.want {
				t.Fatalf("normalizeRemoteAddress(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestNewClientIPResolver_DeduplicatesAndTrims(t *testing.T) {
	resolver := newClientIPResolver([]string{
		"  CF-Connecting-IP  ",
		"cf-connecting-ip",
		"",
		"   ",
		"X-Forwarded-For",
	})

	if len(resolver.trustedHeaders) != 2 {
		t.Fatalf("expected 2 unique headers, got %d (%v)", len(resolver.trustedHeaders), resolver.trustedHeaders)
	}
	if resolver.trustedHeaders[0] != "Cf-Connecting-Ip" {
		t.Errorf("expected canonical first header, got %q", resolver.trustedHeaders[0])
	}
	if resolver.trustedHeaders[1] != "X-Forwarded-For" {
		t.Errorf("expected canonical second header, got %q", resolver.trustedHeaders[1])
	}
}

func TestClientIPResolver_TrustedHeaderEmptyFallsBackToRemoteAddr(t *testing.T) {
	resolver := newClientIPResolver([]string{"CF-Connecting-IP"})
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.RemoteAddr = "198.51.100.1:1234"
	// No CF-Connecting-IP header set.

	if got := resolver.IP(req); got != "198.51.100.1" {
		t.Fatalf("expected fallback to remote addr, got %q", got)
	}
}

func TestRateLimiter_Disabled(t *testing.T) {
	rl := NewRateLimiter(0, 100)
	for i := 0; i < 1000; i++ {
		if !rl.allow("1.2.3.4") {
			t.Fatalf("disabled rate limiter should always allow, denied at iteration %d", i)
		}
	}

	rl2 := NewRateLimiter(10, 0)
	for i := 0; i < 1000; i++ {
		if !rl2.allow("1.2.3.4") {
			t.Fatalf("disabled rate limiter (zero burst) should always allow, denied at iteration %d", i)
		}
	}
}

func TestRateLimiter_NilReceiverAllows(t *testing.T) {
	var rl *RateLimiter
	if !rl.allow("1.2.3.4") {
		t.Fatal("nil rate limiter should always allow")
	}
}
