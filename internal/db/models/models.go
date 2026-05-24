package models

import (
	"time"
)

// Blob represents a blob transaction in the database
type Blob struct {
	ID                int64     `db:"id"`
	NetworkID         int       `db:"network_id"`
	BlockNumber       int64     `db:"block_number"`
	BlobIndex         int       `db:"blob_index"`
	TxHash            string    `db:"tx_hash"`
	FromAddress       string    `db:"from_address"`
	UserAttribution   string    `db:"user_attribution"`
	BlobSizeBytes     int64     `db:"blob_size_bytes"`
	BaseFeePerBlobGas string    `db:"base_fee_per_blob_gas"` // Using string for numeric values to avoid precision issues
	TipPerBlobGas     string    `db:"tip_per_blob_gas"`
	TotalCostETH      string    `db:"total_cost_eth"`
	Timestamp         time.Time `db:"timestamp"`
	Confirmed         bool      `db:"confirmed"`
	IndexerVersion    string    `db:"indexer_version"`
	MaxFeePerBlobGas  *string   `db:"max_fee_per_blob_gas"` // Nullable for pre-migration rows
	BlobGasUsed       *int64    `db:"blob_gas_used"`        // Nullable for pre-migration rows
}

// BlobUser represents a known blob transaction sender
type BlobUser struct {
	ID          int64     `db:"id"`
	NetworkID   int       `db:"network_id"`
	Address     string    `db:"address"`
	Name        string    `db:"name"`
	Description string    `db:"description"`
	Category    string    `db:"category"`
	FirstSeen   time.Time `db:"first_seen"`
	LastSeen    time.Time `db:"last_seen"`
}

// Network represents an Ethereum network
type Network struct {
	ID         int64     `db:"id"`
	ChainID    int       `db:"chain_id"`
	Name       string    `db:"name"`
	StartBlock string    `db:"start_block"`
	IsEnabled  bool      `db:"is_enabled"`
	CreatedAt  time.Time `db:"created_at"`
	UpdatedAt  time.Time `db:"updated_at"`
}

// IndexerMetadata represents metadata about the indexer state
type IndexerMetadata struct {
	ID        int64  `db:"id"`
	NetworkID int    `db:"network_id"`
	Key       string `db:"key"`
	Value     string `db:"value"`
}

// IndexedBlock records a processed block's hashes for chain reorganization detection
type IndexedBlock struct {
	NetworkID   int       `db:"network_id"`
	BlockNumber int64     `db:"block_number"`
	BlockHash   string    `db:"block_hash"`
	ParentHash  string    `db:"parent_hash"`
	IndexedAt   time.Time `db:"indexed_at"`
}

// BlobUserStats holds aggregated blob user statistics returned by queries.
type BlobUserStats struct {
	Address           string    `db:"from_address" json:"address"`
	Name              string    `db:"user_attribution" json:"name"`
	Category          string    `db:"category" json:"category,omitempty"`
	BlobCount         int       `db:"blob_count" json:"blob_count"`
	TotalCostETH      string    `db:"total_cost_eth" json:"total_cost_eth"`
	LastTimestamp     time.Time `db:"last_timestamp" json:"last_timestamp"`
	BlobSharePercent  float64   `db:"blob_share_percent" json:"blob_share_percent,omitempty"`
	SpendSharePercent float64   `db:"spend_share_percent" json:"spend_share_percent,omitempty"`
}

// BlobUserCategoryShare holds category-level blob user market share statistics.
type BlobUserCategoryShare struct {
	Category          string  `db:"category" json:"category"`
	BlobCount         int     `db:"blob_count" json:"blob_count"`
	TotalCostETH      string  `db:"total_cost_eth" json:"total_cost_eth"`
	BlobSharePercent  float64 `db:"blob_share_percent" json:"blob_share_percent"`
	SpendSharePercent float64 `db:"spend_share_percent" json:"spend_share_percent"`
}

// BlockMetrics represents block-level blob pricing data.
type BlockMetrics struct {
	NetworkID        int       `db:"network_id"`
	BlockNumber      int64     `db:"block_number"`
	BlockTimestamp   time.Time `db:"block_timestamp"`
	BlobCount        int       `db:"blob_count"`
	BlobGasUsed      int64     `db:"blob_gas_used"`
	BlobGasTarget    int64     `db:"blob_gas_target"`
	BlobGasLimit     int64     `db:"blob_gas_limit"`
	ExcessBlobGas    int64     `db:"excess_blob_gas"`
	BlobBaseFee      string    `db:"blob_base_fee"`
	UtilizationRatio string    `db:"utilization_ratio"`
	BlobParamsTarget int       `db:"blob_params_target"`
	BlobParamsMax    int       `db:"blob_params_max"`
	UpdateFraction   int64     `db:"update_fraction"`
}

// BlobStatsAggregate is the aggregated result shape for blob stats queries.
type BlobStatsAggregate struct {
	TotalBlobs          int       `db:"total_blobs"`
	TotalConfirmedBlobs int       `db:"total_confirmed_blobs"`
	TotalPendingBlobs   int       `db:"total_pending_blobs"`
	AverageBaseFee      string    `db:"average_base_fee"`
	AverageTip          string    `db:"average_tip"`
	AverageTotalCost    string    `db:"average_total_cost"`
	LastIndexedTime     time.Time `db:"last_indexed_time"`
}

// BlobCountTotals is the aggregated confirmed/pending count shape.
type BlobCountTotals struct {
	Confirmed int `db:"confirmed_count"`
	Pending   int `db:"pending_count"`
}

// Common metadata keys
const (
	MetadataLastIndexedBlock = "last_indexed_block"
	MetadataIndexerVersion   = "indexer_version"
)
