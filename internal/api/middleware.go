package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

// DevModeMiddleware returns a middleware that gates access behind dev mode.
// When dev mode is disabled, all requests to the protected routes receive a
// 404 Not Found response so that the existence of dev endpoints is not leaked.
func DevModeMiddleware(devMode bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !devMode {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				msg := "Not found"
				if err := json.NewEncoder(w).Encode(Response{
					Success:   false,
					Error:     msg,
					ErrorCode: errorCodeFor(http.StatusNotFound, msg),
				}); err != nil {
					logger.Warn("failed to encode dev mode not-found response", zap.Error(err))
				}
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// DevAPIKeyMiddleware protects dev endpoints with a static API key.
// If no key is configured, requests pass through without auth.
func DevAPIKeyMiddleware(requiredAPIKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.TrimSpace(requiredAPIKey) == "" {
				next.ServeHTTP(w, r)
				return
			}

			provided := strings.TrimSpace(r.Header.Get("X-API-Key"))
			if provided == "" {
				authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
				if strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
					provided = strings.TrimSpace(authHeader[len("Bearer "):])
				}
			}

			if subtle.ConstantTimeCompare([]byte(provided), []byte(requiredAPIKey)) != 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				msg := "Unauthorized"
				if err := json.NewEncoder(w).Encode(Response{
					Success:   false,
					Error:     msg,
					ErrorCode: errorCodeFor(http.StatusUnauthorized, msg),
				}); err != nil {
					logger.Warn("failed to encode unauthorized response", zap.Error(err))
				}
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// MaxRequestBodySize is the maximum allowed request body size (1MB).
const MaxRequestBodySize = 1 << 20 // 1 MB

// MaxBytesMiddleware limits the size of incoming request bodies.
// If the body exceeds the limit, the request is rejected with
// 413 Request Entity Too Large.
func MaxBytesMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBodySize)
		next.ServeHTTP(w, r)
	})
}

// RespondMaxBytesError writes a 413 JSON error response. Handlers that
// decode the request body can call this when they detect the body was
// too large.
func RespondMaxBytesError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusRequestEntityTooLarge)
	msg := "Request body too large (limit: 1MB)"
	if err := json.NewEncoder(w).Encode(Response{
		Success:   false,
		Error:     msg,
		ErrorCode: errorCodeFor(http.StatusRequestEntityTooLarge, msg),
	}); err != nil {
		logger.Warn("failed to encode max-bytes error response", zap.Error(err))
	}
}

// LoggerMiddleware logs HTTP requests with details
func LoggerMiddleware(next http.Handler) http.Handler {
	return LoggerMiddlewareWithResolver(newClientIPResolver(nil))(next)
}

// LoggerMiddlewareWithResolver logs HTTP requests using the provided client IP resolver.
func LoggerMiddlewareWithResolver(resolver clientIPResolver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Create a request ID
			requestID := uuid.New().String()
			ctx := context.WithValue(r.Context(), logger.RequestIDKey, requestID)
			r = r.WithContext(ctx)

			// Create a response wrapper to capture status code
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			// Process request
			next.ServeHTTP(ww, r)

			// Log request details
			logger.Info("HTTP request",
				zap.String("method", r.Method),
				zap.String("path", r.URL.Path),
				zap.Int("status", ww.Status()),
				zap.Duration("duration", time.Since(start)),
				zap.String("request_id", requestID),
				zap.String("client_ip", resolver.IP(r)),
				zap.String("remote_addr", r.RemoteAddr),
				zap.String("user_agent", r.UserAgent()),
			)
		})
	}
}

// ContentTypeJSON is a middleware that validates the Content-Type header
// for requests that may carry a body (POST, PUT, PATCH). It requires
// the media type to be "application/json" when a Content-Type header is
// present. Requests with no Content-Type header or an empty body
// (Content-Length 0) are allowed through so that endpoints which do not
// require a body are not unnecessarily rejected. GET, DELETE, OPTIONS,
// and HEAD requests are never checked.
func ContentTypeJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only enforce on methods that typically carry a request body.
		if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch {
			ct := r.Header.Get("Content-Type")

			// Allow requests with no body (Content-Length 0 or missing Content-Type).
			if ct != "" && r.ContentLength != 0 {
				mediaType, _, err := mime.ParseMediaType(ct)
				if err != nil || mediaType != "application/json" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusUnsupportedMediaType)
					msg := "Content-Type must be application/json"
					if err := json.NewEncoder(w).Encode(Response{
						Success:   false,
						Error:     msg,
						ErrorCode: errorCodeFor(http.StatusUnsupportedMediaType, msg),
					}); err != nil {
						logger.Error("Failed to encode unsupported media type response", zap.Error(err))
					}
					return
				}
			}
		}

		next.ServeHTTP(w, r)
	})
}

// SecurityHeadersMiddleware adds basic hardening headers to all responses.
func SecurityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}
