package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewRateLimiter(t *testing.T) {
	rl := NewRateLimiter(10, 20)
	if rl == nil {
		t.Fatal("expected non-nil rate limiter")
	}
	if rl.rate != 10 {
		t.Errorf("expected rate 10, got %f", rl.rate)
	}
	if rl.burst != 20 {
		t.Errorf("expected burst 20, got %f", rl.burst)
	}
	if rl.visitors == nil {
		t.Fatal("expected non-nil visitors map")
	}
}

func TestRateLimiter_AllowFirstRequest(t *testing.T) {
	rl := NewRateLimiter(10, 5)

	// First request from a new IP should always be allowed
	if !rl.allow("192.168.1.1") {
		t.Error("expected first request to be allowed")
	}
}

func TestRateLimiter_AllowUpToBurst(t *testing.T) {
	rl := NewRateLimiter(10, 5)

	ip := "192.168.1.1"
	// Should allow up to burst requests
	for i := 0; i < 5; i++ {
		if !rl.allow(ip) {
			t.Errorf("expected request %d to be allowed", i+1)
		}
	}
}

func TestRateLimiter_DenyOverBurst(t *testing.T) {
	rl := NewRateLimiter(0.001, 3) // Very slow replenishment

	ip := "10.0.0.1"
	// Exhaust the burst
	for i := 0; i < 3; i++ {
		rl.allow(ip)
	}

	// Next request should be denied
	if rl.allow(ip) {
		t.Error("expected request to be denied after burst exhausted")
	}
}

func TestRateLimiter_DifferentIPsIndependent(t *testing.T) {
	rl := NewRateLimiter(0.001, 2)

	// Exhaust burst for IP1
	rl.allow("1.1.1.1")
	rl.allow("1.1.1.1")

	// IP2 should still be allowed
	if !rl.allow("2.2.2.2") {
		t.Error("expected different IP to be allowed independently")
	}
}

func TestRateLimitMiddleware_AllowsRequest(t *testing.T) {
	rl := NewRateLimiter(100, 200)

	handler := RateLimitMiddleware(rl)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.168.1.1:1234"
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rr.Code)
	}
}

func TestRateLimitMiddleware_BlocksExcessiveRequests(t *testing.T) {
	rl := NewRateLimiter(0.001, 1) // 1 burst, very slow replenishment

	handler := RateLimitMiddleware(rl)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// First request passes
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:5000"
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("expected first request to return 200, got %d", rr.Code)
	}

	// Second request should be rate limited
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, req)
	if rr2.Code != http.StatusTooManyRequests {
		t.Errorf("expected second request to return 429, got %d", rr2.Code)
	}

	// Verify JSON error response
	var resp Response
	if err := json.NewDecoder(rr2.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}
	if resp.Success {
		t.Error("expected success to be false")
	}
	if resp.Error != "Rate limit exceeded" {
		t.Errorf("expected error 'Rate limit exceeded', got '%s'", resp.Error)
	}

	// Check Retry-After header
	if rr2.Header().Get("Retry-After") != "1" {
		t.Errorf("expected Retry-After header to be '1', got '%s'", rr2.Header().Get("Retry-After"))
	}
}

func TestRateLimitMiddleware_UsesXRealIP(t *testing.T) {
	rl := NewRateLimiter(0.001, 1)

	handler := RateLimitMiddleware(rl)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// First request with X-Real-Ip
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "proxy:8080"
	req.Header.Set("X-Real-Ip", "client-ip-1")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}

	// Second request with same X-Real-Ip should be rate limited
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, req)
	if rr2.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", rr2.Code)
	}

	// Request with different X-Real-Ip should be allowed
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.RemoteAddr = "proxy:8080"
	req2.Header.Set("X-Real-Ip", "client-ip-2")
	rr3 := httptest.NewRecorder()
	handler.ServeHTTP(rr3, req2)
	if rr3.Code != http.StatusOK {
		t.Errorf("expected 200 for different IP, got %d", rr3.Code)
	}
}
