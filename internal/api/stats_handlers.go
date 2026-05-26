package api

import (
	"context"
	"net/http"
	"time"

	"go.uber.org/zap"

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
		a.respondJSON(w, http.StatusOK, Response{
			Success: true,
			Data:    cached.response,
		})
		return
	}
	a.cacheMu.RUnlock()

	var stats struct {
		TotalBlobs          int       `db:"total_blobs"`
		TotalConfirmedBlobs int       `db:"total_confirmed_blobs"`
		TotalPendingBlobs   int       `db:"total_pending_blobs"`
		AverageBaseFee      string    `db:"average_base_fee"`
		AverageTip          string    `db:"average_tip"`
		AverageTotalCost    string    `db:"average_total_cost"`
		LastIndexedTime     time.Time `db:"last_indexed_time"`
	}

	queryCtx, cancel := context.WithTimeout(r.Context(), aggregateQueryTimeout)
	defer cancel()
	if err := a.db.GetContext(queryCtx, &stats, queryBlobStats, network.ChainID); err != nil {
		logger.Error("Failed to get blob statistics",
			zap.String("network", network.Name),
			zap.Error(err))
		a.respondError(w, http.StatusInternalServerError, "Failed to get blob statistics")
		return
	}

	response := StatsResponse{
		NetworkID:                   network.ChainID,
		NetworkName:                 network.Name,
		TotalBlobs:                  stats.TotalBlobs,
		TotalConfirmedBlobs:         stats.TotalConfirmedBlobs,
		TotalPendingBlobs:           stats.TotalPendingBlobs,
		AverageBaseFeePerBlobGasWei: stats.AverageBaseFee,
		AverageTipPerBlobGasWei:     stats.AverageTip,
		AverageTotalCostWei:         stats.AverageTotalCost,
		AverageBaseFee:              stats.AverageBaseFee,
		AverageTip:                  stats.AverageTip,
		AverageTotalCost:            stats.AverageTotalCost,
		LastIndexedBlock:            a.getLastIndexedBlockFromDB(r.Context(), network.ChainID),
		LastIndexedTime:             stats.LastIndexedTime,
	}

	a.cacheMu.Lock()
	a.statsCache[network.ChainID] = statsCacheEntry{
		response:  response,
		expiresAt: time.Now().Add(aggregateCacheTTL),
	}
	a.cacheMu.Unlock()
	a.respondSuccess(w, response)
}
