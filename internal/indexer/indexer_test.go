package indexer

import (
	"context"
	"crypto/ecdsa"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/a-thomas-22/blob-indexer-api/internal/attribution"
	"github.com/a-thomas-22/blob-indexer-api/internal/blobparams"
	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	_ "github.com/a-thomas-22/blob-indexer-api/internal/testutil"
)

// newTestIndexer creates a minimal Indexer without connecting to any real services.
func newTestIndexer() *Indexer {
	cfg := &config.Config{
		Indexer: config.IndexerConfig{
			Version:                "test-v1",
			BatchSize:              50,
			PollingInterval:        10 * time.Second,
			MempoolPollingInterval: 20 * time.Second,
			MempoolTTL:             30 * time.Minute,
			MempoolCleanupInterval: 5 * time.Minute,
			MaxBlockRetries:        3,
			GapScanInterval:        5 * time.Minute,
			MaxReorgDepth:          64,
		},
	}
	network := config.NetworkConfig{
		Name:       "testnet",
		ChainID:    42,
		StartBlock: "100",
		Enabled:    true,
	}
	ctx, cancel := context.WithCancel(context.Background())
	attrSvc := attribution.NewService(nil, network.ChainID)
	return &Indexer{
		attribution:            attrSvc,
		config:                 cfg,
		network:                network,
		batchSize:              cfg.Indexer.BatchSize,
		pollingInterval:        cfg.Indexer.PollingInterval,
		mempoolPollingInterval: cfg.Indexer.MempoolPollingInterval,
		mempoolTTL:             cfg.Indexer.MempoolTTL,
		mempoolCleanupInterval: cfg.Indexer.MempoolCleanupInterval,
		workerCount:            DefaultWorkerCount,
		maxBlockRetries:        cfg.Indexer.MaxBlockRetries,
		gapScanInterval:        cfg.Indexer.GapScanInterval,
		maxReorgDepth:          cfg.Indexer.MaxReorgDepth,
		ctx:                    ctx,
		cancel:                 cancel,
		indexerVersion:         cfg.Indexer.Version,
		chainConfig:            blobparams.ChainConfigForID(network.ChainID),
		blockTaskCh:            make(chan BlockTask, 1000),
		failedBlocks:           make(map[uint64]int),
		failedBlockNextRetry:   make(map[uint64]time.Time),
		mu:                     sync.Mutex{},
		failedBlocksMu:         sync.Mutex{},
	}
}

func TestNewTestIndexer(t *testing.T) {
	idx := newTestIndexer()
	if idx == nil {
		t.Fatal("expected non-nil indexer")
	}
	if idx.batchSize != 50 {
		t.Errorf("expected batchSize=50, got %d", idx.batchSize)
	}
	if idx.pollingInterval != 10*time.Second {
		t.Errorf("expected pollingInterval=10s, got %s", idx.pollingInterval)
	}
	if idx.mempoolPollingInterval != 20*time.Second {
		t.Errorf("expected mempoolPollingInterval=20s, got %s", idx.mempoolPollingInterval)
	}
	if idx.indexerVersion != "test-v1" {
		t.Errorf("expected indexerVersion='test-v1', got %q", idx.indexerVersion)
	}
}

func TestGetLastIndexedBlock(t *testing.T) {
	idx := newTestIndexer()
	// Initial value should be 0
	if idx.GetLastIndexedBlock() != 0 {
		t.Errorf("expected 0, got %d", idx.GetLastIndexedBlock())
	}
}

func TestGetNetworkInfo(t *testing.T) {
	idx := newTestIndexer()
	info := idx.GetNetworkInfo()
	if info.Name != "testnet" {
		t.Errorf("expected 'testnet', got %q", info.Name)
	}
	if info.ChainID != 42 {
		t.Errorf("expected chain ID 42, got %d", info.ChainID)
	}
}

func TestStop(t *testing.T) {
	idx := newTestIndexer()
	// Stop should not panic even if Start wasn't called
	idx.Stop()
	// Context should be canceled
	select {
	case <-idx.ctx.Done():
		// expected
	default:
		t.Error("expected context to be canceled after Stop")
	}
}

func TestTrackFailedBlock(t *testing.T) {
	idx := newTestIndexer()
	idx.trackFailedBlock(100)
	idx.trackFailedBlock(100)
	idx.trackFailedBlock(200)

	idx.failedBlocksMu.Lock()
	defer idx.failedBlocksMu.Unlock()

	if idx.failedBlocks[100] != 2 {
		t.Errorf("expected 2 failures for block 100, got %d", idx.failedBlocks[100])
	}
	if idx.failedBlocks[200] != 1 {
		t.Errorf("expected 1 failure for block 200, got %d", idx.failedBlocks[200])
	}
}

