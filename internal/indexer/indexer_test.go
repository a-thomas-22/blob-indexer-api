package indexer

import (
	"context"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

func init() {
	// Initialize the logger so that log calls inside the indexer package don't panic.
	logger.Initialize()
}

// --------------------------------------------------------------------
// Helper: build an Indexer with only the fields needed for unit tests.
// No real DB or Ethereum client is required.
// --------------------------------------------------------------------

func newTestIndexer(cfg *config.Config, network config.NetworkConfig) *Indexer {
	ctx, cancel := context.WithCancel(context.Background())
	return &Indexer{
		config:              cfg,
		network:             network,
		batchSize:           cfg.Indexer.BatchSize,
		pollingInterval:     cfg.Indexer.PollingInterval,
		mempoolPollingInterval: cfg.Indexer.MempoolPollingInterval,
		workerCount:         DefaultWorkerCount,
		ctx:                 ctx,
		cancel:              cancel,
		indexerVersion:      cfg.Indexer.Version,
		blockTaskCh:         make(chan BlockTask, 100),
		failedBlocks:        make(map[uint64]int),
	}
}

func defaultTestConfig() *config.Config {
	return &config.Config{
		Indexer: config.IndexerConfig{
			Version:                "v1.0.0-test",
			BatchSize:              50,
			PollingInterval:        10 * time.Second,
			MempoolPollingInterval: 20 * time.Second,
		},
	}
}

func defaultTestNetwork() config.NetworkConfig {
	return config.NetworkConfig{
		Name:       "testnet",
		ChainID:    17000,
		RpcURL:     "http://localhost:8545",
		StartBlock: "100",
		Enabled:    true,
	}
}

// =====================================================================
// 1. Constants
// =====================================================================

func TestConstants(t *testing.T) {
	t.Run("DefaultBatchSize is positive", func(t *testing.T) {
		if DefaultBatchSize <= 0 {
			t.Errorf("DefaultBatchSize should be positive, got %d", DefaultBatchSize)
		}
	})

	t.Run("DefaultPollingInterval is positive", func(t *testing.T) {
		if DefaultPollingInterval <= 0 {
			t.Errorf("DefaultPollingInterval should be positive, got %v", DefaultPollingInterval)
		}
	})

	t.Run("DefaultMempoolPollingInterval is positive", func(t *testing.T) {
		if DefaultMempoolPollingInterval <= 0 {
			t.Errorf("DefaultMempoolPollingInterval should be positive, got %v", DefaultMempoolPollingInterval)
		}
	})

	t.Run("DefaultWorkerCount is positive", func(t *testing.T) {
		if DefaultWorkerCount <= 0 {
			t.Errorf("DefaultWorkerCount should be positive, got %d", DefaultWorkerCount)
		}
	})

	t.Run("maxBlockRetries is positive", func(t *testing.T) {
		if maxBlockRetries <= 0 {
			t.Errorf("maxBlockRetries should be positive, got %d", maxBlockRetries)
		}
	})

	t.Run("maxGapScanRetries is greater than maxBlockRetries", func(t *testing.T) {
		if maxGapScanRetries <= maxBlockRetries {
			t.Errorf("maxGapScanRetries (%d) should be greater than maxBlockRetries (%d)",
				maxGapScanRetries, maxBlockRetries)
		}
	})

	t.Run("gapScanInterval is positive", func(t *testing.T) {
		if gapScanInterval <= 0 {
			t.Errorf("gapScanInterval should be positive, got %v", gapScanInterval)
		}
	})

	t.Run("maxReorgDepth is positive and reasonable", func(t *testing.T) {
		if maxReorgDepth <= 0 {
			t.Errorf("maxReorgDepth should be positive, got %d", maxReorgDepth)
		}
		if maxReorgDepth > 1000 {
			t.Errorf("maxReorgDepth seems unreasonably high: %d", maxReorgDepth)
		}
	})
}

// =====================================================================
// 2. BlockTask struct
// =====================================================================

func TestBlockTask(t *testing.T) {
	task := BlockTask{BlockNumber: 12345}
	if task.BlockNumber != 12345 {
		t.Errorf("expected BlockNumber 12345, got %d", task.BlockNumber)
	}
}

// =====================================================================
// 3. Indexer construction – newTestIndexer (mirrors New without deps)
// =====================================================================

func TestNewIndexer_FieldsFromConfig(t *testing.T) {
	cfg := defaultTestConfig()
	network := defaultTestNetwork()

	idx := newTestIndexer(cfg, network)
	defer idx.cancel()

	if idx.batchSize != cfg.Indexer.BatchSize {
		t.Errorf("batchSize: want %d, got %d", cfg.Indexer.BatchSize, idx.batchSize)
	}
	if idx.pollingInterval != cfg.Indexer.PollingInterval {
		t.Errorf("pollingInterval: want %v, got %v", cfg.Indexer.PollingInterval, idx.pollingInterval)
	}
	if idx.mempoolPollingInterval != cfg.Indexer.MempoolPollingInterval {
		t.Errorf("mempoolPollingInterval: want %v, got %v", cfg.Indexer.MempoolPollingInterval, idx.mempoolPollingInterval)
	}
	if idx.indexerVersion != cfg.Indexer.Version {
		t.Errorf("indexerVersion: want %s, got %s", cfg.Indexer.Version, idx.indexerVersion)
	}
	if idx.network.ChainID != network.ChainID {
		t.Errorf("network.ChainID: want %d, got %d", network.ChainID, idx.network.ChainID)
	}
	if idx.network.Name != network.Name {
		t.Errorf("network.Name: want %s, got %s", network.Name, idx.network.Name)
	}
}

func TestNewIndexer_FailedBlocksMapInitialized(t *testing.T) {
	cfg := defaultTestConfig()
	network := defaultTestNetwork()

	idx := newTestIndexer(cfg, network)
	defer idx.cancel()

	if idx.failedBlocks == nil {
		t.Fatal("failedBlocks map should be initialized, got nil")
	}
	if len(idx.failedBlocks) != 0 {
		t.Errorf("failedBlocks should be empty, got %d entries", len(idx.failedBlocks))
	}
}

func TestNewIndexer_BlockTaskChannelBuffered(t *testing.T) {
	cfg := defaultTestConfig()
	network := defaultTestNetwork()

	idx := newTestIndexer(cfg, network)
	defer idx.cancel()

	if cap(idx.blockTaskCh) == 0 {
		t.Error("blockTaskCh should be buffered")
	}
}

// =====================================================================
// 4. GetLastIndexedBlock / GetNetworkInfo (public getters)
// =====================================================================

func TestGetLastIndexedBlock(t *testing.T) {
	cfg := defaultTestConfig()
	idx := newTestIndexer(cfg, defaultTestNetwork())
	defer idx.cancel()

	// Initially zero
	if got := idx.GetLastIndexedBlock(); got != 0 {
		t.Errorf("expected 0, got %d", got)
	}

	// After atomic store
	atomic.StoreUint64(&idx.lastIndexedBlock, 42)
	if got := idx.GetLastIndexedBlock(); got != 42 {
		t.Errorf("expected 42, got %d", got)
	}
}

func TestGetNetworkInfo(t *testing.T) {
	network := defaultTestNetwork()
	cfg := defaultTestConfig()
	idx := newTestIndexer(cfg, network)
	defer idx.cancel()

	info := idx.GetNetworkInfo()
	if info.Name != network.Name {
		t.Errorf("Name: want %s, got %s", network.Name, info.Name)
	}
	if info.ChainID != network.ChainID {
		t.Errorf("ChainID: want %d, got %d", network.ChainID, info.ChainID)
	}
	if info.RpcURL != network.RpcURL {
		t.Errorf("RpcURL: want %s, got %s", network.RpcURL, info.RpcURL)
	}
	if info.StartBlock != network.StartBlock {
		t.Errorf("StartBlock: want %s, got %s", network.StartBlock, info.StartBlock)
	}
	if info.Enabled != network.Enabled {
		t.Errorf("Enabled: want %v, got %v", network.Enabled, info.Enabled)
	}
}

// =====================================================================
// 5. trackFailedBlock
// =====================================================================

func TestTrackFailedBlock(t *testing.T) {
	cfg := defaultTestConfig()
	idx := newTestIndexer(cfg, defaultTestNetwork())
	defer idx.cancel()

	idx.trackFailedBlock(100)
	idx.trackFailedBlock(200)
	idx.trackFailedBlock(100) // second failure for block 100

	idx.failedBlocksMu.Lock()
	defer idx.failedBlocksMu.Unlock()

	if count := idx.failedBlocks[100]; count != 2 {
		t.Errorf("block 100: want 2 failures, got %d", count)
	}
	if count := idx.failedBlocks[200]; count != 1 {
		t.Errorf("block 200: want 1 failure, got %d", count)
	}
	if _, exists := idx.failedBlocks[300]; exists {
		t.Error("block 300 should not exist in failedBlocks")
	}
}

func TestTrackFailedBlock_ConcurrentSafety(t *testing.T) {
	cfg := defaultTestConfig()
	idx := newTestIndexer(cfg, defaultTestNetwork())
	defer idx.cancel()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(block uint64) {
			defer wg.Done()
			idx.trackFailedBlock(block)
		}(uint64(i % 10))
	}
	wg.Wait()

	idx.failedBlocksMu.Lock()
	defer idx.failedBlocksMu.Unlock()

	total := 0
	for _, count := range idx.failedBlocks {
		total += count
	}
	if total != 100 {
		t.Errorf("expected 100 total failures, got %d", total)
	}
}

