package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	_ "github.com/a-thomas-22/blob-indexer-api/internal/testutil"
)

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

func TestGetBlobStats_BadNetwork(t *testing.T) {
	a := newTestAPI()
	a.networks = map[int]config.NetworkConfig{}
	req := httptest.NewRequest(http.MethodGet, "/?network=999", http.NoBody)
	w := httptest.NewRecorder()
	a.GetBlobStats(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetBlobStats_CacheHit(t *testing.T) {
	db := &mockDB{
		getFn: func(_ context.Context, _ interface{}, _ string, _ ...interface{}) error {
			t.Fatal("DB should not be called on cache hit")
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	a.statsCache[42] = statsCacheEntry{
		response:  StatsResponse{NetworkID: 42, TotalBlobs: 5},
		expiresAt: time.Now().Add(time.Minute),
	}
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetBlobStats(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestStatsResponse_ExplicitWeiFieldsMarshalJSON(t *testing.T) {
	response := StatsResponse{
		AverageBaseFeePerBlobGasWei: "4841467206.84506683",
		AverageTipPerBlobGasWei:     "15678762992.04263056",
		AverageTotalCostWei:         "2207855919292172.4863",
		AverageBaseFee:              "4841467206.84506683",
		AverageTip:                  "15678762992.04263056",
		AverageTotalCost:            "2207855919292172.4863",
	}

	data, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("failed to marshal stats response: %v", err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("failed to unmarshal stats response: %v", err)
	}

	for _, field := range []string{
		"average_base_fee_per_blob_gas_wei",
		"average_tip_per_blob_gas_wei",
		"average_total_cost_wei",
		"average_base_fee",
		"average_tip",
		"average_total_cost",
	} {
		value, ok := got[field].(string)
		if !ok || value == "" {
			t.Fatalf("expected %s to be populated in JSON: %s", field, string(data))
		}
	}
}
