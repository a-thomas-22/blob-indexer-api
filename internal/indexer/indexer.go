package indexer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/misc/eip4844"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/attribution"
	"github.com/a-thomas-22/blob-indexer-api/internal/blobparams"
	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	"github.com/a-thomas-22/blob-indexer-api/internal/ethereum"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

const (
	// DefaultBatchSize is the default number of blocks to process in a batch
	DefaultBatchSize = 100

	// DefaultPollingInterval is the default interval to poll for new blocks
	DefaultPollingInterval = 15 * time.Second

	// DefaultMempoolPollingInterval is the default interval to poll for mempool transactions
	DefaultMempoolPollingInterval = 30 * time.Second

	// DefaultWorkerCount is the default number of workers for parallel processing
	DefaultWorkerCount = 4

	// maxGapScanRetries is the total failure count before a block is considered permanently failed
	maxGapScanRetries = 10
)

// errReorgDetected is returned when a chain reorganization is detected and handled
var errReorgDetected = errors.New("chain reorganization detected")

// BlockTask represents a task to process a block
type BlockTask struct {
	BlockNumber uint64
}

type blobMetrics struct {
	blobSizeBytes     int64
	baseFeePerBlobGas string
	tipPerBlobGas     string
	totalCostETH      string
	maxFeePerBlobGas  *string
	blobGasUsed       *int64
}

func calculateBlobMetrics(tx *types.Transaction, blobBaseFee *big.Int) blobMetrics {
	maxFeePerBlobGas := tx.BlobGasFeeCap()
	tipPerBlobGas := new(big.Int).Sub(maxFeePerBlobGas, blobBaseFee)
	if tipPerBlobGas.Sign() < 0 {
		tipPerBlobGas = big.NewInt(0)
	}

	blobGasUsed := tx.BlobGas()
	totalCost := new(big.Int).Mul(
		new(big.Int).Add(blobBaseFee, tipPerBlobGas),
		new(big.Int).SetUint64(blobGasUsed),
	)

	maxFeeStr := maxFeePerBlobGas.String()
	blobGasUsedInt := int64(blobGasUsed)

	return blobMetrics{
		blobSizeBytes:     int64(blobGasUsed * 128), // Approximate size
		baseFeePerBlobGas: blobBaseFee.String(),
		tipPerBlobGas:     tipPerBlobGas.String(),
		totalCostETH:      totalCost.String(),
		maxFeePerBlobGas:  &maxFeeStr,
		blobGasUsed:       &blobGasUsedInt,
	}
}

// Indexer is responsible for indexing blob transactions
type Indexer struct {
	db                     *db.DB
	ethClient              *ethereum.Client
	attribution            *attribution.Service
	config                 *config.Config
	network                config.NetworkConfig
	batchSize              int
	pollingInterval        time.Duration
	mempoolPollingInterval time.Duration
	workerCount            int
	maxBlockRetries        int
	gapScanInterval        time.Duration
	maxReorgDepth          int
	ctx                    context.Context
	cancel                 context.CancelFunc
	wg                     sync.WaitGroup
	lastIndexedBlock       uint64 // accessed with sync/atomic
	indexerVersion         string
	mu                     sync.Mutex // protects DB metadata writes
	blockTaskCh            chan BlockTask
	useWebsocket           bool
	blockSub               *ethereum.BlockSubscription
	pendingTxSub           *ethereum.PendingTxSubscription
	failedBlocks           map[uint64]int // block number -> cumulative failure count
	failedBlocksMu         sync.Mutex
	reorgDetected          uint32              // atomic flag: 1 = reorg detected, main loop should reset
	chainConfig            *params.ChainConfig // go-ethereum chain config for fork-aware blob math
}

// New creates a new indexer
func New(ctx context.Context, database *db.DB, ethClient *ethereum.Client, cfg *config.Config, network config.NetworkConfig) *Indexer {
	indexerCtx, cancel := context.WithCancel(ctx)

	// Create attribution service and set network ID
	attributionSvc := attribution.NewService(database)
	attributionSvc.SetNetworkID(network.ChainID)

	// Determine the number of workers: use config value, or fall back to CPU-based heuristic
	workerCount := cfg.Indexer.WorkerCount
	if workerCount <= 0 {
		workerCount = DefaultWorkerCount
	}
	if runtime.NumCPU() > 2 && cfg.Indexer.WorkerCount == DefaultWorkerCount {
		workerCount = runtime.NumCPU() - 1 // Leave one core free
	}

	// Check if the client supports websockets
	useWebsocket := ethClient.IsWebsocket()

	return &Indexer{
		db:                     database,
		ethClient:              ethClient,
		attribution:            attributionSvc,
		config:                 cfg,
		network:                network,
		batchSize:              cfg.Indexer.BatchSize,
		pollingInterval:        cfg.Indexer.PollingInterval,
		mempoolPollingInterval: cfg.Indexer.MempoolPollingInterval,
		workerCount:            workerCount,
		maxBlockRetries:        cfg.Indexer.MaxBlockRetries,
		gapScanInterval:        cfg.Indexer.GapScanInterval,
		maxReorgDepth:          cfg.Indexer.MaxReorgDepth,
		ctx:                    indexerCtx,
		cancel:                 cancel,
		indexerVersion:         cfg.Indexer.Version,
		blockTaskCh:            make(chan BlockTask, 1000), // Buffer for block tasks
		useWebsocket:           useWebsocket,
		failedBlocks:           make(map[uint64]int),
		chainConfig:            blobparams.ChainConfigForID(network.ChainID),
	}
}

