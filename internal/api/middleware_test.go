package api

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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

func TestMaxBytesMiddleware_RejectsLargeBody(t *testing.T) {
	handler := MaxBytesMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err != nil {
			RespondMaxBytesError(w)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	tooLarge := bytes.Repeat([]byte("a"), MaxRequestBodySize+1)
	req := httptest.NewRequest(http.MethodPost, "/test", bytes.NewReader(tooLarge))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for oversized body, got %d", w.Code)
	}
}

func TestRequestCounterMiddleware_TracksCounts(t *testing.T) {
	a := &API{}
	handler := a.requestCounterMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := atomic.LoadInt64(&a.activeRequests); got != 1 {
			t.Fatalf("expected activeRequests=1 during request, got %d", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/status", http.NoBody)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if got := atomic.LoadInt64(&a.totalRequests); got != 1 {
		t.Fatalf("expected totalRequests=1, got %d", got)
	}
	if got := atomic.LoadInt64(&a.activeRequests); got != 0 {
		t.Fatalf("expected activeRequests=0 after request, got %d", got)
	}
}

func TestSecurityHeadersMiddleware(t *testing.T) {
	handler := SecurityHeadersMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/status", http.NoBody)
	req.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("expected nosniff header, got %q", got)
	}
	if got := w.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("expected DENY frame header, got %q", got)
	}
	if got := w.Header().Get("Strict-Transport-Security"); got == "" {
		t.Fatal("expected strict transport security header on https")
	}
}
