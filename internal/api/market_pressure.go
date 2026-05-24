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
		cfg = blobparams.ChainConfigForID(latest.NetworkID)
	}
	bp := blobparams.GetBlobParams(cfg, uint64(latest.BlockTimestamp.Unix()))
	targetGas := effectiveBlobTargetGas(latest, bp)
	maxGas := effectiveBlobMaxGas(latest, bp)

	maxedBlocks := 0
	for _, metric := range metrics {
		blobGasUsed := nonNegativeUint64(metric.BlobGasUsed)
		if targetGas > 0 && blobGasUsed > targetGas {
			pressure.RecentBlocksAboveTarget++
		}
		if isMaxedBlobBlock(metric, maxGas) {
			maxedBlocks++
		}
	}

	pressure.ConsecutiveFullBlocks = consecutiveMaxedBlobBlocks(metrics, maxGas)
	pressure.PercentRecentBlocksAtMax = percentage(maxedBlocks, len(metrics))
	pressure.PredictedDirection = predictedMarketDirection(latest, targetGas)
	pressure.NextBlockFeeEstimate = nextBlockFeeEstimateRange(metrics, cfg, targetGas)

	return pressure
}

func effectiveBlobTargetGas(metric models.BlockMetrics, bp blobparams.BlobParams) uint64 {
	if bp.TargetGas > 0 {
		return bp.TargetGas
	}
	if metric.BlobGasTarget > 0 {
		return uint64(metric.BlobGasTarget)
	}
	if metric.BlobParamsTarget > 0 {
		return uint64(metric.BlobParamsTarget) * params.BlobTxBlobGasPerBlob
	}
	return 0
}

func effectiveBlobMaxGas(metric models.BlockMetrics, bp blobparams.BlobParams) uint64 {
	if bp.MaxGas > 0 {
		return bp.MaxGas
	}
	if metric.BlobGasLimit > 0 {
		return uint64(metric.BlobGasLimit)
	}
	if metric.BlobParamsMax > 0 {
		return uint64(metric.BlobParamsMax) * params.BlobTxBlobGasPerBlob
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

func consecutiveMaxedBlobBlocks(metrics []models.BlockMetrics, maxGas uint64) int {
	consecutive := 0
	for _, metric := range metrics {
		if !isMaxedBlobBlock(metric, maxGas) {
			break
		}
		consecutive++
	}
	return consecutive
}

func nextBlockFeeEstimateRange(metrics []models.BlockMetrics, cfg *params.ChainConfig, targetGas uint64) FeeEstimateRangeResponse {
	var low *big.Int
	var high *big.Int

	for _, metric := range metrics {
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
		cfg = blobparams.ChainConfigForID(metric.NetworkID)
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
