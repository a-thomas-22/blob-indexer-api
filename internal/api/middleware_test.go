package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

func init() {
	// Initialize logger for tests
	logger.Initialize()
}

func TestLoggerMiddleware_SetsRequestID(t *testing.T) {
	var capturedRequestID string
	handler := LoggerMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Capture the request ID from context
		if rid, ok := r.Context().Value(logger.RequestIDKey).(string); ok {
			capturedRequestID = rid
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", http.NoBody)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if capturedRequestID == "" {
		t.Error("expected request ID to be set in context")
	}
}

func TestLoggerMiddleware_UniqueRequestIDs(t *testing.T) {
	var ids []string
	handler := LoggerMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rid, ok := r.Context().Value(logger.RequestIDKey).(string); ok {
			ids = append(ids, rid)
		}
		w.WriteHeader(http.StatusOK)
	}))

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/test", http.NoBody)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
	}

	if len(ids) != 3 {
		t.Fatalf("expected 3 request IDs, got %d", len(ids))
	}

	// All IDs should be unique
	seen := make(map[string]bool)
	for _, id := range ids {
		if seen[id] {
			t.Errorf("duplicate request ID: %s", id)
		}
		seen[id] = true
	}
}

func TestLoggerMiddleware_PassesResponseStatusCode(t *testing.T) {
	handler := LoggerMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	req := httptest.NewRequest(http.MethodGet, "/missing", http.NoBody)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", rr.Code)
	}
}

func TestLoggerMiddleware_PassesResponseBody(t *testing.T) {
	handler := LoggerMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello"))
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", http.NoBody)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Body.String() != "hello" {
		t.Errorf("expected body 'hello', got '%s'", rr.Body.String())
	}
}
