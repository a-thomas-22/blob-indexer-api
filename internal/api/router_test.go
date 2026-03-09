package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	_ "github.com/a-thomas-22/blob-indexer-api/internal/testutil"
)

const validTestTxHash = "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// mockDB implements DBProvider for testing.
type mockDB struct {
	selectFn func(ctx context.Context, dest interface{}, query string, args ...interface{}) error
	getFn    func(ctx context.Context, dest interface{}, query string, args ...interface{}) error
	execFn   func(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
}

func (m *mockDB) SelectContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
	if m.selectFn != nil {
		return m.selectFn(ctx, dest, query, args...)
	}
	return nil
}

func (m *mockDB) GetContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
	if m.getFn != nil {
		return m.getFn(ctx, dest, query, args...)
	}
	return nil
}

func (m *mockDB) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	if m.execFn != nil {
		return m.execFn(ctx, query, args...)
	}
	return nil, nil
}

func (m *mockDB) Stats() sql.DBStats {
	return sql.DBStats{
		MaxOpenConnections: 25,
		OpenConnections:    5,
		InUse:              2,
		Idle:               3,
	}
}

func newTestAPI() *API {
	return newTestAPIWithDB(nil)
}

func newTestAPIWithDB(db DBProvider) *API {
	if db == nil {
		db = &mockDB{}
	}
	return &API{
		db: db,
		networks: map[int]config.NetworkConfig{
			42: {Name: "testnet", ChainID: 42, Enabled: true},
		},
		config: &config.Config{
			Server:  config.ServerConfig{Port: 8080, DevMode: true},
			Indexer: config.IndexerConfig{Version: "test-v1"},
		},
		startTime: time.Now(),
	}
}

// setSliceResult is a helper to assign mock data to a dest pointer via reflection.
func setSliceResult(dest, src interface{}) {
	dv := reflect.ValueOf(dest)
	if dv.Kind() == reflect.Ptr {
		dv = dv.Elem()
	}
	sv := reflect.ValueOf(src)
	dv.Set(sv)
}

// setStructResult copies a struct value into the dest pointer.
func setStructResult(dest, src interface{}) {
	dv := reflect.ValueOf(dest)
	if dv.Kind() == reflect.Ptr {
		dv = dv.Elem()
	}
	sv := reflect.ValueOf(src)
	if sv.Kind() == reflect.Ptr {
		sv = sv.Elem()
	}
	dv.Set(sv)
}

// suppress unused warnings
var _ = fmt.Sprintf

// --- getNetworkFromRequest tests ---

func TestGetNetworkFromRequest_ByChainID(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?network=42", http.NoBody)
	network, err := a.getNetworkFromRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if network.ChainID != 42 {
		t.Errorf("expected chain ID 42, got %d", network.ChainID)
	}
}

func TestGetNetworkFromRequest_ByName(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?network=testnet", http.NoBody)
	network, err := a.getNetworkFromRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if network.Name != "testnet" {
		t.Errorf("expected 'testnet', got %q", network.Name)
	}
}

func TestGetNetworkFromRequest_DefaultFirstNetwork(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	network, err := a.getNetworkFromRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if network.ChainID == 0 {
		t.Fatal("expected non-zero chain ID")
	}
}

func TestGetNetworkFromRequest_NoNetworks(t *testing.T) {
	a := &API{networks: map[int]config.NetworkConfig{}}
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	_, err := a.getNetworkFromRequest(req)
	if !errors.Is(err, ErrNoNetworksAvailable) {
		t.Errorf("expected ErrNoNetworksAvailable, got %v", err)
	}
}

func TestGetNetworkFromRequest_NotFound(t *testing.T) {
	a := &API{networks: map[int]config.NetworkConfig{}}
	req := httptest.NewRequest(http.MethodGet, "/?network=999", http.NoBody)
	_, err := a.getNetworkFromRequest(req)
	if !errors.Is(err, ErrNetworkNotFound) {
		t.Errorf("expected ErrNetworkNotFound, got %v", err)
	}
}

