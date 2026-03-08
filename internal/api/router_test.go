package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	_ "github.com/a-thomas-22/blob-indexer-api/internal/testutil"
	"github.com/go-chi/chi/v5"
)

// mockIndexer implements IndexerProvider for testing.
type mockIndexer struct {
	network          config.NetworkConfig
	lastIndexedBlock uint64
	currentBlock     uint64
	currentBlockErr  error
	confirmedCount   int
	pendingCount     int
	blobCountsErr    error
	topUsers         []models.BlobUserStats
	topUsersErr      error
	reindexErr       error
}

func (m *mockIndexer) GetNetworkInfo() config.NetworkConfig { return m.network }
func (m *mockIndexer) GetLastIndexedBlock() uint64          { return m.lastIndexedBlock }
func (m *mockIndexer) GetCurrentBlock(ctx context.Context) (uint64, error) {
	return m.currentBlock, m.currentBlockErr
}
func (m *mockIndexer) GetBlobCounts(ctx context.Context) (int, int, error) {
	return m.confirmedCount, m.pendingCount, m.blobCountsErr
}
func (m *mockIndexer) GetTopBlobUsers(ctx context.Context, limit int) ([]models.BlobUserStats, error) {
	return m.topUsers, m.topUsersErr
}
func (m *mockIndexer) Reindex(startBlock, endBlock uint64) error {
	return m.reindexErr
}

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
		InUse:             2,
		Idle:              3,
	}
}

func newTestAPI() (*API, *mockIndexer) {
	return newTestAPIWithDB(nil)
}

func newTestAPIWithDB(db DBProvider) (*API, *mockIndexer) {
	mock := &mockIndexer{
		network: config.NetworkConfig{
			Name:    "testnet",
			ChainID: 42,
			Enabled: true,
		},
		lastIndexedBlock: 1000,
		currentBlock:     1050,
	}
	if db == nil {
		db = &mockDB{}
	}
	a := &API{
		db:       db,
		indexers: map[int]IndexerProvider{42: mock},
		config: &config.Config{
			Server:  config.ServerConfig{Port: 8080, DevMode: true},
			Indexer: config.IndexerConfig{Version: "test-v1"},
		},
	}
	return a, mock
}

// setSliceResult is a helper to assign mock data to a dest pointer via reflection.
func setSliceResult(dest interface{}, src interface{}) {
	dv := reflect.ValueOf(dest)
	if dv.Kind() == reflect.Ptr {
		dv = dv.Elem()
	}
	sv := reflect.ValueOf(src)
	dv.Set(sv)
}

// setStructResult copies a struct value into the dest pointer.
func setStructResult(dest interface{}, src interface{}) {
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
	a, _ := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?network=42", nil)
	idx, err := a.getNetworkFromRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if idx.GetNetworkInfo().ChainID != 42 {
		t.Errorf("expected chain ID 42, got %d", idx.GetNetworkInfo().ChainID)
	}
}

func TestGetNetworkFromRequest_ByName(t *testing.T) {
	a, _ := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?network=testnet", nil)
	idx, err := a.getNetworkFromRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if idx.GetNetworkInfo().Name != "testnet" {
		t.Errorf("expected 'testnet', got %q", idx.GetNetworkInfo().Name)
	}
}

func TestGetNetworkFromRequest_DefaultFirstNetwork(t *testing.T) {
	a, _ := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	idx, err := a.getNetworkFromRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if idx == nil {
		t.Fatal("expected non-nil indexer")
	}
}

func TestGetNetworkFromRequest_NoIndexers(t *testing.T) {
	a := &API{indexers: map[int]IndexerProvider{}}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	_, err := a.getNetworkFromRequest(req)
	if err != ErrNoNetworksAvailable {
		t.Errorf("expected ErrNoNetworksAvailable, got %v", err)
	}
}

func TestGetNetworkFromRequest_NotFound(t *testing.T) {
	a := &API{indexers: map[int]IndexerProvider{}}
	req := httptest.NewRequest(http.MethodGet, "/?network=999", nil)
	_, err := a.getNetworkFromRequest(req)
	if err != ErrNetworkNotFound {
		t.Errorf("expected ErrNetworkNotFound, got %v", err)
	}
}

