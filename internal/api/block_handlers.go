package api

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

// GetBlockByNumber godoc
// @Summary Get an indexed block by number
// @Description Retrieve a single indexed block with its confirmed blobs and block-level pricing data. The data shape matches the WebSocket new_block event payload, so clients can reuse the same transform. Zero-blob blocks are indexed too and return an empty blobs list; 404 means the block is not indexed (missed slot, ahead of the chain head, or outside the indexed range).
// @Tags blocks
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Param number path int true "Block number (positive integer)"
// @Success 200 {object} Response{data=NewBlockData} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 404 {object} Response "Block not indexed"
// @Failure 500 {object} Response "Internal server error"
// @Router /block/{number} [get]
func (a *API) GetBlockByNumber(w http.ResponseWriter, r *http.Request) {
	network, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	blockNumber, err := strconv.ParseInt(chi.URLParam(r, "number"), 10, 64)
	if err != nil || blockNumber <= 0 {
		a.respondError(w, http.StatusBadRequest, "Invalid block number")
		return
	}

	logger.Debug("Getting block by number",
		zap.String("network", network.Name),
		zap.Int64("block", blockNumber))

	// The indexer commits a block's block_metrics row and its blobs rows in one
	// transaction, and reorg cleanup deletes both transactionally, so every
	// committed snapshot satisfies blob_count == count of blobs rows. The two
	// point reads below each get their own snapshot, though: a reorg rewrite
	// committing between them would yield a torn pair (e.g. blob_count=2 with
	// zero blobs). A count mismatch therefore proves the reads straddled a
	// rewrite — retry once to land on the settled state, and if the tear
	// somehow persists, serve the response uncached so the edge never pins a
	// payload that never existed.
	var (
		metric     models.BlockMetrics
		blobs      []models.Blob
		consistent bool
	)
	for attempt := 0; attempt < 2 && !consistent; attempt++ {
		if err := a.db.GetContext(r.Context(), &metric, queryBlockMetricsForBlock, network.ChainID, blockNumber); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				a.respondError(w, http.StatusNotFound, "Block not found")
				return
			}
			logger.Error("Failed to get block metrics",
				zap.String("network", network.Name),
				zap.Int64("block", blockNumber),
				zap.Error(err))
			a.respondError(w, http.StatusInternalServerError, "Failed to get block")
			return
		}

		blobs = nil
		if err := a.db.SelectContext(r.Context(), &blobs, queryBlobsByBlockNumber, network.ChainID, blockNumber); err != nil {
			logger.Error("Failed to get block blobs",
				zap.String("network", network.Name),
				zap.Int64("block", blockNumber),
				zap.Error(err))
			a.respondError(w, http.StatusInternalServerError, "Failed to get block")
			return
		}

		consistent = len(blobs) == metric.BlobCount
	}

	brs := make([]BlobResponse, 0, len(blobs))
	for _, blob := range blobs {
		brs = append(brs, toBlobResponse(blob, network.Name))
	}
	pricing := toBlockPricingResponse(metric)

	// An indexed block at a height is effectively immutable, so the response is
	// safely cacheable — same reorg self-heal bound as a confirmed blob.
	if consistent {
		setCacheControl(w, indexedBlockCacheTTL, indexedBlockEdgeTTL)
	} else {
		logger.Warn("Serving torn block read uncached",
			zap.String("network", network.Name),
			zap.Int64("block", blockNumber),
			zap.Int("blob_count", metric.BlobCount),
			zap.Int("blob_rows", len(blobs)))
	}
	a.respondSuccess(w, NewBlockData{
		BlockNumber: metric.BlockNumber,
		BlobCount:   metric.BlobCount,
		Timestamp:   metric.BlockTimestamp,
		Blobs:       brs,
		Pricing:     &pricing,
	})
}
