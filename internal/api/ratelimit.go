package api

import (
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"time"
)

type visitor struct {
	tokens   float64
	lastSeen time.Time
}

// RateLimiter implements a per-IP token bucket rate limiter.
type RateLimiter struct {
	visitors       map[string]*visitor
	mu             sync.Mutex
	rate           float64    // tokens per second
	burst          float64    // max tokens
	trustedProxies []*net.IPNet // parsed CIDR ranges for trusted proxies
}

// NewRateLimiter creates a rate limiter. rate is requests per second, burst is the max burst size.
// trustedProxyCIDRs is a list of CIDR strings (e.g., "10.0.0.0/8") whose
// X-Real-Ip / X-Forwarded-For headers will be trusted.
func NewRateLimiter(rate float64, burst int, trustedProxyCIDRs []string) *RateLimiter {
	var trustedNets []*net.IPNet
	for _, cidr := range trustedProxyCIDRs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			// Skip invalid CIDRs silently; they were validated at config time.
			continue
		}
		trustedNets = append(trustedNets, ipNet)
	}

	rl := &RateLimiter{
		visitors:       make(map[string]*visitor),
		rate:           rate,
		burst:          float64(burst),
		trustedProxies: trustedNets,
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
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	v, exists := rl.visitors[ip]
	if !exists {
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

// isTrustedProxy checks whether the given IP is within any of the trusted
// proxy CIDR ranges.
func (rl *RateLimiter) isTrustedProxy(ip net.IP) bool {
	for _, ipNet := range rl.trustedProxies {
		if ipNet.Contains(ip) {
			return true
		}
	}
	return false
}

// extractClientIP determines the real client IP from a request.
// It only trusts X-Real-Ip and X-Forwarded-For headers when the direct
// connection (r.RemoteAddr) comes from a trusted proxy. Otherwise it
// falls back to r.RemoteAddr to prevent header-spoofing attacks.
func (rl *RateLimiter) extractClientIP(r *http.Request) string {
	remoteIP := stripPort(r.RemoteAddr)

	parsedRemote := net.ParseIP(remoteIP)
	if parsedRemote == nil {
		// If we can't parse the remote address, return it as-is.
		return remoteIP
	}

	// Only trust forwarded headers when the direct peer is a known proxy.
	if rl.isTrustedProxy(parsedRemote) {
		if realIP := r.Header.Get("X-Real-Ip"); realIP != "" {
			return realIP
		}
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			// X-Forwarded-For can contain a comma-separated list; the
			// left-most entry is the original client.
			for i := 0; i < len(fwd); i++ {
				if fwd[i] == ',' {
					return fwd[:i]
				}
			}
			return fwd
		}
	}

	return remoteIP
}

// stripPort removes the port suffix from an address string.
// It handles both IPv4 ("1.2.3.4:8080") and IPv6 ("[::1]:8080") formats.
func stripPort(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// addr may already be a bare IP without a port.
		return addr
	}
	return host
}

// RateLimitMiddleware returns an HTTP middleware that enforces per-IP rate limits.
func RateLimitMiddleware(rl *RateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := rl.extractClientIP(r)

			if !rl.allow(ip) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				json.NewEncoder(w).Encode(Response{
					Success: false,
					Error:   "Rate limit exceeded",
				})
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