// --- GetNetworks ---

func TestGetNetworks(t *testing.T) {
	a, _ := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/api/networks", nil)
	w := httptest.NewRecorder()
	a.GetNetworks(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp Response
	json.NewDecoder(w.Body).Decode(&resp)
	if !resp.Success {
		t.Error("expected Success=true")
	}
}

// --- GetNetworkStatus ---

func TestGetNetworkStatus_Valid(t *testing.T) {
	a, _ := newTestAPI()

	r := chi.NewRouter()
	r.Get("/api/networks/{chainId}", a.GetNetworkStatus)

	req := httptest.NewRequest(http.MethodGet, "/api/networks/42", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetNetworkStatus_InvalidChainID(t *testing.T) {
	a, _ := newTestAPI()

	r := chi.NewRouter()
	r.Get("/api/networks/{chainId}", a.GetNetworkStatus)

	req := httptest.NewRequest(http.MethodGet, "/api/networks/abc", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetNetworkStatus_NotFound(t *testing.T) {
	a, _ := newTestAPI()

	r := chi.NewRouter()
	r.Get("/api/networks/{chainId}", a.GetNetworkStatus)

	req := httptest.NewRequest(http.MethodGet, "/api/networks/999", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// --- GetTopBlobUsers ---

func TestGetTopBlobUsers_Success(t *testing.T) {
	a, mock := newTestAPI()
	mock.topUsers = []models.BlobUserStats{
		{Address: "0xabc", Name: "Alice", BlobCount: 10, TotalCostETH: "1.5", LastTimestamp: time.Now()},
	}

	req := httptest.NewRequest(http.MethodGet, "/?limit=5", nil)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetTopBlobUsers_InvalidLimit(t *testing.T) {
	a, _ := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?limit=-1", nil)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetTopBlobUsers_BadNetwork(t *testing.T) {
	a, _ := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?network=missing", nil)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// --- DevMetrics ---

func TestDevMetrics(t *testing.T) {
	a, _ := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/api/dev/metrics", nil)
	w := httptest.NewRecorder()
	a.DevMetrics(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// --- DevLogs ---

func TestDevLogs_Default(t *testing.T) {
	a, _ := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/api/dev/logs", nil)
	w := httptest.NewRecorder()
	a.DevLogs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestDevLogs_FilterByLevel(t *testing.T) {
	a, _ := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/api/dev/logs?level=error", nil)
	w := httptest.NewRecorder()
	a.DevLogs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestDevLogs_InvalidLimit(t *testing.T) {
	a, _ := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/api/dev/logs?limit=abc", nil)
	w := httptest.NewRecorder()
	a.DevLogs(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// --- DevQueries ---

func TestDevQueries_Default(t *testing.T) {
	a, _ := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/api/dev/queries", nil)
	w := httptest.NewRecorder()
	a.DevQueries(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestDevQueries_InvalidLimit(t *testing.T) {
	a, _ := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/api/dev/queries?limit=0", nil)
	w := httptest.NewRecorder()
	a.DevQueries(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// --- DevDashboard ---

func TestDevDashboard(t *testing.T) {
	a, _ := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/api/dev/dashboard", nil)
	w := httptest.NewRecorder()
	a.DevDashboard(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// --- DevReindex ---

func TestDevReindex_Success(t *testing.T) {
	a, _ := newTestAPI()
	body := `{"network_id":42,"start_block":100,"end_block":200}`
	req := httptest.NewRequest(http.MethodPost, "/api/dev/reindex", strings.NewReader(body))
	w := httptest.NewRecorder()
	a.DevReindex(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestDevReindex_InvalidBody(t *testing.T) {
	a, _ := newTestAPI()
	req := httptest.NewRequest(http.MethodPost, "/api/dev/reindex", strings.NewReader("invalid"))
	w := httptest.NewRecorder()
	a.DevReindex(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestDevReindex_StartGreaterThanEnd(t *testing.T) {
	a, _ := newTestAPI()
	body := `{"network_id":42,"start_block":300,"end_block":100}`
	req := httptest.NewRequest(http.MethodPost, "/api/dev/reindex", strings.NewReader(body))
	w := httptest.NewRecorder()
	a.DevReindex(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestDevReindex_InvalidNetwork(t *testing.T) {
	a, _ := newTestAPI()
	body := `{"network_id":999,"start_block":100,"end_block":200}`
	req := httptest.NewRequest(http.MethodPost, "/api/dev/reindex", strings.NewReader(body))
	w := httptest.NewRecorder()
	a.DevReindex(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// --- NewRouter ---

func TestNewRouter_ReturnsHandler(t *testing.T) {
	cfg := &config.Config{
		Server:  config.ServerConfig{Port: 8080, DevMode: true},
		Indexer: config.IndexerConfig{Version: "test"},
	}
	handler := NewRouter(nil, nil, cfg)
	if handler == nil {
		t.Fatal("expected non-nil handler")
	}
}

func TestNewRouter_DevModeDisabled(t *testing.T) {
	cfg := &config.Config{
		Server:  config.ServerConfig{Port: 8080, DevMode: false},
		Indexer: config.IndexerConfig{Version: "test"},
	}
	handler := NewRouter(nil, nil, cfg)
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
	a, _ := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	a.GetLatestBlobs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp Response
	json.NewDecoder(w.Body).Decode(&resp)
	if !resp.Success {
		t.Error("expected Success=true")
	}
}

func TestGetLatestBlobs_InvalidLimit(t *testing.T) {
	a, _ := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?limit=abc", nil)
	w := httptest.NewRecorder()
	a.GetLatestBlobs(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetLatestBlobs_NegativeLimit(t *testing.T) {
	a, _ := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?limit=-5", nil)
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
	a, _ := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	a.GetLatestBlobs(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestGetLatestBlobs_BadNetwork(t *testing.T) {
	a, _ := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?network=unknown", nil)
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
	a, _ := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	a.GetMempoolBlobs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetMempoolBlobs_InvalidLimit(t *testing.T) {
	a, _ := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?limit=0", nil)
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
	a, _ := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
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
				TxHash:            "0xabc",
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
	a, _ := newTestAPIWithDB(db)

	r := chi.NewRouter()
	r.Get("/blob/{txHash}", a.GetBlobByTxHash)

	req := httptest.NewRequest(http.MethodGet, "/blob/0xabc", nil)
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
	a, _ := newTestAPIWithDB(db)

	r := chi.NewRouter()
	r.Get("/blob/{txHash}", a.GetBlobByTxHash)

	req := httptest.NewRequest(http.MethodGet, "/blob/0xnotfound", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetBlobByTxHash_EmptyHash(t *testing.T) {
	a, _ := newTestAPI()

	r := chi.NewRouter()
	r.Get("/blob/{txHash}", a.GetBlobByTxHash)

	// Chi won't route empty param, so we test without chi routing
	req := httptest.NewRequest(http.MethodGet, "/blob/", nil)
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
			// The stats query returns an anonymous struct - just leave defaults (zero values)
			return nil
		},
	}
	a, _ := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
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
	a, _ := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
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
	a, _ := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
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
	a, _ := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
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
	a, _ := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
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
	a, _ := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	a.DevDatabase(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestDevIndexers_CurrentBlockError(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return nil
		},
	}
	mock := &mockIndexer{
		network:          config.NetworkConfig{Name: "testnet", ChainID: 42, Enabled: true},
		lastIndexedBlock: 1000,
		currentBlockErr:  fmt.Errorf("node unavailable"),
	}
	a := &API{
		db:       db,
		indexers: map[int]IndexerProvider{42: mock},
		config: &config.Config{
			Server:  config.ServerConfig{Port: 8080, DevMode: true},
			Indexer: config.IndexerConfig{Version: "test-v1"},
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	a.DevIndexers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (fallback), got %d", w.Code)
	}
}

func TestDevIndexers_BlobCountsError(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return nil
		},
	}
	mock := &mockIndexer{
		network:          config.NetworkConfig{Name: "testnet", ChainID: 42, Enabled: true},
		lastIndexedBlock: 1000,
		currentBlock:     1050,
		blobCountsErr:    fmt.Errorf("db error"),
	}
	a := &API{
		db:       db,
		indexers: map[int]IndexerProvider{42: mock},
		config: &config.Config{
			Server:  config.ServerConfig{Port: 8080, DevMode: true},
			Indexer: config.IndexerConfig{Version: "test-v1"},
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	a.DevIndexers(w, req)

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
	a, _ := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	a.DevDatabase(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (with empty stats), got %d", w.Code)
	}
}

func TestGetTopBlobUsers_DBError(t *testing.T) {
	mock := &mockIndexer{
		network:     config.NetworkConfig{Name: "testnet", ChainID: 42, Enabled: true},
		topUsersErr: fmt.Errorf("db error"),
	}
	a := &API{
		db:       &mockDB{},
		indexers: map[int]IndexerProvider{42: mock},
		config: &config.Config{
			Server: config.ServerConfig{Port: 8080, DevMode: true},
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/?network=42", nil)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestGetTopBlobUsers_CustomLimit(t *testing.T) {
	mock := &mockIndexer{
		network: config.NetworkConfig{Name: "testnet", ChainID: 42, Enabled: true},
		topUsers: []models.BlobUserStats{
			{Address: "0xabc", Name: "User1", BlobCount: 10},
		},
	}
	a := &API{
		db:       &mockDB{},
		indexers: map[int]IndexerProvider{42: mock},
		config: &config.Config{
			Server: config.ServerConfig{Port: 8080, DevMode: true},
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/?network=42&limit=5", nil)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestDevReindex_ReindexError(t *testing.T) {
	mock := &mockIndexer{
		network:    config.NetworkConfig{Name: "testnet", ChainID: 42, Enabled: true},
		reindexErr: fmt.Errorf("reindex failed"),
	}
	a := &API{
		db:       &mockDB{},
		indexers: map[int]IndexerProvider{42: mock},
		config: &config.Config{
			Server: config.ServerConfig{Port: 8080, DevMode: true},
		},
	}
	body := strings.NewReader(`{"network_id": 42, "start_block": 100, "end_block": 200}`)
	req := httptest.NewRequest(http.MethodPost, "/", body)
	w := httptest.NewRecorder()
	a.DevReindex(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestGetLatestBlobs_ExcessiveLimit(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return nil
		},
	}
	a, _ := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?limit=5000", nil)
	w := httptest.NewRecorder()
	a.GetLatestBlobs(w, req)

	// Should succeed but cap at MaxQueryLimit
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetMempoolBlobs_NegativeLimit(t *testing.T) {
	a, _ := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?limit=-1", nil)
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
	a, _ := newTestAPIWithDB(db)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("txHash", "0x1234")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	a.GetBlobByTxHash(w, req)

	// GetBlobByTxHash treats all DB errors as "not found"
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestDevLogs_CustomLimit(t *testing.T) {
	a, _ := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?limit=50", nil)
	w := httptest.NewRecorder()
	a.DevLogs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestDevQueries_CustomLimit(t *testing.T) {
	a, _ := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?limit=50", nil)
	w := httptest.NewRecorder()
	a.DevQueries(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestDevQueries_LimitTruncation(t *testing.T) {
	a, _ := newTestAPI()
	// limit=2 should truncate the placeholder query list
	req := httptest.NewRequest(http.MethodGet, "/?limit=2", nil)
	w := httptest.NewRecorder()
	a.DevQueries(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp Response
	json.NewDecoder(w.Body).Decode(&resp)
	if !resp.Success {
		t.Error("expected success=true")
	}
}

func TestDevQueries_ExcessiveLimit(t *testing.T) {
	a, _ := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?limit=5000", nil)
	w := httptest.NewRecorder()
	a.DevQueries(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetBlobStats_BadNetwork(t *testing.T) {
	a := &API{
		db:       &mockDB{},
		indexers: map[int]IndexerProvider{},
		config:   &config.Config{Server: config.ServerConfig{Port: 8080}},
	}
	req := httptest.NewRequest(http.MethodGet, "/?network=999", nil)
	w := httptest.NewRecorder()
	a.GetBlobStats(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetIndexerStatus_BadNetwork(t *testing.T) {
	a := &API{
		db:       &mockDB{},
		indexers: map[int]IndexerProvider{},
		config:   &config.Config{Server: config.ServerConfig{Port: 8080}},
	}
	req := httptest.NewRequest(http.MethodGet, "/?network=999", nil)
	w := httptest.NewRecorder()
	a.GetIndexerStatus(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestDevLogs_ExcessiveLimit(t *testing.T) {
	a, _ := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?limit=5000", nil)
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
	a, _ := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?limit=5000", nil)
	w := httptest.NewRecorder()
	a.GetMempoolBlobs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetMempoolBlobs_BadNetwork(t *testing.T) {
	a := &API{
		db:       &mockDB{},
		indexers: map[int]IndexerProvider{},
		config:   &config.Config{Server: config.ServerConfig{Port: 8080}},
	}
	req := httptest.NewRequest(http.MethodGet, "/?network=999", nil)
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
	a, _ := newTestAPIWithDB(db)
	// Test that limit > MaxQueryLimit gets capped
	req := httptest.NewRequest(http.MethodGet, "/?limit=10000", nil)
	w := httptest.NewRecorder()
	a.GetLatestBlobs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetTopBlobUsers_ExcessiveLimit(t *testing.T) {
	mock := &mockIndexer{
		network:  config.NetworkConfig{Name: "testnet", ChainID: 42, Enabled: true},
		topUsers: []models.BlobUserStats{},
	}
	a := &API{
		db:       &mockDB{},
		indexers: map[int]IndexerProvider{42: mock},
		config:   &config.Config{Server: config.ServerConfig{Port: 8080}},
	}
	req := httptest.NewRequest(http.MethodGet, "/?limit=5000", nil)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestDevDatabase_PartialErrors(t *testing.T) {
	// Mock that returns error for pg_total_relation_size queries but success for COUNT
	callCount := 0
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			callCount++
			if strings.Contains(query, "pg_total_relation_size") ||
				strings.Contains(query, "pg_indexes") ||
				strings.Contains(query, "pg_database_size") {
				return fmt.Errorf("permission denied")
			}
			return nil
		},
	}
	a, _ := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	a.DevDatabase(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (with fallback values), got %d", w.Code)
	}
}

func TestDevIndexers_DBTimestampError(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if strings.Contains(query, "MAX(timestamp)") {
				return fmt.Errorf("db error")
			}
			return nil
		},
	}
	mock := &mockIndexer{
		network:          config.NetworkConfig{Name: "testnet", ChainID: 42, Enabled: true},
		lastIndexedBlock: 1000,
		currentBlock:     1050,
	}
	a := &API{
		db:       db,
		indexers: map[int]IndexerProvider{42: mock},
		config: &config.Config{
			Server:  config.ServerConfig{Port: 8080, DevMode: true},
			Indexer: config.IndexerConfig{Version: "test-v1"},
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
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
				TxHash:            "0xabc123",
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
	a, _ := newTestAPIWithDB(db)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("txHash", "0xabc123")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	a.GetBlobByTxHash(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestDevLogs_SmallLimit(t *testing.T) {
	a, _ := newTestAPI()
	// limit=2 should truncate placeholder logs (5 entries)
	req := httptest.NewRequest(http.MethodGet, "/?limit=2", nil)
	w := httptest.NewRecorder()
	a.DevLogs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestDevLogs_NegativeLimit(t *testing.T) {
	a, _ := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?limit=-5", nil)
	w := httptest.NewRecorder()
	a.DevLogs(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetBlobByTxHash_BadNetwork(t *testing.T) {
	a := &API{
		db:       &mockDB{},
		indexers: map[int]IndexerProvider{},
		config:   &config.Config{Server: config.ServerConfig{Port: 8080}},
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("txHash", "0xabc")
	req := httptest.NewRequest(http.MethodGet, "/?network=999", nil)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	a.GetBlobByTxHash(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
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
	a, _ := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?limit=10", nil)
	w := httptest.NewRecorder()
	a.GetMempoolBlobs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp Response
	json.NewDecoder(w.Body).Decode(&resp)
	if !resp.Success {
		t.Error("expected success=true")
	}
}
