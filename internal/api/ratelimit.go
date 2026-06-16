package api

import (
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

type visitor struct {
	tokens   float64
	lastSeen time.Time
}

// maxVisitors caps the number of per-IP token buckets the rate limiter retains.
// Because the service sits behind Cloudflare and keys on the resolved client IP,
// a flood of distinct (or spoofed) source IPs could otherwise grow the visitor
// map without bound. When the cap is reached, the least-recently-seen entry is
// evicted to make room for a new visitor.
const maxVisitors = 100_000

// RateLimiter implements a per-IP token bucket rate limiter.
type RateLimiter struct {
	visitors    map[string]*visitor
	mu          sync.Mutex
	rate        float64 // tokens per second
	burst       float64 // max tokens
	maxVisitors int     // cap on retained visitor buckets; <=0 means unlimited
	disabled    bool
}

// NewRateLimiter creates a rate limiter. rate is requests per second, burst is the max burst size.
// A non-positive rate or burst disables limiting.
func NewRateLimiter(rate float64, burst int) *RateLimiter {
	rl := &RateLimiter{
		visitors:    make(map[string]*visitor),
		rate:        rate,
		burst:       float64(burst),
		maxVisitors: maxVisitors,
		disabled:    rate <= 0 || burst <= 0,
	}
	go rl.cleanup()
	return rl
}

func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		rl.mu.Lock()
		for ip, v := range rl.visitors {
			if time.Since(v.lastSeen) > 3*time.Minute {
				delete(rl.visitors, ip)
			}
		}
		rl.mu.Unlock()
	}
}

func (rl *RateLimiter) allow(ip string) bool {
	if rl == nil || rl.disabled {
		return true
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	v, exists := rl.visitors[ip]
	if !exists {
		rl.evictIfFull(now)
		rl.visitors[ip] = &visitor{tokens: rl.burst - 1, lastSeen: now}
		return true
	}

	// Replenish tokens based on elapsed time
	elapsed := now.Sub(v.lastSeen).Seconds()
	v.tokens += elapsed * rl.rate
	if v.tokens > rl.burst {
		v.tokens = rl.burst
	}
	v.lastSeen = now

	if v.tokens < 1 {
		return false
	}

	v.tokens--
	return true
}

// evictIfFull removes the least-recently-seen visitor when the map is at
// capacity, bounding memory under a flood of distinct client IPs. Callers must
// hold rl.mu. now is passed in so eviction shares the caller's clock reading.
func (rl *RateLimiter) evictIfFull(now time.Time) {
	if rl.maxVisitors <= 0 || len(rl.visitors) < rl.maxVisitors {
		return
	}

	var oldestIP string
	oldestSeen := now
	found := false
	for ip, v := range rl.visitors {
		if !found || v.lastSeen.Before(oldestSeen) {
			oldestIP = ip
			oldestSeen = v.lastSeen
			found = true
		}
	}
	if found {
		delete(rl.visitors, oldestIP)
	}
}

// RateLimitMiddleware returns an HTTP middleware that enforces per-IP rate limits.
func RateLimitMiddleware(rl *RateLimiter) func(http.Handler) http.Handler {
	return RateLimitMiddlewareWithResolver(rl, newClientIPResolver(nil))
}

// RateLimitMiddlewareWithResolver returns an HTTP middleware that enforces
// per-client rate limits using the provided client IP resolver.
func RateLimitMiddlewareWithResolver(rl *RateLimiter, resolver clientIPResolver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := resolver.IP(r)

			if !rl.allow(ip) {
				incRateLimitRejections()
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				_ = json.NewEncoder(w).Encode(Response{
					Success: false,
					Error:   "Rate limit exceeded",
				})
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

type clientIPResolver struct {
	trustedHeaders []string
}

func newClientIPResolver(trustedHeaders []string) clientIPResolver {
	headers := make([]string, 0, len(trustedHeaders))
	seen := make(map[string]struct{}, len(trustedHeaders))
	for _, header := range trustedHeaders {
		header = http.CanonicalHeaderKey(strings.TrimSpace(header))
		if header == "" {
			continue
		}
		if _, ok := seen[header]; ok {
			continue
		}
		seen[header] = struct{}{}
		headers = append(headers, header)
	}
	return clientIPResolver{trustedHeaders: headers}
}

// IP returns the best client IP for logging and rate limiting. Trusted headers
// are opt-in because they are spoofable when the origin is directly reachable.
func (r clientIPResolver) IP(req *http.Request) string {
	if ip, ok := firstTrustedHeaderIP(req.Header, r.trustedHeaders); ok {
		return ip
	}

	ip := strings.TrimSpace(middleware.GetClientIP(req.Context()))
	if normalized, ok := normalizeIPAddress(ip); ok {
		return normalized
	}

	return normalizeRemoteAddress(req.RemoteAddr)
}

func firstTrustedHeaderIP(headers http.Header, trustedHeaders []string) (string, bool) {
	for _, header := range trustedHeaders {
		for _, value := range headers.Values(header) {
			for _, candidate := range strings.Split(value, ",") {
				if ip, ok := normalizeIPAddress(candidate); ok {
					return ip, true
				}
			}
		}
	}
	return "", false
}

func normalizeRemoteAddress(remoteAddr string) string {
	remoteAddr = strings.TrimSpace(remoteAddr)
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		if ip, ok := normalizeIPAddress(host); ok {
			return ip
		}
		return host
	}
	if ip, ok := normalizeIPAddress(remoteAddr); ok {
		return ip
	}
	return remoteAddr
}

func normalizeIPAddress(value string) (string, bool) {
	value = strings.Trim(strings.TrimSpace(value), `"`)
	if value == "" || strings.EqualFold(value, "unknown") {
		return "", false
	}

	if host, _, err := net.SplitHostPort(value); err == nil {
		value = host
	}
	value = strings.Trim(value, "[]")

	addr, err := netip.ParseAddr(value)
	if err != nil {
		return "", false
	}
	return addr.String(), true
}
