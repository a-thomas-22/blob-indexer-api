package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
)

type corsPolicy struct {
	enabled               bool
	allowedOrigins        map[string]struct{}
	allowedOriginPatterns []string
	allowAllOrigins       bool
	allowedMethods        []string
	allowedMethodSet      map[string]struct{}
	allowedHeaders        []string
	allowedHeaderSet      map[string]struct{}
	allowedHeadersAll     bool
	exposedHeaders        []string
	allowCredentials      bool
	maxAgeSeconds         int
	// pinnedOrigin is set when exactly one literal origin is allowed (no
	// wildcard, no patterns). Responses then carry a constant
	// Access-Control-Allow-Origin instead of reflecting the request Origin:
	// shared caches like Cloudflare ignore Vary on JSON, so a reflected header
	// would serve whichever requester populated the cache to everyone else.
	pinnedOrigin string
}

func CORSMiddleware(cfg config.CORSConfig) func(http.Handler) http.Handler {
	policy := newCORSPolicy(cfg)
	return func(next http.Handler) http.Handler {
		if !policy.enabled {
			return next
		}

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if policy.isPreflight(r) {
				policy.handlePreflight(w, r)
				return
			}

			// Single-origin deployments emit a constant header set — with or
			// without a request Origin — so every cached copy of a response is
			// valid for the one real frontend. No Vary: Origin either, since
			// the response no longer varies by requester.
			if policy.pinnedOrigin != "" {
				if policy.isMethodAllowed(r.Method) {
					policy.setAllowedOriginHeaders(w.Header(), policy.pinnedOrigin)
					if len(policy.exposedHeaders) > 0 {
						w.Header().Set("Access-Control-Expose-Headers", strings.Join(policy.exposedHeaders, ", "))
					}
				}
				next.ServeHTTP(w, r)
				return
			}

			origin := strings.TrimSpace(r.Header.Get("Origin"))
			if origin != "" {
				addVary(w.Header(), "Origin")
				if policy.isOriginAllowed(origin) && policy.isMethodAllowed(r.Method) {
					policy.setAllowedOriginHeaders(w.Header(), origin)
					if len(policy.exposedHeaders) > 0 {
						w.Header().Set("Access-Control-Expose-Headers", strings.Join(policy.exposedHeaders, ", "))
					}
				}
			}

			next.ServeHTTP(w, r)
		})
	}
}

func newCORSPolicy(cfg config.CORSConfig) corsPolicy {
	policy := corsPolicy{
		enabled:               cfg.Enabled,
		allowedOrigins:        make(map[string]struct{}),
		allowedOriginPatterns: normalizeOriginPatterns(cfg.AllowedOriginPatterns),
		allowAllOrigins:       cfg.AllowAllOrigins,
		allowedMethods:        normalizeMethods(cfg.AllowedMethods),
		allowedMethodSet:      make(map[string]struct{}),
		allowedHeaders:        normalizeHeaders(cfg.AllowedHeaders),
		allowedHeaderSet:      make(map[string]struct{}),
		exposedHeaders:        normalizeHeaders(cfg.ExposedHeaders),
		allowCredentials:      cfg.AllowCredentials,
		maxAgeSeconds:         cfg.MaxAgeSeconds,
	}

	firstLiteralOrigin := ""
	for _, origin := range cfg.AllowedOrigins {
		origin = strings.TrimSpace(origin)
		if origin == "" {
			continue
		}
		if origin == "*" {
			policy.allowAllOrigins = true
			continue
		}
		if firstLiteralOrigin == "" {
			firstLiteralOrigin = origin
		}
		policy.allowedOrigins[strings.ToLower(origin)] = struct{}{}
	}

	if !policy.allowAllOrigins && len(policy.allowedOrigins) == 1 && len(policy.allowedOriginPatterns) == 0 {
		policy.pinnedOrigin = firstLiteralOrigin
	}

	for _, method := range policy.allowedMethods {
		if method == "*" {
			policy.allowedMethodSet["*"] = struct{}{}
			continue
		}
		policy.allowedMethodSet[method] = struct{}{}
	}

	for _, header := range policy.allowedHeaders {
		if header == "*" {
			policy.allowedHeadersAll = true
			continue
		}
		policy.allowedHeaderSet[http.CanonicalHeaderKey(header)] = struct{}{}
	}

	return policy
}

func (p corsPolicy) isPreflight(r *http.Request) bool {
	return r.Method == http.MethodOptions &&
		strings.TrimSpace(r.Header.Get("Origin")) != "" &&
		strings.TrimSpace(r.Header.Get("Access-Control-Request-Method")) != ""
}

