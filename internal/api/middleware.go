package api

import (
	"context"
	"encoding/json"
	"mime"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

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
					if err := json.NewEncoder(w).Encode(Response{
						Success: false,
						Error:   "Content-Type must be application/json",
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