// Start starts the indexer
func (i *Indexer) Start() error {
	logger.Info("Starting indexer...",
		zap.String("network", i.network.Name),
		zap.Int("chain_id", i.network.ChainID),
		zap.Int("worker_count", i.workerCount),
		zap.Bool("websocket_enabled", i.useWebsocket))

	// Initialize the attribution service
	if err := i.attribution.Initialize(i.ctx); err != nil {
		return fmt.Errorf("failed to initialize attribution service: %w", err)
	}

	// Get the last indexed block
	lastBlock, err := i.getLastIndexedBlock()
	if err != nil {
		return fmt.Errorf("failed to get last indexed block: %w", err)
	}
	atomic.StoreUint64(&i.lastIndexedBlock, lastBlock)

	// Determine the starting block
	startBlock, err := i.determineStartBlock()
	if err != nil {
		return fmt.Errorf("failed to determine start block: %w", err)
	}

	// Start the block processing workers
	for w := 1; w <= i.workerCount; w++ {
		i.wg.Add(1)
		go func(workerID int) {
			defer i.wg.Done()
			i.blockProcessingWorker(workerID)
		}(w)
	}

	// Start the block indexer
	i.wg.Add(1)
	go func() {
		defer i.wg.Done()
		i.runBlockIndexer(startBlock)
	}()

	// Start the gap scanner for retrying failed blocks
	i.wg.Add(1)
	go func() {
		defer i.wg.Done()
		i.runGapScanner()
	}()

	// If websocket is available, subscribe to new blocks and pending transactions
	if i.useWebsocket {
		// Subscribe to new blocks
		blockSub, err := i.ethClient.SubscribeToNewHeads(i.ctx, fmt.Sprintf("indexer-%s", i.network.Name))
		if err != nil {
			logger.Warn("Failed to subscribe to new blocks, falling back to polling",
				zap.String("network", i.network.Name),
				zap.Error(err))
		} else {
			i.blockSub = blockSub
			i.wg.Add(1)
			go func() {
				defer i.wg.Done()
				i.handleNewBlockSubscription()
			}()
			logger.Info("Subscribed to new blocks via websocket",
				zap.String("network", i.network.Name))
		}

		// Subscribe to pending transactions
		pendingTxSub, err := i.ethClient.SubscribeToPendingTransactions(i.ctx, fmt.Sprintf("indexer-%s", i.network.Name))
		if err != nil {
			logger.Warn("Failed to subscribe to pending transactions, falling back to polling",
				zap.String("network", i.network.Name),
				zap.Error(err))

			// Start the mempool indexer with polling
			i.wg.Add(1)
			go func() {
				defer i.wg.Done()
				i.runMempoolIndexer()
			}()
		} else {
			i.pendingTxSub = pendingTxSub
			i.wg.Add(1)
			go func() {
				defer i.wg.Done()
				i.handlePendingTransactionSubscription()
			}()
			logger.Info("Subscribed to pending transactions via websocket",
				zap.String("network", i.network.Name))
		}
	} else {
		// Start the mempool indexer with polling
		i.wg.Add(1)
		go func() {
			defer i.wg.Done()
			i.runMempoolIndexer()
		}()
	}

	logger.Info("Indexer started",
		zap.String("network", i.network.Name),
		zap.Uint64("start_block", startBlock))
	return nil
}

// Stop stops the indexer
func (i *Indexer) Stop() {
	logger.Info("Stopping indexer...", zap.String("network", i.network.Name))

	// Cancel the context to signal all goroutines to stop
	i.cancel()

	// Close the block task channel
	close(i.blockTaskCh)

	// Wait for all goroutines to finish
	i.wg.Wait()

	logger.Info("Indexer stopped", zap.String("network", i.network.Name))
}

// getLastIndexedBlock gets the last indexed block from the database
func (i *Indexer) getLastIndexedBlock() (uint64, error) {
	value, err := i.db.GetNetworkMetadata(i.ctx, i.network.ChainID, models.MetadataLastIndexedBlock)
	if err != nil {
		// If the metadata doesn't exist, return 0
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to get last indexed block: %w", err)
	}

	// Parse the block number
	blockNumber, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse last indexed block: %w", err)
	}

	return blockNumber, nil
}

// determineStartBlock determines the starting block for indexing
func (i *Indexer) determineStartBlock() (uint64, error) {
	// If a specific start block is configured, use that
	if i.network.StartBlock != "" {
		// Check if it's a relative block number (e.g., "LATEST-1000")
		if strings.HasPrefix(i.network.StartBlock, "LATEST") {
			// Get the latest block number
			latestBlock, err := i.ethClient.GetLatestBlockNumber(i.ctx)
			if err != nil {
				return 0, fmt.Errorf("failed to get latest block number: %w", err)
			}

			// Parse the offset
			parts := strings.Split(i.network.StartBlock, "-")
			if len(parts) == 2 {
				offset, err := strconv.ParseUint(parts[1], 10, 64)
				if err != nil {
					return 0, fmt.Errorf("failed to parse offset in StartBlock: %w", err)
				}

				// Calculate the start block
				if offset > latestBlock {
					return 0, nil
				}
				return latestBlock - offset, nil
			}

			// No offset, use the latest block
			return latestBlock, nil
		}

		// Parse the block number
		blockNumber, err := strconv.ParseUint(i.network.StartBlock, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("failed to parse StartBlock: %w", err)
		}

		return blockNumber, nil
	}

	// If we have a last indexed block, start from the next block
	lastBlock := atomic.LoadUint64(&i.lastIndexedBlock)
	if lastBlock > 0 {
		return lastBlock + 1, nil
	}

	// Otherwise, start from the EIP-4844 activation block (this is a placeholder)
	// In a real implementation, you would use the actual EIP-4844 activation block
	return 0, nil
}

// blockProcessingWorker processes blocks from the task channel with inline retries
func (i *Indexer) blockProcessingWorker(workerID int) {
	logger.Info("Starting block processing worker",
		zap.String("network", i.network.Name),
		zap.Int("worker_id", workerID))

	for task := range i.blockTaskCh {
		// Check if the context is canceled
		select {
		case <-i.ctx.Done():
			logger.Info("Block processing worker stopped",
				zap.String("network", i.network.Name),
				zap.Int("worker_id", workerID))
			return
		default:
		}

		// Process the block with inline retries and exponential backoff
		var lastErr error
		for attempt := 0; attempt <= i.maxBlockRetries; attempt++ {
			if attempt > 0 {
				delay := time.Duration(1<<uint(attempt-1)) * time.Second
				select {
				case <-time.After(delay):
				case <-i.ctx.Done():
					return
				}
			}

			lastErr = i.processBlock(task.BlockNumber)
			if lastErr == nil {
				break
			}

			// Reorg was detected and handled — don't retry, the main loop will re-queue
			if errors.Is(lastErr, errReorgDetected) {
				lastErr = nil
				break
			}

			if attempt < i.maxBlockRetries {
				logger.Warn("Block processing failed, retrying",
					zap.String("network", i.network.Name),
					zap.Uint64("block", task.BlockNumber),
					zap.Int("attempt", attempt+1),
					zap.Int("max_retries", i.maxBlockRetries),
					zap.Error(lastErr))
			}
		}

		if lastErr != nil {
			logger.Error("Block processing failed after all retries",
				zap.String("network", i.network.Name),
				zap.Uint64("block", task.BlockNumber),
				zap.Int("retries", i.maxBlockRetries),
				zap.Error(lastErr))
			i.trackFailedBlock(task.BlockNumber)
			continue
		}

		// Clear from failed blocks tracking on success
		i.failedBlocksMu.Lock()
		delete(i.failedBlocks, task.BlockNumber)
		i.failedBlocksMu.Unlock()

		// Update the last indexed block
		i.updateLastIndexedBlock(task.BlockNumber)
	}

	logger.Info("Block processing worker exited",
		zap.String("network", i.network.Name),
		zap.Int("worker_id", workerID))
}

