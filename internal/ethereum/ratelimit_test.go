package ethereum

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRateLimitedTransport_NormalRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`))
	}))
	defer srv.Close()

	transport := newRateLimitedTransport(http.DefaultTransport, RateLimitConfig{
		MaxRetries:     3,
		InitialBackoff: 10 * time.Millisecond,
	})

	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber"}`))
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestRateLimitedTransport_429WithRetryAfterSeconds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`))
	}))
	defer srv.Close()

	transport := newRateLimitedTransport(http.DefaultTransport, RateLimitConfig{
		MaxRetries:     3,
		InitialBackoff: 10 * time.Millisecond,
	})

	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber"}`))
	start := time.Now()
	resp, err := transport.RoundTrip(req)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after retry, got %d", resp.StatusCode)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 2 calls, got %d", calls.Load())
	}
	if elapsed < 900*time.Millisecond {
		t.Fatalf("expected at least ~1s backoff, got %s", elapsed)
	}
}

func TestRateLimitedTransport_429WithRetryAfterHTTPDate(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			retryAt := time.Now().Add(500 * time.Millisecond).UTC().Format(http.TimeFormat)
			w.Header().Set("Retry-After", retryAt)
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`))
	}))
	defer srv.Close()

	transport := newRateLimitedTransport(http.DefaultTransport, RateLimitConfig{
		MaxRetries:     3,
		InitialBackoff: 10 * time.Millisecond,
	})

	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(`{}`))
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 2 calls, got %d", calls.Load())
	}
}

func TestRateLimitedTransport_429NoRetryAfterHeader(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`))
	}))
	defer srv.Close()

	transport := newRateLimitedTransport(http.DefaultTransport, RateLimitConfig{
		MaxRetries:     3,
		InitialBackoff: 50 * time.Millisecond,
	})

	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(`{}`))
	start := time.Now()
	resp, err := transport.RoundTrip(req)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	// Initial backoff is 50ms * 2^0 = 50ms
	if elapsed < 40*time.Millisecond {
		t.Fatalf("expected at least ~50ms backoff, got %s", elapsed)
	}
}

func TestRateLimitedTransport_429ExhaustsRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	transport := newRateLimitedTransport(http.DefaultTransport, RateLimitConfig{
		MaxRetries:     2,
		InitialBackoff: 10 * time.Millisecond,
	})

	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(`{}`))
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	// Should return the 429 after exhausting retries.
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", resp.StatusCode)
	}
	// 1 initial + 2 retries = 3 total calls.
	if calls.Load() != 3 {
		t.Fatalf("expected 3 calls, got %d", calls.Load())
	}
}

func TestRateLimitedTransport_SharedBackoff(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`))
	}))
	defer srv.Close()

	transport := newRateLimitedTransport(http.DefaultTransport, RateLimitConfig{
		MaxRetries:     3,
		InitialBackoff: 10 * time.Millisecond,
	})

	// First request triggers 429 and sets shared backoff.
	req1, _ := http.NewRequest("POST", srv.URL, strings.NewReader(`{}`))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := transport.RoundTrip(req1)
		if err != nil {
			t.Errorf("req1 unexpected error: %v", err)
			return
		}
		resp.Body.Close()
	}()

	// Wait a bit for the first request to set backoff, then send second request.
	time.Sleep(100 * time.Millisecond)
	req2, _ := http.NewRequest("POST", srv.URL, strings.NewReader(`{}`))
	start := time.Now()
	resp2, err := transport.RoundTrip(req2)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("req2 unexpected error: %v", err)
	}
	resp2.Body.Close()

	wg.Wait()

	// The second request should have waited for the shared backoff (remainder of ~1s).
	if elapsed < 500*time.Millisecond {
		t.Fatalf("expected second request to wait for shared backoff, elapsed: %s", elapsed)
	}
}

func TestRateLimitedTransport_ContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	transport := newRateLimitedTransport(http.DefaultTransport, RateLimitConfig{
		MaxRetries:     3,
		InitialBackoff: 60 * time.Second,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL, strings.NewReader(`{}`))
	_, err := transport.RoundTrip(req)
	if err == nil {
		t.Fatal("expected context error, got nil")
	}
	if ctx.Err() == nil {
		t.Fatal("expected context to be cancelled")
	}
}

func TestRateLimitedTransport_RateLimiterThrottles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`))
	}))
	defer srv.Close()

	// 2 requests per second with burst of 2.
	transport := newRateLimitedTransport(http.DefaultTransport, RateLimitConfig{
		RequestsPerSecond: 2,
		MaxRetries:        0,
		InitialBackoff:    time.Second,
	})

	// Fire 4 requests sequentially.
	start := time.Now()
	for range 4 {
		req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(`{}`))
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		resp.Body.Close()
	}
	elapsed := time.Since(start)

	// With burst=2 and rate=2/s: first 2 are immediate, then ~500ms each.
	// Total: ~1s for 4 requests.
	if elapsed < 800*time.Millisecond {
		t.Fatalf("expected rate limiting to slow requests, elapsed: %s", elapsed)
	}
}

