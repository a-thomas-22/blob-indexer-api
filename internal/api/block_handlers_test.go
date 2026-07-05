package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	_ "github.com/a-thomas-22/blob-indexer-api/internal/testutil"
)

// newBlockRequest builds a request routed at GetBlockByNumber with the given
// {number} path param.
func newBlockRequest(number string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("number", number)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func testBlockMetrics(blockNumber int64, blobCount int) models.BlockMetrics {
	return models.BlockMetrics{
		ChainID:          42,
		BlockNumber:      blockNumber,
		BlockTimestamp:   time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		BlobCount:        blobCount,
		BlobGasUsed:      int64(blobCount) * 131072,
		BlobGasTarget:    786432,
		BlobGasLimit:     1179648,
		ExcessBlobGas:    0,
		BlobBaseFee:      "1000000",
		UtilizationRatio: "0.5",
		BlobParamsTarget: 6,
		BlobParamsMax:    9,
		UpdateFraction:   5007716,
	}
}

func TestGetBlockByNumber_Success(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if !strings.Contains(query, "FROM block_metrics") {
				t.Fatalf("unexpected get query: %s", query)
			}
			if args[0] != 42 || args[1] != int64(100) {
				t.Fatalf("expected args (42, 100), got %v", args)
			}
			setStructResult(dest, testBlockMetrics(100, 2))
			return nil
		},
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if !strings.Contains(query, "FROM blobs") {
				t.Fatalf("unexpected select query: %s", query)
			}
			if args[0] != 42 || args[1] != int64(100) {
				t.Fatalf("expected args (42, 100), got %v", args)
			}
			setSliceResult(dest, []models.Blob{
				{
					ChainID:           42,
					BlockNumber:       100,
					BlobIndex:         0,
					TxHash:            validTestTxHash,
					FromAddress:       validTestAddress,
					BlobSizeBytes:     131072,
					BaseFeePerBlobGas: "1000000",
					TipPerBlobGas:     "500",
					TotalCostWei:      "131072000000",
					Timestamp:         time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
					Confirmed:         true,
				},
				{
					ChainID:           42,
					BlockNumber:       100,
					BlobIndex:         1,
					TxHash:            validTestTxHash,
					FromAddress:       validTestAddress,
					BlobSizeBytes:     131072,
					BaseFeePerBlobGas: "1000000",
					TipPerBlobGas:     "500",
					TotalCostWei:      "131072000000",
					Timestamp:         time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
					Confirmed:         true,
				},
			})
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	w := httptest.NewRecorder()
	a.GetBlockByNumber(w, newBlockRequest("100"))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Success bool         `json:"success"`
		Data    NewBlockData `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Success {
		t.Error("expected Success=true")
	}
	if resp.Data.BlockNumber != 100 || resp.Data.BlobCount != 2 {
		t.Errorf("unexpected block data: %+v", resp.Data)
	}
	if len(resp.Data.Blobs) != 2 || resp.Data.Blobs[0].BlobIndex != 0 || resp.Data.Blobs[1].BlobIndex != 1 {
		t.Errorf("unexpected blobs: %+v", resp.Data.Blobs)
	}
	if resp.Data.Blobs[0].NetworkName != "testnet" {
		t.Errorf("expected network name testnet, got %q", resp.Data.Blobs[0].NetworkName)
	}
	pricing := resp.Data.Pricing
	if pricing == nil {
		t.Fatal("expected pricing to be set")
	}
	if pricing.BlockNumber != 100 || pricing.BlobCount != 2 || pricing.MaxBlobs != 9 ||
		pricing.TargetBlobs != 6 || pricing.AvailableBlobs != 7 || pricing.IsFull || pricing.IsAboveTarget {
		t.Errorf("unexpected pricing: %+v", pricing)
	}
	wantCache := fmt.Sprintf("public, max-age=%d, s-maxage=%d",
		int(indexedBlockCacheTTL.Seconds()), int(indexedBlockEdgeTTL.Seconds()))
	if got := w.Header().Get("Cache-Control"); got != wantCache {
		t.Errorf("Cache-Control = %q, want %q", got, wantCache)
	}
}

func TestGetBlockByNumber_ZeroBlobBlock(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			setStructResult(dest, testBlockMetrics(101, 0))
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	w := httptest.NewRecorder()
	a.GetBlockByNumber(w, newBlockRequest("101"))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Zero-blob blocks must serialize blobs as an empty array, not null — the
	// frontend reuses the WebSocket new_block transform, which iterates blobs.
	var resp struct {
		Data struct {
			Blobs json.RawMessage `json:"blobs"`
		} `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if got := strings.TrimSpace(string(resp.Data.Blobs)); got != "[]" {
		t.Errorf("blobs = %s, want []", got)
	}
}

func TestGetBlockByNumber_NotFound(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return sql.ErrNoRows
		},
	}
	a := newTestAPIWithDB(db)
	w := httptest.NewRecorder()
	a.GetBlockByNumber(w, newBlockRequest("100"))

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
	var resp Response
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Success {
		t.Error("expected Success=false")
	}
	if resp.ErrorCode != errCodeNotFound {
		t.Errorf("error_code = %q, want %q", resp.ErrorCode, errCodeNotFound)
	}
}

func TestGetBlockByNumber_InvalidNumber(t *testing.T) {
	testCases := []struct {
		name   string
		number string
	}{
		{name: "non-numeric", number: "abc"},
		{name: "empty", number: ""},
		{name: "zero", number: "0"},
		{name: "negative", number: "-5"},
		{name: "decimal", number: "1.5"},
		{name: "overflow", number: "99999999999999999999999"},
		{name: "hex", number: "0x64"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestAPI()
			w := httptest.NewRecorder()
			a.GetBlockByNumber(w, newBlockRequest(tc.number))

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for %q, got %d", tc.number, w.Code)
			}
		})
	}
}

func TestGetBlockByNumber_MetricsDBError(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return fmt.Errorf("db error")
		},
	}
	a := newTestAPIWithDB(db)
	w := httptest.NewRecorder()
	a.GetBlockByNumber(w, newBlockRequest("100"))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestGetBlockByNumber_BlobsDBError(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			setStructResult(dest, testBlockMetrics(100, 2))
			return nil
		},
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return fmt.Errorf("db error")
		},
	}
	a := newTestAPIWithDB(db)
	w := httptest.NewRecorder()
	a.GetBlockByNumber(w, newBlockRequest("100"))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestGetBlockByNumber_BadNetwork(t *testing.T) {
	a := newTestAPI()
	a.networks = map[int]config.NetworkConfig{}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("number", "100")
	req := httptest.NewRequest(http.MethodGet, "/?network=999", http.NoBody)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	a.GetBlockByNumber(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}
