package models

import (
	"time"

	"github.com/lib/pq"
)

// PendingBlockNumber is the internal sentinel used in Blob.BlockNumber for
// pending (mempool) blob rows that have not yet been included in a block.
// Pending rows live in the mempool_blobs table, which has no block_number
// column; API queries project this sentinel so the shared Blob scanning and
// serialization code treats any block_number < 0 as pending and emits JSON
// null on the wire. The confirmed flag remains the source of truth for
// whether a blob is included.
const PendingBlockNumber int64 = -1

// Confirmed is not a stored column: blobs holds confirmed rows only and
// mempool_blobs holds pending rows, so API queries project a literal
// (true/false AS confirmed) matching the source table. The flag stays on the
// struct because it is exposed on the wire and drives serialization
// (explorer URLs, cache TTLs).

// Blob represents a blob transaction in the database
type Blob struct {
	ID                int64     `db:"id"`
	ChainID           int       `db:"chain_id"`
	BlockNumber       int64     `db:"block_number"`
	BlobIndex         int       `db:"blob_index"`
	TxHash            string    `db:"tx_hash"`
	FromAddress       string    `db:"from_address"`
	UserAttribution   string    `db:"user_attribution"`
	BlobSizeBytes     int64     `db:"blob_size_bytes"`
	BaseFeePerBlobGas string    `db:"base_fee_per_blob_gas"` // Using string for numeric values to avoid precision issues
	TipPerBlobGas     string    `db:"tip_per_blob_gas"`
	TotalCostWei      string    `db:"total_cost_wei"`
	Timestamp         time.Time `db:"timestamp"`
	Confirmed         bool      `db:"confirmed"`
	MaxFeePerBlobGas  *string   `db:"max_fee_per_blob_gas"` // Nullable for pre-migration rows
	BlobGasUsed       *int64    `db:"blob_gas_used"`        // Nullable for pre-migration rows
	VersionedHash     *string   `db:"versioned_hash"`       // Nullable for pre-migration rows
	// Execution-layer (EIP-1559) fees of the carrying transaction, in wei.
	// Builders order competing blob transactions by the priority fee they
	// pay on execution gas, so these, not TipPerBlobGas (which is blob
	// fee-cap headroom), show who outbid whom for a block's blob slots.
	// PriorityFeePerGas is the fee actually paid, min(tip cap, fee cap minus
	// the block's base fee); it is unknown until inclusion, so pending rows
	// carry only the two caps. All three are NULL for rows indexed before
	// the columns existed.
	MaxPriorityFeePerGas *string `db:"max_priority_fee_per_gas"`
	MaxFeePerGas         *string `db:"max_fee_per_gas"`
	PriorityFeePerGas    *string `db:"priority_fee_per_gas"`
	// Slot is the beacon slot of the including block, derived at index time
	// from the block timestamp and the network's beacon genesis time. NULL for
	// pending rows (no slot until inclusion, and mempool_blobs has no column —
	// queries project NULL), for confirmed rows indexed before the slot
	// migration, and for networks whose beacon genesis time is unknown.
	Slot *int64 `db:"slot"`
	// VersionedHashes is the transaction's full ordered list of EIP-4844
	// versioned blob hashes. Not a stored column: the API's blob projections
	// compute it from the sibling rows' versioned_hash values, so it is empty
	// for rows indexed before the versioned-hash migration.
	VersionedHashes pq.StringArray `db:"versioned_hashes"`
	// Nonce is the sender's account nonce. Persisted only on mempool_blobs
	// rows (blobs has no nonce column), where it lets the indexer delete
	// superseded pending rows when a fee-bumped replacement reuses the
	// sender's nonce under a new hash. Excluded from scanning: no query
	// selects it, and legacy pending rows hold NULL.
	Nonce uint64 `db:"-"`
	// FirstSeenAt is when the indexer first saw the transaction pending,
	// copied from mempool_blobs.timestamp when the block promoted the row.
	// The block timestamp minus this is the time to inclusion. NULL when the
	// tx was never observed pending (indexed from history, or it arrived
	// with its block) and for rows indexed before migration 000017. For
	// pending rows, queries project mempool_blobs.timestamp here.
	//
	// Accepted limitation: the value cannot be rederived from the chain, so
	// a reindex or reorg cleanup that deletes the confirmed row drops it for
	// good — the reinserted row carries NULL unless the transaction is still
	// pending when it is reinserted.
	FirstSeenAt *time.Time `db:"first_seen_at"`
	// TxIndex is the carrying transaction's position in its block. NULL for
	// pending rows and for confirmed rows indexed before migration 000017
	// that no backfill or reindex has revisited.
	TxIndex *int `db:"tx_index"`
}

