package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
)

func TestBlockPricingResponseOccupancy(t *testing.T) {
	tests := []struct {
		name    string
		metrics models.BlockMetrics
		want    BlockPricingResponse
	}{
		{
			name: "empty block",
			metrics: models.BlockMetrics{
				BlobParamsTarget: 3,
				BlobParamsMax:    6,
			},
			want: BlockPricingResponse{
				TargetBlobs:        3,
				MaxBlobs:           6,
				AvailableBlobs:     6,
				UtilizationPercent: 0,
				IsFull:             false,
				IsAboveTarget:      false,
			},
		},
		{
			name: "at target",
			metrics: models.BlockMetrics{
				BlobCount:        3,
				BlobParamsTarget: 3,
				BlobParamsMax:    6,
			},
			want: BlockPricingResponse{
				TargetBlobs:        3,
				MaxBlobs:           6,
				AvailableBlobs:     3,
				UtilizationPercent: 50,
				IsFull:             false,
				IsAboveTarget:      false,
			},
		},
		{
			name: "above target",
			metrics: models.BlockMetrics{
				BlobCount:        4,
				BlobParamsTarget: 3,
				BlobParamsMax:    6,
			},
			want: BlockPricingResponse{
				TargetBlobs:        3,
				MaxBlobs:           6,
				AvailableBlobs:     2,
				UtilizationPercent: 66.67,
				IsFull:             false,
				IsAboveTarget:      true,
			},
		},
		{
			name: "full block",
			metrics: models.BlockMetrics{
				BlobCount:        6,
				BlobParamsTarget: 3,
				BlobParamsMax:    6,
			},
			want: BlockPricingResponse{
				TargetBlobs:        3,
				MaxBlobs:           6,
				AvailableBlobs:     0,
				UtilizationPercent: 100,
				IsFull:             true,
				IsAboveTarget:      true,
			},
		},
		{
			name: "overfull block clamps availability and percent",
			metrics: models.BlockMetrics{
				BlobCount:        7,
				BlobParamsTarget: 3,
				BlobParamsMax:    6,
			},
			want: BlockPricingResponse{
				TargetBlobs:        3,
				MaxBlobs:           6,
				AvailableBlobs:     0,
				UtilizationPercent: 100,
				IsFull:             true,
				IsAboveTarget:      true,
			},
		},
		{
			name: "gas fallback",
			metrics: models.BlockMetrics{
				BlobCount:     2,
				BlobGasTarget: 393216,
				BlobGasLimit:  786432,
			},
			want: BlockPricingResponse{
				TargetBlobs:        3,
				MaxBlobs:           6,
				AvailableBlobs:     4,
				UtilizationPercent: 33.33,
				IsFull:             false,
				IsAboveTarget:      false,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toBlockPricingResponse(tt.metrics)
			assertOccupancyFields(t, got, tt.want)
		})
	}
}

func TestGetBlobPricingIncludesOccupancyFields(t *testing.T) {
	blockTime := time.Unix(1700000000, 0).UTC()
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			metrics := dest.(*[]models.BlockMetrics)
			*metrics = []models.BlockMetrics{
				{
					NetworkID:        42,
					BlockNumber:      100,
					BlockTimestamp:   blockTime,
					BlobCount:        4,
					BlobGasUsed:      524288,
					BlobGasTarget:    393216,
					BlobGasLimit:     786432,
					ExcessBlobGas:    100000,
					BlobBaseFee:      "1",
					UtilizationRatio: "1.333333",
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
		t.Fatal("expected success=true")
	}
	if len(resp.Data.RecentBlocks) != 1 {
		t.Fatalf("expected 1 recent block, got %d", len(resp.Data.RecentBlocks))
	}

	block := resp.Data.RecentBlocks[0]
	assertOccupancyFields(t, block, BlockPricingResponse{
		TargetBlobs:        3,
		MaxBlobs:           6,
		AvailableBlobs:     2,
		UtilizationPercent: 66.67,
		IsFull:             false,
		IsAboveTarget:      true,
	})

	if block.BlobParamsTarget != 3 {
		t.Errorf("expected legacy blob_params_target=3, got %d", block.BlobParamsTarget)
	}
	if block.BlobParamsMax != 6 {
		t.Errorf("expected legacy blob_params_max=6, got %d", block.BlobParamsMax)
	}
	if block.UtilizationRatio != "1.333333" {
		t.Errorf("expected legacy utilization_ratio=1.333333, got %q", block.UtilizationRatio)
	}
}

func assertOccupancyFields(t *testing.T, got, want BlockPricingResponse) {
	t.Helper()
	if got.TargetBlobs != want.TargetBlobs {
		t.Errorf("TargetBlobs = %d, want %d", got.TargetBlobs, want.TargetBlobs)
	}
	if got.MaxBlobs != want.MaxBlobs {
		t.Errorf("MaxBlobs = %d, want %d", got.MaxBlobs, want.MaxBlobs)
	}
	if got.AvailableBlobs != want.AvailableBlobs {
		t.Errorf("AvailableBlobs = %d, want %d", got.AvailableBlobs, want.AvailableBlobs)
	}
	if got.UtilizationPercent != want.UtilizationPercent {
		t.Errorf("UtilizationPercent = %v, want %v", got.UtilizationPercent, want.UtilizationPercent)
	}
	if got.IsFull != want.IsFull {
		t.Errorf("IsFull = %v, want %v", got.IsFull, want.IsFull)
	}
	if got.IsAboveTarget != want.IsAboveTarget {
		t.Errorf("IsAboveTarget = %v, want %v", got.IsAboveTarget, want.IsAboveTarget)
	}
}
