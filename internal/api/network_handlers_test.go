package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	_ "github.com/a-thomas-22/blob-indexer-api/internal/testutil"
)

func TestGetNetworkFromRequest(t *testing.T) {
	tests := []struct {
		name       string
		url        string
		setup      func(a *API)
		wantName   string
		wantChain  int
		wantErr    error
		wantNonNil bool
	}{
		{
			name:      "by chain id",
			url:       "/?network=42",
			wantChain: 42,
		},
		{
			name:     "by name",
			url:      "/?network=testnet",
			wantName: "testnet",
		},
		{
			name:       "default first network",
			url:        "/",
			wantNonNil: true,
		},
		{
			name:    "no networks",
			url:     "/",
			wantErr: ErrNoNetworksAvailable,
			setup: func(a *API) {
				a.networks = map[int]config.NetworkConfig{}
			},
		},
		{
			name:    "network not found",
			url:     "/?network=999",
			wantErr: ErrNetworkNotFound,
			setup: func(a *API) {
				a.networks = map[int]config.NetworkConfig{}
			},
		},
		{
			name:    "multiple networks require explicit selector",
			url:     "/",
			wantErr: ErrNetworkParamRequired,
			setup: func(a *API) {
				a.networks = map[int]config.NetworkConfig{
					1:        {Name: "mainnet", ChainID: 1, Enabled: true},
					11155111: {Name: "sepolia", ChainID: 11155111, Enabled: true},
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestAPI()
			if tc.setup != nil {
				tc.setup(a)
			}
			req := httptest.NewRequest(http.MethodGet, tc.url, http.NoBody)
			network, err := a.getNetworkFromRequest(req)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("expected error %v, got %v", tc.wantErr, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantChain != 0 && network.ChainID != tc.wantChain {
				t.Fatalf("expected chain ID %d, got %d", tc.wantChain, network.ChainID)
			}
			if tc.wantName != "" && network.Name != tc.wantName {
				t.Fatalf("expected network name %q, got %q", tc.wantName, network.Name)
			}
			if tc.wantNonNil && network.ChainID == 0 {
				t.Fatal("expected non-zero chain ID")
			}
		})
	}
}

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

func TestGetNetworks_IncludesFreshness(t *testing.T) {
	indexedAt := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	headAt := indexedAt.Add(12 * time.Second)
	wsAt := headAt.Add(time.Second)
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			setSliceResult(dest, []freshnessMetadataRow{
				{Key: models.MetadataLastIndexedBlock, Value: "100"},
				{Key: models.MetadataCurrentChainHead, Value: "123"},
				{Key: models.MetadataLastIndexedAt, Value: models.FormatMetadataTimestamp(indexedAt)},
				{Key: models.MetadataChainHeadUpdatedAt, Value: models.FormatMetadataTimestamp(headAt)},
				{Key: models.MetadataWebSocketFreshnessAt, Value: models.FormatMetadataTimestamp(wsAt)},
			})
			return nil
		},
	}
	a := newTestAPIWithDB(db)

	req := httptest.NewRequest(http.MethodGet, "/api/networks", http.NoBody)
	w := httptest.NewRecorder()
	a.GetNetworks(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp struct {
		Success bool              `json:"success"`
		Data    []NetworkResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("expected one network, got %d", len(resp.Data))
	}
	got := resp.Data[0]
	if got.LastIndexedBlock != 100 {
		t.Fatalf("expected last indexed block 100, got %d", got.LastIndexedBlock)
	}
	if got.CurrentChainHead == nil || *got.CurrentChainHead != 123 {
		t.Fatalf("expected current chain head 123, got %v", got.CurrentChainHead)
	}
	if got.IndexerLagBlocks == nil || *got.IndexerLagBlocks != 23 {
		t.Fatalf("expected indexer lag 23, got %v", got.IndexerLagBlocks)
	}
	if got.LastIndexedAt == nil || !got.LastIndexedAt.Equal(indexedAt) {
		t.Fatalf("expected last indexed at %s, got %v", indexedAt, got.LastIndexedAt)
	}
	if got.ChainHeadUpdatedAt == nil || !got.ChainHeadUpdatedAt.Equal(headAt) {
		t.Fatalf("expected chain head updated at %s, got %v", headAt, got.ChainHeadUpdatedAt)
	}
	if got.WebSocketFreshnessAt == nil || !got.WebSocketFreshnessAt.Equal(wsAt) {
		t.Fatalf("expected websocket freshness at %s, got %v", wsAt, got.WebSocketFreshnessAt)
	}
}

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

func TestGetNetworkStatus_IncludesFreshness(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			setSliceResult(dest, []freshnessMetadataRow{
				{Key: models.MetadataLastIndexedBlock, Value: "200"},
				{Key: models.MetadataCurrentChainHead, Value: "205"},
			})
			return nil
		},
	}
	a := newTestAPIWithDB(db)

	r := chi.NewRouter()
	r.Get("/api/networks/{chainId}", a.GetNetworkStatus)

	req := httptest.NewRequest(http.MethodGet, "/api/networks/42", http.NoBody)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp struct {
		Success bool                  `json:"success"`
		Data    NetworkStatusResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Data.LastIndexedBlock != 200 {
		t.Fatalf("expected last indexed block 200, got %d", resp.Data.LastIndexedBlock)
	}
	if resp.Data.CurrentChainHead == nil || *resp.Data.CurrentChainHead != 205 {
		t.Fatalf("expected current chain head 205, got %v", resp.Data.CurrentChainHead)
	}
	if resp.Data.IndexerLagBlocks == nil || *resp.Data.IndexerLagBlocks != 5 {
		t.Fatalf("expected indexer lag 5, got %v", resp.Data.IndexerLagBlocks)
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
