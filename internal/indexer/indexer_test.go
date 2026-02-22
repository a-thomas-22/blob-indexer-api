package indexer

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

func TestMain(m *testing.M) {
	logger.Initialize()
	os.Exit(m.Run())
}

func TestConstants(t *testing.T) {
	if DefaultBatchSize != 100 {
		t.Errorf("expected DefaultBatchSize 100, got %d", DefaultBatchSize)
	}
	if DefaultPollingInterval != 15*time.Second {
		t.Errorf("expected DefaultPollingInterval 15s, got %s", DefaultPollingInterval)
	}
	if DefaultMempoolPollingInterval != 30*time.Second {
		t.Errorf("expected DefaultMempoolPollingInterval 30s, got %s", DefaultMempoolPollingInterval)
	}
	if DefaultWorkerCount != 4 {
		t.Errorf("expected DefaultWorkerCount 4, got %d", DefaultWorkerCount)
	}
	if maxBlockRetries != 3 {
		t.Errorf("expected maxBlockRetries 3, got %d", maxBlockRetries)
	}
	if maxGapScanRetries != 10 {
		t.Errorf("expected maxGapScanRetries 10, got %d", maxGapScanRetries)
	}
	if gapScanInterval != 5*time.Minute {
		t.Errorf("expected gapScanInterval 5m, got %s", gapScanInterval)
	}
	if maxReorgDepth != 64 {
		t.Errorf("expected maxReorgDepth 64, got %d", maxReorgDepth)
	}
}

func TestErrReorgDetected(t *testing.T) {
	if errReorgDetected == nil {
		t.Fatal("expected errReorgDetected to be non-nil")
	}
	if errReorgDetected.Error() != "chain reorganization detected" {
		t.Errorf("expected 'chain reorganization detected', got %q", errReorgDetected.Error())
	}
}

func TestBlockTask(t *testing.T) {
	task := BlockTask{BlockNumber: 12345}
	if task.BlockNumber != 12345 {
		t.Errorf("expected BlockNumber 12345, got %d", task.BlockNumber)
	}
}

func TestTrackFailedBlock(t *testing.T) {
	idx := &Indexer{
		failedBlocks: make(map[uint64]int),
	}

	idx.trackFailedBlock(100)
	if idx.failedBlocks[100] != 1 {
		t.Errorf("expected failed count 1, got %d", idx.failedBlocks[100])
	}

	idx.trackFailedBlock(100)
	if idx.failedBlocks[100] != 2 {
		t.Errorf("expected failed count 2, got %d", idx.failedBlocks[100])
	}

	idx.trackFailedBlock(200)
	if idx.failedBlocks[200] != 1 {
		t.Errorf("expected failed count 1 for block 200, got %d", idx.failedBlocks[200])
	}
}

func TestGetLastIndexedBlock(t *testing.T) {
	idx := &Indexer{}
	if idx.GetLastIndexedBlock() != 0 {
		t.Errorf("expected 0, got %d", idx.GetLastIndexedBlock())
	}
}

func TestGetLastIndexedBlock_WithValue(t *testing.T) {
	idx := &Indexer{}
	atomic.StoreUint64(&idx.lastIndexedBlock, 42)
	if idx.GetLastIndexedBlock() != 42 {
		t.Errorf("expected 42, got %d", idx.GetLastIndexedBlock())
	}
}

func TestGetNetworkInfo(t *testing.T) {
	expected := config.NetworkConfig{
		Name:       "sepolia",
		ChainID:    11155111,
		RpcURL:     "https://sepolia.example.com",
		StartBlock: "LATEST-100",
		Enabled:    true,
	}

	idx := &Indexer{
		network: expected,
	}

	info := idx.GetNetworkInfo()
	if info.Name != expected.Name {
		t.Errorf("expected Name %q, got %q", expected.Name, info.Name)
	}
	if info.ChainID != expected.ChainID {
		t.Errorf("expected ChainID %d, got %d", expected.ChainID, info.ChainID)
	}
	if info.RpcURL != expected.RpcURL {
		t.Errorf("expected RpcURL %q, got %q", expected.RpcURL, info.RpcURL)
	}
	if info.StartBlock != expected.StartBlock {
		t.Errorf("expected StartBlock %q, got %q", expected.StartBlock, info.StartBlock)
	}
	if info.Enabled != expected.Enabled {
		t.Errorf("expected Enabled %v, got %v", expected.Enabled, info.Enabled)
	}
}

func TestRetryFailedBlocks_EmptyMap(t *testing.T) {
	idx := &Indexer{
		failedBlocks: make(map[uint64]int),
	}

	// Should not panic and return early with empty map
	idx.retryFailedBlocks()
}

func TestRetryFailedBlocks_WithFailedBlocks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	idx := &Indexer{
		failedBlocks: make(map[uint64]int),
		blockTaskCh:  make(chan BlockTask, 10),
		ctx:          ctx,
	}

	// Add some failed blocks within retry limit
	idx.failedBlocks[100] = 1
	idx.failedBlocks[200] = 5

	idx.retryFailedBlocks()

	// Should have re-queued 2 blocks
	count := len(idx.blockTaskCh)
	if count != 2 {
		t.Errorf("expected 2 re-queued blocks, got %d", count)
	}
}

func TestRetryFailedBlocks_ExceedsMaxRetries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	idx := &Indexer{
		failedBlocks: make(map[uint64]int),
		blockTaskCh:  make(chan BlockTask, 10),
		ctx:          ctx,
	}

	// Add a block that exceeded max retries
	idx.failedBlocks[100] = maxGapScanRetries + 1
	// And one within limit
	idx.failedBlocks[200] = 1

	idx.retryFailedBlocks()

	// Should have re-queued only 1 block (the one within limit)
	count := len(idx.blockTaskCh)
	if count != 1 {
		t.Errorf("expected 1 re-queued block, got %d", count)
	}
}

func TestStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	idx := &Indexer{
		ctx:         ctx,
		cancel:      cancel,
		blockTaskCh: make(chan BlockTask, 10),
		network: config.NetworkConfig{
			Name: "test",
		},
	}

	// Stop should not panic
	idx.Stop()

	// Context should be cancelled
	select {
	case <-ctx.Done():
		// expected
	default:
		t.Error("expected context to be cancelled after Stop")
	}
}
