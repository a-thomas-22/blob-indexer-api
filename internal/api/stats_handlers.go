package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

// StatsResponse is a response containing blob statistics
type StatsResponse struct {
	NetworkID           int    `json:"network_id,omitempty"`
	NetworkName         string `json:"network_name,omitempty"`
	TotalBlobs          int    `json:"total_blobs"`
	TotalConfirmedBlobs int    `json:"total_confirmed_blobs"`
	TotalPendingBlobs   int    `json:"total_pending_blobs"`
	// Average base fee per blob gas in wei. Aggregate averages may include fractional decimal precision.
	AverageBaseFeePerBlobGasWei string `json:"average_base_fee_per_blob_gas_wei" example:"4841467206.84506683"`
	// Average priority tip per blob gas in wei. Aggregate averages may include fractional decimal precision.
	AverageTipPerBlobGasWei string `json:"average_tip_per_blob_gas_wei" example:"15678762992.04263056"`
	// Average realized blob base-fee cost in wei. Aggregate averages may include fractional decimal precision.
	AverageTotalCostWei string `json:"average_total_cost_wei" example:"2207855919292172.4863"`
	// Deprecated alias: use average_base_fee_per_blob_gas_wei.
	AverageBaseFee string `json:"average_base_fee" extensions:"x-deprecated,x-replacement=average_base_fee_per_blob_gas_wei" example:"4841467206.84506683"`
	// Deprecated alias: use average_tip_per_blob_gas_wei.
	AverageTip string `json:"average_tip" extensions:"x-deprecated,x-replacement=average_tip_per_blob_gas_wei" example:"15678762992.04263056"`
	// Deprecated alias: use average_total_cost_wei.
	AverageTotalCost string    `json:"average_total_cost" extensions:"x-deprecated,x-replacement=average_total_cost_wei" example:"2207855919292172.4863"`
	LastIndexedBlock uint64    `json:"last_indexed_block"`
	LastIndexedTime  time.Time `json:"last_indexed_time"`
}

// GetBlobStats godoc
// @Summary Get blob statistics
// @Description Retrieve statistics about blob transactions
// @Tags stats
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Success 200 {object} Response{data=StatsResponse} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 500 {object} Response "Internal server error"
// @Failure 503 {object} Response "Database overloaded; retry later"
// @Router /stats [get]
func (a *API) GetBlobStats(w http.ResponseWriter, r *http.Request) {
	network, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	logger.Debug("Getting blob statistics", zap.String("network", network.Name))

	a.cacheMu.RLock()
	if cached, ok := a.statsCache[network.ChainID]; ok && time.Now().Before(cached.expiresAt) {
		a.cacheMu.RUnlock()
		setCacheControl(w, statsCacheTTL)
		a.respondJSON(w, http.StatusOK, Response{
			Success: true,
			Data:    cached.response,
		})
		return
	}
	a.cacheMu.RUnlock()

	cacheKey := fmt.Sprintf("stats:%d", network.ChainID)
	value, err, _ := a.aggregateGroup.Do(cacheKey, func() (interface{}, error) {
		a.cacheMu.RLock()
		if cached, ok := a.statsCache[network.ChainID]; ok && time.Now().Before(cached.expiresAt) {
			a.cacheMu.RUnlock()
			return cached.response, nil
		}
		a.cacheMu.RUnlock()

		return a.queryBlobStats(aggregateWorkContext(r), network.ChainID, network.Name)
	})
	if err != nil {
		logger.Error("Failed to get blob statistics",
			zap.String("network", network.Name),
			zap.Error(err))
		a.respondAggregateError(w, err, "Failed to get blob statistics")
		return
	}

	// The singleflight closure above always returns StatsResponse or an error,
	// so the assertion's ok value can never be false here.
	response, _ := value.(StatsResponse)

	setCacheControl(w, statsCacheTTL)
	a.respondSuccess(w, response)
}

func (a *API) queryBlobStats(ctx context.Context, networkID int, networkName string) (StatsResponse, error) {
	var stats models.BlobStatsAggregate

	queryCtx, cancel := context.WithTimeout(ctx, aggregateQueryTimeout)
	defer cancel()
	if err := a.db.GetContext(queryCtx, &stats, queryBlobStats, networkID); err != nil {
		return StatsResponse{}, err
	}

	response := toStatsResponse(stats, networkID, networkName)

	a.cacheMu.Lock()
	a.statsCache[networkID] = statsCacheEntry{
		response:  response,
		expiresAt: time.Now().Add(statsCacheTTL),
	}
	a.cacheMu.Unlock()
	return response, nil
}

func toStatsResponse(stats models.BlobStatsAggregate, networkID int, networkName string) StatsResponse {
	return StatsResponse{
		NetworkID:                   networkID,
		NetworkName:                 networkName,
		TotalBlobs:                  stats.TotalBlobs,
		TotalConfirmedBlobs:         stats.TotalConfirmedBlobs,
		TotalPendingBlobs:           stats.TotalPendingBlobs,
		AverageBaseFeePerBlobGasWei: stats.AverageBaseFee,
		AverageTipPerBlobGasWei:     stats.AverageTip,
		AverageTotalCostWei:         stats.AverageTotalCost,
		AverageBaseFee:              stats.AverageBaseFee,
		AverageTip:                  stats.AverageTip,
		AverageTotalCost:            stats.AverageTotalCost,
		LastIndexedBlock:            stats.LastIndexedBlock,
		LastIndexedTime:             stats.LastIndexedTime,
	}
}
