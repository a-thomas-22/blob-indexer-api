package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestMetricsHandler_ExposesOperationalMetrics(t *testing.T) {
	a := newTestAPI()
	atomic.AddInt64(&a.totalRequests, 3)
	atomic.AddInt64(&a.activeRequests, 1)
	incRateLimitRejections()

	r := chi.NewRouter()
	r.Handle("/metrics", a.metricsHandler())

	req := httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"blob_indexer_http_requests_total",
		"blob_indexer_http_active_requests",
		"blob_indexer_websocket_connections",
		"blob_indexer_rate_limit_rejections_total",
		`blob_indexer_db_connections{state="open"}`,
		`blob_indexer_db_connections{state="max_open"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
}

func TestIncRateLimitRejections_Increments(t *testing.T) {
	before := atomic.LoadInt64(&rateLimitRejections)
	incRateLimitRejections()
	if got := atomic.LoadInt64(&rateLimitRejections); got != before+1 {
		t.Fatalf("expected rejections to increase by 1, got %d -> %d", before, got)
	}
}
