package api

import (
	"context"
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
	// EarliestIndexedBlock and LatestIndexedBlock bound the network's indexed
	// coverage, letting consumers distinguish a block below the indexing start
	// from one not yet produced. Omitted when nothing is indexed yet.
	EarliestIndexedBlock *int64 `json:"earliest_indexed_block,omitempty"`
	LatestIndexedBlock   *int64 `json:"latest_indexed_block,omitempty"`
	FreshnessResponse
}

// indexedBlockCoverage carries the MIN/MAX indexed block bounds for a network.
// Both are nil when the network has no indexed blocks.
type indexedBlockCoverage struct {
	EarliestIndexedBlock *int64 `db:"earliest_indexed_block"`
	LatestIndexedBlock   *int64 `db:"latest_indexed_block"`
}

// getIndexedBlockCoverageFromDB reads the indexed block range for a network.
// Coverage is additive status metadata, so failures degrade to absent bounds
// instead of failing the whole /status response.
func (a *API) getIndexedBlockCoverageFromDB(ctx context.Context, networkID int) indexedBlockCoverage {
	var coverage indexedBlockCoverage
	if err := a.db.GetContext(ctx, &coverage, queryIndexedBlockCoverage, networkID); err != nil {
		logger.Error("Failed to get indexed block coverage",
			zap.Int("chain_id", networkID),
			zap.Error(err))
		return indexedBlockCoverage{}
	}
	return coverage
}

// GetIndexerStatus godoc
// @Summary Get indexer status
// @Description Retrieve the current status of the indexer, including the indexed block coverage range (earliest_indexed_block / latest_indexed_block)
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

	var indexedTime time.Time
	if err := a.db.GetContext(r.Context(), &indexedTime, queryNetworkLastIndexedTime, network.ChainID); err != nil {
		logger.Error("Failed to get last indexed time",
			zap.String("network", network.Name),
			zap.Error(err))
		a.respondError(w, http.StatusInternalServerError, "Failed to get last indexed time")
		return
	}

	uptime := time.Since(a.startTime).Truncate(time.Second).String()

	freshness := a.getNetworkFreshnessFromDB(r.Context(), network.ChainID)
	coverage := a.getIndexedBlockCoverageFromDB(r.Context(), network.ChainID)
	response := StatusResponse{
		ChainID:              network.ChainID,
		NetworkName:          network.Name,
		LastIndexedBlock:     freshness.LastIndexedBlock,
		IndexerVersion:       a.config.Indexer.Version,
		Uptime:               uptime,
		LastIndexedTime:      indexedTime,
		Backfill:             freshness.backfillResponse(),
		EarliestIndexedBlock: coverage.EarliestIndexedBlock,
		LatestIndexedBlock:   coverage.LatestIndexedBlock,
		FreshnessResponse:    freshness.FreshnessResponse,
	}

	setCacheControl(w, networkStatusCacheTTL, networkStatusEdgeTTL)
	a.respondSuccess(w, response)
}
