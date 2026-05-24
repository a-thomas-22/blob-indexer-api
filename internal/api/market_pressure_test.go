package api

import (
	"math/big"
	"testing"
	"time"

	"github.com/a-thomas-22/blob-indexer-api/internal/blobparams"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
)

func TestBuildMarketPressure_MixedUtilization(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	cfg := blobparams.ChainConfigForID(42)
	metrics := []models.BlockMetrics{
		testPressureMetric(104, now, 786432, 40_000_000),
		testPressureMetric(103, now.Add(-12*time.Second), 786432, 30_000_000),
		testPressureMetric(102, now.Add(-24*time.Second), 393216, 20_000_000),
		testPressureMetric(101, now.Add(-36*time.Second), 0, 0),
	}

	got := buildMarketPressure(metrics, cfg)

	if got.RecentBlocksAboveTarget != 2 {
		t.Fatalf("RecentBlocksAboveTarget = %d, want 2", got.RecentBlocksAboveTarget)
	}
	if got.ConsecutiveFullBlocks != 2 {
		t.Fatalf("ConsecutiveFullBlocks = %d, want 2", got.ConsecutiveFullBlocks)
	}
	if got.PercentRecentBlocksAtMax != 50 {
		t.Fatalf("PercentRecentBlocksAtMax = %f, want 50", got.PercentRecentBlocksAtMax)
	}
	if got.PredictedDirection != marketPressureDirectionUp {
		t.Fatalf("PredictedDirection = %q, want %q", got.PredictedDirection, marketPressureDirectionUp)
	}

	low, ok := new(big.Int).SetString(got.NextBlockFeeEstimate.Low, 10)
	if !ok {
		t.Fatalf("failed to parse low fee estimate %q", got.NextBlockFeeEstimate.Low)
	}
	high, ok := new(big.Int).SetString(got.NextBlockFeeEstimate.High, 10)
	if !ok {
		t.Fatalf("failed to parse high fee estimate %q", got.NextBlockFeeEstimate.High)
	}
	if low.Sign() <= 0 {
		t.Fatalf("low fee estimate = %s, want positive", low.String())
	}
	if high.Cmp(low) < 0 {
		t.Fatalf("high fee estimate %s is lower than low estimate %s", high.String(), low.String())
	}
}

func TestBuildMarketPressure_EmptyMetrics(t *testing.T) {
	got := buildMarketPressure(nil, blobparams.ChainConfigForID(42))

	if got.RecentBlocksAboveTarget != 0 {
		t.Fatalf("RecentBlocksAboveTarget = %d, want 0", got.RecentBlocksAboveTarget)
	}
	if got.ConsecutiveFullBlocks != 0 {
		t.Fatalf("ConsecutiveFullBlocks = %d, want 0", got.ConsecutiveFullBlocks)
	}
	if got.PercentRecentBlocksAtMax != 0 {
		t.Fatalf("PercentRecentBlocksAtMax = %f, want 0", got.PercentRecentBlocksAtMax)
	}
	if got.PredictedDirection != marketPressureDirectionFlat {
		t.Fatalf("PredictedDirection = %q, want %q", got.PredictedDirection, marketPressureDirectionFlat)
	}
	if got.NextBlockFeeEstimate.Low != "0" || got.NextBlockFeeEstimate.High != "0" {
		t.Fatalf("NextBlockFeeEstimate = %+v, want zero range", got.NextBlockFeeEstimate)
	}
}

func testPressureMetric(blockNumber int64, timestamp time.Time, blobGasUsed, excessBlobGas int64) models.BlockMetrics {
	return models.BlockMetrics{
		NetworkID:        42,
		BlockNumber:      blockNumber,
		BlockTimestamp:   timestamp,
		BlobCount:        int(blobGasUsed / 131072),
		BlobGasUsed:      blobGasUsed,
		BlobGasTarget:    393216,
		BlobGasLimit:     786432,
		ExcessBlobGas:    excessBlobGas,
		BlobBaseFee:      "1",
		UtilizationRatio: "0.000000",
		BlobParamsTarget: 3,
		BlobParamsMax:    6,
		UpdateFraction:   3338477,
	}
}
