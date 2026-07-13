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
	"github.com/lib/pq"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	_ "github.com/a-thomas-22/blob-indexer-api/internal/testutil"
)

const validTestVersionedHash = "0x01bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestGetLatestBlobs_Success(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			blobs := dest.(*[]models.Blob)
			*blobs = []models.Blob{
				{
					ChainID:           42,
					BlockNumber:       100,
					BlobIndex:         0,
					TxHash:            "0xabc",
					FromAddress:       "0x123",
					BlobSizeBytes:     131072,
					BaseFeePerBlobGas: "1000",
					TipPerBlobGas:     "100",
					TotalCostWei:      "0.001",
					Timestamp:         time.Now(),
					Confirmed:         true,
					VersionedHashes:   pq.StringArray{validTestVersionedHash},
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
	var resp struct {
		Success bool           `json:"success"`
		Data    []BlobResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Success {
		t.Error("expected Success=true")
	}
	if len(resp.Data) != 1 || len(resp.Data[0].VersionedHashes) != 1 || resp.Data[0].VersionedHashes[0] != validTestVersionedHash {
		t.Errorf("expected versioned_hashes [%s] on list rows, got %+v", validTestVersionedHash, resp.Data)
	}
	wantCache := fmt.Sprintf("public, max-age=%d, s-maxage=%d",
		int(latestBlobsCacheTTL.Seconds()), int(latestBlobsEdgeTTL.Seconds()))
	if got := w.Header().Get("Cache-Control"); got != wantCache {
		t.Errorf("Cache-Control = %q, want %q", got, wantCache)
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

func TestGetBlobReplacements_Success(t *testing.T) {
	replacedAt := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if !strings.Contains(query, "FROM blob_replacements") || strings.Contains(query, "replaced_tx_hash = $2") {
				t.Fatalf("expected unfiltered replacements query, got %q", query)
			}
			if len(args) != 3 || args[0] != 42 || args[1] != 25 || args[2] != 0 {
				t.Fatalf("unexpected args: %v", args)
			}
			events := dest.(*[]models.BlobReplacement)
			*events = []models.BlobReplacement{{
				ReplacedTxHash:    "0xold",
				ReplacementTxHash: "0xnew",
				FromAddress:       "0xsender",
				Nonce:             7,
				ReplacedAt:        replacedAt,
			}}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetBlobReplacements(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var envelope struct {
		Success bool                      `json:"success"`
		Data    []BlobReplacementResponse `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !envelope.Success || len(envelope.Data) != 1 {
		t.Fatalf("unexpected envelope: %+v", envelope)
	}
	got := envelope.Data[0]
	if got.ChainID != 42 || got.NetworkName != "testnet" ||
		got.ReplacedTxHash != "0xold" || got.ReplacementTxHash != "0xnew" ||
		got.FromAddress != "0xsender" || got.Nonce != 7 || !got.ReplacedAt.Equal(replacedAt) {
		t.Fatalf("unexpected replacement response: %+v", got)
	}
}

func TestGetBlobReplacements_TxHashFilter(t *testing.T) {
	txHash := "0x" + strings.Repeat("ab", 32)
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if !strings.Contains(query, "replaced_tx_hash = $2 OR replacement_tx_hash = $2") {
				t.Fatalf("expected filtered replacements query, got %q", query)
			}
			if len(args) != 4 || args[0] != 42 || args[1] != txHash || args[2] != 25 || args[3] != 0 {
				t.Fatalf("unexpected args: %v", args)
			}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?tx_hash="+txHash, http.NoBody)
	w := httptest.NewRecorder()
	a.GetBlobReplacements(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetBlobReplacements_InvalidTxHash(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?tx_hash=nothex", http.NoBody)
	w := httptest.NewRecorder()
	a.GetBlobReplacements(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetBlobReplacements_InvalidLimit(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?limit=0", http.NoBody)
	w := httptest.NewRecorder()
	a.GetBlobReplacements(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetBlobReplacements_DBError(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return fmt.Errorf("db error")
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetBlobReplacements(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestGetMempoolPressure_Success(t *testing.T) {
	oldest := time.Now().Add(-5 * time.Minute).UTC()
	newest := time.Now().Add(-30 * time.Second).UTC()
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			switch {
			case strings.Contains(query, "FROM block_metrics"):
				if got := args[0]; got != 42 {
					t.Fatalf("expected network arg 42, got %v", got)
				}
				baseFee := dest.(*string)
				*baseFee = "1000"
				return nil
			case strings.Contains(query, "limited_pending"):
				if got := args[0]; got != 42 {
					t.Fatalf("expected network arg 42, got %v", got)
				}
				if got := args[1]; got != mempoolPressureSampleLimit+1 {
					t.Fatalf("expected sample overflow limit %d, got %v", mempoolPressureSampleLimit+1, got)
				}
				if got := args[2]; got != mempoolPressureSampleLimit {
					t.Fatalf("expected sample limit %d, got %v", mempoolPressureSampleLimit, got)
				}
				if got := args[3]; got != "1000" {
					t.Fatalf("expected latest base fee arg 1000, got %v", got)
				}
				pressure := dest.(*mempoolPressureAggregate)
				*pressure = mempoolPressureAggregate{
					PendingBlobCount:     3,
					PendingBlobGas:       393216,
					PendingUniqueSenders: 2,
					MaxFeeMin:            "900",
					MaxFeeAvg:            "1300",
					MaxFeeMedian:         "1200",
					MaxFeeP95:            "1800",
					MaxFeeMax:            "1800",
					OldestAgeSeconds:     300,
					NewestAgeSeconds:     30,
					AverageAgeSeconds:    120,
					OldestTimestamp:      sql.NullTime{Time: oldest, Valid: true},
					NewestTimestamp:      sql.NullTime{Time: newest, Valid: true},
					LikelyIncludable:     2,
					Underpriced:          1,
					SampleTruncated:      true,
				}
				return nil
			default:
				t.Fatalf("unexpected query: %s", query)
				return nil
			}
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetMempoolPressure(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		Success bool                    `json:"success"`
		Data    MempoolPressureResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Success {
		t.Fatal("expected success=true")
	}
	if resp.Data.PendingBlobCount != 3 {
		t.Fatalf("expected pending blob count 3, got %d", resp.Data.PendingBlobCount)
	}
	if resp.Data.MaxFeePerBlobGas.P95 != "1800" {
		t.Fatalf("expected p95 1800, got %q", resp.Data.MaxFeePerBlobGas.P95)
	}
	if !resp.Data.Includability.PricingAvailable {
		t.Fatal("expected pricing to be available")
	}
	if resp.Data.Includability.LikelyIncludableCount != 2 || resp.Data.Includability.UnderpricedCount != 1 {
		t.Fatalf("unexpected includability counts: %+v", resp.Data.Includability)
	}
	if !resp.Data.SampleTruncated {
		t.Fatal("expected sample_truncated=true")
	}
}

func TestGetMempoolPressure_NoBlockMetrics(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			switch {
			case strings.Contains(query, "FROM block_metrics"):
				return sql.ErrNoRows
			case strings.Contains(query, "limited_pending"):
				if args[3] != nil {
					t.Fatalf("expected nil base fee arg without block metrics, got %v", args[3])
				}
				pressure := dest.(*mempoolPressureAggregate)
				*pressure = mempoolPressureAggregate{
					PendingBlobCount:     2,
					PendingBlobGas:       262144,
					PendingUniqueSenders: 2,
					MaxFeeMin:            "0",
					MaxFeeAvg:            "0",
					MaxFeeMedian:         "0",
					MaxFeeP95:            "0",
					MaxFeeMax:            "0",
					UnknownPricing:       2,
				}
				return nil
			default:
				t.Fatalf("unexpected query: %s", query)
				return nil
			}
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetMempoolPressure(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		Success bool                    `json:"success"`
		Data    MempoolPressureResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Data.Includability.PricingAvailable {
		t.Fatal("expected pricing_available=false")
	}
	if resp.Data.Includability.LatestBlobBaseFee != "0" {
		t.Fatalf("expected default latest base fee 0, got %q", resp.Data.Includability.LatestBlobBaseFee)
	}
	if resp.Data.Includability.UnknownPricingCount != 2 {
		t.Fatalf("expected unknown pricing count 2, got %d", resp.Data.Includability.UnknownPricingCount)
	}
}

func TestGetMempoolPressure_CacheHit(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			t.Fatal("DB should not be called on cache hit")
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	a.mempoolCache[42] = mempoolPressureCacheEntry{
		response: MempoolPressureResponse{
			ChainID:          42,
			PendingBlobCount: 5,
		},
		expiresAt: time.Now().Add(time.Minute),
	}
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetMempoolPressure(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		Success bool                    `json:"success"`
		Data    MempoolPressureResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Success || resp.Data.PendingBlobCount != 5 {
		t.Fatalf("unexpected cached response: %+v", resp)
	}
}

func TestGetMempoolPressure_BaseFeeDBError(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if strings.Contains(query, "FROM block_metrics") {
				return fmt.Errorf("base fee query failed")
			}
			t.Fatalf("unexpected query after base fee failure: %s", query)
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetMempoolPressure(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestGetMempoolPressure_AggregateDBError(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			switch {
			case strings.Contains(query, "FROM block_metrics"):
				baseFee := dest.(*string)
				*baseFee = "1000"
				return nil
			case strings.Contains(query, "limited_pending"):
				return fmt.Errorf("pressure query failed")
			default:
				t.Fatalf("unexpected query: %s", query)
				return nil
			}
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetMempoolPressure(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestGetBlobByTxHash_Success(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			blob := dest.(*models.Blob)
			*blob = models.Blob{
				ChainID:           42,
				BlockNumber:       100,
				TxHash:            validTestTxHash,
				FromAddress:       "0x123",
				BlobSizeBytes:     131072,
				BaseFeePerBlobGas: "1000",
				TipPerBlobGas:     "100",
				TotalCostWei:      "0.001",
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
	wantCache := fmt.Sprintf("public, max-age=%d, s-maxage=%d",
		int(confirmedBlobCacheTTL.Seconds()), int(confirmedBlobEdgeTTL.Seconds()))
	if got := w.Header().Get("Cache-Control"); got != wantCache {
		t.Errorf("confirmed blob Cache-Control = %q, want %q", got, wantCache)
	}
}

func TestGetBlobByTxHash_PendingNotCached(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			blob := dest.(*models.Blob)
			*blob = models.Blob{
				ChainID:           42,
				BlockNumber:       models.PendingBlockNumber,
				TxHash:            validTestTxHash,
				FromAddress:       "0x123",
				BlobSizeBytes:     131072,
				BaseFeePerBlobGas: "1000",
				TipPerBlobGas:     "100",
				TotalCostWei:      "1",
				Timestamp:         time.Now(),
				Confirmed:         false,
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
	if got := w.Header().Get("Cache-Control"); got != "" {
		t.Errorf("pending blob must not be cached, got Cache-Control = %q", got)
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

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
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
	a := newTestAPI()
	a.networks = map[int]config.NetworkConfig{}
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

func TestGetBlobByTxHash_WithBlobData(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			setStructResult(dest, &models.Blob{
				ChainID:           42,
				BlockNumber:       100,
				BlobIndex:         0,
				TxHash:            validTestTxHash,
				FromAddress:       "0xsender",
				BlobSizeBytes:     131072,
				BaseFeePerBlobGas: "1000000",
				TipPerBlobGas:     "500",
				TotalCostWei:      "0.001",
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

func TestGetBlobByTxHash_BadNetwork(t *testing.T) {
	a := newTestAPI()
	a.networks = map[int]config.NetworkConfig{}
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

func TestGetBlobByVersionedHash_Success(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if query != queryBlobByVersionedHash {
				t.Errorf("unexpected query: %q", query)
			}
			if len(args) != 2 || args[0] != validTestVersionedHash || args[1] != 42 {
				t.Errorf("unexpected query args: %v", args)
			}
			versionedHash := validTestVersionedHash
			blob := dest.(*models.Blob)
			*blob = models.Blob{
				ChainID:           42,
				BlockNumber:       100,
				TxHash:            validTestTxHash,
				FromAddress:       "0x123",
				BlobSizeBytes:     131072,
				BaseFeePerBlobGas: "1000",
				TipPerBlobGas:     "100",
				TotalCostWei:      "0.001",
				Timestamp:         time.Now(),
				Confirmed:         true,
				VersionedHash:     &versionedHash,
				VersionedHashes:   pq.StringArray{validTestVersionedHash},
			}
			return nil
		},
	}
	a := newTestAPIWithDB(db)

	r := chi.NewRouter()
	r.Get("/blob/by-hash/{versionedHash}", a.GetBlobByVersionedHash)

	req := httptest.NewRequest(http.MethodGet, "/blob/by-hash/"+validTestVersionedHash, http.NoBody)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		Success bool         `json:"success"`
		Data    BlobResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Success {
		t.Error("expected Success=true")
	}
	if resp.Data.TxHash != validTestTxHash {
		t.Errorf("tx_hash = %q, want %q", resp.Data.TxHash, validTestTxHash)
	}
	if resp.Data.BlockNumber == nil || *resp.Data.BlockNumber != 100 {
		t.Errorf("block_number = %v, want 100", resp.Data.BlockNumber)
	}
	if len(resp.Data.VersionedHashes) != 1 || resp.Data.VersionedHashes[0] != validTestVersionedHash {
		t.Errorf("versioned_hashes = %v, want [%s]", resp.Data.VersionedHashes, validTestVersionedHash)
	}
	wantCache := fmt.Sprintf("public, max-age=%d, s-maxage=%d",
		int(confirmedBlobCacheTTL.Seconds()), int(confirmedBlobEdgeTTL.Seconds()))
	if got := w.Header().Get("Cache-Control"); got != wantCache {
		t.Errorf("confirmed blob Cache-Control = %q, want %q", got, wantCache)
	}
}

func TestGetBlobByVersionedHash_NormalizesCase(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if len(args) != 2 || args[0] != validTestVersionedHash {
				t.Errorf("expected lowercased hash arg, got %v", args)
			}
			setStructResult(dest, &models.Blob{
				ChainID:           42,
				BlockNumber:       100,
				TxHash:            validTestTxHash,
				FromAddress:       "0x123",
				BlobSizeBytes:     131072,
				BaseFeePerBlobGas: "1000",
				TipPerBlobGas:     "100",
				TotalCostWei:      "0.001",
				Timestamp:         time.Now(),
				Confirmed:         true,
			})
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	rctx := chi.NewRouteContext()
	// Uppercase hex digits must still match: normalization runs before both
	// the version check and the case-sensitive array containment query.
	rctx.URLParams.Add("versionedHash", "0x01"+strings.ToUpper(strings.TrimPrefix(validTestVersionedHash, "0x01")))
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	a.GetBlobByVersionedHash(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetBlobByVersionedHash_PendingNotCached(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			setStructResult(dest, &models.Blob{
				ChainID:           42,
				BlockNumber:       models.PendingBlockNumber,
				TxHash:            validTestTxHash,
				FromAddress:       "0x123",
				BlobSizeBytes:     131072,
				BaseFeePerBlobGas: "1000",
				TipPerBlobGas:     "100",
				TotalCostWei:      "1",
				Timestamp:         time.Now(),
				Confirmed:         false,
				VersionedHashes:   pq.StringArray{validTestVersionedHash},
			})
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	r := chi.NewRouter()
	r.Get("/blob/by-hash/{versionedHash}", a.GetBlobByVersionedHash)

	req := httptest.NewRequest(http.MethodGet, "/blob/by-hash/"+validTestVersionedHash, http.NoBody)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := w.Header().Get("Cache-Control"); got != "" {
		t.Errorf("pending blob must not be cached, got Cache-Control = %q", got)
	}
}

func TestGetBlobByVersionedHash_NotFound(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return sql.ErrNoRows
		},
	}
	a := newTestAPIWithDB(db)

	r := chi.NewRouter()
	r.Get("/blob/by-hash/{versionedHash}", a.GetBlobByVersionedHash)

	req := httptest.NewRequest(http.MethodGet, "/blob/by-hash/"+validTestVersionedHash, http.NoBody)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
	var resp Response
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Success {
		t.Error("expected Success=false for 404")
	}
}

func TestGetBlobByVersionedHash_DBError(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return fmt.Errorf("db error")
		},
	}
	a := newTestAPIWithDB(db)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("versionedHash", validTestVersionedHash)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	a.GetBlobByVersionedHash(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestGetBlobByVersionedHash_BadNetwork(t *testing.T) {
	a := newTestAPI()
	a.networks = map[int]config.NetworkConfig{}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("versionedHash", validTestVersionedHash)
	req := httptest.NewRequest(http.MethodGet, "/?network=999", http.NoBody)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	a.GetBlobByVersionedHash(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetBlobByVersionedHash_EmptyHash(t *testing.T) {
	a := newTestAPI()

	// Call directly - versionedHash will be empty from chi.URLParam
	req := httptest.NewRequest(http.MethodGet, "/blob/by-hash/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetBlobByVersionedHash(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetBlobByVersionedHash_InvalidFormat(t *testing.T) {
	testCases := []struct {
		name          string
		versionedHash string
	}{
		{
			name:          "missing 0x prefix with 64 hex chars",
			versionedHash: "01" + strings.Repeat("b", 62),
		},
		{
			name:          "with 0x prefix but wrong length",
			versionedHash: "0x01bb",
		},
		{
			name:          "with 0x prefix and non-hex characters",
			versionedHash: "0x01" + strings.Repeat("g", 62),
		},
		{
			name:          "completely invalid string",
			versionedHash: "invalid-hash",
		},
		{
			name:          "well-formed hash with wrong version byte",
			versionedHash: "0x02" + strings.Repeat("b", 62),
		},
		{
			name:          "well-formed hash with no version byte",
			versionedHash: validTestTxHash,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestAPIWithDB(&mockDB{})
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("versionedHash", tc.versionedHash)
			req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
			req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
			w := httptest.NewRecorder()
			a.GetBlobByVersionedHash(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for invalid versioned hash, got %d", w.Code)
			}
		})
	}
}

func TestGetBlobPricing_Success(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			metrics := dest.(*[]models.BlockMetrics)
			*metrics = []models.BlockMetrics{
				{
					ChainID:          42,
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
	var resp struct {
		Success bool            `json:"success"`
		Data    PricingResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Success {
		t.Error("expected Success=true")
	}
	if got := resp.Data.CurrentBaseFeeGwei; got != "0.000000001" {
		t.Fatalf("current_base_fee_gwei = %v, want 0.000000001", got)
	}
	if got := resp.Data.PredictedNextFeeGwei; got == "" {
		t.Fatalf("predicted_next_fee_gwei = %v, want non-empty", got)
	}
	if len(resp.Data.RecentBlocks) == 0 {
		t.Fatal("expected at least one recent block")
	}
	if got := resp.Data.RecentBlocks[0].BlobBaseFeeGwei; got != "0.000000001" {
		t.Fatalf("blob_base_fee_gwei = %v, want 0.000000001", got)
	}
	if resp.Data.MarketPressure.PredictedDirection != marketPressureDirectionFlat {
		t.Errorf("expected flat market pressure direction, got %q", resp.Data.MarketPressure.PredictedDirection)
	}
	if resp.Data.MarketPressure.NextBlockFeeEstimate.Low == "" {
		t.Error("expected low next-block fee estimate")
	}
	if resp.Data.MarketPressure.NextBlockFeeEstimate.High == "" {
		t.Error("expected high next-block fee estimate")
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
	req := httptest.NewRequest(http.MethodGet, "/?blocks=abc", http.NoBody)
	w := httptest.NewRecorder()
	a.GetBlobPricing(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// TestGetBlobPricing_BlocksClamping verifies out-of-range blocks values are
// clamped rather than rejected: below 1 falls back to the default window,
// above MaxPricingBlocks caps there, and in-range values (including the
// frontend's 1h request of 300) pass through unchanged.
func TestGetBlobPricing_BlocksClamping(t *testing.T) {
	tests := []struct {
		name   string
		target string
		want   int
	}{
		{"default when absent", "/", DefaultPricingBlocks},
		{"zero clamps to default", "/?blocks=0", DefaultPricingBlocks},
		{"negative clamps to default", "/?blocks=-1", DefaultPricingBlocks},
		{"one hour of mainnet slots", "/?blocks=300", 300},
		{"max passes through", "/?blocks=512", MaxPricingBlocks},
		{"above max clamps to max", "/?blocks=9999", MaxPricingBlocks},
		{"int overflow clamps to max", "/?blocks=99999999999999999999", MaxPricingBlocks},
		{"negative overflow clamps to default", "/?blocks=-99999999999999999999", DefaultPricingBlocks},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotLimit interface{}
			db := &mockDB{
				selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
					gotLimit = args[1]
					return nil
				},
			}
			a := newTestAPIWithDB(db)
			w := httptest.NewRecorder()
			a.GetBlobPricing(w, httptest.NewRequest(http.MethodGet, tt.target, http.NoBody))

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			if gotLimit != tt.want {
				t.Fatalf("query limit = %v, want %d", gotLimit, tt.want)
			}
		})
	}
}

func TestGetBlobPricing_BadNetwork(t *testing.T) {
	a := newTestAPI()
	a.networks = map[int]config.NetworkConfig{}
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
					ChainID:           42,
					BlockNumber:       models.PendingBlockNumber,
					TxHash:            "0xpending",
					FromAddress:       "0xsender",
					BlobSizeBytes:     131072,
					BaseFeePerBlobGas: "1000000",
					TipPerBlobGas:     "500",
					TotalCostWei:      "0.001",
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

func TestGetLatestBlobs_WithAddressFilter(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if !strings.Contains(query, "from_address") {
				t.Error("expected address-filtered query")
			}
			blobs := dest.(*[]models.Blob)
			*blobs = []models.Blob{
				{
					ChainID:           42,
					BlockNumber:       100,
					BlobIndex:         0,
					TxHash:            "0xabc",
					FromAddress:       validTestAddress,
					BlobSizeBytes:     131072,
					BaseFeePerBlobGas: "1000",
					TipPerBlobGas:     "100",
					TotalCostWei:      "0.001",
					Timestamp:         time.Now(),
					Confirmed:         true,
				},
			}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?from="+validTestAddress, http.NoBody)
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

func TestGetLatestBlobs_InvalidAddress(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?from=notanaddress", http.NoBody)
	w := httptest.NewRecorder()
	a.GetLatestBlobs(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetLatestBlobs_AddressFilterDBError(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return fmt.Errorf("db error")
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?from="+validTestAddress, http.NoBody)
	w := httptest.NewRecorder()
	a.GetLatestBlobs(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestGetMempoolBlobs_WithAddressFilter(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if !strings.Contains(query, "from_address") {
				t.Error("expected address-filtered query")
			}
			blobs := dest.(*[]models.Blob)
			*blobs = []models.Blob{}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?from="+validTestAddress, http.NoBody)
	w := httptest.NewRecorder()
	a.GetMempoolBlobs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetMempoolBlobs_InvalidAddress(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?from=xyz", http.NoBody)
	w := httptest.NewRecorder()
	a.GetMempoolBlobs(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetMempoolBlobs_AddressFilterDBError(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return fmt.Errorf("db error")
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?from="+validTestAddress, http.NoBody)
	w := httptest.NewRecorder()
	a.GetMempoolBlobs(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

// firstBlobRawBlockNumber decodes the response envelope and returns the raw
// JSON token for data[0].block_number, so tests can distinguish a literal null
// from a number on the wire.
func firstBlobRawBlockNumber(t *testing.T, body []byte) json.RawMessage {
	t.Helper()
	var envelope struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(envelope.Data) == 0 {
		t.Fatalf("expected at least one blob in response, got none")
	}
	raw, ok := envelope.Data[0]["block_number"]
	if !ok {
		t.Fatalf("block_number field missing from blob response")
	}
	return raw
}

func TestToBlobResponse_PendingBlockNumberSerializesNull(t *testing.T) {
	resp := toBlobResponse(models.Blob{
		ChainID:           42,
		BlockNumber:       models.PendingBlockNumber,
		TxHash:            "0xpending",
		FromAddress:       "0xsender",
		BaseFeePerBlobGas: "1000000",
		TipPerBlobGas:     "500",
		TotalCostWei:      "0",
		Timestamp:         time.Now(),
		Confirmed:         false,
	}, "testnet")

	if resp.BlockNumber != nil {
		t.Fatalf("expected nil BlockNumber for pending blob, got %d", *resp.BlockNumber)
	}

	encoded, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("failed to marshal response: %v", err)
	}
	raw := firstBlobRawBlockNumber(t, []byte(`{"data":[`+string(encoded)+`]}`))
	if string(raw) != "null" {
		t.Fatalf("expected block_number null on the wire, got %s", raw)
	}
}

func TestToBlobResponse_ConfirmedBlockNumberSerializesNumber(t *testing.T) {
	resp := toBlobResponse(models.Blob{
		ChainID:           42,
		BlockNumber:       100,
		TxHash:            "0xconfirmed",
		FromAddress:       "0xsender",
		BaseFeePerBlobGas: "1000000",
		TipPerBlobGas:     "500",
		TotalCostWei:      "0",
		Timestamp:         time.Now(),
		Confirmed:         true,
	}, "testnet")

	if resp.BlockNumber == nil {
		t.Fatal("expected non-nil BlockNumber for confirmed blob")
	}
	if *resp.BlockNumber != 100 {
		t.Fatalf("expected BlockNumber 100, got %d", *resp.BlockNumber)
	}

	encoded, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("failed to marshal response: %v", err)
	}
	raw := firstBlobRawBlockNumber(t, []byte(`{"data":[`+string(encoded)+`]}`))
	if string(raw) != "100" {
		t.Fatalf("expected block_number 100 on the wire, got %s", raw)
	}
}

func TestGetMempoolBlobs_PendingBlockNumberNull(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			setSliceResult(dest, []models.Blob{
				{
					ChainID:           42,
					BlockNumber:       models.PendingBlockNumber,
					TxHash:            "0xpending",
					FromAddress:       "0xsender",
					BlobSizeBytes:     131072,
					BaseFeePerBlobGas: "1000000",
					TipPerBlobGas:     "500",
					TotalCostWei:      "0",
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
	raw := firstBlobRawBlockNumber(t, w.Body.Bytes())
	if string(raw) != "null" {
		t.Fatalf("expected pending block_number null on the wire, got %s", raw)
	}
}

func TestGetLatestBlobs_ConfirmedBlockNumberNumber(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			setSliceResult(dest, []models.Blob{
				{
					ChainID:           42,
					BlockNumber:       100,
					TxHash:            "0xconfirmed",
					FromAddress:       "0xsender",
					BlobSizeBytes:     131072,
					BaseFeePerBlobGas: "1000000",
					TipPerBlobGas:     "500",
					TotalCostWei:      "0",
					Timestamp:         time.Now(),
					Confirmed:         true,
				},
			})
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?limit=10", http.NoBody)
	w := httptest.NewRecorder()
	a.GetLatestBlobs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	raw := firstBlobRawBlockNumber(t, w.Body.Bytes())
	if string(raw) != "100" {
		t.Fatalf("expected confirmed block_number 100 on the wire, got %s", raw)
	}
}

// TestBlobListCaches_HitDBOnce verifies the hot list endpoints serve repeat
// requests from the in-process cache instead of re-querying the database, and
// that the address-filtered and paginated shapes bypass the cache.
func TestBlobListCaches_HitDBOnce(t *testing.T) {
	cases := []struct {
		name    string
		handler func(a *API) http.HandlerFunc
	}{
		{"latest", func(a *API) http.HandlerFunc { return a.GetLatestBlobs }},
		{"mempool", func(a *API) http.HandlerFunc { return a.GetMempoolBlobs }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			db := &mockDB{
				selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
					calls++
					blobs := dest.(*[]models.Blob)
					*blobs = []models.Blob{{
						ChainID: 42, BlockNumber: 100, TxHash: "0xabc", FromAddress: "0x123",
						BaseFeePerBlobGas: "1000", TipPerBlobGas: "100", TotalCostWei: "1",
						Timestamp: time.Now(), Confirmed: true,
					}}
					return nil
				},
			}
			a := newTestAPIWithDB(db)
			handler := tc.handler(a)

			for i := 0; i < 3; i++ {
				w := httptest.NewRecorder()
				handler(w, httptest.NewRequest(http.MethodGet, "/?limit=10", http.NoBody))
				if w.Code != http.StatusOK {
					t.Fatalf("request %d: expected 200, got %d", i, w.Code)
				}
			}
			if calls != 1 {
				t.Fatalf("expected 1 DB call for identical cacheable requests, got %d", calls)
			}

			// A different limit is a different cache key.
			w := httptest.NewRecorder()
			handler(w, httptest.NewRequest(http.MethodGet, "/?limit=20", http.NoBody))
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			if calls != 2 {
				t.Fatalf("expected a DB call for a new limit, got %d total", calls)
			}

			// Pagination and address filters bypass the cache entirely.
			for _, target := range []string{"/?limit=10&offset=5", "/?limit=10&from=0x000000000000000000000000000000000000dEaD"} {
				before := calls
				w := httptest.NewRecorder()
				handler(w, httptest.NewRequest(http.MethodGet, target, http.NoBody))
				if w.Code != http.StatusOK {
					t.Fatalf("%s: expected 200, got %d", target, w.Code)
				}
				if calls != before+1 {
					t.Fatalf("%s: expected an uncached DB call", target)
				}
			}
		})
	}
}

func TestGetMempoolBlobs_SetsCacheControl(t *testing.T) {
	a := newTestAPIWithDB(&mockDB{})
	w := httptest.NewRecorder()
	a.GetMempoolBlobs(w, httptest.NewRequest(http.MethodGet, "/", http.NoBody))

	wantCache := fmt.Sprintf("public, max-age=%d, s-maxage=%d",
		int(mempoolBlobsCacheTTL.Seconds()), int(mempoolBlobsEdgeTTL.Seconds()))
	if got := w.Header().Get("Cache-Control"); got != wantCache {
		t.Errorf("Cache-Control = %q, want %q", got, wantCache)
	}
}

// TestGetBlobPricing_CacheHitsDBOnce verifies repeat pricing requests for the
// same (network, blocks) key are served from the in-process cache.
func TestGetBlobPricing_CacheHitsDBOnce(t *testing.T) {
	calls := 0
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			calls++
			metrics := dest.(*[]models.BlockMetrics)
			*metrics = []models.BlockMetrics{{
				ChainID: 42, BlockNumber: 100, BlockTimestamp: time.Now(),
				BlobCount: 3, BlobGasUsed: 393216, BlobGasTarget: 786432, BlobGasLimit: 1179648,
				BlobBaseFee: "1000", UtilizationRatio: "0.33",
				BlobParamsTarget: 6, BlobParamsMax: 9, UpdateFraction: 5007716,
			}}
			return nil
		},
	}
	a := newTestAPIWithDB(db)

	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		a.GetBlobPricing(w, httptest.NewRequest(http.MethodGet, "/?blocks=30", http.NoBody))
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i, w.Code)
		}
	}
	if calls != 1 {
		t.Fatalf("expected 1 DB call for identical pricing requests, got %d", calls)
	}

	// A different blocks value is a distinct cache key.
	w := httptest.NewRecorder()
	a.GetBlobPricing(w, httptest.NewRequest(http.MethodGet, "/?blocks=100", http.NoBody))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if calls != 2 {
		t.Fatalf("expected a DB call for a new blocks value, got %d total", calls)
	}
}

// TestCacheInvalidation_DropsPerNetworkEntries verifies the poller-driven
// invalidation hooks clear exactly the target network's cached responses.
func TestCacheInvalidation_DropsPerNetworkEntries(t *testing.T) {
	a := newTestAPI()
	future := time.Now().Add(time.Hour)

	a.latestBlobsCache["latest_blobs:42:10"] = blobListCacheEntry{expiresAt: future}
	a.latestBlobsCache["latest_blobs:7:10"] = blobListCacheEntry{expiresAt: future}
	a.pricingCache["pricing:42:30"] = pricingCacheEntry{expiresAt: future}
	a.statsCache[42] = statsCacheEntry{expiresAt: future}
	a.mempoolBlobsCache["mempool_blobs:42:10"] = blobListCacheEntry{expiresAt: future}
	a.mempoolCache[42] = mempoolPressureCacheEntry{expiresAt: future}

	a.invalidateBlockCaches(42)
	if _, ok := a.latestBlobsCache["latest_blobs:42:10"]; ok {
		t.Error("latest blobs cache for network 42 should be dropped")
	}
	if _, ok := a.latestBlobsCache["latest_blobs:7:10"]; !ok {
		t.Error("other networks' latest blobs cache must survive")
	}
	if _, ok := a.pricingCache["pricing:42:30"]; ok {
		t.Error("pricing cache for network 42 should be dropped")
	}
	if _, ok := a.statsCache[42]; ok {
		t.Error("stats cache for network 42 should be dropped")
	}
	if _, ok := a.mempoolBlobsCache["mempool_blobs:42:10"]; !ok {
		t.Error("mempool caches must survive block invalidation")
	}

	a.invalidateMempoolCaches(42)
	if _, ok := a.mempoolBlobsCache["mempool_blobs:42:10"]; ok {
		t.Error("mempool blobs cache for network 42 should be dropped")
	}
	if _, ok := a.mempoolCache[42]; ok {
		t.Error("mempool pressure cache for network 42 should be dropped")
	}
}