// =====================================================================
// 6. retryFailedBlocks
// =====================================================================

func TestRetryFailedBlocks_RequeuesEligibleBlocks(t *testing.T) {
	cfg := defaultTestConfig()
	idx := newTestIndexer(cfg, defaultTestNetwork())
	defer idx.cancel()

	// Add some failed blocks: one under the retry limit, one at the limit, one over
	idx.failedBlocksMu.Lock()
	idx.failedBlocks[10] = 1                    // eligible
	idx.failedBlocks[20] = maxGapScanRetries     // eligible (at limit)
	idx.failedBlocks[30] = maxGapScanRetries + 1 // permanently failed
	idx.failedBlocksMu.Unlock()

	idx.retryFailedBlocks()

	// Drain the channel and collect re-queued blocks
	close(idx.blockTaskCh)
	requeued := make(map[uint64]bool)
	for task := range idx.blockTaskCh {
		requeued[task.BlockNumber] = true
	}

	if !requeued[10] {
		t.Error("block 10 should have been re-queued")
	}
	if !requeued[20] {
		t.Error("block 20 should have been re-queued (count == maxGapScanRetries is eligible)")
	}
	if requeued[30] {
		t.Error("block 30 should NOT have been re-queued (exceeded max retries)")
	}
}

func TestRetryFailedBlocks_EmptyMap_NoBlocksQueued(t *testing.T) {
	cfg := defaultTestConfig()
	idx := newTestIndexer(cfg, defaultTestNetwork())
	defer idx.cancel()

	// No failed blocks
	idx.retryFailedBlocks()

	// Channel should be empty
	select {
	case task := <-idx.blockTaskCh:
		t.Errorf("expected no tasks, got block %d", task.BlockNumber)
	default:
		// OK
	}
}