func TestConstants(t *testing.T) {
	if DefaultBatchSize != 100 {
		t.Errorf("expected DefaultBatchSize=100, got %d", DefaultBatchSize)
	}
	if DefaultPollingInterval != 15*time.Second {
		t.Errorf("expected DefaultPollingInterval=15s, got %s", DefaultPollingInterval)
	}
	if DefaultMempoolPollingInterval != 30*time.Second {
		t.Errorf("expected DefaultMempoolPollingInterval=30s, got %s", DefaultMempoolPollingInterval)
	}
	if DefaultWorkerCount != 4 {
		t.Errorf("expected DefaultWorkerCount=4, got %d", DefaultWorkerCount)
	}
}

func TestCalculateBlobMetrics_UsesRealizedBlobBaseFeeCost(t *testing.T) {
	tx := types.NewTx(&types.BlobTx{
		BlobFeeCap: uint256.NewInt(5),
		BlobHashes: []common.Hash{
			{1},
			{2},
		},
	})
	blobBaseFee := big.NewInt(2)
	const gasPerBlob int64 = 131072

	metrics := calculateBlobMetrics(tx, blobBaseFee)

	wantCost := new(big.Int).Mul(blobBaseFee, big.NewInt(gasPerBlob)).String()
	if metrics.totalCostETH != wantCost {
		t.Fatalf("expected per-blob totalCostETH = baseFee * %d = %s, got %q", gasPerBlob, wantCost, metrics.totalCostETH)
	}
	capCost := new(big.Int).Mul(big.NewInt(5), big.NewInt(gasPerBlob)).String()
	if metrics.totalCostETH == capCost {
		t.Fatal("expected totalCostETH not to use max fee cap cost")
	}
	if metrics.maxFeePerBlobGas == nil || *metrics.maxFeePerBlobGas != "5" {
		t.Fatalf("expected max fee cap to be preserved, got %v", metrics.maxFeePerBlobGas)
	}
	if metrics.blobGasUsed == nil || *metrics.blobGasUsed != gasPerBlob {
		t.Fatalf("expected per-blob blob gas %d, got %v", gasPerBlob, metrics.blobGasUsed)
	}
	if metrics.blobSizeBytes != gasPerBlob {
		t.Fatalf("expected per-blob size %d bytes, got %d", gasPerBlob, metrics.blobSizeBytes)
	}
}

func TestDetermineStartBlock_NumericBlock(t *testing.T) {
	idx := newTestIndexer()
	idx.network.StartBlock = "12345"
	idx.lastIndexedBlock = 0
	block, err := idx.determineStartBlock()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if block != 12345 {
		t.Errorf("expected 12345, got %d", block)
	}
}

func TestDetermineStartBlock_NumericBlockWithExistingProgress(t *testing.T) {
	idx := newTestIndexer()
	idx.network.StartBlock = "12345"
	idx.lastIndexedBlock = 20000

	block, err := idx.determineStartBlock()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if block != 20001 {
		t.Errorf("expected 20001, got %d", block)
	}
}

func TestDetermineStartBlock_InvalidNumber(t *testing.T) {
	idx := newTestIndexer()
	idx.network.StartBlock = "not-a-number"
	_, err := idx.determineStartBlock()
	if err == nil {
		t.Error("expected error for invalid block number")
	}
}

func TestDetermineStartBlock_EmptyStartBlock_WithLastIndexed(t *testing.T) {
	idx := newTestIndexer()
	idx.network.StartBlock = ""
	idx.lastIndexedBlock = 500
	block, err := idx.determineStartBlock()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if block != 501 {
		t.Errorf("expected 501, got %d", block)
	}
}

func TestDetermineStartBlock_EmptyStartBlock_NoLastIndexed(t *testing.T) {
	idx := newTestIndexer()
	idx.network.StartBlock = ""
	idx.lastIndexedBlock = 0
	block, err := idx.determineStartBlock()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if block != 0 {
		t.Errorf("expected 0, got %d", block)
	}
}

func TestRetryFailedBlocks_NoFailedBlocks(t *testing.T) {
	idx := newTestIndexer()
	// Should not panic or block with no failed blocks
	idx.retryFailedBlocks()
}

