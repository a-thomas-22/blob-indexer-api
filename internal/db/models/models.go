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

// Common metadata keys
const (
	MetadataLastIndexedBlock = "last_indexed_block"
	MetadataIndexerVersion   = "indexer_version"
)
