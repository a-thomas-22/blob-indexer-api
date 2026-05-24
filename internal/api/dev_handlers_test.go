package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	_ "github.com/a-thomas-22/blob-indexer-api/internal/testutil"
)

func TestDevMetrics(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/api/dev/metrics", http.NoBody)
	w := httptest.NewRecorder()
	a.DevMetrics(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestDevLogs_Default(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/api/dev/logs", http.NoBody)
	w := httptest.NewRecorder()
	a.DevLogs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestDevLogs_FilterByLevel(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/api/dev/logs?level=error", http.NoBody)
	w := httptest.NewRecorder()
	a.DevLogs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestDevLogs_InvalidLimit(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/api/dev/logs?limit=abc", http.NoBody)
	w := httptest.NewRecorder()
	a.DevLogs(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestDevLogs_InvalidLevel(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/api/dev/logs?level=critical", http.NoBody)
	w := httptest.NewRecorder()
	a.DevLogs(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestDevQueries_Default(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/api/dev/queries", http.NoBody)
	w := httptest.NewRecorder()
	a.DevQueries(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestDevQueries_InvalidLimit(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/api/dev/queries?limit=0", http.NoBody)
	w := httptest.NewRecorder()
	a.DevQueries(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestDevDashboard(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/api/dev/dashboard", http.NoBody)
	w := httptest.NewRecorder()
	a.DevDashboard(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestDevIndexers_Success(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.DevIndexers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestDevDatabase_Success(t *testing.T) {
	callCount := 0
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			callCount++
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.DevDatabase(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestDevDatabase_DBError(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return fmt.Errorf("db error")
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.DevDatabase(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (with empty stats), got %d", w.Code)
	}
}

func TestDevLogs_CustomLimit(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?limit=50", http.NoBody)
	w := httptest.NewRecorder()
	a.DevLogs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestDevQueries_CustomLimit(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?limit=50", http.NoBody)
	w := httptest.NewRecorder()
	a.DevQueries(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestDevQueries_LimitTruncation(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?limit=2", http.NoBody)
	w := httptest.NewRecorder()
	a.DevQueries(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp Response
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Success {
		t.Error("expected success=true")
	}
}

func TestDevQueries_ExcessiveLimit(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?limit=5000", http.NoBody)
	w := httptest.NewRecorder()
	a.DevQueries(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestDevLogs_ExcessiveLimit(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?limit=5000", http.NoBody)
	w := httptest.NewRecorder()
	a.DevLogs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestDevDatabase_PartialErrors(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if strings.Contains(query, "pg_total_relation_size") ||
				strings.Contains(query, "pg_indexes") ||
				strings.Contains(query, "pg_database_size") {
				return fmt.Errorf("permission denied")
			}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.DevDatabase(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (with fallback values), got %d", w.Code)
	}
}

func TestDevIndexers_DBTimestampError(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if strings.Contains(query, "MAX(timestamp)") || strings.Contains(query, "COALESCE") {
				return fmt.Errorf("db error")
			}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.DevIndexers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (with fallback timestamp), got %d", w.Code)
	}
}

func TestDevLogs_SmallLimit(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?limit=2", http.NoBody)
	w := httptest.NewRecorder()
	a.DevLogs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestDevLogs_NegativeLimit(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?limit=-5", http.NoBody)
	w := httptest.NewRecorder()
	a.DevLogs(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestDevModeMiddleware_Disabled(t *testing.T) {
	handler := DevModeMiddleware(false)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when dev mode disabled, got %d", w.Code)
	}
}

func TestDevModeMiddleware_Enabled(t *testing.T) {
	handler := DevModeMiddleware(true)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 when dev mode enabled, got %d", w.Code)
	}
}

func TestDevAPIKeyMiddleware_KeyMissing(t *testing.T) {
	handler := DevAPIKeyMiddleware("")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 when dev API key is not configured (skip auth), got %d", w.Code)
	}
}

func TestDevAPIKeyMiddleware_RejectsInvalidKey(t *testing.T) {
	handler := DevAPIKeyMiddleware("secret")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.Header.Set("X-API-Key", "wrong")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for invalid API key, got %d", w.Code)
	}
}

func TestDevAPIKeyMiddleware_AllowsBearerToken(t *testing.T) {
	handler := DevAPIKeyMiddleware("secret")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for valid bearer token, got %d", w.Code)
	}
}