// updateLastIndexedBlock atomically updates the last indexed block and persists to DB
func (i *Indexer) updateLastIndexedBlock(blockNumber uint64) {
	for {
		current := atomic.LoadUint64(&i.lastIndexedBlock)
		if blockNumber <= current {
			return
		}
		if atomic.CompareAndSwapUint64(&i.lastIndexedBlock, current, blockNumber) {
			// Serialize DB writes to prevent out-of-order metadata updates
			i.mu.Lock()
			if err := i.db.SetNetworkMetadata(i.ctx, i.network.ChainID, models.MetadataLastIndexedBlock, strconv.FormatUint(blockNumber, 10)); err != nil {
				logger.Error("Failed to update last indexed block metadata",
					zap.String("network", i.network.Name),
					zap.Error(err))
			}
			i.mu.Unlock()
			return
		}
	}
}

// trackFailedBlock records a block that failed processing for later retry by the gap scanner
func (i *Indexer) trackFailedBlock(blockNumber uint64) {
	i.failedBlocksMu.Lock()
	i.failedBlocks[blockNumber]++
	i.failedBlocksMu.Unlock()
}

// runBlockIndexer runs the block indexer
func (i *Indexer) runBlockIndexer(startBlock uint64) {
	logger.Info("Block indexer starting",
		zap.String("network", i.network.Name),
		zap.Uint64("start_block", startBlock))

	currentBlock := startBlock
	ticker := time.NewTicker(i.pollingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-i.ctx.Done():
			logger.Info("Block indexer stopped", zap.String("network", i.network.Name))
			return
		case <-ticker.C:
			// Only use the ticker when we're caught up
		}

		// Check if a reorg was handled and we need to reset position
		if atomic.CompareAndSwapUint32(&i.reorgDetected, 1, 0) {
			newStart := atomic.LoadUint64(&i.lastIndexedBlock) + 1
			logger.Info("Resetting indexer position after chain reorganization",
				zap.String("network", i.network.Name),
				zap.Uint64("old_position", currentBlock),
				zap.Uint64("new_position", newStart))
			currentBlock = newStart
		}

		// Get the latest block number
		latestBlock, err := i.ethClient.GetLatestBlockNumber(i.ctx)
		if err != nil {
			logger.Error("Failed to get latest block number",
				zap.String("network", i.network.Name),
				zap.Error(err))
			continue
		}

		// If we're caught up, wait for the next tick
		if currentBlock > latestBlock {
			continue
		}

		// Process blocks in batches
		endBlock := currentBlock + uint64(i.batchSize) - 1
		if endBlock > latestBlock {
			endBlock = latestBlock
		}

		logger.Info("Processing blocks",
			zap.String("network", i.network.Name),
			zap.Uint64("start_block", currentBlock),
			zap.Uint64("end_block", endBlock))

		// Queue blocks for processing — blocks on send until channel has space
		for blockNumber := currentBlock; blockNumber <= endBlock; blockNumber++ {
			select {
			case <-i.ctx.Done():
				return
			case i.blockTaskCh <- BlockTask{BlockNumber: blockNumber}:
			}
		}

		// Update the current block
		currentBlock = endBlock + 1
	}
}

// handleNewBlockSubscription handles the websocket subscription to new blocks
func (i *Indexer) handleNewBlockSubscription() {
	logger.Info("Starting new block subscription handler",
		zap.String("network", i.network.Name))

	for {
		select {
		case <-i.ctx.Done():
			logger.Info("New block subscription handler stopped",
				zap.String("network", i.network.Name))
			return
		case err := <-i.blockSub.Subscription.Err():
			logger.Error("Block subscription error, reconnecting...",
				zap.String("network", i.network.Name),
				zap.Error(err))

			// Try to resubscribe
			blockSub, err := i.ethClient.SubscribeToNewHeads(i.ctx, fmt.Sprintf("indexer-%s", i.network.Name))
			if err != nil {
				logger.Error("Failed to resubscribe to new blocks, falling back to polling",
					zap.String("network", i.network.Name),
					zap.Error(err))
				return
			}
			i.blockSub = blockSub

		case header := <-i.blockSub.Headers:
			// Process the new block
			blockNumber := header.Number.Uint64()

			// Use atomic read — no lock needed
			current := atomic.LoadUint64(&i.lastIndexedBlock)
			if blockNumber > current {
				// Block instead of dropping when channel is full
				select {
				case i.blockTaskCh <- BlockTask{BlockNumber: blockNumber}:
					logger.Debug("Queued new block from subscription",
						zap.String("network", i.network.Name),
						zap.Uint64("block", blockNumber))
				case <-i.ctx.Done():
					return
				}
			}
		}
	}
}

// handlePendingTransactionSubscription handles the websocket subscription to pending transactions
func (i *Indexer) handlePendingTransactionSubscription() {
	logger.Info("Starting pending transaction subscription handler",
		zap.String("network", i.network.Name))

	for {
		select {
		case <-i.ctx.Done():
			logger.Info("Pending transaction subscription handler stopped",
				zap.String("network", i.network.Name))
			return
		case err := <-i.pendingTxSub.Subscription.Err():
			logger.Error("Pending transaction subscription error, reconnecting...",
				zap.String("network", i.network.Name),
				zap.Error(err))

			// Try to resubscribe
			pendingTxSub, err := i.ethClient.SubscribeToPendingTransactions(i.ctx, fmt.Sprintf("indexer-%s", i.network.Name))
			if err != nil {
				logger.Error("Failed to resubscribe to pending transactions, falling back to polling",
					zap.String("network", i.network.Name),
					zap.Error(err))
				return
			}
			i.pendingTxSub = pendingTxSub

		case hash := <-i.pendingTxSub.Hashes:
			// Process the pending transaction
			i.processPendingTransaction(hash)
		}
	}
}

