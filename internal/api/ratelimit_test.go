package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// defaultTrustedProxies returns the standard private-network CIDR ranges
// used as defaults in production configuration.
func defaultTrustedProxies() []string {
	return []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"127.0.0.0/8",
		"::1/128",
	}
}

func TestExtractClientIP_TrustedProxy_XRealIp(t *testing.T) {
	rl := NewRateLimiter(10, 20, defaultTrustedProxies())

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.1:12345"
	r.Header.Set("X-Real-Ip", "203.0.113.50")

	got := rl.extractClientIP(r)
	if got != "203.0.113.50" {
		t.Errorf("expected X-Real-Ip to be trusted from proxy 10.0.0.1, got %q", got)
	}
}

func TestExtractClientIP_TrustedProxy_XForwardedFor(t *testing.T) {
	rl := NewRateLimiter(10, 20, defaultTrustedProxies())

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "172.16.5.10:8080"
	r.Header.Set("X-Forwarded-For", "198.51.100.20, 10.0.0.1")

	got := rl.extractClientIP(r)
	if got != "198.51.100.20" {
		t.Errorf("expected first X-Forwarded-For entry from trusted proxy, got %q", got)
	}
}

func TestExtractClientIP_TrustedProxy_XForwardedFor_SingleEntry(t *testing.T) {
	rl := NewRateLimiter(10, 20, defaultTrustedProxies())

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "127.0.0.1:9999"
	r.Header.Set("X-Forwarded-For", "198.51.100.99")

	got := rl.extractClientIP(r)
	if got != "198.51.100.99" {
		t.Errorf("expected single X-Forwarded-For entry from trusted proxy, got %q", got)
	}
}

func TestExtractClientIP_UntrustedSource_IgnoresHeaders(t *testing.T) {
	rl := NewRateLimiter(10, 20, defaultTrustedProxies())

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.100:54321" // public IP, not a trusted proxy
	r.Header.Set("X-Real-Ip", "1.2.3.4")
	r.Header.Set("X-Forwarded-For", "5.6.7.8")

	got := rl.extractClientIP(r)
	if got != "203.0.113.100" {
		t.Errorf("expected RemoteAddr IP when source is untrusted, got %q", got)
	}
}

func TestExtractClientIP_NoHeaders_ReturnsRemoteAddr(t *testing.T) {
	rl := NewRateLimiter(10, 20, defaultTrustedProxies())

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "192.168.1.50:12345"

	got := rl.extractClientIP(r)
	// No X-Real-Ip or X-Forwarded-For headers, so even though the source
	// is trusted, we fall back to the stripped RemoteAddr.
	if got != "192.168.1.50" {
		t.Errorf("expected stripped RemoteAddr, got %q", got)
	}
}

func TestExtractClientIP_IPv6_TrustedLoopback(t *testing.T) {
	rl := NewRateLimiter(10, 20, defaultTrustedProxies())

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "[::1]:8080"
	r.Header.Set("X-Real-Ip", "2001:db8::1")

	got := rl.extractClientIP(r)
	if got != "2001:db8::1" {
		t.Errorf("expected X-Real-Ip from IPv6 loopback proxy, got %q", got)
	}
}

func TestExtractClientIP_EmptyTrustedProxies_NeverTrustsHeaders(t *testing.T) {
	rl := NewRateLimiter(10, 20, nil) // no trusted proxies

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.1:12345"
	r.Header.Set("X-Real-Ip", "1.2.3.4")

	got := rl.extractClientIP(r)
	if got != "10.0.0.1" {
		t.Errorf("with no trusted proxies, should always use RemoteAddr, got %q", got)
	}
}

func TestExtractClientIP_CustomTrustedProxy(t *testing.T) {
	rl := NewRateLimiter(10, 20, []string{"203.0.113.0/24"})

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.5:443"
	r.Header.Set("X-Real-Ip", "198.51.100.1")

	got := rl.extractClientIP(r)
	if got != "198.51.100.1" {
		t.Errorf("expected X-Real-Ip from custom trusted proxy, got %q", got)
	}
}

func TestExtractClientIP_XRealIp_PrioritizedOverXForwardedFor(t *testing.T) {
	rl := NewRateLimiter(10, 20, defaultTrustedProxies())

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.1:12345"
	r.Header.Set("X-Real-Ip", "203.0.113.50")
	r.Header.Set("X-Forwarded-For", "198.51.100.20")

	got := rl.extractClientIP(r)
	if got != "203.0.113.50" {
		t.Errorf("X-Real-Ip should take priority over X-Forwarded-For, got %q", got)
	}
}

func TestStripPort(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"192.168.1.1:8080", "192.168.1.1"},
		{"192.168.1.1", "192.168.1.1"},
		{"[::1]:8080", "::1"},
		{"::1", "::1"},
		{"[2001:db8::1]:443", "2001:db8::1"},
	}

	for _, tt := range tests {
		got := stripPort(tt.input)
		if got != tt.want {
			t.Errorf("stripPort(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestRateLimitMiddleware_UsesExtractedIP(t *testing.T) {
	// Create a rate limiter with a very low limit so we can trigger it.
	rl := NewRateLimiter(0.001, 1, defaultTrustedProxies())

	handler := RateLimitMiddleware(rl)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// First request from an untrusted source with a spoofed header should
	// use RemoteAddr, not the spoofed X-Real-Ip.
	makeRequest := func(remoteAddr, xRealIP string) int {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = remoteAddr
		if xRealIP != "" {
			r.Header.Set("X-Real-Ip", xRealIP)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}

	// First request from 203.0.113.1 (untrusted) spoofing X-Real-Ip as different
	// IPs should still rate-limit on the real IP.
	status := makeRequest("203.0.113.1:1234", "1.1.1.1")
	if status != http.StatusOK {
		t.Fatalf("first request should succeed, got %d", status)
	}

	// Second request from same real IP with a different spoofed header should
	// be rate-limited (since burst=1 and rate is near zero).
	status = makeRequest("203.0.113.1:1234", "2.2.2.2")
	if status != http.StatusTooManyRequests {
		t.Fatalf("second request with same real IP should be rate-limited, got %d", status)
	}
}