// BlockBuilder records who built an indexed block, from the header's fee
// recipient and extra data, plus the pending-pool snapshot aggregates taken
// when the block arrived live. One row per indexed block, keyed like
// block_metrics. See migration 000017 for the column semantics.
type BlockBuilder struct {
	ChainID        int       `db:"chain_id"`
	BlockNumber    int64     `db:"block_number"`
	BlockTimestamp time.Time `db:"block_timestamp"`
	// FeeRecipient is header.coinbase in the same checksummed form as
	// Blob.FromAddress.
	FeeRecipient string `db:"fee_recipient"`
	// ExtraData is header.extraData as 0x-prefixed hex ("0x" when empty).
	ExtraData string `db:"extra_data"`
	// BuilderKey groups blocks by builder; BuilderName is its display
	// label. Both are resolved by the indexer's builder registry from the
	// raw fields and are never empty.
	BuilderKey  string `db:"builder_key"`
	BuilderName string `db:"builder_name"`
	TxCount     int    `db:"tx_count"`
	// ProposerPaymentWei and ProposerPaymentTo describe the block's last
	// transaction when it was sent by the fee recipient (the conventional
	// MEV-Boost proposer payment); both are NULL otherwise.
	ProposerPaymentWei *string `db:"proposer_payment_wei"`
	ProposerPaymentTo  *string `db:"proposer_payment_to"`
	// CandidateSnapshot is true when the block arrived live and the indexer
	// classified the pending blob pool against it; the aggregates below are
	// NULL when it is false.
	CandidateSnapshot     bool    `db:"candidate_snapshot"`
	PendingCandidateTxs   *int    `db:"pending_candidate_txs"`
	EligibleSkippedTxs    *int    `db:"eligible_skipped_txs"`
	EligibleSkippedBlobs  *int    `db:"eligible_skipped_blobs"`
	EligibleSkippedMaxTip *string `db:"eligible_skipped_max_tip"`
}

// Classification of a pending blob transaction a live block did not include
// (blob_inclusion_candidates.reason). Only CandidateEligible supports a
// claim about builder behavior; the others explain why inclusion was
// impossible or unlikely regardless of the builder. When several apply, the
// first in this order wins.
const (
	CandidateTooRecent        = "too_recent"
	CandidateNonceGap         = "nonce_gap"
	CandidatePricedOutBlobFee = "priced_out_blob_fee"
	CandidatePricedOutExecFee = "priced_out_exec_fee"
	CandidateNoRoom           = "no_room"
	CandidateEligible         = "eligible"
)

// BlobInclusionCandidate is one pending blob transaction our node had seen
// when a live block arrived and that the block did not include. Rows are
// pruned after the indexer's retention window; the per-block aggregates on
// BlockBuilder are permanent.
type BlobInclusionCandidate struct {
	ChainID              int       `db:"chain_id"`
	BlockNumber          int64     `db:"block_number"`
	BlockTimestamp       time.Time `db:"block_timestamp"`
	TxHash               string    `db:"tx_hash"`
	FromAddress          string    `db:"from_address"`
	UserAttribution      string    `db:"user_attribution"`
	Nonce                *int64    `db:"nonce"`
	BlobCount            int       `db:"blob_count"`
	MaxPriorityFeePerGas *string   `db:"max_priority_fee_per_gas"`
	MaxFeePerGas         *string   `db:"max_fee_per_gas"`
	MaxFeePerBlobGas     *string   `db:"max_fee_per_blob_gas"`
	FirstSeenAt          time.Time `db:"first_seen_at"`
	Reason               string    `db:"reason"`
}

