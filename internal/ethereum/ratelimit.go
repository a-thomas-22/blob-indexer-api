package ethereum

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/time/rate"

	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

// RateLimitConfig holds configuration for RPC rate limiting and 429 handling.
type RateLimitConfig struct {
	// RequestsPerSecond is the proactive rate limit. 0 means no proactive limiting.
	RequestsPerSecond float64
	// MaxRetries is the number of times to retry after a 429 response.
	MaxRetries int
	// InitialBackoff is the base backoff duration when no Retry-After header is present.
	InitialBackoff time.Duration
}

// rateLimitedTransport is an http.RoundTripper that handles HTTP 429 responses
// and optionally rate-limits outgoing requests via a token bucket.
type rateLimitedTransport struct {
	base           http.RoundTripper
	limiter        *rate.Limiter // nil if proactive limiting is disabled
	mu             sync.RWMutex
	backoffUntil   time.Time // shared: no requests before this time
	maxRetries     int
	initialBackoff time.Duration
}

// newProactiveLimiter builds the token bucket for cfg.RequestsPerSecond, or
// nil when proactive limiting is disabled. The burst equals one second of
// requests (at least one), so a rate below 1/s still admits a single call.
func newProactiveLimiter(cfg RateLimitConfig) *rate.Limiter {
	if cfg.RequestsPerSecond <= 0 {
		return nil
	}
	burst := int(cfg.RequestsPerSecond)
	if burst < 1 {
		burst = 1
	}
	return rate.NewLimiter(rate.Limit(cfg.RequestsPerSecond), burst)
}

// newRateLimitedTransport creates a rate-limited transport wrapping base.
// If cfg.RequestsPerSecond <= 0, the proactive limiter is not created.
func newRateLimitedTransport(base http.RoundTripper, cfg RateLimitConfig) *rateLimitedTransport {
	return &rateLimitedTransport{
		base:           base,
		maxRetries:     cfg.MaxRetries,
		initialBackoff: cfg.InitialBackoff,
		limiter:        newProactiveLimiter(cfg),
	}
}

// RoundTrip implements http.RoundTripper with 429 detection, retry, and rate limiting.
func (t *rateLimitedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Buffer the request body so we can replay it on retries.
	var bodyBytes []byte
	if req.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
	}

	var lastResp *http.Response
	for attempt := 0; attempt <= t.maxRetries; attempt++ {
		// Wait for any shared 429 backoff to expire.
		t.mu.RLock()
		waitUntil := t.backoffUntil
		t.mu.RUnlock()
		if wait := time.Until(waitUntil); wait > 0 {
			select {
			case <-time.After(wait):
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}

		// Wait for proactive rate limiter.
		if t.limiter != nil {
			if err := t.limiter.Wait(req.Context()); err != nil {
				return nil, err
			}
		}

		// Clone request with fresh body.
		clone := req.Clone(req.Context())
		if bodyBytes != nil {
			clone.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			clone.ContentLength = int64(len(bodyBytes))
		}

		resp, err := t.base.RoundTrip(clone)
		if err != nil {
			return nil, err
		}

		if resp.StatusCode != http.StatusTooManyRequests {
			return resp, nil
		}

		// Got 429 — parse Retry-After and set shared backoff.
		defaultBackoff := t.initialBackoff * (1 << uint(attempt))
		retryAfterStr := resp.Header.Get("Retry-After")
		backoff := parseRetryAfter(retryAfterStr, defaultBackoff)

		logger.Warn("RPC rate limited (HTTP 429)",
			zap.Int("attempt", attempt+1),
			zap.Int("max_retries", t.maxRetries),
			zap.Duration("backoff", backoff),
			zap.String("retry_after", retryAfterStr))

		t.mu.Lock()
		if newUntil := time.Now().Add(backoff); newUntil.After(t.backoffUntil) {
			t.backoffUntil = newUntil
		}
		t.mu.Unlock()

		// Close the 429 response body before retrying.
		if lastResp != nil {
			lastResp.Body.Close()
		}
		lastResp = resp

		if attempt < t.maxRetries {
			select {
			case <-time.After(backoff):
			case <-req.Context().Done():
				resp.Body.Close()
				return nil, req.Context().Err()
			}
		}
	}

	// All retries exhausted — return the last 429 response so go-ethereum
	// surfaces it as an rpc.HTTPError.
	return lastResp, nil
}

// parseRetryAfter parses the Retry-After header value per RFC 7231.
// Supports integer seconds ("120") and HTTP-date formats.
// Returns defaultBackoff if the header is empty, unparseable, or indicates a past time.
func parseRetryAfter(headerValue string, defaultBackoff time.Duration) time.Duration {
	if headerValue == "" {
		return defaultBackoff
	}
	// Try integer seconds.
	if seconds, err := strconv.Atoi(headerValue); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	// Try HTTP-date.
	if t, err := http.ParseTime(headerValue); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return defaultBackoff
}
