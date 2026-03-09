package ethereum

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"
)

func TestIsWebsocket(t *testing.T) {
	c := &Client{isWebsocket: true}
	if !c.IsWebsocket() {
		t.Error("expected IsWebsocket() = true")
	}

	c2 := &Client{isWebsocket: false}
	if c2.IsWebsocket() {
		t.Error("expected IsWebsocket() = false")
	}
}

func TestIsBlobTransaction(t *testing.T) {
	c := &Client{}

	// Regular (legacy) transaction
	legacyTx := types.NewTransaction(0, common.Address{}, big.NewInt(0), 21000, big.NewInt(1), nil)
	if c.IsBlobTransaction(legacyTx) {
		t.Error("legacy tx should not be a blob transaction")
	}

	// Dynamic fee transaction (EIP-1559)
	dynamicTx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   big.NewInt(1),
		Nonce:     0,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(1),
		Gas:       21000,
	})
	if c.IsBlobTransaction(dynamicTx) {
		t.Error("dynamic fee tx should not be a blob transaction")
	}

	// Blob transaction (EIP-4844)
	blobTx := types.NewTx(&types.BlobTx{
		ChainID:    uint256.NewInt(1),
		Nonce:      0,
		GasTipCap:  uint256.NewInt(1),
		GasFeeCap:  uint256.NewInt(1),
		Gas:        21000,
		BlobFeeCap: uint256.NewInt(1),
		BlobHashes: []common.Hash{{}},
	})
	if !c.IsBlobTransaction(blobTx) {
		t.Error("blob tx should be a blob transaction")
	}
}

func TestGetBlockTimestamp(t *testing.T) {
	c := &Client{}
	now := uint64(time.Now().Unix())
	header := &types.Header{Time: now}
	block := types.NewBlockWithHeader(header)

	ts := c.GetBlockTimestamp(block)
	if ts.Unix() != int64(now) {
		t.Errorf("expected timestamp %d, got %d", now, ts.Unix())
	}
}

func TestGetBlockTimestamp_ZeroTime(t *testing.T) {
	c := &Client{}
	header := &types.Header{Time: 0}
	block := types.NewBlockWithHeader(header)

	ts := c.GetBlockTimestamp(block)
	if !ts.Equal(time.Unix(0, 0)) {
		t.Errorf("expected zero time, got %v", ts)
	}
}

func TestClose_EmptyClient(t *testing.T) {
	// Create a minimal client with no subscriptions
	// Can't test Close() without a real ethClient/rpcClient since they have unexported fields
	// Just verify the subscription maps work correctly
	c := &Client{
		blockSubs:     make(map[string]*BlockSubscription),
		pendingTxSubs: make(map[string]*PendingTxSubscription),
	}

	// Unsubscribe from non-existent subscriptions should be no-ops
	c.UnsubscribeFromNewHeads("nonexistent")
	c.UnsubscribeFromPendingTransactions("nonexistent")

	if len(c.blockSubs) != 0 {
		t.Error("expected empty blockSubs")
	}
	if len(c.pendingTxSubs) != 0 {
		t.Error("expected empty pendingTxSubs")
	}
}

func TestSubscribeToNewHeads_RequiresWebsocket(t *testing.T) {
	c := &Client{
		isWebsocket:   false,
		blockSubs:     make(map[string]*BlockSubscription),
		pendingTxSubs: make(map[string]*PendingTxSubscription),
	}

	_, err := c.SubscribeToNewHeads(context.TODO(), "test")
	if err == nil {
		t.Error("expected error for non-websocket client")
	}
}

func TestSubscribeToPendingTransactions_RequiresWebsocket(t *testing.T) {
	c := &Client{
		isWebsocket:   false,
		blockSubs:     make(map[string]*BlockSubscription),
		pendingTxSubs: make(map[string]*PendingTxSubscription),
	}

	_, err := c.SubscribeToPendingTransactions(context.TODO(), "test")
	if err == nil {
		t.Error("expected error for non-websocket client")
	}
}

func TestGetBlobBaseFee_CacheHit(t *testing.T) {
	c := &Client{
		blobBaseFeeCache: big.NewInt(42000),
		blobBaseFeeTime:  time.Now(), // fresh cache
	}

	fee, err := c.GetBlobBaseFee(context.TODO(), 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fee.Int64() != 42000 {
		t.Errorf("expected 42000, got %d", fee.Int64())
	}
}

func TestGetBlobBaseFee_CacheReturnsNewInstance(t *testing.T) {
	original := big.NewInt(42000)
	c := &Client{
		blobBaseFeeCache: original,
		blobBaseFeeTime:  time.Now(),
	}

	fee, _ := c.GetBlobBaseFee(context.TODO(), 100)
	// Modifying the returned value should not affect the cache
	fee.SetInt64(0)

	fee2, _ := c.GetBlobBaseFee(context.TODO(), 100)
	if fee2.Int64() != 42000 {
		t.Errorf("cache was modified, expected 42000, got %d", fee2.Int64())
	}
}
