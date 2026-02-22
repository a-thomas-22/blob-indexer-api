package ethereum

import (
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
)

func TestIsBlobTransaction(t *testing.T) {
	client := &Client{}

	tests := []struct {
		name     string
		txType   uint8
		expected bool
	}{
		{"blob transaction", types.BlobTxType, true},
		{"legacy transaction", types.LegacyTxType, false},
		{"access list transaction", types.AccessListTxType, false},
		{"dynamic fee transaction", types.DynamicFeeTxType, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tx *types.Transaction
			switch tt.txType {
			case types.LegacyTxType:
				tx = types.NewTx(&types.LegacyTx{})
			case types.AccessListTxType:
				tx = types.NewTx(&types.AccessListTx{})
			case types.DynamicFeeTxType:
				tx = types.NewTx(&types.DynamicFeeTx{})
			case types.BlobTxType:
				tx = types.NewTx(&types.BlobTx{})
			}

			result := client.IsBlobTransaction(tx)
			if result != tt.expected {
				t.Errorf("IsBlobTransaction() = %v, want %v for tx type %d", result, tt.expected, tt.txType)
			}
		})
	}
}

func TestGetBlockTimestamp(t *testing.T) {
	client := &Client{}

	// Create a block header with a known timestamp
	timestamp := uint64(1700000000) // 2023-11-14T22:13:20Z
	header := &types.Header{
		Time: timestamp,
	}
	block := types.NewBlockWithHeader(header)

	result := client.GetBlockTimestamp(block)
	expected := time.Unix(int64(timestamp), 0)

	if !result.Equal(expected) {
		t.Errorf("GetBlockTimestamp() = %v, want %v", result, expected)
	}
}

func TestIsWebsocket(t *testing.T) {
	tests := []struct {
		name     string
		isWS     bool
		expected bool
	}{
		{"websocket client", true, true},
		{"http client", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &Client{isWebsocket: tt.isWS}
			if client.IsWebsocket() != tt.expected {
				t.Errorf("IsWebsocket() = %v, want %v", client.IsWebsocket(), tt.expected)
			}
		})
	}
}

func TestClient_BlobBaseFeeCaching(t *testing.T) {
	client := &Client{
		blobBaseFeeCache: big.NewInt(1000000),
		blobBaseFeeTime:  time.Now(),
	}

	// Verify cache fields are accessible
	if client.blobBaseFeeCache.Cmp(big.NewInt(1000000)) != 0 {
		t.Error("expected cached blob base fee to be 1000000")
	}

	if time.Since(client.blobBaseFeeTime) > time.Second {
		t.Error("expected recent cache time")
	}
}

func TestClient_SubscriptionMaps(t *testing.T) {
	client := &Client{
		blockSubs:     make(map[string]*BlockSubscription),
		pendingTxSubs: make(map[string]*PendingTxSubscription),
	}

	if len(client.blockSubs) != 0 {
		t.Error("expected empty block subscriptions map")
	}
	if len(client.pendingTxSubs) != 0 {
		t.Error("expected empty pending tx subscriptions map")
	}
}
