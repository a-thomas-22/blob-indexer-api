package api

import (
	"database/sql"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

// BlobInclusionBlockResponse is one block on a transaction's inclusion
// timeline: who built it and how full its blob space was. The occupancy
// fields come from block_metrics and the builder from block_builders; each
// is omitted when its row is missing (a builder backfill that has not
// reached the height, or a reorg rewrite in flight).
type BlobInclusionBlockResponse struct {
	BlockNumber    int64     `json:"block_number"`
	BlockTimestamp time.Time `json:"block_timestamp"`
	// BlobCount and MaxBlobs are the block's blob occupancy: how many blobs
	// it carried against the most it could have.
	BlobCount *int `json:"blob_count,omitempty"`
	MaxBlobs  *int `json:"max_blobs,omitempty"`
	// BlobBaseFee is the block's blob base fee in wei, with its gwei
	// companion — what a priced_out_blob_fee reason was measured against.
	BlobBaseFee     *string               `json:"blob_base_fee,omitempty"`
	BlobBaseFeeGwei string                `json:"blob_base_fee_gwei,omitempty"`
	Builder         *BlockBuilderResponse `json:"builder,omitempty"`
}

// BlobInclusionSkippedBlockResponse is a block that arrived while the
// transaction was pending in our node's pool and did not include it, with
// the reason the indexer classified the miss under at the time.
//
// Only reason = "eligible" says anything about the builder: the transaction
// was old enough to have propagated, first in its sender's nonce order,
// priced above both base fees, and small enough for the block's remaining
// blob space. The other reasons explain why inclusion was impossible or
// unlikely whatever the builder did. Our node's pool is not the builder's,
// so even an eligible miss is "visible to our node and not included".
type BlobInclusionSkippedBlockResponse struct {
	BlobInclusionBlockResponse
	// WaitedMs is how long the transaction had been pending when this block
	// was produced: block_timestamp minus first_seen_at. Signed for the same
	// reason time_to_inclusion_ms is; omitted when first_seen_at is unknown.
	WaitedMs *int64 `json:"waited_ms,omitempty"`
	Reason   string `json:"reason" enums:"eligible,too_recent,nonce_gap,priced_out_blob_fee,priced_out_exec_fee,no_room"`
}

// BlobInclusionIncludedBlockResponse is the block that finally carried the
// transaction.
type BlobInclusionIncludedBlockResponse struct {
	BlobInclusionBlockResponse
	// Slot and TxIndex mirror the same fields on /blob/{txHash}.
	Slot    *uint64 `json:"slot,omitempty" example:"11813607"`
	TxIndex *int    `json:"tx_index,omitempty" example:"12"`
}

// BlobInclusionWindowResponse bounds the blocks the transaction waited
// through and says how many of them can carry detail at all. Only blocks
// the indexer saw arrive live have their pending pool classified
// (snapshot_blocks); a block indexed from history, or one whose candidate
// rows the retention prune has since removed, leaves a gap in `skipped`
// that is not evidence the block included nothing.
type BlobInclusionWindowResponse struct {
	// FromBlock is the first block produced after the transaction was first
	// seen (or the first block that recorded it as a candidate, whichever
	// is earlier); ToBlock is the block before inclusion, or the newest
	// indexed block while the transaction is pending. ToBlock is below
	// FromBlock when no block has been produced in the wait yet: the
	// transaction landed in the very next block, or it is pending and
	// newer than the newest indexed block.
	FromBlock int64 `json:"from_block"`
	ToBlock   int64 `json:"to_block"`
	// Blocks is how many indexed blocks lie in the window; SnapshotBlocks how
	// many of them classified their pending pool, which is the most that
	// could appear in `skipped`.
	Blocks         int64 `json:"blocks"`
	SnapshotBlocks int64 `json:"snapshot_blocks"`
}

// BlobInclusionResponse is the /blob/{txHash}/inclusion payload: how long a
// blob transaction waited for a block and which blocks passed it by while it
// did.
type BlobInclusionResponse struct {
	ChainID     int    `json:"chain_id"`
	NetworkName string `json:"network_name,omitempty"`
	TxHash      string `json:"tx_hash"`
	Confirmed   bool   `json:"confirmed"`
	// FirstSeenAt and TimeToInclusionMs are the same fields /blob/{txHash}
	// reports, with the same omissions: never seen pending, or indexed
	// before they were stored.
	FirstSeenAt       *time.Time `json:"first_seen_at,omitempty"`
	TimeToInclusionMs *int64     `json:"time_to_inclusion_ms,omitempty" example:"4200"`
	// Included is null while the transaction is pending.
	Included *BlobInclusionIncludedBlockResponse `json:"included"`
	// Window is null only when nothing bounds the wait: the transaction was
	// never seen pending and no block recorded it as a candidate.
	Window *BlobInclusionWindowResponse `json:"window"`
	// SkippedBlocks counts every block that recorded the transaction as a
	// candidate and left it out; EligibleSkippedBlocks those whose reason
	// was "eligible". Both cover the whole retained history even when
	// `skipped` is truncated.
	SkippedBlocks         int64 `json:"skipped_blocks"`
	EligibleSkippedBlocks int64 `json:"eligible_skipped_blocks"`
	// Skipped lists those blocks oldest first. It holds at most
	// blobInclusionSkippedLimit entries, the most recent ones; when it does
	// not cover every counted block, SkippedTruncated is true and the
	// blocks left out are the oldest, between window.from_block and the
	// first listed entry. Empty when the transaction was included from
	// history or its candidate rows have been pruned.
	Skipped          []BlobInclusionSkippedBlockResponse `json:"skipped"`
	SkippedTruncated bool                                `json:"skipped_truncated"`
}

// GetBlobInclusion godoc
// @Summary Get a blob transaction's inclusion timeline
// @Description How long a blob transaction waited between our node first seeing it pending and a block including it, and which blocks arrived in between without including it — each with its builder, its blob occupancy and the reason the indexer classified the miss under. Only reason "eligible" reflects a builder choice; the others explain why the transaction could not have been included. The per-block detail exists only for blocks the indexer saw arrive live and is pruned after the indexer's candidate retention window (about a week), so `window` reports how many blocks in the wait could carry detail at all. A pending transaction has a null `included` and its window ends at the newest indexed block.
// @Tags blobs
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Param txHash path string true "Transaction hash"
// @Success 200 {object} Response{data=BlobInclusionResponse} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 404 {object} Response "Blob not found"
// @Failure 500 {object} Response "Internal server error"
// @Router /blob/{txHash}/inclusion [get]
func (a *API) GetBlobInclusion(w http.ResponseWriter, r *http.Request) {
	network, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	txHash := chi.URLParam(r, "txHash")
	if txHash == "" {
		a.respondError(w, http.StatusBadRequest, "Transaction hash is required")
		return
	}
	if !strings.HasPrefix(txHash, "0x") || !common.IsHexHash(txHash) {
		a.respondError(w, http.StatusBadRequest, "Invalid transaction hash format")
		return
	}

	logger.Debug("Getting blob inclusion timeline",
		zap.String("network", network.Name),
		zap.String("tx_hash", txHash))

	fail := func(what string, err error) {
		logger.Error("Failed to get blob inclusion timeline",
			zap.String("network", network.Name),
			zap.String("tx_hash", txHash),
			zap.String("step", what),
			zap.Error(err))
		a.respondError(w, http.StatusInternalServerError, "Failed to get blob inclusion")
	}

	// The same lookup /blob/{txHash} makes: the confirmed row wins over a
	// pending one, and the stored hash casing is what the candidate rows
	// were written with, so the rest of the reads use blob.TxHash.
	var blob models.Blob
	if err := a.db.GetContext(r.Context(), &blob, queryBlobByTxHash, txHash, network.ChainID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			logger.Warn("Blob not found",
				zap.String("network", network.Name),
				zap.String("tx_hash", txHash))
			a.respondError(w, http.StatusNotFound, "Blob not found")
			return
		}
		fail("blob", err)
		return
	}

	response := BlobInclusionResponse{
		ChainID:           network.ChainID,
		NetworkName:       network.Name,
		TxHash:            blob.TxHash,
		Confirmed:         blob.Confirmed,
		FirstSeenAt:       blob.FirstSeenAt,
		TimeToInclusionMs: blobTimeToInclusionMs(blob),
		Skipped:           []BlobInclusionSkippedBlockResponse{},
	}

	// The including block bounds the history: candidate rows at or past it
	// cannot exist for this transaction, but a reorg rewrite in flight could
	// briefly leave some, and they would not be part of this wait.
	var includedAt sql.NullInt64
	if blob.Confirmed {
		includedAt = sql.NullInt64{Int64: blob.BlockNumber, Valid: true}
	}
	var firstSeen sql.NullTime
	if blob.FirstSeenAt != nil {
		firstSeen = sql.NullTime{Time: *blob.FirstSeenAt, Valid: true}
	}

	var summary blobInclusionSummaryRow
	if err := a.db.GetContext(r.Context(), &summary, queryBlobInclusionSummary, network.ChainID, blob.TxHash, firstSeen, includedAt); err != nil {
		fail("summary", err)
		return
	}
	response.SkippedBlocks = summary.SkippedBlocks
	response.EligibleSkippedBlocks = summary.EligibleSkippedBlocks
	if summary.FromBlock.Valid && summary.ToBlock.Valid {
		response.Window = &BlobInclusionWindowResponse{
			FromBlock:      summary.FromBlock.Int64,
			ToBlock:        summary.ToBlock.Int64,
			Blocks:         summary.WindowBlocks,
			SnapshotBlocks: summary.WindowSnapshotBlocks,
		}
	}

	var rows []blobInclusionBlockRow
	if err := a.db.SelectContext(r.Context(), &rows, queryBlobInclusionBlocks, network.ChainID, blob.TxHash, includedAt, blobInclusionSkippedLimit); err != nil {
		fail("timeline blocks", err)
		return
	}
	for _, row := range rows {
		if row.Included {
			response.Included = &BlobInclusionIncludedBlockResponse{
				BlobInclusionBlockResponse: toBlobInclusionBlock(row, blob.Timestamp),
				Slot:                       blobSlot(blob, network),
				TxIndex:                    blob.TxIndex,
			}
			continue
		}
		response.Skipped = append(response.Skipped, toBlobInclusionSkipped(row, blob.FirstSeenAt))
	}
	if blob.Confirmed && response.Included == nil {
		// The included arm yields a row for every confirmed transaction even
		// when both of its joins miss, so this only guards a read that
		// returned nothing at all; the block is still known from the blob.
		response.Included = &BlobInclusionIncludedBlockResponse{
			BlobInclusionBlockResponse: BlobInclusionBlockResponse{BlockNumber: blob.BlockNumber, BlockTimestamp: blob.Timestamp},
			Slot:                       blobSlot(blob, network),
			TxIndex:                    blob.TxIndex,
		}
	}
	sort.Slice(response.Skipped, func(i, j int) bool {
		return response.Skipped[i].BlockNumber < response.Skipped[j].BlockNumber
	})
	response.SkippedTruncated = summary.SkippedBlocks > int64(len(response.Skipped))

	// A confirmed transaction's wait is settled; only the pruning of its
	// detail changes the payload, and that self-heals within the TTL.
	if blob.Confirmed {
		setCacheControl(w, confirmedBlobCacheTTL, confirmedBlobEdgeTTL)
	}
	a.respondSuccess(w, response)
}

