package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func decodeHealth(t *testing.T, body []byte) HealthResponse {
	t.Helper()
	var resp HealthResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	return resp
}

func TestHealthz_AlwaysOKWithoutDB(t *testing.T) {
	// A nil getFn would still pass; use a getFn that fails to prove healthz
	// never touches the database.
	db := &mockDB{getFn: func(context.Context, interface{}, string, ...interface{}) error {
		t.Fatalf("healthz must not query the database")
		return nil
	}}
	a := newTestAPIWithDB(db)

	rr := httptest.NewRecorder()
	a.Healthz(rr, httptest.NewRequest(http.MethodGet, "/api/v1/healthz", http.NoBody))

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if got := decodeHealth(t, rr.Body.Bytes()).Status; got != "ok" {
		t.Fatalf("expected status ok, got %q", got)
	}
}

func TestReadyz_DatabaseUp(t *testing.T) {
	db := &mockDB{getFn: func(context.Context, interface{}, string, ...interface{}) error {
		return nil
	}}
	a := newTestAPIWithDB(db)

	rr := httptest.NewRecorder()
	a.Readyz(rr, httptest.NewRequest(http.MethodGet, "/api/v1/readyz", http.NoBody))

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if got := decodeHealth(t, rr.Body.Bytes()).Status; got != "ok" {
		t.Fatalf("expected status ok, got %q", got)
	}
}

func TestReadyz_DatabaseDown(t *testing.T) {
	db := &mockDB{getFn: func(context.Context, interface{}, string, ...interface{}) error {
		return errors.New("connection refused")
	}}
	a := newTestAPIWithDB(db)

	rr := httptest.NewRecorder()
	a.Readyz(rr, httptest.NewRequest(http.MethodGet, "/api/v1/readyz", http.NoBody))

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rr.Code)
	}
	if got := decodeHealth(t, rr.Body.Bytes()).Status; got != "unavailable" {
		t.Fatalf("expected status unavailable, got %q", got)
	}
}