// processPendingTransaction processes a single pending transaction by its hash
func (i *Indexer) processPendingTransaction(hash common.Hash) {
	// Get the transaction details
	tx, isPending, err := i.ethClient.GetTransactionByHash(i.ctx, hash)
	if err != nil {
		logger.Error("Failed to get pending transaction",
			zap.String("network", i.network.Name),
			zap.String("tx_hash", hash.Hex()),
			zap.Error(err))
		return
	}

	// Skip if not pending or not a blob transaction
	if !isPending || !i.ethClient.IsBlobTransaction(tx) {
		return
	}

	// Get the blob base fee from the latest block header
	latestBlockNum, err := i.ethClient.GetLatestBlockNumber(i.ctx)
	if err != nil {
		logger.Error("Failed to get latest block number for pending tx",
			zap.String("network", i.network.Name),
			zap.Error(err))
		return
	}

	latestBlock, err := i.ethClient.GetBlockByNumber(i.ctx, latestBlockNum)
	if err != nil {
		logger.Error("Failed to get latest block for pending tx",
			zap.String("network", i.network.Name),
			zap.Error(err))
		return
	}

	var blobBaseFee *big.Int
	if latestBlock.Header().ExcessBlobGas != nil {
		blobBaseFee = eip4844.CalcBlobFee(i.chainConfig, latestBlock.Header())
	} else {
		blobBaseFee = big.NewInt(1)
	}

	// Get the sender address
	from, err := i.getSender(tx)
	if err != nil {
		logger.Error("Failed to get sender for pending transaction",
			zap.String("network", i.network.Name),
			zap.String("tx_hash", hash.Hex()),
			zap.Error(err))
		return
	}

	// Get the user attribution
	userAttribution := i.attribution.GetUserAttribution(from)

	metrics := calculateBlobMetrics(tx, blobBaseFee)

	// Create the blob record
	blob := models.Blob{
		NetworkID:         i.network.ChainID,
		BlockNumber:       -1, // Pending transaction
		BlobIndex:         0,  // Placeholder
		TxHash:            hash.Hex(),
		FromAddress:       from,
		UserAttribution:   userAttribution,
		BlobSizeBytes:     metrics.blobSizeBytes,
		BaseFeePerBlobGas: metrics.baseFeePerBlobGas,
		TipPerBlobGas:     metrics.tipPerBlobGas,
		TotalCostETH:      metrics.totalCostETH,
		Timestamp:         time.Now(),
		Confirmed:         false,
		IndexerVersion:    i.indexerVersion,
		MaxFeePerBlobGas:  metrics.maxFeePerBlobGas,
		BlobGasUsed:       metrics.blobGasUsed,
	}

	// Insert the blob record
	if err := i.insertPendingBlob(blob); err != nil {
		logger.Error("Failed to insert pending blob record",
			zap.String("network", i.network.Name),
			zap.String("tx_hash", hash.Hex()),
			zap.Error(err))
	}
}

// processBlock processes a single block with reorg detection and batch inserts
func (i *Indexer) processBlock(blockNumber uint64) error {
	// Get the block
	block, err := i.ethClient.GetBlockByNumber(i.ctx, blockNumber)
	if err != nil {
		return fmt.Errorf("failed to get block %d: %w", blockNumber, err)
	}

	// Check for chain reorganization by comparing parent hash
	if err := i.checkForReorg(blockNumber, block); err != nil {
		return err
	}

	// Compute blob base fee from the block header (fixes historical accuracy bug
	// where eth_blobBaseFee RPC returned the current fee, not the block's actual fee)
	header := block.Header()
	var blobBaseFee *big.Int
	if header.ExcessBlobGas != nil {
		blobBaseFee = eip4844.CalcBlobFee(i.chainConfig, header)
	} else {
		blobBaseFee = big.NewInt(1) // pre-4844 block, minimum fee
	}

	// Get the block timestamp
	timestamp := i.ethClient.GetBlockTimestamp(block)

	// Extract block-level blob metrics from the header
	bp := blobparams.GetBlobParams(i.chainConfig, block.Time())

	var blockBlobGasUsed uint64
	if header.BlobGasUsed != nil {
		blockBlobGasUsed = *header.BlobGasUsed
	}
	var excessBlobGas uint64
	if header.ExcessBlobGas != nil {
		excessBlobGas = *header.ExcessBlobGas
	}

	var utilizationRatio float64
	if bp.TargetGas > 0 {
		utilizationRatio = float64(blockBlobGasUsed) / float64(bp.TargetGas)
	}

	// Collect all blob records for this block
	blobs := make([]models.Blob, 0, len(block.Transactions()))
	var attributedUsers []string
	blobTxCount := 0

	for txIndex, tx := range block.Transactions() {
		// Check if it's a blob transaction
		if !i.ethClient.IsBlobTransaction(tx) {
			continue
		}
		blobTxCount++

		// Get the sender address
		from, err := i.getSender(tx)
		if err != nil {
			logger.Error("Failed to get sender for transaction",
				zap.String("network", i.network.Name),
				zap.String("tx_hash", tx.Hash().Hex()),
				zap.Error(err))
			continue
		}

		// Get the user attribution
		userAttribution := i.attribution.GetUserAttribution(from)

		metrics := calculateBlobMetrics(tx, blobBaseFee)

		blobs = append(blobs, models.Blob{
			NetworkID:         i.network.ChainID,
			BlockNumber:       int64(blockNumber),
			BlobIndex:         txIndex,
			TxHash:            tx.Hash().Hex(),
			FromAddress:       from,
			UserAttribution:   userAttribution,
			BlobSizeBytes:     metrics.blobSizeBytes,
			BaseFeePerBlobGas: metrics.baseFeePerBlobGas,
			TipPerBlobGas:     metrics.tipPerBlobGas,
			TotalCostETH:      metrics.totalCostETH,
			Timestamp:         timestamp,
			Confirmed:         true,
			IndexerVersion:    i.indexerVersion,
			MaxFeePerBlobGas:  metrics.maxFeePerBlobGas,
			BlobGasUsed:       metrics.blobGasUsed,
		})

		if userAttribution != "" {
			attributedUsers = append(attributedUsers, from)
		}
	}

	// Build block-level metrics
	blockMetrics := &models.BlockMetrics{
		NetworkID:        i.network.ChainID,
		BlockNumber:      int64(blockNumber),
		BlockTimestamp:   timestamp,
		BlobCount:        blobTxCount,
		BlobGasUsed:      int64(blockBlobGasUsed),
		BlobGasTarget:    int64(bp.TargetGas),
		BlobGasLimit:     int64(bp.MaxGas),
		ExcessBlobGas:    int64(excessBlobGas),
		BlobBaseFee:      blobBaseFee.String(),
		UtilizationRatio: fmt.Sprintf("%.6f", utilizationRatio),
		BlobParamsTarget: bp.Target,
		BlobParamsMax:    bp.Max,
		UpdateFraction:   int64(bp.UpdateFraction),
	}

	// Insert all blobs, block metrics, and indexed block in a single transaction
	indexedBlock := models.IndexedBlock{
		NetworkID:   i.network.ChainID,
		BlockNumber: int64(blockNumber),
		BlockHash:   block.Hash().Hex(),
		ParentHash:  block.ParentHash().Hex(),
	}

	if err := i.insertBlockData(blobs, indexedBlock, blockMetrics); err != nil {
		return fmt.Errorf("failed to insert block data for block %d: %w", blockNumber, err)
	}

	// Update user last seen timestamps in batch (non-critical, don't fail the block)
	if len(attributedUsers) > 0 {
		if err := i.attribution.BatchUpdateUserLastSeen(i.ctx, attributedUsers); err != nil {
			logger.Error("Failed to batch update user last seen",
				zap.String("network", i.network.Name),
				zap.Int("address_count", len(attributedUsers)),
				zap.Error(err))
		}
	}

	return nil
}