func (p corsPolicy) handlePreflight(w http.ResponseWriter, r *http.Request) {
	headers := w.Header()
	addVary(headers, "Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers")

	origin := strings.TrimSpace(r.Header.Get("Origin"))
	requestMethod := strings.TrimSpace(r.Header.Get("Access-Control-Request-Method"))
	requestHeaders := parseAccessControlRequestHeaders(r.Header.Get("Access-Control-Request-Headers"))

	if p.isOriginAllowed(origin) && p.isMethodAllowed(requestMethod) && p.areHeadersAllowed(requestHeaders) {
		p.setAllowedOriginHeaders(headers, origin)
		if len(p.allowedMethods) > 0 {
			headers.Set("Access-Control-Allow-Methods", strings.Join(p.allowedMethods, ", "))
		}
		if p.allowedHeadersAll {
			headers.Set("Access-Control-Allow-Headers", strings.Join(requestHeaders, ", "))
		} else if len(p.allowedHeaders) > 0 {
			headers.Set("Access-Control-Allow-Headers", strings.Join(p.allowedHeaders, ", "))
		}
		if p.maxAgeSeconds > 0 {
			headers.Set("Access-Control-Max-Age", strconv.Itoa(p.maxAgeSeconds))
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

func (p corsPolicy) setAllowedOriginHeaders(headers http.Header, origin string) {
	headers.Set("Access-Control-Allow-Origin", origin)
	if p.allowCredentials {
		headers.Set("Access-Control-Allow-Credentials", "true")
	}
}

func (p corsPolicy) isOriginAllowed(origin string) bool {
	if origin == "" {
		return false
	}
	if p.allowAllOrigins {
		return true
	}

	normalized := strings.ToLower(origin)
	if _, ok := p.allowedOrigins[normalized]; ok {
		return true
	}
	for _, pattern := range p.allowedOriginPatterns {
		if wildcardMatch(pattern, normalized) {
			return true
		}
	}
	return false
}

func (p corsPolicy) isMethodAllowed(method string) bool {
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		return false
	}
	if _, ok := p.allowedMethodSet["*"]; ok {
		return true
	}
	_, ok := p.allowedMethodSet[method]
	return ok
}

func (p corsPolicy) areHeadersAllowed(headers []string) bool {
	if p.allowedHeadersAll || len(headers) == 0 {
		return true
	}
	for _, header := range headers {
		if _, ok := p.allowedHeaderSet[http.CanonicalHeaderKey(header)]; !ok {
			return false
		}
	}
	return true
}

func normalizeOriginPatterns(patterns []string) []string {
	normalized := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		if pattern != "" {
			normalized = append(normalized, pattern)
		}
	}
	return normalized
}

func normalizeMethods(methods []string) []string {
	normalized := make([]string, 0, len(methods))
	for _, method := range methods {
		method = strings.ToUpper(strings.TrimSpace(method))
		if method != "" {
			normalized = append(normalized, method)
		}
	}
	return normalized
}

func normalizeHeaders(headers []string) []string {
	normalized := make([]string, 0, len(headers))
	for _, header := range headers {
		header = strings.TrimSpace(header)
		if header != "" {
			normalized = append(normalized, header)
		}
	}
	return normalized
}

func parseAccessControlRequestHeaders(value string) []string {
	parts := strings.Split(value, ",")
	headers := make([]string, 0, len(parts))
	for _, part := range parts {
		header := strings.TrimSpace(part)
		if header != "" {
			headers = append(headers, http.CanonicalHeaderKey(header))
		}
	}
	return headers
}

func addVary(headers http.Header, values ...string) {
	seen := map[string]struct{}{}
	existingValues := headers.Values("Vary")
	ordered := make([]string, 0, len(existingValues)+len(values))

	for _, existing := range existingValues {
		for _, part := range strings.Split(existing, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			key := strings.ToLower(part)
			if _, ok := seen[key]; !ok {
				seen[key] = struct{}{}
				ordered = append(ordered, part)
			}
		}
	}

	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			ordered = append(ordered, value)
		}
	}

	if len(ordered) > 0 {
		headers.Set("Vary", strings.Join(ordered, ", "))
	}
}

func wildcardMatch(pattern, value string) bool {
	if pattern == "*" {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}

	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(value, parts[0]) {
		return false
	}

	position := len(parts[0])
	for _, part := range parts[1 : len(parts)-1] {
		if part == "" {
			continue
		}
		index := strings.Index(value[position:], part)
		if index < 0 {
			return false
		}
		position += index + len(part)
	}

	last := parts[len(parts)-1]
	return last == "" || strings.HasSuffix(value[position:], last)
}