// BlobReplacement records a pending blob transaction the indexer evicted
// because the sender reused its nonce: a fee-bumped replacement observed
// either in the mempool or as a confirmed transaction. One row per replaced
// hash; a re-observed replacement upserts so the latest replacement wins.
type BlobReplacement struct {
	ChainID           int       `db:"chain_id"`
	ReplacedTxHash    string    `db:"replaced_tx_hash"`
	ReplacementTxHash string    `db:"replacement_tx_hash"`
	FromAddress       string    `db:"from_address"`
	Nonce             int64     `db:"nonce"`
	ReplacedAt        time.Time `db:"replaced_at"`
	// Fee context of both sides of the bump, in wei, and when the replaced
	// transaction was first seen pending. All NULL on rows written before
	// migration 000017.
	ReplacedMaxPriorityFeePerGas    *string    `db:"replaced_max_priority_fee_per_gas"`
	ReplacedMaxFeePerBlobGas        *string    `db:"replaced_max_fee_per_blob_gas"`
	ReplacedFirstSeenAt             *time.Time `db:"replaced_first_seen_at"`
	ReplacementMaxPriorityFeePerGas *string    `db:"replacement_max_priority_fee_per_gas"`
	ReplacementMaxFeePerBlobGas     *string    `db:"replacement_max_fee_per_blob_gas"`
}

// BlobUser represents a known blob transaction sender
type BlobUser struct {
	ID          int64     `db:"id"`
	ChainID     int       `db:"chain_id"`
	Address     string    `db:"address"`
	Name        string    `db:"name"`
	Description string    `db:"description"`
	Category    string    `db:"category"`
	FirstSeen   time.Time `db:"first_seen"`
	LastSeen    time.Time `db:"last_seen"`
}

// Network represents an Ethereum network
type Network struct {
	ChainID    int       `db:"chain_id"`
	Name       string    `db:"name"`
	StartBlock string    `db:"start_block"`
	IsEnabled  bool      `db:"is_enabled"`
	CreatedAt  time.Time `db:"created_at"`
	UpdatedAt  time.Time `db:"updated_at"`
}

// IndexerMetadata represents metadata about the indexer state
type IndexerMetadata struct {
	ID      int64  `db:"id"`
	ChainID int    `db:"chain_id"`
	Key     string `db:"key"`
	Value   string `db:"value"`
}

// IndexedBlock records a processed block's hashes for chain reorganization detection
type IndexedBlock struct {
	ChainID     int       `db:"chain_id"`
	BlockNumber int64     `db:"block_number"`
	BlockHash   string    `db:"block_hash"`
	ParentHash  string    `db:"parent_hash"`
	IndexedAt   time.Time `db:"indexed_at"`
}

// BlockReindexRequest records an operator-requested block reindex range.
type BlockReindexRequest struct {
	ID          int64      `db:"id" json:"id"`
	ChainID     int        `db:"chain_id" json:"chain_id"`
	StartBlock  int64      `db:"start_block" json:"start_block"`
	EndBlock    int64      `db:"end_block" json:"end_block"`
	Status      string     `db:"status" json:"status"`
	RequestedBy string     `db:"requested_by" json:"requested_by"`
	Reason      string     `db:"reason" json:"reason"`
	Attempts    int        `db:"attempts" json:"attempts"`
	LastError   *string    `db:"last_error" json:"last_error,omitempty"`
	ClaimedBy   *string    `db:"claimed_by" json:"claimed_by,omitempty"`
	RequestedAt time.Time  `db:"requested_at" json:"requested_at"`
	StartedAt   *time.Time `db:"started_at" json:"started_at,omitempty"`
	CompletedAt *time.Time `db:"completed_at" json:"completed_at,omitempty"`
	UpdatedAt   time.Time  `db:"updated_at" json:"updated_at"`
}