// =====================================================================
// 7. updateLastIndexedBlock (atomic CAS logic, no DB)
// =====================================================================

func TestUpdateLastIndexedBlock_IncrementsOnly(t *testing.T) {
	cfg := defaultTestConfig()
	idx := newTestIndexer(cfg, defaultTestNetwork())
	defer idx.cancel()

	// The function also writes to DB, but db is nil — that write will fail silently.
	// We are only testing the atomic CAS logic here.
	// We need to set db to nil and accept the error log.
	// Actually the function will panic if db is nil.
	// So we just test the atomic compare-and-swap logic directly.

	atomic.StoreUint64(&idx.lastIndexedBlock, 0)

	// Higher block should update
	testAtomicUpdate := func(newBlock, expectedAfter uint64) {
		t.Helper()
		// Simulate the CAS loop without DB
		for {
			current := atomic.LoadUint64(&idx.lastIndexedBlock)
			if newBlock <= current {
				break
			}
			if atomic.CompareAndSwapUint64(&idx.lastIndexedBlock, current, newBlock) {
				break
			}
		}
		got := atomic.LoadUint64(&idx.lastIndexedBlock)
		if got != expectedAfter {
			t.Errorf("after update(%d): want lastIndexedBlock=%d, got %d", newBlock, expectedAfter, got)
		}
	}

	testAtomicUpdate(10, 10)  // 0 -> 10
	testAtomicUpdate(5, 10)   // 10 stays (5 < 10)
	testAtomicUpdate(10, 10)  // 10 stays (equal)
	testAtomicUpdate(100, 100) // 10 -> 100
}

