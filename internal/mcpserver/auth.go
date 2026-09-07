package mcpserver

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

type principalContextKey struct{}

// unauthorizedBody mirrors the REST error envelope so clients (and humans
// probing with curl) see a familiar shape.
type unauthorizedBody struct {
	Success   bool   `json:"success"`
	Error     string `json:"error"`
	ErrorCode string `json:"error_code"`
}

func principalFromContext(ctx context.Context) (*principal, bool) {
	p, ok := ctx.Value(principalContextKey{}).(*principal)
	return p, ok && p != nil
}

// bearerFromRequest extracts the presented credential: an Authorization
// Bearer token (what MCP clients send) or an X-API-Key header, matching the
// dev endpoint convention.
func bearerFromRequest(r *http.Request) string {
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(authHeader) > len("bearer ") && strings.EqualFold(authHeader[:len("bearer ")], "bearer ") {
		return strings.TrimSpace(authHeader[len("bearer "):])
	}
	return strings.TrimSpace(r.Header.Get("X-API-Key"))
}

// lookup resolves a presented secret to a principal. The presented value is
// hashed and compared against every configured digest in constant time, so
// response timing depends on neither which key matched nor the configured
// secrets' lengths.
func (s *Server) lookup(secret string) (*principal, bool) {
	if secret == "" {
		return nil, false
	}
	digest := sha256.Sum256([]byte(secret))
	var match *principal
	for _, p := range s.principals {
		if subtle.ConstantTimeCompare(digest[:], p.digest[:]) == 1 && match == nil {
			match = p
		}
	}
	return match, match != nil
}

// authenticate rejects requests that do not present a configured key and
// attaches the matching principal to the request context otherwise.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.lookup(bearerFromRequest(r))
		if !ok {
			logger.Warn("MCP request rejected: missing or invalid API key",
				zap.String("path", r.URL.Path),
				zap.String("method", r.Method))
			w.Header().Set("WWW-Authenticate", `Bearer realm="blob-indexer-mcp"`)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(unauthorizedBody{
				Success:   false,
				Error:     "Unauthorized: present a configured MCP API key as a Bearer token",
				ErrorCode: "unauthorized",
			})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalContextKey{}, p)))
	})
}