// BlobUserStats holds aggregated blob user statistics returned by queries.
type BlobUserStats struct {
	Address           string    `db:"from_address" json:"address"`
	Name              string    `db:"user_attribution" json:"name"`
	Category          string    `db:"category" json:"category,omitempty"`
	BlobCount         int       `db:"blob_count" json:"blob_count"`
	TotalCostWei      string    `db:"total_cost_wei" json:"total_cost_wei"`
	LastTimestamp     time.Time `db:"last_timestamp" json:"last_timestamp"`
	BlobSharePercent  float64   `db:"blob_share_percent" json:"blob_share_percent,omitempty"`
	SpendSharePercent float64   `db:"spend_share_percent" json:"spend_share_percent,omitempty"`
}

// BlobUserGroupStats holds one row of the entity-grouped users leaderboard:
// attributed addresses collapse into one row per attribution entity, while
// unattributed addresses stay one row each, keyed by their address.
type BlobUserGroupStats struct {
	Key               string         `db:"group_key" json:"key"`
	Name              string         `db:"user_attribution" json:"name"`
	Category          string         `db:"category" json:"category,omitempty"`
	Addresses         pq.StringArray `db:"addresses" json:"addresses"`
	BlobCount         int            `db:"blob_count" json:"blob_count"`
	TotalCostWei      string         `db:"total_cost_wei" json:"total_cost_wei"`
	LastTimestamp     time.Time      `db:"last_timestamp" json:"last_timestamp"`
	BlobSharePercent  float64        `db:"blob_share_percent" json:"blob_share_percent,omitempty"`
	SpendSharePercent float64        `db:"spend_share_percent" json:"spend_share_percent,omitempty"`
}

// BlobUserCategoryShare holds category-level blob user market share statistics.
type BlobUserCategoryShare struct {
	Category          string  `db:"category" json:"category"`
	BlobCount         int     `db:"blob_count" json:"blob_count"`
	TotalCostWei      string  `db:"total_cost_wei" json:"total_cost_wei"`
	BlobSharePercent  float64 `db:"blob_share_percent" json:"blob_share_percent"`
	SpendSharePercent float64 `db:"spend_share_percent" json:"spend_share_percent"`
}

// BlockMetrics represents block-level blob pricing data.
type BlockMetrics struct {
	ChainID        int       `db:"chain_id"`
	BlockNumber    int64     `db:"block_number"`
	BlockTimestamp time.Time `db:"block_timestamp"`
	BlobCount      int       `db:"blob_count"`
	BlobGasUsed    int64     `db:"blob_gas_used"`
	BlobGasTarget  int64     `db:"blob_gas_target"`
	BlobGasLimit   int64     `db:"blob_gas_limit"`
	ExcessBlobGas  int64     `db:"excess_blob_gas"`
	BlobBaseFee    string    `db:"blob_base_fee"`
	// BaseFeeWei is the block's execution-layer (EIP-1559) base fee in wei,
	// stored as text (NUMERIC column) to preserve precision. It feeds the
	// EIP-7918 reserve-price branch of the next-fee prediction. Rows indexed
	// before migration 000010 carry "0", which disables that branch and falls
	// back to the legacy pre-Osaka estimate.
	BaseFeeWei       string `db:"base_fee_wei"`
	UtilizationRatio string `db:"utilization_ratio"`
	BlobParamsTarget int    `db:"blob_params_target"`
	BlobParamsMax    int    `db:"blob_params_max"`
	UpdateFraction   int64  `db:"update_fraction"`
}

// BlobStatsAggregate is the aggregated result shape for blob stats queries.
type BlobStatsAggregate struct {
	TotalBlobs          int       `db:"total_blobs"`
	TotalConfirmedBlobs int       `db:"total_confirmed_blobs"`
	TotalPendingBlobs   int       `db:"total_pending_blobs"`
	AverageBaseFee      string    `db:"average_base_fee"`
	AverageTip          string    `db:"average_tip"`
	AverageTotalCost    string    `db:"average_total_cost"`
	LastIndexedBlock    uint64    `db:"last_indexed_block"`
	LastIndexedTime     time.Time `db:"last_indexed_time"`
}

