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
// @Description Retrieve a single indexed block with its confirmed blobs, block-level pricing data, and the builder that produced it. The shared fields match the WebSocket new_block event payload, so clients can reuse the same transform; candidates is REST-only and lists the pending blob transactions our node had seen that the block did not include. builder is omitted for blocks the builder backfill has not reached, and candidates is empty when no live snapshot was taken or the retention window has pruned it. Zero-blob blocks are indexed too and return an empty blobs list; 404 means the block is not indexed (missed slot, ahead of the chain head, or outside the indexed range).
// @Tags blocks
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Param number path int true "Block number (positive integer)"
// @Success 200 {object} Response{data=BlockDetailResponse} "Success"
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
		brs = append(brs, toBlobResponse(blob, network))
	}
	pricing := toBlockPricingResponse(metric)

	// The builder row is written in the same transaction as block_metrics,
	// so it is either present or absent — never torn against the reads
	// above. A missing row just means the backfill has not reached this
	// height, which is not an error.
	var builder *BlockBuilderResponse
	var builderRow models.BlockBuilder
	switch err := a.db.GetContext(r.Context(), &builderRow, queryBlockBuilderForBlock, network.ChainID, blockNumber); {
	case err == nil:
		response := toBlockBuilderResponse(builderRow)
		builder = &response
	case errors.Is(err, sql.ErrNoRows):
	default:
		logger.Error("Failed to get block builder",
			zap.String("network", network.Name),
			zap.Int64("block", blockNumber),
			zap.Error(err))
		a.respondError(w, http.StatusInternalServerError, "Failed to get block")
		return
	}

	// Candidate detail is pruned on a retention window, so an empty list is
	// the normal outcome for anything but a recent, live-indexed block.
	var candidateRows []models.BlobInclusionCandidate
	if err := a.db.SelectContext(r.Context(), &candidateRows, queryBlobInclusionCandidatesForBlock, network.ChainID, blockNumber); err != nil {
		logger.Error("Failed to get block inclusion candidates",
			zap.String("network", network.Name),
			zap.Int64("block", blockNumber),
			zap.Error(err))
		a.respondError(w, http.StatusInternalServerError, "Failed to get block")
		return
	}

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
	a.respondSuccess(w, BlockDetailResponse{
		NewBlockData: NewBlockData{
			BlockNumber: metric.BlockNumber,
			BlobCount:   metric.BlobCount,
			Timestamp:   metric.BlockTimestamp,
			Blobs:       brs,
			Pricing:     &pricing,
			Builder:     builder,
		},
		Candidates: toBlobInclusionCandidateResponses(candidateRows),
	})
}
