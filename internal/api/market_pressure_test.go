package api

import (
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/consensus/misc/eip4844"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"

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

func TestBuildMarketPressure_FallbacksAndDirections(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	metrics := []models.BlockMetrics{
		{
			ChainID:        42,
			BlockNumber:    3,
			BlockTimestamp: now,
			BlobGasUsed:    2,
			BlobGasTarget:  4,
			BlobGasLimit:   8,
			ExcessBlobGas:  6,
		},
		{
			ChainID:        42,
			BlockNumber:    2,
			BlockTimestamp: now.Add(-12 * time.Second),
			BlobGasUsed:    -1,
			BlobGasTarget:  4,
			BlobGasLimit:   8,
			ExcessBlobGas:  -1,
		},
	}

	got := buildMarketPressure(metrics, nil)

	if got.PredictedDirection != marketPressureDirectionDown {
		t.Fatalf("PredictedDirection = %q, want %q", got.PredictedDirection, marketPressureDirectionDown)
	}
	if got.RecentBlocksAboveTarget != 0 {
		t.Fatalf("RecentBlocksAboveTarget = %d, want 0", got.RecentBlocksAboveTarget)
	}
	if got.ConsecutiveFullBlocks != 0 {
		t.Fatalf("ConsecutiveFullBlocks = %d, want 0", got.ConsecutiveFullBlocks)
	}
	if got.PercentRecentBlocksAtMax != 0 {
		t.Fatalf("PercentRecentBlocksAtMax = %f, want 0", got.PercentRecentBlocksAtMax)
	}
	if got.NextBlockFeeEstimate.Low == "" || got.NextBlockFeeEstimate.High == "" {
		t.Fatalf("NextBlockFeeEstimate = %+v, want populated range", got.NextBlockFeeEstimate)
	}
}

func TestBuildMarketPressure_UsesPerMetricBlobLimits(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	metrics := []models.BlockMetrics{
		{
			ChainID:        42,
			BlockNumber:    3,
			BlockTimestamp: now,
			BlobGasUsed:    8,
			BlobGasTarget:  4,
			BlobGasLimit:   8,
			ExcessBlobGas:  6,
		},
		{
			ChainID:        42,
			BlockNumber:    2,
			BlockTimestamp: now.Add(-12 * time.Second),
			BlobGasUsed:    10,
			BlobGasTarget:  12,
			BlobGasLimit:   20,
			ExcessBlobGas:  6,
		},
	}

	got := buildMarketPressure(metrics, blobparams.ChainConfigForID(42))

	if got.RecentBlocksAboveTarget != 1 {
		t.Fatalf("RecentBlocksAboveTarget = %d, want 1", got.RecentBlocksAboveTarget)
	}
	if got.ConsecutiveFullBlocks != 1 {
		t.Fatalf("ConsecutiveFullBlocks = %d, want 1", got.ConsecutiveFullBlocks)
	}
	if got.PercentRecentBlocksAtMax != 50 {
		t.Fatalf("PercentRecentBlocksAtMax = %f, want 50", got.PercentRecentBlocksAtMax)
	}
	if got.PredictedDirection != marketPressureDirectionUp {
		t.Fatalf("PredictedDirection = %q, want %q", got.PredictedDirection, marketPressureDirectionUp)
	}
}

func TestMarketPressureHelpers(t *testing.T) {
	metric := models.BlockMetrics{BlobParamsTarget: 3, BlobParamsMax: 6}
	wantTargetGas := uint64(3 * params.BlobTxBlobGasPerBlob)
	if got := effectiveBlobTargetGas(metric, blobparams.BlobParams{}); got != wantTargetGas {
		t.Fatalf("effectiveBlobTargetGas = %d, want %d", got, wantTargetGas)
	}
	wantMaxGas := uint64(6 * params.BlobTxBlobGasPerBlob)
	if got := effectiveBlobMaxGas(metric, blobparams.BlobParams{}); got != wantMaxGas {
		t.Fatalf("effectiveBlobMaxGas = %d, want %d", got, wantMaxGas)
	}

	if got := predictedMarketDirection(models.BlockMetrics{BlobGasUsed: 4}, 4); got != marketPressureDirectionFlat {
		t.Fatalf("predictedMarketDirection = %q, want flat", got)
	}
	if got := predictedMarketDirection(models.BlockMetrics{BlobGasUsed: 5}, 4); got != marketPressureDirectionUp {
		t.Fatalf("predictedMarketDirection = %q, want up", got)
	}
	if got := predictedMarketDirection(models.BlockMetrics{BlobGasUsed: 0}, 0); got != marketPressureDirectionFlat {
		t.Fatalf("predictedMarketDirection = %q, want flat for zero target", got)
	}

	if got := percentage(1, 3); got != 33.33 {
		t.Fatalf("percentage = %f, want 33.33", got)
	}
	if got := nextBlockFeeEstimateRange(nil, blobparams.ChainConfigForID(42)); got.Low != "0" || got.High != "0" {
		t.Fatalf("nextBlockFeeEstimateRange = %+v, want zero range", got)
	}
}

// osakaTestChainConfig builds a post-Osaka chain config with an explicit blob
// schedule so the EIP-7918 reserve-price branch of the next-fee prediction can
// be exercised deterministically. Osaka has no blob schedule of its own — it
// inherits Prague's — and only the post-Osaka path (used at the predicted
// next-block timestamp) matters here.
func osakaTestChainConfig(target, maxBlobs int, updateFraction uint64) *params.ChainConfig {
	zero := uint64(0)
	osaka := uint64(1_000)
	bc := &params.BlobConfig{Target: target, Max: maxBlobs, UpdateFraction: updateFraction}
	return &params.ChainConfig{
		ChainID:     big.NewInt(9999),
		LondonBlock: big.NewInt(0),
		CancunTime:  &zero,
		PragueTime:  &zero,
		OsakaTime:   &osaka,
		BlobScheduleConfig: &params.BlobScheduleConfig{
			Cancun: bc,
			Prague: bc,
		},
	}
}

// TestPredictNextBlockBlobFee_EIP7918 pins the reserve-price behavior with the
// worked example from the pricing accuracy fix: target/max 6/9,
// update_fraction 5,007,716, parent excess 3,210,000, six blobs used. Below the
// reserve price (post-Osaka) excess grows to 3,472,144 (fee 2); above it, the
// legacy EIP-4844 formula gives excess 3,210,000 (fee 1).
func TestPredictNextBlockBlobFee_EIP7918(t *testing.T) {
	cfg := osakaTestChainConfig(6, 9, 5_007_716)
	parentTime := time.Unix(2_000, 0).UTC()
	nextTime := uint64(parentTime.Unix()) + ethereumSlotTimeSeconds

	// Reference fees for the two mandated excess values, computed with
	// go-ethereum directly so each assertion pins which branch was taken.
	feeForExcess := func(excess uint64) *big.Int {
		return eip4844.CalcBlobFee(cfg, &types.Header{Time: nextTime, ExcessBlobGas: &excess})
	}
	const (
		reserveExcess = uint64(3_472_144) // parentExcess + usedGas*(max-target)/max
		legacyExcess  = uint64(3_210_000) // parentExcess + usedGas - targetGas
	)
	reserveFee := feeForExcess(reserveExcess)
	legacyFee := feeForExcess(legacyExcess)
	if reserveFee.Cmp(legacyFee) == 0 {
		t.Fatalf("test setup invalid: reserve and legacy fees coincide (%s); cannot distinguish branches", reserveFee)
	}

	base := models.BlockMetrics{
		ChainID:        9999,
		BlockTimestamp: parentTime,
		BlobCount:      6,
		BlobGasUsed:    6 * params.BlobTxBlobGasPerBlob,
		ExcessBlobGas:  3_210_000,
	}

	cases := []struct {
		name       string
		baseFeeWei string
		want       *big.Int
	}{
		{"below reserve price applies EIP-7918 branch", "1000000000", reserveFee}, // 1 Gwei
		{"above reserve price uses legacy formula", "1", legacyFee},               // 1 wei
		{"zero execution base fee falls back to legacy", "0", legacyFee},          // pre-migration rows
		{"malformed execution base fee falls back to legacy", "not-a-number", legacyFee},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			metric := base
			metric.BaseFeeWei = tc.baseFeeWei
			got := predictNextBlockBlobFee(cfg, metric)
			if got.Cmp(tc.want) != 0 {
				t.Fatalf("predictNextBlockBlobFee = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestPredictNextBlockBlobFee_Fallbacks covers the nil-config and inactive-fork
// guards.
func TestPredictNextBlockBlobFee_Fallbacks(t *testing.T) {
	// nil config resolves to the compiled config for the metric's chain.
	metric := models.BlockMetrics{
		ChainID:        42,
		BlockTimestamp: time.Unix(2_000, 0).UTC(),
		BlobGasUsed:    params.BlobTxBlobGasPerBlob,
		ExcessBlobGas:  100_000,
		BaseFeeWei:     "1000000000",
	}
	if got := predictNextBlockBlobFee(nil, metric); got.Sign() <= 0 {
		t.Fatalf("predictNextBlockBlobFee with nil cfg = %s, want positive fee", got)
	}

	// A schedule whose only fork activates after the predicted block leaves no
	// active blob config; the prediction must fall back to the minimum fee
	// instead of panicking inside go-ethereum.
	future := uint64(1 << 40)
	inactive := &params.ChainConfig{
		ChainID:     big.NewInt(9999),
		LondonBlock: big.NewInt(0),
		CancunTime:  &future,
		BlobScheduleConfig: &params.BlobScheduleConfig{
			Cancun: &params.BlobConfig{Target: 3, Max: 6, UpdateFraction: 3_338_477},
		},
	}
	if got := predictNextBlockBlobFee(inactive, metric); got.Cmp(big.NewInt(params.BlobTxMinBlobGasprice)) != 0 {
		t.Fatalf("predictNextBlockBlobFee with inactive fork = %s, want min fee %d", got, params.BlobTxMinBlobGasprice)
	}
}

func testPressureMetric(blockNumber int64, timestamp time.Time, blobGasUsed, excessBlobGas int64) models.BlockMetrics {
	return models.BlockMetrics{
		ChainID:          42,
		BlockNumber:      blockNumber,
		BlockTimestamp:   timestamp,
		BlobCount:        int(blobGasUsed / params.BlobTxBlobGasPerBlob),
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