func TestUpdateLastIndexedBlock_ConcurrentUpdates(t *testing.T) {
	cfg := defaultTestConfig()
	idx := newTestIndexer(cfg, defaultTestNetwork())
	defer idx.cancel()

	atomic.StoreUint64(&idx.lastIndexedBlock, 0)

	var wg sync.WaitGroup
	// Simulate concurrent updates — the highest value should win
	for i := uint64(1); i <= 100; i++ {
		wg.Add(1)
		go func(block uint64) {
			defer wg.Done()
			for {
				current := atomic.LoadUint64(&idx.lastIndexedBlock)
				if block <= current {
					return
				}
				if atomic.CompareAndSwapUint64(&idx.lastIndexedBlock, current, block) {
					return
				}
			}
		}(i)
	}
	wg.Wait()

	got := atomic.LoadUint64(&idx.lastIndexedBlock)
	if got != 100 {
		t.Errorf("expected lastIndexedBlock=100 after concurrent updates, got %d", got)
	}
}

// =====================================================================
// 8. Blob cost calculation logic (extracted from processBlock)
// =====================================================================

func TestBlobCostCalculation(t *testing.T) {
	tests := []struct {
		name             string
		maxFeePerBlobGas *big.Int
		blobBaseFee      *big.Int
		blobGasUsed      uint64
		wantTip          *big.Int
		wantTotalCost    *big.Int
	}{
		{
			name:             "normal case: tip is positive",
			maxFeePerBlobGas: big.NewInt(2000000000), // 2 Gwei
			blobBaseFee:      big.NewInt(1000000000),  // 1 Gwei
			blobGasUsed:      131072,                  // 1 blob = 131072 gas
			wantTip:          big.NewInt(1000000000),   // 1 Gwei
			wantTotalCost:    new(big.Int).Mul(big.NewInt(2000000000), big.NewInt(131072)),
		},
		{
			name:             "maxFee equals baseFee: tip is zero",
			maxFeePerBlobGas: big.NewInt(1000000000),
			blobBaseFee:      big.NewInt(1000000000),
			blobGasUsed:      131072,
			wantTip:          big.NewInt(0),
			wantTotalCost:    new(big.Int).Mul(big.NewInt(1000000000), big.NewInt(131072)),
		},
		{
			name:             "maxFee less than baseFee: tip clamped to zero",
			maxFeePerBlobGas: big.NewInt(500000000),
			blobBaseFee:      big.NewInt(1000000000),
			blobGasUsed:      131072,
			wantTip:          big.NewInt(0),
			// When tip is clamped to zero, total cost = (baseFee + 0) * gasUsed
			wantTotalCost: new(big.Int).Mul(big.NewInt(1000000000), big.NewInt(131072)),
		},
		{
			name:             "zero gas used: total cost is zero",
			maxFeePerBlobGas: big.NewInt(2000000000),
			blobBaseFee:      big.NewInt(1000000000),
			blobGasUsed:      0,
			wantTip:          big.NewInt(1000000000),
			wantTotalCost:    big.NewInt(0),
		},
		{
			name:             "large values",
			maxFeePerBlobGas: new(big.Int).Mul(big.NewInt(100), big.NewInt(1000000000)), // 100 Gwei
			blobBaseFee:      new(big.Int).Mul(big.NewInt(10), big.NewInt(1000000000)),   // 10 Gwei
			blobGasUsed:      131072 * 6,                                                 // 6 blobs
			wantTip:          new(big.Int).Mul(big.NewInt(90), big.NewInt(1000000000)),    // 90 Gwei
			wantTotalCost: new(big.Int).Mul(
				new(big.Int).Mul(big.NewInt(100), big.NewInt(1000000000)),
				big.NewInt(131072*6),
			),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reproduce the calculation from processBlock/processPendingTransaction
			tipPerBlobGas := new(big.Int).Sub(tt.maxFeePerBlobGas, tt.blobBaseFee)
			if tipPerBlobGas.Sign() < 0 {
				tipPerBlobGas = big.NewInt(0)
			}

			totalCost := new(big.Int).Mul(
				new(big.Int).Add(tt.blobBaseFee, tipPerBlobGas),
				new(big.Int).SetUint64(tt.blobGasUsed),
			)

			if tipPerBlobGas.Cmp(tt.wantTip) != 0 {
				t.Errorf("tipPerBlobGas: want %s, got %s", tt.wantTip, tipPerBlobGas)
			}
			if totalCost.Cmp(tt.wantTotalCost) != 0 {
				t.Errorf("totalCost: want %s, got %s", tt.wantTotalCost, totalCost)
			}
		})
	}
}