func TestRateLimitedTransport_RateLimiterDisabled(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`))
	}))
	defer srv.Close()

	// RequestsPerSecond = 0 means no proactive limiting.
	transport := newRateLimitedTransport(http.DefaultTransport, RateLimitConfig{
		RequestsPerSecond: 0,
		MaxRetries:        0,
		InitialBackoff:    time.Second,
	})

	if transport.limiter != nil {
		t.Fatal("expected limiter to be nil when RequestsPerSecond is 0")
	}

	start := time.Now()
	for range 10 {
		req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(`{}`))
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		resp.Body.Close()
	}
	elapsed := time.Since(start)

	if calls.Load() != 10 {
		t.Fatalf("expected 10 calls, got %d", calls.Load())
	}
	// Without rate limiting, 10 sequential requests to localhost should be very fast.
	if elapsed > 2*time.Second {
		t.Fatalf("requests took too long without rate limiting: %s", elapsed)
	}
}

func TestRateLimitedTransport_RequestBodyPreserved(t *testing.T) {
	var bodies []string
	var mu sync.Mutex
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`))
	}))
	defer srv.Close()

	transport := newRateLimitedTransport(http.DefaultTransport, RateLimitConfig{
		MaxRetries:     1,
		InitialBackoff: 10 * time.Millisecond,
	})

	payload := `{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber"}`
	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(payload))
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(bodies))
	}
	for i, b := range bodies {
		if b != payload {
			t.Errorf("request %d body mismatch: got %q, want %q", i, b, payload)
		}
	}
}

func TestRateLimitedTransport_NilBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	transport := newRateLimitedTransport(http.DefaultTransport, RateLimitConfig{
		MaxRetries:     1,
		InitialBackoff: 10 * time.Millisecond,
	})

	req, _ := http.NewRequest("GET", srv.URL, nil)
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestParseRetryAfter_Seconds(t *testing.T) {
	d := parseRetryAfter("120", time.Second)
	if d != 120*time.Second {
		t.Fatalf("expected 120s, got %s", d)
	}
}

func TestParseRetryAfter_Empty(t *testing.T) {
	d := parseRetryAfter("", 5*time.Second)
	if d != 5*time.Second {
		t.Fatalf("expected 5s default, got %s", d)
	}
}

func TestParseRetryAfter_Invalid(t *testing.T) {
	d := parseRetryAfter("not-a-number", 3*time.Second)
	if d != 3*time.Second {
		t.Fatalf("expected 3s default, got %s", d)
	}
}

func TestParseRetryAfter_NegativeSeconds(t *testing.T) {
	d := parseRetryAfter("-1", 2*time.Second)
	if d != 2*time.Second {
		t.Fatalf("expected 2s default for negative, got %s", d)
	}
}

func TestParseRetryAfter_ZeroSeconds(t *testing.T) {
	d := parseRetryAfter("0", 2*time.Second)
	if d != 2*time.Second {
		t.Fatalf("expected 2s default for zero, got %s", d)
	}
}

func TestParseRetryAfter_HTTPDate(t *testing.T) {
	future := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	d := parseRetryAfter(future, time.Second)
	// Should be roughly 30 seconds (allow some tolerance).
	if d < 28*time.Second || d > 32*time.Second {
		t.Fatalf("expected ~30s, got %s", d)
	}
}

func TestParseRetryAfter_PastDate(t *testing.T) {
	past := time.Now().Add(-10 * time.Second).UTC().Format(http.TimeFormat)
	d := parseRetryAfter(past, 5*time.Second)
	if d != 5*time.Second {
		t.Fatalf("expected 5s default for past date, got %s", d)
	}
}
