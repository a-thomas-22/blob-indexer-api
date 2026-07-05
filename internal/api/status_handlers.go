package api

import (
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

// StatusResponse is a response containing indexer status
type StatusResponse struct {
	ChainID          int              `json:"chain_id,omitempty"`
	NetworkName      string           `json:"network_name,omitempty"`
	LastIndexedBlock uint64           `json:"last_indexed_block"`
	IndexerVersion   string           `json:"indexer_version"`
	Uptime           string           `json:"uptime"`
	LastIndexedTime  time.Time        `json:"last_indexed_time"`
	Backfill         BackfillResponse `json:"backfill"`
	FreshnessResponse
}

// GetIndexerStatus godoc
// @Summary Get indexer status
// @Description Retrieve the current status of the indexer
// @Tags status
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Success 200 {object} Response{data=StatusResponse} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 500 {object} Response "Internal server error"
// @Router /status [get]
func (a *API) GetIndexerStatus(w http.ResponseWriter, r *http.Request) {
	network, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	logger.Debug("Getting indexer status", zap.String("network", network.Name))

	var lastIndexedTime *time.Time
	query := "SELECT MAX(timestamp) FROM blobs WHERE chain_id = $1"
	if err := a.db.GetContext(r.Context(), &lastIndexedTime, query, network.ChainID); err != nil {
		logger.Error("Failed to get last indexed time",
			zap.String("network", network.Name),
			zap.Error(err))
		a.respondError(w, http.StatusInternalServerError, "Failed to get last indexed time")
		return
	}

	uptime := time.Since(a.startTime).Truncate(time.Second).String()

	var indexedTime time.Time
	if lastIndexedTime != nil {
		indexedTime = *lastIndexedTime
	}

	freshness := a.getNetworkFreshnessFromDB(r.Context(), network.ChainID)
	response := StatusResponse{
		ChainID:           network.ChainID,
		NetworkName:       network.Name,
		LastIndexedBlock:  freshness.LastIndexedBlock,
		IndexerVersion:    a.config.Indexer.Version,
		Uptime:            uptime,
		LastIndexedTime:   indexedTime,
		Backfill:          freshness.backfillResponse(),
		FreshnessResponse: freshness.FreshnessResponse,
	}

	a.respondSuccess(w, response)
}
