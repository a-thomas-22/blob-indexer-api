package api

import (
	"context"
	"encoding/json"
	"net/http"
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
				json.NewEncoder(w).Encode(Response{
					Success: false,
					Error:   "Not found",
				})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// LoggerMiddleware logs HTTP requests with details
func LoggerMiddleware(next http.Handler) http.Handler {
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
			zap.String("remote_addr", r.RemoteAddr),
			zap.String("user_agent", r.UserAgent()),
		)
	})
}
