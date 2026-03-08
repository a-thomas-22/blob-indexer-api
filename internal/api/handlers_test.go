package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db"
	"github.com/a-thomas-22/blob-indexer-api/internal/indexer"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

func init() {
	// Initialize the global logger so handler log calls do not panic.
	logger.Initialize()
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// testConfig returns a minimal config suitable for tests.
func testConfig() *config.Config {
	return &config.Config{
		Server: config.ServerConfig{Port: 8080, DevMode: false},
		Indexer: config.IndexerConfig{
			Version:   "v0.0.0-test",
			BatchSize: 10,
		},
		Networks: []config.NetworkConfig{
			{Name: "mainnet", ChainID: 1, RpcURL: "http://localhost:8545", StartBlock: "0", Enabled: true},
		},
	}
}

// testNetwork returns a default test network configuration.
func testNetwork() config.NetworkConfig {
	return config.NetworkConfig{
		Name:    "mainnet",
		ChainID: 1,
		Enabled: true,
	}
}

// newMockDB creates a *db.DB backed by sqlmock. Callers must defer mockDB.Close().
func newMockDB(t *testing.T) (*db.DB, sqlmock.Sqlmock) {
	t.Helper()
	rawDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	sqlxDB := sqlx.NewDb(rawDB, "sqlmock")
	return &db.DB{DB: sqlxDB}, mock
}

// newTestAPI constructs an API with a single mock-backed indexer for chain ID 1.
func newTestAPI(t *testing.T) (*API, sqlmock.Sqlmock) {
	t.Helper()
	cfg := testConfig()
	mockDB, mock := newMockDB(t)
	network := testNetwork()

	idx := indexer.NewForTest(mockDB, cfg, network, 12345)

	api := &API{
		db:       mockDB,
		indexers: map[int]*indexer.Indexer{1: idx},
		config:   cfg,
	}
	return api, mock
}

// newTestRouter creates an http.Handler via NewRouter with a mock-backed indexer.
func newTestRouter(t *testing.T) (http.Handler, sqlmock.Sqlmock) {
	t.Helper()
	cfg := testConfig()
	mockDB, mock := newMockDB(t)
	network := testNetwork()
	idx := indexer.NewForTest(mockDB, cfg, network, 12345)
	indexers := map[int]*indexer.Indexer{1: idx}
	router := NewRouter(mockDB, indexers, cfg)
	return router, mock
}

// withChiURLParam returns a new request whose context contains a chi route
// context with the specified URL parameter key/value pair.
func withChiURLParam(r *http.Request, key, value string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(key, value)
	ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
	return r.WithContext(ctx)
}

// parseResponse is a test helper that decodes a JSON response body.
func parseResponse(t *testing.T, rec *httptest.ResponseRecorder) Response {
	t.Helper()
	var resp Response
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}
	return resp
}

// blobColumns returns the column names used for blob query results.
func blobColumns() []string {
	return []string{
		"id", "network_id", "block_number", "blob_index", "tx_hash",
		"from_address", "user_attribution", "blob_size_bytes",
		"base_fee_per_blob_gas", "tip_per_blob_gas", "total_cost_eth",
		"timestamp", "confirmed", "indexer_version",
	}
}

// ---------------------------------------------------------------------------
// Route registration tests
// ---------------------------------------------------------------------------