// checkForReorg checks if the parent hash of the current block matches our stored hash
// for the previous block. If not, a chain reorganization has occurred.
func (i *Indexer) checkForReorg(blockNumber uint64, block *types.Block) error {
	if blockNumber == 0 {
		return nil
	}

	storedHash, err := i.db.GetIndexedBlockHash(i.ctx, i.network.ChainID, blockNumber-1)
	if err != nil {
		// Previous block not in our index — can't check (initial sync, gap, or first block)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("failed to get indexed block hash for block %d: %w", blockNumber-1, err)
	}

	parentHash := block.ParentHash().Hex()
	if storedHash != parentHash {
		logger.Warn("Chain reorganization detected",
			zap.String("network", i.network.Name),
			zap.Uint64("block", blockNumber),
			zap.String("expected_parent", storedHash),
			zap.String("actual_parent", parentHash))
		return i.handleReorg(blockNumber)
	}

	return nil
}

// handleReorg handles a chain reorganization by finding the fork point,
// deleting invalidated data, and signaling the main loop to reset.
func (i *Indexer) handleReorg(fromBlock uint64) error {
	// Walk back to find the fork point
	forkBlock := fromBlock - 1
	for depth := 0; depth < i.maxReorgDepth && forkBlock > 0; depth++ {
		block, err := i.ethClient.GetBlockByNumber(i.ctx, forkBlock)
		if err != nil {
			return fmt.Errorf("failed to get block %d during reorg scan: %w", forkBlock, err)
		}

		storedHash, err := i.db.GetIndexedBlockHash(i.ctx, i.network.ChainID, forkBlock)
		if err != nil {
			// Past our indexed range
			break
		}

		if storedHash == block.Hash().Hex() {
			// Found the fork point — this block is still valid
			break
		}

		forkBlock--
	}

	logger.Warn("Handling chain reorganization",
		zap.String("network", i.network.Name),
		zap.Uint64("from_block", fromBlock),
		zap.Uint64("fork_point", forkBlock),
		zap.Uint64("invalidated_blocks", fromBlock-forkBlock-1))

	// Delete invalidated data
	if err := i.db.DeleteBlobsFromBlock(i.ctx, i.network.ChainID, int64(forkBlock+1)); err != nil {
		return fmt.Errorf("failed to delete reorged blobs: %w", err)
	}
	if err := i.db.DeleteBlockMetricsFromBlock(i.ctx, i.network.ChainID, int64(forkBlock+1)); err != nil {
		return fmt.Errorf("failed to delete reorged block metrics: %w", err)
	}
	if err := i.db.DeleteIndexedBlocksFromBlock(i.ctx, i.network.ChainID, forkBlock+1); err != nil {
		return fmt.Errorf("failed to delete reorged indexed blocks: %w", err)
	}

	// Reset lastIndexedBlock to the fork point
	atomic.StoreUint64(&i.lastIndexedBlock, forkBlock)
	i.mu.Lock()
	if err := i.db.SetNetworkMetadata(i.ctx, i.network.ChainID, models.MetadataLastIndexedBlock, strconv.FormatUint(forkBlock, 10)); err != nil {
		logger.Error("Failed to update last indexed block after reorg",
			zap.String("network", i.network.Name),
			zap.Error(err))
	}
	i.mu.Unlock()

	// Signal the main indexer loop to reset its position
	atomic.StoreUint32(&i.reorgDetected, 1)

	return fmt.Errorf("reorg handled, rewound from %d to %d: %w", fromBlock, forkBlock, errReorgDetected)
}