// toBlobInclusionBlock converts a timeline row to its wire shape. The
// timestamp falls back to the caller's when the row carried none, which only
// the included arm can do (both its rows missing).
func toBlobInclusionBlock(row blobInclusionBlockRow, fallbackTimestamp time.Time) BlobInclusionBlockResponse {
	block := BlobInclusionBlockResponse{
		BlockNumber:    row.BlockNumber,
		BlockTimestamp: row.timestamp(fallbackTimestamp),
	}
	if row.BlobCount != nil {
		count := *row.BlobCount
		block.BlobCount = &count
		maxBlobs := blobSpaceLimit(derefInt(row.BlobParamsMax), derefInt64(row.BlobGasLimit))
		block.MaxBlobs = &maxBlobs
	}
	if row.BlobBaseFee != nil {
		if fee := strings.TrimSpace(*row.BlobBaseFee); fee != "" {
			block.BlobBaseFee = &fee
			block.BlobBaseFeeGwei = formatWeiAsGwei(fee)
		}
	}
	if row.BuilderKey.Valid {
		builder := toBlockBuilderResponse(models.BlockBuilder{
			BlockNumber:           row.BlockNumber,
			BlockTimestamp:        block.BlockTimestamp,
			FeeRecipient:          row.FeeRecipient.String,
			ExtraData:             row.ExtraData.String,
			BuilderKey:            row.BuilderKey.String,
			BuilderName:           row.BuilderName.String,
			TxCount:               derefInt(row.TxCount),
			ProposerPaymentWei:    row.ProposerPaymentWei,
			ProposerPaymentTo:     row.ProposerPaymentTo,
			CandidateSnapshot:     row.CandidateSnapshot,
			PendingCandidateTxs:   row.PendingCandidateTxs,
			EligibleSkippedTxs:    row.EligibleSkippedTxs,
			EligibleSkippedBlobs:  row.EligibleSkippedBlobs,
			EligibleSkippedMaxTip: row.EligibleSkippedMaxTip,
		})
		block.Builder = &builder
	}
	return block
}

// toBlobInclusionSkipped converts a skipped-block row, measuring its wait
// from the transaction's first-seen time when that is known. A skipped row
// always carries its candidate row's timestamp, so no fallback is needed.
func toBlobInclusionSkipped(row blobInclusionBlockRow, firstSeenAt *time.Time) BlobInclusionSkippedBlockResponse {
	entry := BlobInclusionSkippedBlockResponse{
		BlobInclusionBlockResponse: toBlobInclusionBlock(row, time.Time{}),
		Reason:                     row.Reason,
	}
	if firstSeenAt != nil {
		waited := entry.BlockTimestamp.Sub(*firstSeenAt).Milliseconds()
		entry.WaitedMs = &waited
	}
	return entry
}

func derefInt(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func derefInt64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}