func TestRoutes_Exist(t *testing.T) {
	router, mock := newTestRouter(t)

	routes := []struct {
		method string
		path   string
	}{
		{"GET", "/api/networks/"},
		{"GET", "/api/networks/1"},
		{"GET", "/api/blob/latest"},
		{"GET", "/api/blob/mempool"},
		{"GET", "/api/blob/0xabc"},
		{"GET", "/api/users/"},
		{"GET", "/api/stats/"},
		{"GET", "/api/status"},
		{"GET", "/api/dev/metrics"},
		{"GET", "/api/dev/dashboard"},
		{"GET", "/api/dev/logs"},
		{"GET", "/api/dev/queries"},
	}

	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			// Set up a permissive mock expectation so DB queries do not fail.
			mock.ExpectQuery(".*").WillReturnRows(sqlmock.NewRows([]string{"id"}))

			req := httptest.NewRequest(rt.method, rt.path, nil)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code == http.StatusMethodNotAllowed {
				t.Errorf("route %s %s returned 405; expected a valid handler", rt.method, rt.path)
			}

			// Distinguish chi's built-in 404 (plain text) from an application-level
			// 404 (JSON body from our handler). If the Content-Type is JSON the
			// handler was reached, which means the route is registered.
			if rec.Code == http.StatusNotFound {
				ct := rec.Header().Get("Content-Type")
				if ct != "application/json" {
					t.Errorf("route %s %s returned 404 without JSON body; route may not be registered", rt.method, rt.path)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// GetLatestBlobs
// ---------------------------------------------------------------------------

func TestGetLatestBlobs_InvalidLimit(t *testing.T) {
	api, _ := newTestAPI(t)

	req := httptest.NewRequest("GET", "/api/blob/latest?limit=abc", nil)
	rec := httptest.NewRecorder()
	api.GetLatestBlobs(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rec.Code)
	}
	resp := parseResponse(t, rec)
	if resp.Success {
		t.Error("expected success=false for invalid limit")
	}
	if resp.Error != "Invalid limit parameter" {
		t.Errorf("unexpected error message: %q", resp.Error)
	}
}

func TestGetLatestBlobs_NegativeLimit(t *testing.T) {
	api, _ := newTestAPI(t)

	req := httptest.NewRequest("GET", "/api/blob/latest?limit=-5", nil)
	rec := httptest.NewRecorder()
	api.GetLatestBlobs(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rec.Code)
	}
}

func TestGetLatestBlobs_ZeroLimit(t *testing.T) {
	api, _ := newTestAPI(t)

	req := httptest.NewRequest("GET", "/api/blob/latest?limit=0", nil)
	rec := httptest.NewRecorder()
	api.GetLatestBlobs(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rec.Code)
	}
}