// insertBlockData inserts all blobs, block metrics, and records the indexed block in a single
// database transaction. This ensures atomicity — either the entire block is recorded or nothing is.
func (i *Indexer) insertBlockData(blobs []models.Blob, indexedBlock models.IndexedBlock, blockMetrics *models.BlockMetrics) error {
	tx, err := i.db.BeginTxx(i.ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Insert blobs using a prepared statement within the transaction
	if len(blobs) > 0 {
		blobStmt, err := tx.PrepareContext(i.ctx, `
			INSERT INTO blobs (
				network_id, block_number, blob_index, tx_hash, from_address, user_attribution,
				blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_eth,
				timestamp, confirmed, indexer_version, max_fee_per_blob_gas, blob_gas_used
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
			ON CONFLICT (network_id, block_number, blob_index) DO UPDATE SET
				tx_hash = EXCLUDED.tx_hash,
				from_address = EXCLUDED.from_address,
				user_attribution = EXCLUDED.user_attribution,
				blob_size_bytes = EXCLUDED.blob_size_bytes,
				base_fee_per_blob_gas = EXCLUDED.base_fee_per_blob_gas,
				tip_per_blob_gas = EXCLUDED.tip_per_blob_gas,
				total_cost_eth = EXCLUDED.total_cost_eth,
				timestamp = EXCLUDED.timestamp,
				confirmed = EXCLUDED.confirmed,
				indexer_version = EXCLUDED.indexer_version,
				max_fee_per_blob_gas = EXCLUDED.max_fee_per_blob_gas,
				blob_gas_used = EXCLUDED.blob_gas_used
		`)
		if err != nil {
			return fmt.Errorf("failed to prepare blob statement: %w", err)
		}
		defer blobStmt.Close()

		for _, blob := range blobs {
			if _, err := blobStmt.ExecContext(i.ctx,
				blob.NetworkID, blob.BlockNumber, blob.BlobIndex, blob.TxHash, blob.FromAddress, blob.UserAttribution,
				blob.BlobSizeBytes, blob.BaseFeePerBlobGas, blob.TipPerBlobGas, blob.TotalCostETH,
				blob.Timestamp, blob.Confirmed, blob.IndexerVersion, blob.MaxFeePerBlobGas, blob.BlobGasUsed,
			); err != nil {
				return fmt.Errorf("failed to insert blob (tx: %s): %w", blob.TxHash, err)
			}
		}
	}

	// Insert block-level blob metrics
	if blockMetrics != nil {
		_, err = tx.ExecContext(i.ctx, `
			INSERT INTO block_metrics (
				network_id, block_number, block_timestamp, blob_count,
				blob_gas_used, blob_gas_target, blob_gas_limit,
				excess_blob_gas, blob_base_fee, utilization_ratio,
				blob_params_target, blob_params_max, update_fraction
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
			ON CONFLICT (network_id, block_number) DO UPDATE SET
				block_timestamp = EXCLUDED.block_timestamp,
				blob_count = EXCLUDED.blob_count,
				blob_gas_used = EXCLUDED.blob_gas_used,
				blob_gas_target = EXCLUDED.blob_gas_target,
				blob_gas_limit = EXCLUDED.blob_gas_limit,
				excess_blob_gas = EXCLUDED.excess_blob_gas,
				blob_base_fee = EXCLUDED.blob_base_fee,
				utilization_ratio = EXCLUDED.utilization_ratio,
				blob_params_target = EXCLUDED.blob_params_target,
				blob_params_max = EXCLUDED.blob_params_max,
				update_fraction = EXCLUDED.update_fraction
		`, blockMetrics.NetworkID, blockMetrics.BlockNumber, blockMetrics.BlockTimestamp, blockMetrics.BlobCount,
			blockMetrics.BlobGasUsed, blockMetrics.BlobGasTarget, blockMetrics.BlobGasLimit,
			blockMetrics.ExcessBlobGas, blockMetrics.BlobBaseFee, blockMetrics.UtilizationRatio,
			blockMetrics.BlobParamsTarget, blockMetrics.BlobParamsMax, blockMetrics.UpdateFraction)
		if err != nil {
			return fmt.Errorf("failed to insert block metrics: %w", err)
		}
	}

	// Record the indexed block for reorg detection
	_, err = tx.ExecContext(i.ctx, `
		INSERT INTO indexed_blocks (network_id, block_number, block_hash, parent_hash)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (network_id, block_number) DO UPDATE SET
			block_hash = EXCLUDED.block_hash,
			parent_hash = EXCLUDED.parent_hash,
			indexed_at = NOW()
	`, indexedBlock.NetworkID, indexedBlock.BlockNumber, indexedBlock.BlockHash, indexedBlock.ParentHash)
	if err != nil {
		return fmt.Errorf("failed to record indexed block: %w", err)
	}

	return tx.Commit()
}

// getSender gets the sender address for a transaction
func (i *Indexer) getSender(tx *types.Transaction) (string, error) {
	// Get the signer for the transaction
	signer := types.LatestSignerForChainID(big.NewInt(int64(i.network.ChainID)))

	// Get the sender address
	sender, err := types.Sender(signer, tx)
	if err != nil {
		// Check if the error is due to chain ID mismatch
		if strings.Contains(err.Error(), "invalid chain id") {
			// Try to extract the chain ID from the transaction
			txChainID := tx.ChainId()
			if txChainID != nil {
				logger.Warn("Transaction has different chain ID than network config",
					zap.String("network", i.network.Name),
					zap.Int("network_chain_id", i.network.ChainID),
					zap.String("tx_chain_id", txChainID.String()),
					zap.String("tx_hash", tx.Hash().Hex()))

				// Try with the transaction's chain ID instead
				alternateSigner := types.LatestSignerForChainID(txChainID)
				sender, err = types.Sender(alternateSigner, tx)
				if err != nil {
					return "", fmt.Errorf("failed to get sender with tx chain ID: %w", err)
				}
				return sender.Hex(), nil
			}
		}
		return "", fmt.Errorf("failed to get sender: %w", err)
	}

	return sender.Hex(), nil
}

// runMempoolIndexer runs the mempool indexer
func (i *Indexer) runMempoolIndexer() {
	logger.Info("Mempool indexer starting", zap.String("network", i.network.Name))

	ticker := time.NewTicker(i.mempoolPollingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-i.ctx.Done():
			logger.Info("Mempool indexer stopped", zap.String("network", i.network.Name))
			return
		case <-ticker.C:
			// Process pending transactions
			if err := i.processPendingTransactions(); err != nil {
				logger.Error("Failed to process pending transactions",
					zap.String("network", i.network.Name),
					zap.Error(err))
			}
		}
	}
}

