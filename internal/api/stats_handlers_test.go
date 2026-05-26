package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
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

func TestGetBlobStats_PopulatesExplicitWeiFields(t *testing.T) {
	db := &mockDB{
		getFn: func(_ context.Context, dest interface{}, _ string, _ ...interface{}) error {
			setStatsFields(dest, map[string]interface{}{
				"TotalBlobs":          7,
				"TotalConfirmedBlobs": 5,
				"TotalPendingBlobs":   2,
				"AverageBaseFee":      "4841467206.84506683",
				"AverageTip":          "15678762992.04263056",
				"AverageTotalCost":    "2207855919292172.4863",
				"LastIndexedTime":     time.Unix(1700000000, 0).UTC(),
			})
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

	var resp struct {
		Success bool          `json:"success"`
		Data    StatsResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Success {
		t.Fatal("expected success response")
	}
	if resp.Data.TotalBlobs != 7 || resp.Data.TotalConfirmedBlobs != 5 || resp.Data.TotalPendingBlobs != 2 {
		t.Fatalf("unexpected blob counts: %+v", resp.Data)
	}
	if resp.Data.AverageBaseFeePerBlobGasWei != "4841467206.84506683" ||
		resp.Data.AverageTipPerBlobGasWei != "15678762992.04263056" ||
		resp.Data.AverageTotalCostWei != "2207855919292172.4863" {
		t.Fatalf("missing explicit wei fields: %+v", resp.Data)
	}
	if resp.Data.AverageBaseFee != resp.Data.AverageBaseFeePerBlobGasWei ||
		resp.Data.AverageTip != resp.Data.AverageTipPerBlobGasWei ||
		resp.Data.AverageTotalCost != resp.Data.AverageTotalCostWei {
		t.Fatalf("deprecated aliases must mirror explicit wei fields: %+v", resp.Data)
	}
}

// setStatsFields assigns named fields into the anonymous struct passed to db.GetContext
// from GetBlobStats by reflection. No-op when dest isn't a struct (e.g. the secondary
// queryLastIndexedBlock call that scans into *string).
func setStatsFields(dest interface{}, values map[string]interface{}) {
	dv := reflect.ValueOf(dest)
	if dv.Kind() == reflect.Pointer {
		dv = dv.Elem()
	}
	if dv.Kind() != reflect.Struct {
		return
	}
	for name, value := range values {
		f := dv.FieldByName(name)
		if !f.IsValid() || !f.CanSet() {
			continue
		}
		f.Set(reflect.ValueOf(value))
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
