package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
	_ "github.com/a-thomas-22/blob-indexer-api/internal/testutil"
)

func TestLoggerMiddleware_SetsRequestID(t *testing.T) {
	var capturedID string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := r.Context().Value(logger.RequestIDKey).(string)
		if !ok || id == "" {
			t.Error("expected requestID in context")
		}
		capturedID = id
		w.WriteHeader(http.StatusOK)
	})

	handler := LoggerMiddleware(inner)
	req := httptest.NewRequest(http.MethodGet, "/test", http.NoBody)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if capturedID == "" {
		t.Error("requestID was not set")
	}
}

func TestLoggerMiddleware_PassesThrough(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	handler := LoggerMiddleware(inner)
	req := httptest.NewRequest(http.MethodGet, "/test", http.NoBody)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusTeapot {
		t.Errorf("expected 418, got %d", w.Code)
	}
}