func TestGetLatestBlobs_HappyPath(t *testing.T) {
	api, mock := newTestAPI(t)

	now := time.Now()
	rows := sqlmock.NewRows(blobColumns()).AddRow(
		1, 1, int64(100), 0, "0xabc",
		"0xsender", "test-user", int64(131072),
		"1000000000", "100000000", "0.001",
		now, true, "v1.0.0",
	)

	mock.ExpectQuery("SELECT \\* FROM blobs").
		WithArgs(1, 10).
		WillReturnRows(rows)

	req := httptest.NewRequest("GET", "/api/blob/latest", nil)
	rec := httptest.NewRecorder()
	api.GetLatestBlobs(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	resp := parseResponse(t, rec)
	if !resp.Success {
		t.Errorf("expected success=true, got false; error: %s", resp.Error)
	}
	if resp.Data == nil {
		t.Error("expected non-nil data")
	}
}

func TestGetLatestBlobs_LimitClamped(t *testing.T) {
	api, mock := newTestAPI(t)

	// Requesting limit=200 should be clamped to MaxQueryLimit (100).
	rows := sqlmock.NewRows(blobColumns())
	mock.ExpectQuery("SELECT \\* FROM blobs").
		WithArgs(1, MaxQueryLimit).
		WillReturnRows(rows)

	req := httptest.NewRequest("GET", "/api/blob/latest?limit=200", nil)
	rec := httptest.NewRecorder()
	api.GetLatestBlobs(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// GetMempoolBlobs
// ---------------------------------------------------------------------------

func TestGetMempoolBlobs_HappyPath(t *testing.T) {
	api, mock := newTestAPI(t)

	rows := sqlmock.NewRows(blobColumns())
	mock.ExpectQuery("SELECT \\* FROM blobs").
		WithArgs(1, 10).
		WillReturnRows(rows)

	req := httptest.NewRequest("GET", "/api/blob/mempool", nil)
	rec := httptest.NewRecorder()
	api.GetMempoolBlobs(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	resp := parseResponse(t, rec)
	if !resp.Success {
		t.Errorf("expected success=true")
	}
}

func TestGetMempoolBlobs_InvalidLimit(t *testing.T) {
	api, _ := newTestAPI(t)

	req := httptest.NewRequest("GET", "/api/blob/mempool?limit=xyz", nil)
	rec := httptest.NewRecorder()
	api.GetMempoolBlobs(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// GetBlobByTxHash
// ---------------------------------------------------------------------------

func TestGetBlobByTxHash_HappyPath(t *testing.T) {
	api, mock := newTestAPI(t)

	now := time.Now()
	rows := sqlmock.NewRows(blobColumns()).AddRow(
		1, 1, int64(100), 0, "0xdeadbeef",
		"0xsender", "test-user", int64(131072),
		"1000000000", "100000000", "0.001",
		now, true, "v1.0.0",
	)

	mock.ExpectQuery("SELECT \\* FROM blobs WHERE tx_hash").
		WithArgs("0xdeadbeef", 1).
		WillReturnRows(rows)

	req := httptest.NewRequest("GET", "/api/blob/0xdeadbeef", nil)
	req = withChiURLParam(req, "txHash", "0xdeadbeef")

	rec := httptest.NewRecorder()
	api.GetBlobByTxHash(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	resp := parseResponse(t, rec)
	if !resp.Success {
		t.Errorf("expected success=true")
	}
}

func TestGetBlobByTxHash_NotFound(t *testing.T) {
	api, mock := newTestAPI(t)

	// Return empty rows to trigger sql.ErrNoRows in GetContext.
	mock.ExpectQuery("SELECT \\* FROM blobs WHERE tx_hash").
		WithArgs("0xnotfound", 1).
		WillReturnRows(sqlmock.NewRows(blobColumns()))

	req := httptest.NewRequest("GET", "/api/blob/0xnotfound", nil)
	req = withChiURLParam(req, "txHash", "0xnotfound")

	rec := httptest.NewRecorder()
	api.GetBlobByTxHash(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", rec.Code)
	}
}

func TestGetBlobByTxHash_MissingTxHash(t *testing.T) {
	api, _ := newTestAPI(t)

	// No chi route context -> txHash URL param will be empty.
	req := httptest.NewRequest("GET", "/api/blob/", nil)
	rec := httptest.NewRecorder()
	api.GetBlobByTxHash(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// GetTopBlobUsers
// ---------------------------------------------------------------------------

func TestGetTopBlobUsers_HappyPath(t *testing.T) {
	api, mock := newTestAPI(t)

	now := time.Now()
	rows := sqlmock.NewRows([]string{
		"from_address", "user_attribution", "blob_count", "total_cost_eth", "last_timestamp",
	}).AddRow("0xuser1", "User One", 50, "1.5", now)

	mock.ExpectQuery("SELECT").
		WithArgs(1, 10).
		WillReturnRows(rows)

	req := httptest.NewRequest("GET", "/api/users/", nil)
	rec := httptest.NewRecorder()
	api.GetTopBlobUsers(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	resp := parseResponse(t, rec)
	if !resp.Success {
		t.Errorf("expected success=true")
	}
}

func TestGetTopBlobUsers_InvalidLimit(t *testing.T) {
	api, _ := newTestAPI(t)

	req := httptest.NewRequest("GET", "/api/users/?limit=bad", nil)
	rec := httptest.NewRecorder()
	api.GetTopBlobUsers(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// GetBlobStats
// ---------------------------------------------------------------------------

func TestGetBlobStats_HappyPath(t *testing.T) {
	api, mock := newTestAPI(t)

	now := time.Now()
	rows := sqlmock.NewRows([]string{
		"total_blobs", "total_confirmed_blobs", "total_pending_blobs",
		"average_base_fee", "average_tip", "average_total_cost", "last_indexed_time",
	}).AddRow(100, 90, 10, "1000000000", "100000000", "0.01", now)

	mock.ExpectQuery("SELECT").
		WithArgs(1).
		WillReturnRows(rows)

	req := httptest.NewRequest("GET", "/api/stats/", nil)
	rec := httptest.NewRecorder()
	api.GetBlobStats(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	resp := parseResponse(t, rec)
	if !resp.Success {
		t.Errorf("expected success=true")
	}
}

// ---------------------------------------------------------------------------
// GetIndexerStatus
// ---------------------------------------------------------------------------

func TestGetIndexerStatus_HappyPath(t *testing.T) {
	api, mock := newTestAPI(t)

	now := time.Now()
	rows := sqlmock.NewRows([]string{"max"}).AddRow(now)

	mock.ExpectQuery("SELECT MAX").
		WithArgs(1).
		WillReturnRows(rows)

	req := httptest.NewRequest("GET", "/api/status", nil)
	rec := httptest.NewRecorder()
	api.GetIndexerStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	resp := parseResponse(t, rec)
	if !resp.Success {
		t.Errorf("expected success=true")
	}
}

// ---------------------------------------------------------------------------
// GetNetworks
// ---------------------------------------------------------------------------

func TestGetNetworks_HappyPath(t *testing.T) {
	api, _ := newTestAPI(t)

	req := httptest.NewRequest("GET", "/api/networks/", nil)
	rec := httptest.NewRecorder()
	api.GetNetworks(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	resp := parseResponse(t, rec)
	if !resp.Success {
		t.Errorf("expected success=true")
	}
	if resp.Data == nil {
		t.Error("expected non-nil data for networks")
	}
}

func TestGetNetworks_EmptyIndexers(t *testing.T) {
	cfg := testConfig()
	mockDB, _ := newMockDB(t)

	api := &API{
		db:       mockDB,
		indexers: map[int]*indexer.Indexer{},
		config:   cfg,
	}

	req := httptest.NewRequest("GET", "/api/networks/", nil)
	rec := httptest.NewRecorder()
	api.GetNetworks(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	resp := parseResponse(t, rec)
	if !resp.Success {
		t.Errorf("expected success=true even with no networks")
	}
}

// ---------------------------------------------------------------------------
// GetNetworkStatus
// ---------------------------------------------------------------------------

func TestGetNetworkStatus_HappyPath(t *testing.T) {
	api, _ := newTestAPI(t)

	req := httptest.NewRequest("GET", "/api/networks/1", nil)
	req = withChiURLParam(req, "chainId", "1")

	rec := httptest.NewRecorder()
	api.GetNetworkStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	resp := parseResponse(t, rec)
	if !resp.Success {
		t.Errorf("expected success=true")
	}
}

func TestGetNetworkStatus_InvalidChainID(t *testing.T) {
	api, _ := newTestAPI(t)

	req := httptest.NewRequest("GET", "/api/networks/abc", nil)
	req = withChiURLParam(req, "chainId", "abc")

	rec := httptest.NewRecorder()
	api.GetNetworkStatus(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rec.Code)
	}
	resp := parseResponse(t, rec)
	if resp.Error != "Invalid chain ID" {
		t.Errorf("unexpected error: %q", resp.Error)
	}
}

func TestGetNetworkStatus_NotFound(t *testing.T) {
	api, _ := newTestAPI(t)

	req := httptest.NewRequest("GET", "/api/networks/999", nil)
	req = withChiURLParam(req, "chainId", "999")

	rec := httptest.NewRecorder()
	api.GetNetworkStatus(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// getNetworkFromRequest (internal helper)
// ---------------------------------------------------------------------------

func TestGetNetworkFromRequest_DefaultNetwork(t *testing.T) {
	api, _ := newTestAPI(t)

	req := httptest.NewRequest("GET", "/", nil)
	idx, err := api.getNetworkFromRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	info := idx.GetNetworkInfo()
	if info.ChainID != 1 {
		t.Errorf("expected chain ID 1, got %d", info.ChainID)
	}
}

func TestGetNetworkFromRequest_ByChainID(t *testing.T) {
	api, _ := newTestAPI(t)

	req := httptest.NewRequest("GET", "/?network=1", nil)
	idx, err := api.getNetworkFromRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if idx.GetNetworkInfo().ChainID != 1 {
		t.Error("expected chain ID 1")
	}
}

func TestGetNetworkFromRequest_ByName(t *testing.T) {
	api, _ := newTestAPI(t)

	req := httptest.NewRequest("GET", "/?network=mainnet", nil)
	idx, err := api.getNetworkFromRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if idx.GetNetworkInfo().Name != "mainnet" {
		t.Errorf("expected network name mainnet, got %s", idx.GetNetworkInfo().Name)
	}
}

func TestGetNetworkFromRequest_NotFound(t *testing.T) {
	api, _ := newTestAPI(t)

	req := httptest.NewRequest("GET", "/?network=unknown", nil)
	_, err := api.getNetworkFromRequest(req)
	if err == nil {
		t.Error("expected error for unknown network")
	}
}

func TestGetNetworkFromRequest_NoIndexers(t *testing.T) {
	cfg := testConfig()
	mockDB, _ := newMockDB(t)
	api := &API{
		db:       mockDB,
		indexers: map[int]*indexer.Indexer{},
		config:   cfg,
	}

	req := httptest.NewRequest("GET", "/", nil)
	_, err := api.getNetworkFromRequest(req)
	if err == nil {
		t.Error("expected error when no indexers available")
	}
}

// ---------------------------------------------------------------------------
// respondJSON / respondError helpers
// ---------------------------------------------------------------------------

func TestRespondJSON_SetsContentType(t *testing.T) {
	api, _ := newTestAPI(t)
	rec := httptest.NewRecorder()
	api.respondJSON(rec, http.StatusOK, Response{Success: true, Data: "hello"})

	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %s", ct)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestRespondError_ReturnsCorrectStatus(t *testing.T) {
	api, _ := newTestAPI(t)
	rec := httptest.NewRecorder()
	api.respondError(rec, http.StatusTeapot, "I am a teapot")

	if rec.Code != http.StatusTeapot {
		t.Errorf("expected 418, got %d", rec.Code)
	}
	resp := parseResponse(t, rec)
	if resp.Success {
		t.Error("expected success=false")
	}
	if resp.Error != "I am a teapot" {
		t.Errorf("unexpected error message: %q", resp.Error)
	}
}

// ---------------------------------------------------------------------------
// End-to-end route tests via the full chi router
// ---------------------------------------------------------------------------

func TestRouterGetLatestBlobs_InvalidLimit(t *testing.T) {
	router, _ := newTestRouter(t)

	req := httptest.NewRequest("GET", "/api/blob/latest?limit=bad", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 via router, got %d", rec.Code)
	}
}

func TestRouterGetNetworkStatus_InvalidChainID(t *testing.T) {
	router, _ := newTestRouter(t)

	req := httptest.NewRequest("GET", "/api/networks/notanumber", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 via router, got %d", rec.Code)
	}
}

func TestRouterGetNetworkStatus_NotFound(t *testing.T) {
	router, _ := newTestRouter(t)

	req := httptest.NewRequest("GET", "/api/networks/9999", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 via router, got %d", rec.Code)
	}
}

func TestRouterGetNetworks_HappyPath(t *testing.T) {
	router, _ := newTestRouter(t)

	req := httptest.NewRequest("GET", "/api/networks/", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 via router, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestRouterGetNetworkStatus_HappyPath(t *testing.T) {
	router, _ := newTestRouter(t)

	req := httptest.NewRequest("GET", "/api/networks/1", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 via router, got %d; body: %s", rec.Code, rec.Body.String())
	}
}
