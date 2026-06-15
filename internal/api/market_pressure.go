package api

import (
	"math"
	"math/big"

	"github.com/ethereum/go-ethereum/consensus/misc/eip4844"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"

	"github.com/a-thomas-22/blob-indexer-api/internal/blobparams"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
)

const ethereumSlotTimeSeconds uint64 = 12

const (
	marketPressureDirectionUp   = "up"
	marketPressureDirectionFlat = "flat"
	marketPressureDirectionDown = "down"
)

func buildMarketPressure(metrics []models.BlockMetrics, cfg *params.ChainConfig) MarketPressureResponse {
	pressure := MarketPressureResponse{
		PredictedDirection: marketPressureDirectionFlat,
		NextBlockFeeEstimate: FeeEstimateRangeResponse{
			Low:  "0",
			High: "0",
		},
	}
	if len(metrics) == 0 {
		return pressure
	}

	latest := metrics[0]
	if cfg == nil {
		cfg = blobparams.ChainConfigForID(latest.ChainID)
	}

	maxedBlocks := 0
	for _, metric := range metrics {
		bp := blobparamsForMetric(metric, cfg)
		targetGas := effectiveBlobTargetGas(metric, bp)
		maxGas := effectiveBlobMaxGas(metric, bp)
		blobGasUsed := nonNegativeUint64(metric.BlobGasUsed)
		if targetGas > 0 && blobGasUsed > targetGas {
			pressure.RecentBlocksAboveTarget++
		}
		if isMaxedBlobBlock(metric, maxGas) {
			maxedBlocks++
		}
	}

	latestTargetGas := effectiveBlobTargetGas(latest, blobparamsForMetric(latest, cfg))

	pressure.ConsecutiveFullBlocks = consecutiveMaxedBlobBlocks(metrics, cfg)
	pressure.PercentRecentBlocksAtMax = percentage(maxedBlocks, len(metrics))
	pressure.PredictedDirection = predictedMarketDirection(latest, latestTargetGas)
	pressure.NextBlockFeeEstimate = nextBlockFeeEstimateRange(metrics, cfg)

	return pressure
}

func blobparamsForMetric(metric models.BlockMetrics, cfg *params.ChainConfig) blobparams.BlobParams {
	if cfg == nil {
		cfg = blobparams.ChainConfigForID(metric.ChainID)
	}
	return blobparams.GetBlobParams(cfg, uint64(metric.BlockTimestamp.Unix()))
}

func effectiveBlobTargetGas(metric models.BlockMetrics, bp blobparams.BlobParams) uint64 {
	if metric.BlobGasTarget > 0 {
		return uint64(metric.BlobGasTarget)
	}
	if metric.BlobParamsTarget > 0 {
		return uint64(metric.BlobParamsTarget) * params.BlobTxBlobGasPerBlob
	}
	if bp.TargetGas > 0 {
		return bp.TargetGas
	}
	return 0
}

func effectiveBlobMaxGas(metric models.BlockMetrics, bp blobparams.BlobParams) uint64 {
	if metric.BlobGasLimit > 0 {
		return uint64(metric.BlobGasLimit)
	}
	if metric.BlobParamsMax > 0 {
		return uint64(metric.BlobParamsMax) * params.BlobTxBlobGasPerBlob
	}
	if bp.MaxGas > 0 {
		return bp.MaxGas
	}
	return 0
}

func predictedMarketDirection(metric models.BlockMetrics, targetGas uint64) string {
	if targetGas == 0 {
		return marketPressureDirectionFlat
	}

	blobGasUsed := nonNegativeUint64(metric.BlobGasUsed)
	switch {
	case blobGasUsed > targetGas:
		return marketPressureDirectionUp
	case blobGasUsed < targetGas:
		return marketPressureDirectionDown
	default:
		return marketPressureDirectionFlat
	}
}

func isMaxedBlobBlock(metric models.BlockMetrics, maxGas uint64) bool {
	if maxGas == 0 {
		return false
	}
	return nonNegativeUint64(metric.BlobGasUsed) >= maxGas
}

func consecutiveMaxedBlobBlocks(metrics []models.BlockMetrics, cfg *params.ChainConfig) int {
	consecutive := 0
	for _, metric := range metrics {
		maxGas := effectiveBlobMaxGas(metric, blobparamsForMetric(metric, cfg))
		if !isMaxedBlobBlock(metric, maxGas) {
			break
		}
		consecutive++
	}
	return consecutive
}

func nextBlockFeeEstimateRange(metrics []models.BlockMetrics, cfg *params.ChainConfig) FeeEstimateRangeResponse {
	var low *big.Int
	var high *big.Int

	for _, metric := range metrics {
		targetGas := effectiveBlobTargetGas(metric, blobparamsForMetric(metric, cfg))
		fee := predictNextBlockBlobFee(cfg, metric, targetGas)
		if low == nil || fee.Cmp(low) < 0 {
			low = new(big.Int).Set(fee)
		}
		if high == nil || fee.Cmp(high) > 0 {
			high = new(big.Int).Set(fee)
		}
	}

	if low == nil || high == nil {
		return FeeEstimateRangeResponse{Low: "0", High: "0"}
	}
	return FeeEstimateRangeResponse{Low: low.String(), High: high.String()}
}

func predictNextBlockBlobFee(cfg *params.ChainConfig, metric models.BlockMetrics, targetGas uint64) *big.Int {
	if cfg == nil {
		cfg = blobparams.ChainConfigForID(metric.ChainID)
	}
	if targetGas == 0 && metric.BlobGasTarget > 0 {
		targetGas = uint64(metric.BlobGasTarget)
	}

	nextExcess := calcNextExcessBlobGas(
		nonNegativeUint64(metric.ExcessBlobGas),
		nonNegativeUint64(metric.BlobGasUsed),
		targetGas,
	)
	nextHeader := &types.Header{
		Time:          uint64(metric.BlockTimestamp.Unix()) + ethereumSlotTimeSeconds,
		ExcessBlobGas: &nextExcess,
	}
	return eip4844.CalcBlobFee(cfg, nextHeader)
}

func nonNegativeUint64(value int64) uint64 {
	if value <= 0 {
		return 0
	}
	return uint64(value)
}

func percentage(count, total int) float64 {
	if total == 0 {
		return 0
	}
	return math.Round((float64(count)*100/float64(total))*100) / 100
}