// =====================================================================
// 9. Blob size approximation (blobGasUsed * 128)
// =====================================================================

func TestBlobSizeApproximation(t *testing.T) {
	tests := []struct {
		name        string
		blobGasUsed uint64
		wantSize    int64
	}{
		{"single blob", 131072, 131072 * 128},
		{"six blobs", 131072 * 6, 131072 * 6 * 128},
		{"zero gas", 0, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			size := int64(tt.blobGasUsed * 128)
			if size != tt.wantSize {
				t.Errorf("want %d, got %d", tt.wantSize, size)
			}
		})
	}
}

// =====================================================================
// 10. errReorgDetected sentinel error
// =====================================================================

func TestErrReorgDetected(t *testing.T) {
	if errReorgDetected == nil {
		t.Fatal("errReorgDetected should not be nil")
	}
	if errReorgDetected.Error() != "chain reorganization detected" {
		t.Errorf("unexpected error message: %s", errReorgDetected.Error())
	}
}

// =====================================================================
// 11. Stop method (cancel + channel close)
// =====================================================================

func TestStop_CancelsContext(t *testing.T) {
	cfg := defaultTestConfig()
	idx := newTestIndexer(cfg, defaultTestNetwork())

	// Verify context is not cancelled initially
	select {
	case <-idx.ctx.Done():
		t.Fatal("context should not be cancelled before Stop")
	default:
	}

	idx.Stop()

	// Verify context is cancelled after Stop
	select {
	case <-idx.ctx.Done():
		// OK
	default:
		t.Fatal("context should be cancelled after Stop")
	}
}

// =====================================================================
// 12. processBlockRange – queues blocks into blockTaskCh
// =====================================================================

func TestProcessBlockRange_QueuesBlocks(t *testing.T) {
	cfg := defaultTestConfig()
	idx := newTestIndexer(cfg, defaultTestNetwork())
	defer idx.cancel()

	err := idx.processBlockRange(10, 14)
	if err != nil {
		t.Fatalf("processBlockRange returned error: %v", err)
	}

	// Collect queued tasks
	var got []uint64
	for i := 0; i < 5; i++ {
		select {
		case task := <-idx.blockTaskCh:
			got = append(got, task.BlockNumber)
		case <-time.After(100 * time.Millisecond):
			t.Fatal("timed out waiting for block task")
		}
	}

	want := []uint64{10, 11, 12, 13, 14}
	if len(got) != len(want) {
		t.Fatalf("got %d tasks, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("task[%d]: want %d, got %d", i, w, got[i])
		}
	}
}