func TestRetryFailedBlocks_WithinRetryLimit(t *testing.T) {
	idx := newTestIndexer()
	idx.failedBlocks[100] = 3 // within maxGapScanRetries(10)

	idx.retryFailedBlocks()

	// Block should be re-queued into blockTaskCh
	select {
	case task := <-idx.blockTaskCh:
		if task.BlockNumber != 100 {
			t.Errorf("expected block 100, got %d", task.BlockNumber)
		}
	default:
		t.Error("expected block task to be queued")
	}
}

func TestRetryFailedBlocks_ExceedsRetryLimit(t *testing.T) {
	idx := newTestIndexer()
	idx.failedBlocks[100] = maxGapScanRetries + 1 // exceeds max

	idx.retryFailedBlocks()

	// Block should not be re-queued immediately, but it stays tracked for safety-net retries.
	select {
	case task := <-idx.blockTaskCh:
		t.Errorf("unexpected block task %d", task.BlockNumber)
	default:
		// expected - nothing queued
	}
	idx.failedBlocksMu.Lock()
	defer idx.failedBlocksMu.Unlock()
	if _, ok := idx.failedBlocks[100]; !ok {
		t.Fatal("expected block 100 to remain tracked")
	}
	if _, ok := idx.failedBlockNextRetry[100]; !ok {
		t.Fatal("expected block 100 to have a scheduled safety-net retry")
	}
}

func TestRetryFailedBlocks_ExceedsRetryLimitSafetyRetryDue(t *testing.T) {
	idx := newTestIndexer()
	idx.failedBlocks[100] = maxGapScanRetries + 1
	idx.failedBlockNextRetry[100] = time.Now().Add(-time.Minute)

	idx.retryFailedBlocks()

	select {
	case task := <-idx.blockTaskCh:
		if task.BlockNumber != 100 {
			t.Errorf("expected block 100, got %d", task.BlockNumber)
		}
	default:
		t.Error("expected block task to be queued")
	}
}

func TestRetryFailedBlocks_MixedBlocks(t *testing.T) {
	idx := newTestIndexer()
	idx.failedBlocks[100] = 2                     // should retry
	idx.failedBlocks[200] = maxGapScanRetries + 5 // should defer to safety-net retry

	idx.retryFailedBlocks()

	// Only block 100 should be re-queued
	retried := make(map[uint64]bool)
	for {
		select {
		case task := <-idx.blockTaskCh:
			retried[task.BlockNumber] = true
		default:
			goto done
		}
	}
done:
	if !retried[100] {
		t.Error("expected block 100 to be retried")
	}
	if retried[200] {
		t.Error("block 200 should not be retried")
	}
	idx.failedBlocksMu.Lock()
	defer idx.failedBlocksMu.Unlock()
	if _, ok := idx.failedBlocks[200]; !ok {
		t.Fatal("expected block 200 to remain tracked")
	}
}

func TestBlockTask(t *testing.T) {
	task := BlockTask{BlockNumber: 42}
	if task.BlockNumber != 42 {
		t.Errorf("expected BlockNumber=42, got %d", task.BlockNumber)
	}
}

func TestGetSender_Success(t *testing.T) {
	idx := newTestIndexer()
	idx.network.ChainID = 1

	// Generate a key and sign a transaction
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	signer := types.LatestSignerForChainID(big.NewInt(1))
	tx := types.MustSignNewTx(key, signer, &types.DynamicFeeTx{
		ChainID:   big.NewInt(1),
		Nonce:     0,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(1),
		Gas:       21000,
		To:        &common.Address{},
	})

	addr, err := idx.getSender(tx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedAddr := crypto.PubkeyToAddress(key.PublicKey).Hex()
	if addr != expectedAddr {
		t.Errorf("expected %s, got %s", expectedAddr, addr)
	}
}

func TestGetSender_WrongChainID(t *testing.T) {
	idx := newTestIndexer()
	idx.network.ChainID = 999 // different from tx chain ID

	key, _ := crypto.GenerateKey()
	signer := types.LatestSignerForChainID(big.NewInt(1))
	tx := types.MustSignNewTx(key, signer, &types.DynamicFeeTx{
		ChainID:   big.NewInt(1),
		Nonce:     0,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(1),
		Gas:       21000,
		To:        &common.Address{},
	})

	// Should still succeed by falling back to tx's chain ID
	addr, err := idx.getSender(tx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedAddr := crypto.PubkeyToAddress(key.PublicKey).Hex()
	if addr != expectedAddr {
		t.Errorf("expected %s, got %s", expectedAddr, addr)
	}
}

func TestGetSender_UnsignedTx(t *testing.T) {
	idx := newTestIndexer()

	// Unsigned transaction should fail
	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   big.NewInt(1),
		Nonce:     0,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(1),
		Gas:       21000,
	})

	_, err := idx.getSender(tx)
	if err == nil {
		t.Error("expected error for unsigned transaction")
	}
}