// BlobCountTotals is the aggregated confirmed/pending count shape.
type BlobCountTotals struct {
	Confirmed int `db:"confirmed_count"`
	Pending   int `db:"pending_count"`
}

// Common metadata keys
const (
	MetadataLastIndexedBlock     = "last_indexed_block"
	MetadataIndexerVersion       = "indexer_version"
	MetadataCurrentChainHead     = "current_chain_head"
	MetadataChainHeadUpdatedAt   = "current_chain_head_updated_at"
	MetadataLastIndexedAt        = "last_indexed_at"
	MetadataWebSocketFreshnessAt = "websocket_freshness_at"
	MetadataBackfillActive       = "backfill_active"
	MetadataBackfillStartBlock   = "backfill_start_block"
	MetadataBackfillCurrentBlock = "backfill_current_block"
	MetadataBackfillTargetBlock  = "backfill_target_block"
	MetadataBackfillUpdatedAt    = "backfill_updated_at"
	MetadataBackfillCompletedAt  = "backfill_completed_at"
	// MetadataReorgRewindFrom / MetadataReorgInvalidatedThrough persist the
	// block range a reorg invalidated, written in the same transaction as the
	// reorg deletions. They survive a crash between the deletions and the
	// re-indexing of the range, and are cleared only once indexed_blocks
	// provably covers the range again.
	MetadataReorgRewindFrom         = "reorg_rewind_from"
	MetadataReorgInvalidatedThrough = "reorg_invalidated_through"
	// MetadataStreakBackfillBlock is the highest block the /records streak
	// backfill has rebuilt. It makes the backfill resumable across restarts:
	// a fresh deploy walks all indexed history once and later starts pick up
	// where the last run stopped.
	MetadataStreakBackfillBlock = "records_streak_backfill_block"
	// MetadataStreakBackfillKinds fingerprints the streak definitions the
	// checkpoint above was produced with. A watermark alone would silently
	// apply a changed or newly added predicate to new blocks only, because a
	// network that finished its backfill never revisits history. Storing what
	// was built alongside how far makes a definition change reset the
	// checkpoint automatically instead of needing a hand-written DELETE in
	// whichever migration introduced it.
	MetadataStreakBackfillKinds = "records_streak_backfill_kinds"
	// MetadataPriorityFeeBackfillBlock is the highest block the priority fee
	// backfill has walked. Rows indexed before migration 000015 hold no
	// execution-layer fees; the backfill refetches their blocks and fills the
	// fees in place, and this checkpoint lets a restart resume the walk.
	MetadataPriorityFeeBackfillBlock = "priority_fee_backfill_block"
	// MetadataBlockBuilderBackfillFloor is the lowest block the block builder
	// backfill has verified: every indexed block from it up to the tip
	// carries a block_builders row. Blocks indexed before migration 000017
	// have none; the backfill walks newest first, refetches them and inserts
	// the row (filling blobs.tx_index alongside), and a restart resumes the
	// walk just below this floor.
	MetadataBlockBuilderBackfillFloor = "block_builder_backfill_floor"
	// MetadataBlockBuilderBackfillBlock was the checkpoint of the oldest-first
	// builder backfill earlier releases ran: the highest block of a complete
	// prefix of history, the opposite of the floor. The backfill deletes it on
	// every start so the two are never confused; the constant remains for
	// that cleanup. The 000017 down migration deletes the same key by its
	// literal name, so the two spellings must be kept in sync by hand.
	MetadataBlockBuilderBackfillBlock = "block_builder_backfill_block"
	// MetadataBlockBuilderRegistryVersion fingerprints the builder registry
	// the block_builders labels were resolved with. When the running
	// binary's registry differs, the indexer relabels existing rows from
	// their raw fields before recording the new fingerprint.
	MetadataBlockBuilderRegistryVersion = "block_builder_registry_version"
)

// FormatMetadataTimestamp serializes metadata timestamps consistently.
func FormatMetadataTimestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// ParseMetadataTimestamp parses timestamps stored in indexer_metadata.
func ParseMetadataTimestamp(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, value)
}