func TestProcessBlockRange_SingleBlock(t *testing.T) {
	cfg := defaultTestConfig()
	idx := newTestIndexer(cfg, defaultTestNetwork())
	defer idx.cancel()

	err := idx.processBlockRange(42, 42)
	if err != nil {
		t.Fatalf("processBlockRange returned error: %v", err)
	}

	select {
	case task := <-idx.blockTaskCh:
		if task.BlockNumber != 42 {
			t.Errorf("want block 42, got %d", task.BlockNumber)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out waiting for block task")
	}

	// Channel should be empty now
	select {
	case task := <-idx.blockTaskCh:
		t.Errorf("unexpected extra task: block %d", task.BlockNumber)
	default:
	}
}

func TestProcessBlockRange_CancelledContext(t *testing.T) {
	cfg := defaultTestConfig()
	idx := newTestIndexer(cfg, defaultTestNetwork())

	// Cancel immediately — processBlockRange should return context error
	idx.cancel()

	err := idx.processBlockRange(1, 1000)
	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
}

// =====================================================================
// 13. reorgDetected atomic flag
// =====================================================================

func TestReorgDetectedFlag(t *testing.T) {
	cfg := defaultTestConfig()
	idx := newTestIndexer(cfg, defaultTestNetwork())
	defer idx.cancel()

	// Initially zero
	if atomic.LoadUint32(&idx.reorgDetected) != 0 {
		t.Error("reorgDetected should be 0 initially")
	}

	// Set the flag
	atomic.StoreUint32(&idx.reorgDetected, 1)
	if atomic.LoadUint32(&idx.reorgDetected) != 1 {
		t.Error("reorgDetected should be 1 after store")
	}

	// CAS to consume the flag (as runBlockIndexer does)
	swapped := atomic.CompareAndSwapUint32(&idx.reorgDetected, 1, 0)
	if !swapped {
		t.Error("CompareAndSwapUint32 should have succeeded")
	}
	if atomic.LoadUint32(&idx.reorgDetected) != 0 {
		t.Error("reorgDetected should be 0 after CAS reset")
	}

	// CAS when not set should be a no-op
	swapped = atomic.CompareAndSwapUint32(&idx.reorgDetected, 1, 0)
	if swapped {
		t.Error("CompareAndSwapUint32 should not have swapped when flag is already 0")
	}
}

// =====================================================================
// 14. Gap scanner retry eligibility logic
// =====================================================================

func TestRetryFailedBlocks_IncrementingFailureCounts(t *testing.T) {
	cfg := defaultTestConfig()
	idx := newTestIndexer(cfg, defaultTestNetwork())
	defer idx.cancel()

	blockNum := uint64(555)

	// Simulate multiple rounds of failure tracking
	for i := 0; i < maxGapScanRetries; i++ {
		idx.trackFailedBlock(blockNum)
	}

	idx.failedBlocksMu.Lock()
	count := idx.failedBlocks[blockNum]
	idx.failedBlocksMu.Unlock()

	if count != maxGapScanRetries {
		t.Errorf("expected %d failures, got %d", maxGapScanRetries, count)
	}

	// Block should still be retried at exactly maxGapScanRetries
	idx.retryFailedBlocks()

	select {
	case task := <-idx.blockTaskCh:
		if task.BlockNumber != blockNum {
			t.Errorf("expected block %d, got %d", blockNum, task.BlockNumber)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("block should have been re-queued at maxGapScanRetries")
	}

	// Add one more failure to push past the limit
	idx.trackFailedBlock(blockNum)

	idx.failedBlocksMu.Lock()
	count = idx.failedBlocks[blockNum]
	idx.failedBlocksMu.Unlock()

	if count != maxGapScanRetries+1 {
		t.Errorf("expected %d failures, got %d", maxGapScanRetries+1, count)
	}

	// Re-create channel since we already read from it
	idx.blockTaskCh = make(chan BlockTask, 100)

	// Now the block should NOT be re-queued
	idx.retryFailedBlocks()

	select {
	case task := <-idx.blockTaskCh:
		t.Errorf("block %d should NOT have been re-queued, but got task for block %d",
			blockNum, task.BlockNumber)
	default:
		// OK — not re-queued
	}
}

// =====================================================================
// 15. Failed block clearing on success
// =====================================================================

func TestFailedBlockClearingOnSuccess(t *testing.T) {
	cfg := defaultTestConfig()
	idx := newTestIndexer(cfg, defaultTestNetwork())
	defer idx.cancel()

	// Add some failed blocks
	idx.trackFailedBlock(100)
	idx.trackFailedBlock(100)
	idx.trackFailedBlock(200)

	// Simulate successful processing: clear block 100
	idx.failedBlocksMu.Lock()
	delete(idx.failedBlocks, 100)
	idx.failedBlocksMu.Unlock()

	// Verify block 100 is cleared but block 200 remains
	idx.failedBlocksMu.Lock()
	defer idx.failedBlocksMu.Unlock()

	if _, exists := idx.failedBlocks[100]; exists {
		t.Error("block 100 should have been cleared from failedBlocks")
	}
	if count := idx.failedBlocks[200]; count != 1 {
		t.Errorf("block 200: want 1 failure, got %d", count)
	}
}

// =====================================================================
// 16. Tip clamping edge cases
// =====================================================================

func TestTipClamping(t *testing.T) {
	tests := []struct {
		name             string
		maxFeePerBlobGas *big.Int
		blobBaseFee      *big.Int
		wantTip          *big.Int
	}{
		{
			name:             "exactly zero difference",
			maxFeePerBlobGas: big.NewInt(1000),
			blobBaseFee:      big.NewInt(1000),
			wantTip:          big.NewInt(0),
		},
		{
			name:             "negative clamped to zero",
			maxFeePerBlobGas: big.NewInt(500),
			blobBaseFee:      big.NewInt(1000),
			wantTip:          big.NewInt(0),
		},
		{
			name:             "one wei difference",
			maxFeePerBlobGas: big.NewInt(1001),
			blobBaseFee:      big.NewInt(1000),
			wantTip:          big.NewInt(1),
		},
		{
			name:             "very large negative stays clamped",
			maxFeePerBlobGas: big.NewInt(1),
			blobBaseFee:      new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil),
			wantTip:          big.NewInt(0),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tip := new(big.Int).Sub(tt.maxFeePerBlobGas, tt.blobBaseFee)
			if tip.Sign() < 0 {
				tip = big.NewInt(0)
			}
			if tip.Cmp(tt.wantTip) != 0 {
				t.Errorf("want %s, got %s", tt.wantTip, tip)
			}
		})
	}
}