// processPendingTransactions processes pending transactions from the mempool
func (i *Indexer) processPendingTransactions() error {
	// Get pending transactions
	pendingTxs, err := i.ethClient.GetPendingTransactions(i.ctx)
	if err != nil {
		return fmt.Errorf("failed to get pending transactions: %w", err)
	}

	// Get the blob base fee from the latest block header
	latestBlockNum, err := i.ethClient.GetLatestBlockNumber(i.ctx)
	if err != nil {
		return fmt.Errorf("failed to get latest block number: %w", err)
	}

	latestBlock, err := i.ethClient.GetBlockByNumber(i.ctx, latestBlockNum)
	if err != nil {
		return fmt.Errorf("failed to get latest block: %w", err)
	}

	var blobBaseFee *big.Int
	if latestBlock.Header().ExcessBlobGas != nil {
		blobBaseFee = eip4844.CalcBlobFee(i.chainConfig, latestBlock.Header())
	} else {
		blobBaseFee = big.NewInt(1)
	}

	// Process each pending transaction
	for _, tx := range pendingTxs {
		// Check if it's a blob transaction
		if !i.ethClient.IsBlobTransaction(tx) {
			continue
		}

		// Get the sender address
		from, err := i.getSender(tx)
		if err != nil {
			logger.Error("Failed to get sender for pending transaction",
				zap.String("network", i.network.Name),
				zap.String("tx_hash", tx.Hash().Hex()),
				zap.Error(err))
			continue
		}

		// Get the user attribution
		userAttribution := i.attribution.GetUserAttribution(from)

		metrics := calculateBlobMetrics(tx, blobBaseFee)

		// Create the blob record
		blob := models.Blob{
			NetworkID:         i.network.ChainID,
			BlockNumber:       -1, // Pending transaction
			BlobIndex:         0,  // Placeholder
			TxHash:            tx.Hash().Hex(),
			FromAddress:       from,
			UserAttribution:   userAttribution,
			BlobSizeBytes:     metrics.blobSizeBytes,
			BaseFeePerBlobGas: metrics.baseFeePerBlobGas,
			TipPerBlobGas:     metrics.tipPerBlobGas,
			TotalCostETH:      metrics.totalCostETH,
			Timestamp:         time.Now(),
			Confirmed:         false,
			IndexerVersion:    i.indexerVersion,
			MaxFeePerBlobGas:  metrics.maxFeePerBlobGas,
			BlobGasUsed:       metrics.blobGasUsed,
		}

		// Insert the blob record
		if err := i.insertPendingBlob(blob); err != nil {
			logger.Error("Failed to insert pending blob record",
				zap.String("network", i.network.Name),
				zap.String("tx_hash", tx.Hash().Hex()),
				zap.Error(err))
			continue
		}
	}

	return nil
}

// insertPendingBlob inserts a pending blob record into the database
func (i *Indexer) insertPendingBlob(blob models.Blob) error {
	// For pending transactions, we need to use a different approach

	// First, check if any blob with the same network_id, block_number, and blob_index exists
	// This handles the unique constraint "blobs_network_id_block_number_blob_index_key"
	var existingID int
	var existingTxHash string
	checkUniqueQuery := `
		SELECT id, tx_hash FROM blobs
		WHERE network_id = $1 AND block_number = $2 AND blob_index = $3
		LIMIT 1
	`
	err := i.db.QueryRowContext(i.ctx, checkUniqueQuery,
		blob.NetworkID, blob.BlockNumber, blob.BlobIndex).Scan(&existingID, &existingTxHash)

	// If we found an existing record with the same network_id, block_number, and blob_index
	if err == nil {
		// If it's the same transaction, update it
		if existingTxHash == blob.TxHash {
			query := `
				UPDATE blobs SET
					from_address = $3,
					user_attribution = $4,
					blob_size_bytes = $5,
					base_fee_per_blob_gas = $6,
					tip_per_blob_gas = $7,
					total_cost_eth = $8,
					timestamp = $9,
					confirmed = $10,
					indexer_version = $11,
					max_fee_per_blob_gas = $12,
					blob_gas_used = $13
				WHERE id = $1 AND tx_hash = $2
			`
			_, err = i.db.ExecContext(i.ctx, query,
				existingID, blob.TxHash, blob.FromAddress, blob.UserAttribution,
				blob.BlobSizeBytes, blob.BaseFeePerBlobGas, blob.TipPerBlobGas, blob.TotalCostETH,
				blob.Timestamp, blob.Confirmed, blob.IndexerVersion,
				blob.MaxFeePerBlobGas, blob.BlobGasUsed,
			)
			return err
		}

		// If it's a different transaction, use a different blob_index
		// Find the next available blob_index for this network and block
		var maxBlobIndex int
		blobIndexQuery := `
			SELECT COALESCE(MAX(blob_index), -1) FROM blobs
			WHERE network_id = $1 AND block_number = $2
		`
		err = i.db.QueryRowContext(i.ctx, blobIndexQuery,
			blob.NetworkID, blob.BlockNumber).Scan(&maxBlobIndex)
		if err != nil {
			return fmt.Errorf("failed to get max blob index: %w", err)
		}

		// Use the next available blob_index
		blob.BlobIndex = maxBlobIndex + 1
	} else if !errors.Is(err, sql.ErrNoRows) {
		// If there was an error other than "no rows", return it
		return fmt.Errorf("failed to check for existing blob: %w", err)
	}

	// Now check if this specific transaction already exists as a pending transaction
	var existsPending bool
	checkPendingQuery := `
		SELECT EXISTS (
			SELECT 1 FROM blobs
			WHERE network_id = $1 AND tx_hash = $2 AND block_number < 0
		)
	`
	err = i.db.QueryRowContext(i.ctx, checkPendingQuery,
		blob.NetworkID, blob.TxHash).Scan(&existsPending)
	if err != nil {
		return fmt.Errorf("failed to check if pending blob exists: %w", err)
	}

	if existsPending {
		// Update the existing pending transaction
		query := `
			UPDATE blobs SET
				blob_index = $3,
				from_address = $4,
				user_attribution = $5,
				blob_size_bytes = $6,
				base_fee_per_blob_gas = $7,
				tip_per_blob_gas = $8,
				total_cost_eth = $9,
				timestamp = $10,
				confirmed = $11,
				indexer_version = $12,
				max_fee_per_blob_gas = $13,
				blob_gas_used = $14
			WHERE network_id = $1 AND tx_hash = $2 AND block_number < 0
		`
		_, err = i.db.ExecContext(i.ctx, query,
			blob.NetworkID, blob.TxHash, blob.BlobIndex, blob.FromAddress, blob.UserAttribution,
			blob.BlobSizeBytes, blob.BaseFeePerBlobGas, blob.TipPerBlobGas, blob.TotalCostETH,
			blob.Timestamp, blob.Confirmed, blob.IndexerVersion,
			blob.MaxFeePerBlobGas, blob.BlobGasUsed,
		)
	} else {
		// Insert a new record
		query := `
			INSERT INTO blobs (
				network_id, block_number, blob_index, tx_hash, from_address, user_attribution,
				blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_eth,
				timestamp, confirmed, indexer_version, max_fee_per_blob_gas, blob_gas_used
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15
			)
		`
		_, err = i.db.ExecContext(i.ctx, query,
			blob.NetworkID, blob.BlockNumber, blob.BlobIndex, blob.TxHash, blob.FromAddress, blob.UserAttribution,
			blob.BlobSizeBytes, blob.BaseFeePerBlobGas, blob.TipPerBlobGas, blob.TotalCostETH,
			blob.Timestamp, blob.Confirmed, blob.IndexerVersion,
			blob.MaxFeePerBlobGas, blob.BlobGasUsed,
		)
	}

	return err
}

