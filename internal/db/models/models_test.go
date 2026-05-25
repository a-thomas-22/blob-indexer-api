package models

import (
	"testing"
	"time"
)

func TestBlobModel(t *testing.T) {
	now := time.Now()
	blob := Blob{
		ID:                1,
		NetworkID:         1,
		BlockNumber:       12345,
		BlobIndex:         0,
		TxHash:            "0xabc123",
		FromAddress:       "0xdef456",
		UserAttribution:   "Optimism",
		BlobSizeBytes:     131072,
		BaseFeePerBlobGas: "1000000000",
		TipPerBlobGas:     "100000000",
		TotalCostETH:      "0.001",
		Timestamp:         now,
		Confirmed:         true,
		IndexerVersion:    "v1.0.0",
	}

	if blob.ID != 1 {
		t.Errorf("expected ID 1, got %d", blob.ID)
	}
	if blob.NetworkID != 1 {
		t.Errorf("expected NetworkID 1, got %d", blob.NetworkID)
	}
	if blob.BlockNumber != 12345 {
		t.Errorf("expected BlockNumber 12345, got %d", blob.BlockNumber)
	}
	if blob.TxHash != "0xabc123" {
		t.Errorf("expected TxHash '0xabc123', got %q", blob.TxHash)
	}
	if !blob.Confirmed {
		t.Error("expected Confirmed to be true")
	}
}

func TestBlobUserModel(t *testing.T) {
	now := time.Now()
	user := BlobUser{
		ID:          1,
		NetworkID:   1,
		Address:     "0xabc",
		Name:        "Optimism",
		Description: "Optimism rollup",
		Category:    "rollup",
		FirstSeen:   now,
		LastSeen:    now,
	}

	if user.Name != "Optimism" {
		t.Errorf("expected Name 'Optimism', got %q", user.Name)
	}
	if user.Category != "rollup" {
		t.Errorf("expected Category 'rollup', got %q", user.Category)
	}
}

func TestNetworkModel(t *testing.T) {
	network := Network{
		ID:         1,
		ChainID:    1,
		Name:       "mainnet",
		StartBlock: "12345",
		IsEnabled:  true,
	}

	if network.Name != "mainnet" {
		t.Errorf("expected Name 'mainnet', got %q", network.Name)
	}
	if !network.IsEnabled {
		t.Error("expected IsEnabled to be true")
	}
}

func TestIndexerMetadataModel(t *testing.T) {
	meta := IndexerMetadata{
		ID:        1,
		NetworkID: 1,
		Key:       MetadataLastIndexedBlock,
		Value:     "12345",
	}

	if meta.Key != "last_indexed_block" {
		t.Errorf("expected Key 'last_indexed_block', got %q", meta.Key)
	}
}

func TestIndexedBlockModel(t *testing.T) {
	now := time.Now()
	block := IndexedBlock{
		NetworkID:   1,
		BlockNumber: 12345,
		BlockHash:   "0xhash",
		ParentHash:  "0xparent",
		IndexedAt:   now,
	}

	if block.BlockNumber != 12345 {
		t.Errorf("expected BlockNumber 12345, got %d", block.BlockNumber)
	}
	if block.BlockHash != "0xhash" {
		t.Errorf("expected BlockHash '0xhash', got %q", block.BlockHash)
	}
}

func TestMetadataConstants(t *testing.T) {
	if MetadataLastIndexedBlock != "last_indexed_block" {
		t.Errorf("expected MetadataLastIndexedBlock to be 'last_indexed_block', got %q", MetadataLastIndexedBlock)
	}
	if MetadataIndexerVersion != "indexer_version" {
		t.Errorf("expected MetadataIndexerVersion to be 'indexer_version', got %q", MetadataIndexerVersion)
	}
	if MetadataCurrentChainHead != "current_chain_head" {
		t.Errorf("expected MetadataCurrentChainHead to be 'current_chain_head', got %q", MetadataCurrentChainHead)
	}
	if MetadataChainHeadUpdatedAt != "current_chain_head_updated_at" {
		t.Errorf("expected MetadataChainHeadUpdatedAt to be 'current_chain_head_updated_at', got %q", MetadataChainHeadUpdatedAt)
	}
	if MetadataLastIndexedAt != "last_indexed_at" {
		t.Errorf("expected MetadataLastIndexedAt to be 'last_indexed_at', got %q", MetadataLastIndexedAt)
	}
	if MetadataWebSocketFreshnessAt != "websocket_freshness_at" {
		t.Errorf("expected MetadataWebSocketFreshnessAt to be 'websocket_freshness_at', got %q", MetadataWebSocketFreshnessAt)
	}
	if MetadataBackfillActive != "backfill_active" {
		t.Errorf("expected MetadataBackfillActive to be 'backfill_active', got %q", MetadataBackfillActive)
	}
	if MetadataBackfillStartBlock != "backfill_start_block" {
		t.Errorf("expected MetadataBackfillStartBlock to be 'backfill_start_block', got %q", MetadataBackfillStartBlock)
	}
	if MetadataBackfillCurrentBlock != "backfill_current_block" {
		t.Errorf("expected MetadataBackfillCurrentBlock to be 'backfill_current_block', got %q", MetadataBackfillCurrentBlock)
	}
	if MetadataBackfillTargetBlock != "backfill_target_block" {
		t.Errorf("expected MetadataBackfillTargetBlock to be 'backfill_target_block', got %q", MetadataBackfillTargetBlock)
	}
	if MetadataBackfillUpdatedAt != "backfill_updated_at" {
		t.Errorf("expected MetadataBackfillUpdatedAt to be 'backfill_updated_at', got %q", MetadataBackfillUpdatedAt)
	}
	if MetadataBackfillCompletedAt != "backfill_completed_at" {
		t.Errorf("expected MetadataBackfillCompletedAt to be 'backfill_completed_at', got %q", MetadataBackfillCompletedAt)
	}
}

func TestMetadataTimestampRoundTrip(t *testing.T) {
	ts := time.Date(2026, 5, 24, 12, 34, 56, 789, time.FixedZone("test", -5*60*60))

	encoded := FormatMetadataTimestamp(ts)
	decoded, err := ParseMetadataTimestamp(encoded)
	if err != nil {
		t.Fatalf("ParseMetadataTimestamp() error = %v", err)
	}

	if !decoded.Equal(ts) {
		t.Fatalf("expected decoded timestamp %s to equal %s", decoded, ts)
	}
	if decoded.Location() != time.UTC {
		t.Fatalf("expected decoded timestamp to use UTC location, got %s", decoded.Location())
	}
}

func TestBlobPendingTransaction(t *testing.T) {
	blob := Blob{
		NetworkID:   1,
		BlockNumber: -1, // Pending transaction marker
		Confirmed:   false,
	}

	if blob.BlockNumber != -1 {
		t.Errorf("expected BlockNumber -1 for pending tx, got %d", blob.BlockNumber)
	}
	if blob.Confirmed {
		t.Error("expected Confirmed to be false for pending tx")
	}
}