// suppress unused import warning
var _ *ecdsa.PrivateKey

func TestProcessBlockRange_Success(t *testing.T) {
	idx := newTestIndexer()
	err := idx.processBlockRange(10, 14)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have queued 5 blocks (10, 11, 12, 13, 14)
	var blocks []uint64
	for {
		select {
		case task := <-idx.blockTaskCh:
			blocks = append(blocks, task.BlockNumber)
		default:
			goto done
		}
	}
done:
	if len(blocks) != 5 {
		t.Fatalf("expected 5 blocks queued, got %d", len(blocks))
	}
	for i, expected := range []uint64{10, 11, 12, 13, 14} {
		if blocks[i] != expected {
			t.Errorf("block[%d] = %d, want %d", i, blocks[i], expected)
		}
	}
}

func TestProcessBlockRange_CancelledContext(t *testing.T) {
	idx := newTestIndexer()
	// Use a tiny channel so it blocks quickly
	idx.blockTaskCh = make(chan BlockTask, 1)
	idx.cancel() // cancel immediately

	err := idx.processBlockRange(1, 100)
	if err == nil {
		t.Error("expected error for canceled context")
	}
}

func TestProcessBlockRange_SingleBlock(t *testing.T) {
	idx := newTestIndexer()
	err := idx.processBlockRange(42, 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	select {
	case task := <-idx.blockTaskCh:
		if task.BlockNumber != 42 {
			t.Errorf("expected block 42, got %d", task.BlockNumber)
		}
	default:
		t.Error("expected one block queued")
	}
}

func TestErrReorgDetected(t *testing.T) {
	if errReorgDetected.Error() != "chain reorganization detected" {
		t.Errorf("unexpected error message: %s", errReorgDetected.Error())
	}
}

func TestInternalConstants(t *testing.T) {
	idx := newTestIndexer()

	if idx.maxBlockRetries != 3 {
		t.Errorf("expected maxBlockRetries=3, got %d", idx.maxBlockRetries)
	}
	if maxGapScanRetries != 10 {
		t.Errorf("expected maxGapScanRetries=10, got %d", maxGapScanRetries)
	}
	if idx.gapScanInterval != 5*time.Minute {
		t.Errorf("expected gapScanInterval=5m, got %s", idx.gapScanInterval)
	}
	if idx.maxReorgDepth != 64 {
		t.Errorf("expected maxReorgDepth=64, got %d", idx.maxReorgDepth)
	}
}

func TestRetryFailedBlocks_AllExceeded(t *testing.T) {
	idx := newTestIndexer()
	idx.failedBlocks[100] = maxGapScanRetries + 1
	idx.failedBlocks[200] = maxGapScanRetries + 2

	idx.retryFailedBlocks()

	// toRetry is empty, hits early return
	select {
	case task := <-idx.blockTaskCh:
		t.Errorf("unexpected block task %d", task.BlockNumber)
	default:
		// expected
	}
	if len(idx.failedBlocks) != 2 {
		t.Fatalf("expected exceeded blocks to remain tracked, got %d entries", len(idx.failedBlocks))
	}
	if len(idx.failedBlockNextRetry) != 2 {
		t.Fatalf("expected safety-net retries to be scheduled, got %d entries", len(idx.failedBlockNextRetry))
	}
}

func TestRetryFailedBlocks_CancelledContext(t *testing.T) {
	idx := newTestIndexer()
	idx.blockTaskCh = make(chan BlockTask) // zero-buffer channel
	idx.failedBlocks[100] = 1
	idx.failedBlocks[200] = 1

	idx.cancel() // cancel immediately
	idx.retryFailedBlocks()
	// Should not hang
}

func TestProcessBlockRange_EmptyRange(t *testing.T) {
	idx := newTestIndexer()
	err := idx.processBlockRange(10, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	select {
	case task := <-idx.blockTaskCh:
		t.Errorf("unexpected block task %d", task.BlockNumber)
	default:
		// expected - nothing queued for reversed range
	}
}

func TestRunGapScanner_StopsOnCancel(t *testing.T) {
	idx := newTestIndexer()

	done := make(chan struct{})
	go func() {
		idx.runGapScanner()
		close(done)
	}()

	// Cancel the context
	idx.cancel()

	select {
	case <-done:
		// expected - gap scanner stopped
	case <-time.After(2 * time.Second):
		t.Error("gap scanner did not stop after context cancellation")
	}
}