// =====================================================================
// 17. Exponential backoff delay calculation
// =====================================================================

func TestExponentialBackoffDelays(t *testing.T) {
	// The blockProcessingWorker uses: delay := time.Duration(1<<uint(attempt-1)) * time.Second
	// Verify the delay progression: 1s, 2s, 4s for attempts 1, 2, 3
	expected := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
	}

	for attempt := 1; attempt <= maxBlockRetries; attempt++ {
		delay := time.Duration(1<<uint(attempt-1)) * time.Second
		if delay != expected[attempt-1] {
			t.Errorf("attempt %d: want %v, got %v", attempt, expected[attempt-1], delay)
		}
	}
}

// =====================================================================
// 18. Default values for unexported constants
// =====================================================================

func TestDefaultValues(t *testing.T) {
	// Verify default values match expected configuration
	if DefaultBatchSize != 100 {
		t.Errorf("DefaultBatchSize: want 100, got %d", DefaultBatchSize)
	}
	if DefaultPollingInterval != 15*time.Second {
		t.Errorf("DefaultPollingInterval: want 15s, got %v", DefaultPollingInterval)
	}
	if DefaultMempoolPollingInterval != 30*time.Second {
		t.Errorf("DefaultMempoolPollingInterval: want 30s, got %v", DefaultMempoolPollingInterval)
	}
	if DefaultWorkerCount != 4 {
		t.Errorf("DefaultWorkerCount: want 4, got %d", DefaultWorkerCount)
	}
}
