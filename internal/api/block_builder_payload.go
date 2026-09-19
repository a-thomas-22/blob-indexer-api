package api

import (
	"encoding/hex"
	"strings"
	"time"

	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
)

// BlockBuilderResponse identifies who built one block and, when the indexer
// saw the block arrive live, what its own pending blob pool looked like at
// that moment.
//
// key/name are what the indexer's builder registry resolved from the raw
// header fields; fee_recipient and extra_data stay authoritative, so a later
// registry release can relabel a block without the raw evidence changing.
type BlockBuilderResponse struct {
	Key  string `json:"key" example:"titan"`
	Name string `json:"name" example:"Titan"`
	// Known is true only when the registry recognized the builder; false
	// means key was derived from the extra data or the fee recipient.
	Known bool `json:"known"`
	// FeeRecipient is the header's coinbase. Under MEV-Boost it is usually
	// the builder's own address; for a locally built block it is the
	// proposer's fee recipient, so it is ambiguous on its own.
	FeeRecipient string `json:"fee_recipient"`
	// ExtraData is the header's extraData as 0x-hex; ExtraDataText is its
	// decoding when every byte is printable ASCII, omitted otherwise.
	ExtraData     string `json:"extra_data" example:"0x6265617665726275696c642e6f7267"`
	ExtraDataText string `json:"extra_data_text,omitempty" example:"beaverbuild.org"`
	// TxCount is the number of transactions in the block, blob-carrying or
	// not.
	TxCount int `json:"tx_count"`
	// ProposerPaymentWei and ProposerPaymentTo describe the block's last
	// transaction when the fee recipient sent it — the conventional
	// MEV-Boost proposer payment. Both are omitted otherwise, which is a
	// heuristic: some builders pay the proposer by setting coinbase to the
	// proposer's fee recipient instead.
	ProposerPaymentWei *string `json:"proposer_payment_wei,omitempty" example:"42000000000000000"`
	ProposerPaymentEth string  `json:"proposer_payment_eth,omitempty" example:"0.042"`
	ProposerPaymentTo  *string `json:"proposer_payment_to,omitempty"`
	// CandidateSnapshot reports whether the indexer classified its pending
	// blob pool against this block. It is false for blocks indexed from
	// history — the pool is meaningless for a block that arrived long ago —
	// and the four aggregates below are then omitted.
	CandidateSnapshot     bool    `json:"candidate_snapshot"`
	PendingCandidateTxs   *int    `json:"pending_candidate_txs,omitempty"`
	EligibleSkippedTxs    *int    `json:"eligible_skipped_txs,omitempty"`
	EligibleSkippedBlobs  *int    `json:"eligible_skipped_blobs,omitempty"`
	EligibleSkippedMaxTip *string `json:"eligible_skipped_max_tip,omitempty"`
	// EligibleSkippedMaxTipGwei is the gwei companion of the wei value above.
	EligibleSkippedMaxTipGwei string `json:"eligible_skipped_max_tip_gwei,omitempty"`
}

// BlobInclusionCandidateResponse is one pending blob transaction our node had
// seen when a block arrived and that the block did not include, with the
// reason it is classified under.
//
// Only reason = "eligible" supports a claim about builder behavior; the
// others (too_recent, nonce_gap, priced_out_blob_fee, priced_out_exec_fee,
// no_room) explain why inclusion was impossible or unlikely regardless of the
// builder. Our node's mempool is not the builder's, so present even eligible
// rows as "visible to our node and not included".
type BlobInclusionCandidateResponse struct {
	TxHash          string `json:"tx_hash"`
	FromAddress     string `json:"from_address"`
	UserAttribution string `json:"user_attribution,omitempty"`
	Nonce           *int64 `json:"nonce,omitempty"`
	BlobCount       int    `json:"blob_count"`
	// Fee caps in wei as decimal strings, with gwei companions. Omitted when
	// the pending row carried no cap.
	MaxPriorityFeePerGas     *string   `json:"max_priority_fee_per_gas,omitempty"`
	MaxPriorityFeePerGasGwei string    `json:"max_priority_fee_per_gas_gwei,omitempty"`
	MaxFeePerGas             *string   `json:"max_fee_per_gas,omitempty"`
	MaxFeePerGasGwei         string    `json:"max_fee_per_gas_gwei,omitempty"`
	MaxFeePerBlobGas         *string   `json:"max_fee_per_blob_gas,omitempty"`
	MaxFeePerBlobGasGwei     string    `json:"max_fee_per_blob_gas_gwei,omitempty"`
	FirstSeenAt              time.Time `json:"first_seen_at"`
	Reason                   string    `json:"reason" enums:"eligible,too_recent,nonce_gap,priced_out_blob_fee,priced_out_exec_fee,no_room"`
}