// --- GetNetworks ---

func TestGetNetworks(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/api/networks", http.NoBody)
	w := httptest.NewRecorder()
	a.GetNetworks(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp Response
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Success {
		t.Error("expected Success=true")
	}
}

// --- GetNetworkStatus ---

func TestGetNetworkStatus_Valid(t *testing.T) {
	a := newTestAPI()

	r := chi.NewRouter()
	r.Get("/api/networks/{chainId}", a.GetNetworkStatus)

	req := httptest.NewRequest(http.MethodGet, "/api/networks/42", http.NoBody)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetNetworkStatus_InvalidChainID(t *testing.T) {
	a := newTestAPI()

	r := chi.NewRouter()
	r.Get("/api/networks/{chainId}", a.GetNetworkStatus)

	req := httptest.NewRequest(http.MethodGet, "/api/networks/abc", http.NoBody)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetNetworkStatus_NotFound(t *testing.T) {
	a := newTestAPI()

	r := chi.NewRouter()
	r.Get("/api/networks/{chainId}", a.GetNetworkStatus)

	req := httptest.NewRequest(http.MethodGet, "/api/networks/999", http.NoBody)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// --- GetTopBlobUsers ---

func TestGetTopBlobUsers_Success(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			users := dest.(*[]models.BlobUserStats)
			*users = []models.BlobUserStats{
				{Address: "0xabc", Name: "Alice", BlobCount: 10, TotalCostETH: "1.5", LastTimestamp: time.Now()},
			}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?limit=5", http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetTopBlobUsers_InvalidLimit(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?limit=-1", http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetTopBlobUsers_BadNetwork(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?network=missing", http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// --- DevMetrics ---

func TestDevMetrics(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/api/dev/metrics", http.NoBody)
	w := httptest.NewRecorder()
	a.DevMetrics(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// --- DevLogs ---

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

// --- DevQueries ---

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

// --- DevDashboard ---

func TestDevDashboard(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/api/dev/dashboard", http.NoBody)
	w := httptest.NewRecorder()
	a.DevDashboard(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// --- NewRouter ---

func TestNewRouter_ReturnsHandler(t *testing.T) {
	cfg := &config.Config{
		Server:  config.ServerConfig{Port: 8080, DevMode: true},
		Indexer: config.IndexerConfig{Version: "test"},
	}
	handler := NewRouter(nil, cfg)
	if handler == nil {
		t.Fatal("expected non-nil handler")
	}
}

func TestNewRouter_DevModeDisabled(t *testing.T) {
	cfg := &config.Config{
		Server:  config.ServerConfig{Port: 8080, DevMode: false},
		Indexer: config.IndexerConfig{Version: "test"},
	}
	handler := NewRouter(nil, cfg)
	if handler == nil {
		t.Fatal("expected non-nil handler")
	}
}

// --- DB-dependent handler tests ---

func TestGetLatestBlobs_Success(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			blobs := dest.(*[]models.Blob)
			*blobs = []models.Blob{
				{
					NetworkID:         42,
					BlockNumber:       100,
					BlobIndex:         0,
					TxHash:            "0xabc",
					FromAddress:       "0x123",
					BlobSizeBytes:     131072,
					BaseFeePerBlobGas: "1000",
					TipPerBlobGas:     "100",
					TotalCostETH:      "0.001",
					Timestamp:         time.Now(),
					Confirmed:         true,
				},
			}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetLatestBlobs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp Response
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Success {
		t.Error("expected Success=true")
	}
}

func TestGetLatestBlobs_InvalidLimit(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?limit=abc", http.NoBody)
	w := httptest.NewRecorder()
	a.GetLatestBlobs(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetLatestBlobs_NegativeLimit(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?limit=-5", http.NoBody)
	w := httptest.NewRecorder()
	a.GetLatestBlobs(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetLatestBlobs_DBError(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return fmt.Errorf("db error")
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetLatestBlobs(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestGetLatestBlobs_BadNetwork(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?network=unknown", http.NoBody)
	w := httptest.NewRecorder()
	a.GetLatestBlobs(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetMempoolBlobs_Success(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			blobs := dest.(*[]models.Blob)
			*blobs = []models.Blob{}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetMempoolBlobs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetMempoolBlobs_InvalidLimit(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?limit=0", http.NoBody)
	w := httptest.NewRecorder()
	a.GetMempoolBlobs(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetMempoolBlobs_DBError(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return fmt.Errorf("db error")
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetMempoolBlobs(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestGetBlobByTxHash_Success(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			blob := dest.(*models.Blob)
			*blob = models.Blob{
				NetworkID:         42,
				BlockNumber:       100,
				TxHash:            validTestTxHash,
				FromAddress:       "0x123",
				BlobSizeBytes:     131072,
				BaseFeePerBlobGas: "1000",
				TipPerBlobGas:     "100",
				TotalCostETH:      "0.001",
				Timestamp:         time.Now(),
				Confirmed:         true,
			}
			return nil
		},
	}
	a := newTestAPIWithDB(db)

	r := chi.NewRouter()
	r.Get("/blob/{txHash}", a.GetBlobByTxHash)

	req := httptest.NewRequest(http.MethodGet, "/blob/"+validTestTxHash, http.NoBody)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetBlobByTxHash_NotFound(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return sql.ErrNoRows
		},
	}
	a := newTestAPIWithDB(db)

	r := chi.NewRouter()
	r.Get("/blob/{txHash}", a.GetBlobByTxHash)

	req := httptest.NewRequest(http.MethodGet, "/blob/"+validTestTxHash, http.NoBody)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetBlobByTxHash_EmptyHash(t *testing.T) {
	a := newTestAPI()

	r := chi.NewRouter()
	r.Get("/blob/{txHash}", a.GetBlobByTxHash)

	// Chi won't route empty param, so we test without chi routing
	req := httptest.NewRequest(http.MethodGet, "/blob/", http.NoBody)
	w := httptest.NewRecorder()
	// Call directly - txHash will be empty from chi.URLParam
	a.GetBlobByTxHash(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetBlobStats_Success(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetBlobStats(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetBlobStats_DBError(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return fmt.Errorf("db error")
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetBlobStats(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestGetIndexerStatus_Success(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetIndexerStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetIndexerStatus_DBError(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return fmt.Errorf("db error")
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetIndexerStatus(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
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

func TestGetTopBlobUsers_DBError(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return fmt.Errorf("db error")
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?network=42", http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestGetTopBlobUsers_CustomLimit(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			users := dest.(*[]models.BlobUserStats)
			*users = []models.BlobUserStats{
				{Address: "0xabc", Name: "User1", BlobCount: 10},
			}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?network=42&limit=5", http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetLatestBlobs_ExcessiveLimit(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?limit=5000", http.NoBody)
	w := httptest.NewRecorder()
	a.GetLatestBlobs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetMempoolBlobs_NegativeLimit(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?limit=-1", http.NoBody)
	w := httptest.NewRecorder()
	a.GetMempoolBlobs(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetBlobByTxHash_DBError(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return fmt.Errorf("db error")
		},
	}
	a := newTestAPIWithDB(db)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("txHash", validTestTxHash)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	a.GetBlobByTxHash(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
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

func TestGetBlobStats_BadNetwork(t *testing.T) {
	a := &API{
		db:       &mockDB{},
		networks: map[int]config.NetworkConfig{},
		config:   &config.Config{Server: config.ServerConfig{Port: 8080}},
	}
	req := httptest.NewRequest(http.MethodGet, "/?network=999", http.NoBody)
	w := httptest.NewRecorder()
	a.GetBlobStats(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetIndexerStatus_BadNetwork(t *testing.T) {
	a := &API{
		db:       &mockDB{},
		networks: map[int]config.NetworkConfig{},
		config:   &config.Config{Server: config.ServerConfig{Port: 8080}},
	}
	req := httptest.NewRequest(http.MethodGet, "/?network=999", http.NoBody)
	w := httptest.NewRecorder()
	a.GetIndexerStatus(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
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

func TestGetMempoolBlobs_ExcessiveLimit(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?limit=5000", http.NoBody)
	w := httptest.NewRecorder()
	a.GetMempoolBlobs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetMempoolBlobs_BadNetwork(t *testing.T) {
	a := &API{
		db:       &mockDB{},
		networks: map[int]config.NetworkConfig{},
		config:   &config.Config{Server: config.ServerConfig{Port: 8080}},
	}
	req := httptest.NewRequest(http.MethodGet, "/?network=999", http.NoBody)
	w := httptest.NewRecorder()
	a.GetMempoolBlobs(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetLatestBlobs_LargeLimit(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?limit=10000", http.NoBody)
	w := httptest.NewRecorder()
	a.GetLatestBlobs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetTopBlobUsers_ExcessiveLimit(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			users := dest.(*[]models.BlobUserStats)
			*users = []models.BlobUserStats{}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?limit=5000", http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

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
	a := &API{
		db: db,
		networks: map[int]config.NetworkConfig{
			42: {Name: "testnet", ChainID: 42, Enabled: true},
		},
		config: &config.Config{
			Server:  config.ServerConfig{Port: 8080, DevMode: true},
			Indexer: config.IndexerConfig{Version: "test-v1"},
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.DevIndexers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (with fallback timestamp), got %d", w.Code)
	}
}

func TestGetBlobByTxHash_WithBlobData(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			setStructResult(dest, &models.Blob{
				NetworkID:         42,
				BlockNumber:       100,
				BlobIndex:         0,
				TxHash:            validTestTxHash,
				FromAddress:       "0xsender",
				BlobSizeBytes:     131072,
				BaseFeePerBlobGas: "1000000",
				TipPerBlobGas:     "500",
				TotalCostETH:      "0.001",
				Timestamp:         time.Now(),
				Confirmed:         true,
			})
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("txHash", validTestTxHash)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	a.GetBlobByTxHash(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
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

func TestGetBlobByTxHash_BadNetwork(t *testing.T) {
	a := &API{
		db:       &mockDB{},
		networks: map[int]config.NetworkConfig{},
		config:   &config.Config{Server: config.ServerConfig{Port: 8080}},
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("txHash", validTestTxHash)
	req := httptest.NewRequest(http.MethodGet, "/?network=999", http.NoBody)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	a.GetBlobByTxHash(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetBlobByTxHash_InvalidFormat(t *testing.T) {
	testCases := []struct {
		name   string
		txHash string
	}{
		{
			name:   "missing 0x prefix with 64 hex chars",
			txHash: strings.Repeat("a", 64),
		},
		{
			name:   "with 0x prefix but wrong length",
			txHash: "0xabc",
		},
		{
			name:   "with 0x prefix and non-hex characters",
			txHash: "0xgggggggggggggggggggggggggggggggggggggggggggggggggggggggggggggg",
		},
		{
			name:   "completely invalid string",
			txHash: "invalid-hash",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestAPIWithDB(&mockDB{})
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("txHash", tc.txHash)
			req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
			req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
			w := httptest.NewRecorder()
			a.GetBlobByTxHash(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for invalid hash format, got %d", w.Code)
			}
		})
	}
}

func TestRespondMaxBytesError(t *testing.T) {
	w := httptest.NewRecorder()
	RespondMaxBytesError(w)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", w.Code)
	}
	var resp Response
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Success {
		t.Error("expected Success=false")
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

func TestGetLastIndexedBlockFromDB_ValidValue(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			v := dest.(*string)
			*v = "12345"
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	block := a.getLastIndexedBlockFromDB(context.Background(), 42)
	if block != 12345 {
		t.Fatalf("expected 12345, got %d", block)
	}
}

func TestGetLastIndexedBlockFromDB_DBError(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return fmt.Errorf("db error")
		},
	}
	a := newTestAPIWithDB(db)
	block := a.getLastIndexedBlockFromDB(context.Background(), 42)
	if block != 0 {
		t.Fatalf("expected 0, got %d", block)
	}
}

func TestContentTypeJSON_RejectsNonJSON(t *testing.T) {
	handler := ContentTypeJSON(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("data"))
	req.Header.Set("Content-Type", "text/plain")
	req.ContentLength = 4
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415, got %d", w.Code)
	}
}

func TestContentTypeJSON_AllowsJSON(t *testing.T) {
	handler := ContentTypeJSON(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = 2
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestContentTypeJSON_SkipsGET(t *testing.T) {
	handler := ContentTypeJSON(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// --- GetBlobPricing ---

func TestGetBlobPricing_Success(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			metrics := dest.(*[]models.BlockMetrics)
			*metrics = []models.BlockMetrics{
				{
					NetworkID:        42,
					BlockNumber:      100,
					BlockTimestamp:   time.Now(),
					BlobCount:        3,
					BlobGasUsed:      393216,
					BlobGasTarget:    393216,
					BlobGasLimit:     786432,
					ExcessBlobGas:    100000,
					BlobBaseFee:      "1",
					UtilizationRatio: "1.000000",
					BlobParamsTarget: 3,
					BlobParamsMax:    6,
					UpdateFraction:   3338477,
				},
			}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?blocks=5", http.NoBody)
	w := httptest.NewRecorder()
	a.GetBlobPricing(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp Response
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Success {
		t.Error("expected Success=true")
	}
}

func TestGetBlobPricing_EmptyMetrics(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetBlobPricing(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetBlobPricing_InvalidBlocks(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?blocks=-1", http.NoBody)
	w := httptest.NewRecorder()
	a.GetBlobPricing(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetBlobPricing_BadNetwork(t *testing.T) {
	a := &API{
		db:       &mockDB{},
		networks: map[int]config.NetworkConfig{},
		config:   &config.Config{Server: config.ServerConfig{Port: 8080}},
	}
	req := httptest.NewRequest(http.MethodGet, "/?network=999", http.NoBody)
	w := httptest.NewRecorder()
	a.GetBlobPricing(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetBlobPricing_DBError(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return fmt.Errorf("db error")
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetBlobPricing(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestGetMempoolBlobs_WithData(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			setSliceResult(dest, []models.Blob{
				{
					NetworkID:         42,
					BlockNumber:       -1,
					TxHash:            "0xpending",
					FromAddress:       "0xsender",
					BlobSizeBytes:     131072,
					BaseFeePerBlobGas: "1000000",
					TipPerBlobGas:     "500",
					TotalCostETH:      "0.001",
					Timestamp:         time.Now(),
					Confirmed:         false,
				},
			})
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?limit=10", http.NoBody)
	w := httptest.NewRecorder()
	a.GetMempoolBlobs(w, req)

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

func TestCalcNextExcessBlobGas(t *testing.T) {
	tests := []struct {
		name      string
		excess    uint64
		gasUsed   uint64
		targetGas uint64
		want      uint64
	}{
		{"below target returns zero", 0, 100000, 393216, 0},
		{"at target returns excess", 100000, 393216, 393216, 100000},
		{"above target", 100000, 500000, 393216, 206784},
		{"zero excess zero used", 0, 0, 393216, 0},
		{"large excess", 10000000, 786432, 393216, 10393216},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := calcNextExcessBlobGas(tt.excess, tt.gasUsed, tt.targetGas)
			if got != tt.want {
				t.Errorf("calcNextExcessBlobGas(%d, %d, %d) = %d, want %d",
					tt.excess, tt.gasUsed, tt.targetGas, got, tt.want)
			}
		})
	}
}