// processBlockRange processes a range of blocks by queuing them for the worker pool
func (i *Indexer) processBlockRange(startBlock, endBlock uint64) error {
	logger.Info("Queuing block range for processing",
		zap.String("network", i.network.Name),
		zap.Uint64("start_block", startBlock),
		zap.Uint64("end_block", endBlock))

	// Queue blocks for processing
	for blockNumber := startBlock; blockNumber <= endBlock; blockNumber++ {
		select {
		case <-i.ctx.Done():
			return i.ctx.Err()
		case i.blockTaskCh <- BlockTask{BlockNumber: blockNumber}:
			// Block successfully queued
		}
	}

	return nil
}

// Reindex reindexes blocks in the specified range
func (i *Indexer) Reindex(startBlock, endBlock uint64) error {
	logger.Info("Reindexing blocks",
		zap.String("network", i.network.Name),
		zap.Uint64("start_block", startBlock),
		zap.Uint64("end_block", endBlock))

	// Delete existing blob records in the range
	query := "DELETE FROM blobs WHERE network_id = $1 AND block_number >= $2 AND block_number <= $3"
	_, err := i.db.ExecContext(i.ctx, query, i.network.ChainID, startBlock, endBlock)
	if err != nil {
		return fmt.Errorf("failed to delete existing blob records: %w", err)
	}

	// Delete existing block metrics in the range
	query = "DELETE FROM block_metrics WHERE network_id = $1 AND block_number >= $2 AND block_number <= $3"
	_, err = i.db.ExecContext(i.ctx, query, i.network.ChainID, startBlock, endBlock)
	if err != nil {
		return fmt.Errorf("failed to delete existing block metrics: %w", err)
	}

	// Delete existing indexed block records in the range
	query = "DELETE FROM indexed_blocks WHERE network_id = $1 AND block_number >= $2 AND block_number <= $3"
	_, err = i.db.ExecContext(i.ctx, query, i.network.ChainID, startBlock, endBlock)
	if err != nil {
		return fmt.Errorf("failed to delete existing indexed block records: %w", err)
	}

	// Process the block range
	return i.processBlockRange(startBlock, endBlock)
}

// runGapScanner periodically retries blocks that failed processing
func (i *Indexer) runGapScanner() {
	logger.Info("Gap scanner starting", zap.String("network", i.network.Name))

	ticker := time.NewTicker(i.gapScanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-i.ctx.Done():
			logger.Info("Gap scanner stopped", zap.String("network", i.network.Name))
			return
		case <-ticker.C:
			i.retryFailedBlocks()
		}
	}
}

// retryFailedBlocks re-queues blocks that previously failed processing
func (i *Indexer) retryFailedBlocks() {
	i.failedBlocksMu.Lock()
	if len(i.failedBlocks) == 0 {
		i.failedBlocksMu.Unlock()
		return
	}

	var toRetry []uint64
	for block, count := range i.failedBlocks {
		if count <= maxGapScanRetries {
			toRetry = append(toRetry, block)
		} else {
			logger.Error("Block permanently failed, exceeded max retries",
				zap.String("network", i.network.Name),
				zap.Uint64("block", block),
				zap.Int("total_attempts", count))
		}
	}
	i.failedBlocksMu.Unlock()

	if len(toRetry) == 0 {
		return
	}

	logger.Info("Gap scanner re-queuing failed blocks",
		zap.String("network", i.network.Name),
		zap.Int("count", len(toRetry)))

	for _, blockNum := range toRetry {
		select {
		case <-i.ctx.Done():
			return
		case i.blockTaskCh <- BlockTask{BlockNumber: blockNum}:
		}
	}
}

// GetLastIndexedBlock gets the last indexed block
func (i *Indexer) GetLastIndexedBlock() uint64 {
	return atomic.LoadUint64(&i.lastIndexedBlock)
}

// GetNetworkInfo gets the network information
func (i *Indexer) GetNetworkInfo() config.NetworkConfig {
	return i.network
}

// GetCurrentBlock gets the current block number from the Ethereum node
func (i *Indexer) GetCurrentBlock(ctx context.Context) (uint64, error) {
	return i.ethClient.GetLatestBlockNumber(ctx)
}

// GetBlobCounts gets the counts of confirmed and pending blobs for this network
func (i *Indexer) GetBlobCounts(ctx context.Context) (confirmedCount, pendingCount int, err error) {
	query := `
		SELECT
			SUM(CASE WHEN confirmed = true THEN 1 ELSE 0 END) as confirmed_count,
			SUM(CASE WHEN confirmed = false THEN 1 ELSE 0 END) as pending_count
		FROM blobs
		WHERE network_id = $1
	`

	var counts struct {
		ConfirmedCount int `db:"confirmed_count"`
		PendingCount   int `db:"pending_count"`
	}

	err = i.db.GetContext(ctx, &counts, query, i.network.ChainID)
	if err != nil {
		return 0, 0, err
	}

	return counts.ConfirmedCount, counts.PendingCount, nil
}

// GetTopBlobUsers gets the top blob users by number of blobs for this network
func (i *Indexer) GetTopBlobUsers(ctx context.Context, limit, offset int) ([]models.BlobUserStats, error) {
	return i.attribution.GetTopBlobUsers(ctx, limit, offset)
}