// BlockDetailResponse is the /block/{number} payload: the same shape the
// WebSocket new_block event carries, plus the per-transaction candidate
// detail behind the block's builder aggregates. The list is empty when the
// block was indexed from history (no snapshot was taken) or when the
// indexer's retention window has pruned the rows.
type BlockDetailResponse struct {
	NewBlockData
	Candidates []BlobInclusionCandidateResponse `json:"candidates"`
}

// printableExtraDataText decodes a 0x-hex extra-data field to text when every
// byte is printable ASCII, which is what builders that stamp a label put
// there. Anything else (an empty field, a commitment hash, a client's binary
// version blob) yields "" and the field is omitted.
func printableExtraDataText(extraData string) string {
	trimmed := strings.TrimPrefix(strings.TrimPrefix(extraData, "0x"), "0X")
	raw, err := hex.DecodeString(trimmed)
	if err != nil || len(raw) == 0 {
		return ""
	}
	for _, b := range raw {
		if b < 0x20 || b > 0x7e {
			return ""
		}
	}
	return strings.TrimSpace(string(raw))
}

// toBlockBuilderResponse converts a stored builder row to its wire shape.
func toBlockBuilderResponse(builder models.BlockBuilder) BlockBuilderResponse {
	response := BlockBuilderResponse{
		Key:                   builder.BuilderKey,
		Name:                  builder.BuilderName,
		Known:                 builderKeyKnown(builder.BuilderKey),
		FeeRecipient:          builder.FeeRecipient,
		ExtraData:             builder.ExtraData,
		ExtraDataText:         printableExtraDataText(builder.ExtraData),
		TxCount:               builder.TxCount,
		ProposerPaymentWei:    builder.ProposerPaymentWei,
		ProposerPaymentTo:     builder.ProposerPaymentTo,
		CandidateSnapshot:     builder.CandidateSnapshot,
		PendingCandidateTxs:   builder.PendingCandidateTxs,
		EligibleSkippedTxs:    builder.EligibleSkippedTxs,
		EligibleSkippedBlobs:  builder.EligibleSkippedBlobs,
		EligibleSkippedMaxTip: builder.EligibleSkippedMaxTip,
	}
	if builder.ProposerPaymentWei != nil {
		response.ProposerPaymentEth = formatWeiAsETH(*builder.ProposerPaymentWei)
	}
	response.EligibleSkippedMaxTipGwei = formatOptionalWeiAsGwei(builder.EligibleSkippedMaxTip)
	return response
}

// toBlobInclusionCandidateResponses converts stored candidate rows to their
// wire shape, preserving the query's ordering (highest bid first).
func toBlobInclusionCandidateResponses(candidates []models.BlobInclusionCandidate) []BlobInclusionCandidateResponse {
	responses := make([]BlobInclusionCandidateResponse, 0, len(candidates))
	for _, candidate := range candidates {
		responses = append(responses, BlobInclusionCandidateResponse{
			TxHash:                   candidate.TxHash,
			FromAddress:              candidate.FromAddress,
			UserAttribution:          candidate.UserAttribution,
			Nonce:                    candidate.Nonce,
			BlobCount:                candidate.BlobCount,
			MaxPriorityFeePerGas:     candidate.MaxPriorityFeePerGas,
			MaxPriorityFeePerGasGwei: formatOptionalWeiAsGwei(candidate.MaxPriorityFeePerGas),
			MaxFeePerGas:             candidate.MaxFeePerGas,
			MaxFeePerGasGwei:         formatOptionalWeiAsGwei(candidate.MaxFeePerGas),
			MaxFeePerBlobGas:         candidate.MaxFeePerBlobGas,
			MaxFeePerBlobGasGwei:     formatOptionalWeiAsGwei(candidate.MaxFeePerBlobGas),
			FirstSeenAt:              candidate.FirstSeenAt,
			Reason:                   candidate.Reason,
		})
	}
	return responses
}

// blockBuildersByNumber indexes builder rows by block number for the
// multi-block reads (the WebSocket broadcast and reconnect snapshot).
func blockBuildersByNumber(builders []models.BlockBuilder) map[int64]BlockBuilderResponse {
	byNumber := make(map[int64]BlockBuilderResponse, len(builders))
	for _, builder := range builders {
		byNumber[builder.BlockNumber] = toBlockBuilderResponse(builder)
	}
	return byNumber
}
