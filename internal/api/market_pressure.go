package api

import (
	"math"
	"math/big"
	"strings"

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
		fee := predictNextBlockBlobFee(cfg, metric)
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

// predictNextBlockBlobFee estimates the blob base fee of the block following
// metric. It reconstructs the next block's excess blob gas with go-ethereum's
// fork-aware eip4844.CalcExcessBlobGas rather than a hand-rolled subtraction, so
// post-Osaka chains get the EIP-7918 reserve-price branch: when the blob base
// fee sits at or below a reserve derived from the execution base fee, excess
// grows toward the scaled floor instead of decaying by target. That branch
// reads the parent block's execution base fee (metric.BaseFeeWei); rows indexed
// before it was recorded carry "0", which zeroes the reserve price so the branch
// never fires and the estimate matches the legacy pre-Osaka formula.
//
// The target, max, and update-fraction all come from cfg at the next block's
// timestamp — the same fork-aware schedule the indexer used to write the block —
// so no per-block target argument is threaded through.
func predictNextBlockBlobFee(cfg *params.ChainConfig, metric models.BlockMetrics) *big.Int {
	if cfg == nil {
		cfg = blobparams.ChainConfigForID(metric.ChainID)
	}

	var parentTime uint64
	if ts := metric.BlockTimestamp.Unix(); ts > 0 {
		parentTime = uint64(ts)
	}
	nextTime := parentTime + ethereumSlotTimeSeconds

	// eip4844.CalcExcessBlobGas and CalcBlobFee both panic when no blob config is
	// active at nextTime (a pre-Cancun timestamp, or a pathological learned
	// schedule whose earliest boundary is later). Guard on the same
	// GetBlobParams().Max == 0 signal the indexer uses and fall back to the
	// minimum blob fee rather than crash the request.
	if blobparams.GetBlobParams(cfg, nextTime).Max == 0 {
		return big.NewInt(params.BlobTxMinBlobGasprice)
	}

	excess := nonNegativeUint64(metric.ExcessBlobGas)
	used := nonNegativeUint64(metric.BlobGasUsed)
	parent := &types.Header{
		Time:          parentTime,
		ExcessBlobGas: &excess,
		BlobGasUsed:   &used,
		BaseFee:       parseWeiOrZero(metric.BaseFeeWei),
	}

	nextExcess := eip4844.CalcExcessBlobGas(cfg, parent, nextTime)
	nextHeader := &types.Header{
		Time:          nextTime,
		ExcessBlobGas: &nextExcess,
	}
	return eip4844.CalcBlobFee(cfg, nextHeader)
}

// parseWeiOrZero parses a base-10 wei value (as stored in NUMERIC columns) into
// a big.Int, returning zero for empty or malformed input.
func parseWeiOrZero(wei string) *big.Int {
	if v, ok := new(big.Int).SetString(strings.TrimSpace(wei), 10); ok {
		return v
	}
	return big.NewInt(0)
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
