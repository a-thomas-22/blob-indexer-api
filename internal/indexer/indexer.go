package indexer

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/a-thomas-22/blob-indexer-api/internal/attribution"
	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	"github.com/a-thomas-22/blob-indexer-api/internal/ethereum"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"go.uber.org/zap"
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
)

// BlockTask represents a task to process a block
type BlockTask struct {
	BlockNumber uint64
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
	ctx                    context.Context
	cancel                 context.CancelFunc
	wg                     sync.WaitGroup
	lastIndexedBlock       uint64
	indexerVersion         string
	mu                     sync.Mutex
	blockTaskCh            chan BlockTask
	useWebsocket           bool
	blockSub               *ethereum.BlockSubscription
	pendingTxSub           *ethereum.PendingTxSubscription
}

// New creates a new indexer
func New(ctx context.Context, db *db.DB, ethClient *ethereum.Client, cfg *config.Config, network config.NetworkConfig) *Indexer {
	indexerCtx, cancel := context.WithCancel(ctx)

	// Create attribution service and set network ID
	attributionSvc := attribution.NewService(db)
	attributionSvc.SetNetworkID(network.ChainID)

	// Determine the number of workers based on CPU cores
	workerCount := DefaultWorkerCount
	if runtime.NumCPU() > 2 {
		workerCount = runtime.NumCPU() - 1 // Leave one core free
	}

	// Check if the client supports websockets
	useWebsocket := ethClient.IsWebsocket()

	return &Indexer{
		db:                     db,
		ethClient:              ethClient,
		attribution:            attributionSvc,
		config:                 cfg,
		network:                network,
		batchSize:              cfg.Indexer.BatchSize,
		pollingInterval:        cfg.Indexer.PollingInterval,
		mempoolPollingInterval: cfg.Indexer.MempoolPollingInterval,
		workerCount:            workerCount,
		ctx:                    indexerCtx,
		cancel:                 cancel,
		indexerVersion:         cfg.Indexer.Version,
		blockTaskCh:            make(chan BlockTask, 1000), // Buffer for block tasks
		useWebsocket:           useWebsocket,
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
	i.lastIndexedBlock = lastBlock

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
		return 0, nil
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
	if i.lastIndexedBlock > 0 {
		return i.lastIndexedBlock + 1, nil
	}

	// Otherwise, start from the EIP-4844 activation block (this is a placeholder)
	// In a real implementation, you would use the actual EIP-4844 activation block
	return 0, nil
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

			// Update the last indexed block if this is higher
			i.mu.Lock()
			if blockNumber > i.lastIndexedBlock {
				// Queue the block for processing
				select {
				case i.blockTaskCh <- BlockTask{BlockNumber: blockNumber}:
					logger.Debug("Queued new block from subscription",
						zap.String("network", i.network.Name),
						zap.Uint64("block", blockNumber))
				default:
					logger.Warn("Block task channel full, dropping block",
						zap.String("network", i.network.Name),
						zap.Uint64("block", blockNumber))
				}
			}
			i.mu.Unlock()
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

	// Get the blob base fee for the latest block
	latestBlock, err := i.ethClient.GetLatestBlockNumber(i.ctx)
	if err != nil {
		logger.Error("Failed to get latest block number for pending tx",
			zap.String("network", i.network.Name),
			zap.Error(err))
		return
	}

	blobBaseFee, err := i.ethClient.GetBlobBaseFee(i.ctx, latestBlock)
	if err != nil {
		logger.Error("Failed to get blob base fee for pending tx",
			zap.String("network", i.network.Name),
			zap.Error(err))
		return
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

	// Calculate the tip per blob gas
	maxFeePerBlobGas := tx.BlobGasFeeCap()
	tipPerBlobGas := new(big.Int).Sub(maxFeePerBlobGas, blobBaseFee)
	if tipPerBlobGas.Sign() < 0 {
		tipPerBlobGas = big.NewInt(0)
	}

	// Calculate the total cost
	blobGasUsed := tx.BlobGas()
	totalCost := new(big.Int).Mul(
		new(big.Int).Add(blobBaseFee, tipPerBlobGas),
		new(big.Int).SetUint64(blobGasUsed),
	)

	// Create the blob record
	blob := models.Blob{
		NetworkID:         i.network.ChainID,
		BlockNumber:       -1, // Pending transaction
		BlobIndex:         0,  // Placeholder
		TxHash:            hash.Hex(),
		FromAddress:       from,
		UserAttribution:   userAttribution,
		BlobSizeBytes:     int64(blobGasUsed * 128), // Approximate size
		BaseFeePerBlobGas: blobBaseFee.String(),
		TipPerBlobGas:     tipPerBlobGas.String(),
		TotalCostETH:      totalCost.String(),
		Timestamp:         time.Now(),
		Confirmed:         false,
		IndexerVersion:    i.indexerVersion,
	}

	// Insert the blob record
	if err := i.insertPendingBlob(blob); err != nil {
		logger.Error("Failed to insert pending blob record",
			zap.String("network", i.network.Name),
			zap.String("tx_hash", hash.Hex()),
			zap.Error(err))
	}
}

// blockProcessingWorker processes blocks from the task channel
func (i *Indexer) blockProcessingWorker(workerID int) {
	logger.Info("Starting block processing worker",
		zap.String("network", i.network.Name),
		zap.Int("worker_id", workerID))

	for task := range i.blockTaskCh {
		// Check if the context is cancelled
		select {
		case <-i.ctx.Done():
			logger.Info("Block processing worker stopped",
				zap.String("network", i.network.Name),
				zap.Int("worker_id", workerID))
			return
		default:
		}

		// Process the block
		if err := i.processBlock(task.BlockNumber); err != nil {
			logger.Error("Failed to process block",
				zap.String("network", i.network.Name),
				zap.Uint64("block", task.BlockNumber),
				zap.Int("worker_id", workerID),
				zap.Error(err))
			continue
		}

		// Update the last indexed block if this is higher
		i.mu.Lock()
		if task.BlockNumber > i.lastIndexedBlock {
			i.lastIndexedBlock = task.BlockNumber
			// Update the metadata with network-specific key
			if err := i.db.SetNetworkMetadata(i.ctx, i.network.ChainID, models.MetadataLastIndexedBlock, strconv.FormatUint(task.BlockNumber, 10)); err != nil {
				logger.Error("Failed to update last indexed block metadata",
					zap.String("network", i.network.Name),
					zap.Error(err))
			}
		}
		i.mu.Unlock()
	}

	logger.Info("Block processing worker exited",
		zap.String("network", i.network.Name),
		zap.Int("worker_id", workerID))
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

		// Get the latest block number
		latestBlock, err := i.ethClient.GetLatestBlockNumber(i.ctx)
		if err != nil {
			logger.Error("Failed to get latest block number",
				zap.String("network", i.network.Name),
				zap.Error(err))

			// If we're behind, don't wait for the ticker
			if currentBlock <= latestBlock {
				continue
			}

			// If we're caught up, wait for the next tick
			select {
			case <-i.ctx.Done():
				return
			case <-ticker.C:
			}
			continue
		}

		// If we're caught up, wait for the next tick
		if currentBlock > latestBlock {
			select {
			case <-i.ctx.Done():
				return
			case <-ticker.C:
			}
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

		// Queue blocks for processing
		for blockNumber := currentBlock; blockNumber <= endBlock; blockNumber++ {
			select {
			case i.blockTaskCh <- BlockTask{BlockNumber: blockNumber}:
				// Block successfully queued
			default:
				logger.Warn("Block task channel full, waiting...",
					zap.String("network", i.network.Name),
					zap.Uint64("block", blockNumber))

				// Wait for space in the channel
				i.blockTaskCh <- BlockTask{BlockNumber: blockNumber}
			}
		}

		// Update the current block
		currentBlock = endBlock + 1
	}
}

// processBlock processes a single block
func (i *Indexer) processBlock(blockNumber uint64) error {
	// Get the block
	block, err := i.ethClient.GetBlockByNumber(i.ctx, blockNumber)
	if err != nil {
		return fmt.Errorf("failed to get block %d: %w", blockNumber, err)
	}

	// Get the blob base fee for the block
	blobBaseFee, err := i.ethClient.GetBlobBaseFee(i.ctx, blockNumber)
	if err != nil {
		return fmt.Errorf("failed to get blob base fee for block %d: %w", blockNumber, err)
	}

	// Get the block timestamp
	timestamp := i.ethClient.GetBlockTimestamp(block)

	// Process each transaction in the block
	for txIndex, tx := range block.Transactions() {
		// Check if it's a blob transaction
		if !i.ethClient.IsBlobTransaction(tx) {
			continue
		}

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

		// Calculate the tip per blob gas
		maxFeePerBlobGas := tx.BlobGasFeeCap()
		tipPerBlobGas := new(big.Int).Sub(maxFeePerBlobGas, blobBaseFee)
		if tipPerBlobGas.Sign() < 0 {
			tipPerBlobGas = big.NewInt(0)
		}

		// Calculate the total cost
		blobGasUsed := tx.BlobGas()
		totalCost := new(big.Int).Mul(
			new(big.Int).Add(blobBaseFee, tipPerBlobGas),
			new(big.Int).SetUint64(blobGasUsed),
		)

		// Create the blob record
		blob := models.Blob{
			NetworkID:         i.network.ChainID,
			BlockNumber:       int64(blockNumber),
			BlobIndex:         txIndex,
			TxHash:            tx.Hash().Hex(),
			FromAddress:       from,
			UserAttribution:   userAttribution,
			BlobSizeBytes:     int64(blobGasUsed * 128), // Approximate size
			BaseFeePerBlobGas: blobBaseFee.String(),
			TipPerBlobGas:     tipPerBlobGas.String(),
			TotalCostETH:      totalCost.String(),
			Timestamp:         timestamp,
			Confirmed:         true,
			IndexerVersion:    i.indexerVersion,
		}

		// Insert the blob record
		if err := i.insertBlob(blob); err != nil {
			logger.Error("Failed to insert blob record",
				zap.String("network", i.network.Name),
				zap.String("tx_hash", tx.Hash().Hex()),
				zap.Error(err))
			continue
		}

		// Update the user's last seen timestamp
		if userAttribution != "" {
			if err := i.attribution.UpdateUserLastSeen(i.ctx, from); err != nil {
				logger.Error("Failed to update user last seen",
					zap.String("network", i.network.Name),
					zap.String("address", from),
					zap.Error(err))
			}
		}
	}

	return nil
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

// insertBlob inserts a blob record into the database
func (i *Indexer) insertBlob(blob models.Blob) error {
	query := `
		INSERT INTO blobs (
			network_id, block_number, blob_index, tx_hash, from_address, user_attribution,
			blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_eth,
			timestamp, confirmed, indexer_version
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13
		)
		ON CONFLICT (network_id, block_number, blob_index) DO UPDATE SET
			tx_hash = $4,
			from_address = $5,
			user_attribution = $6,
			blob_size_bytes = $7,
			base_fee_per_blob_gas = $8,
			tip_per_blob_gas = $9,
			total_cost_eth = $10,
			timestamp = $11,
			confirmed = $12,
			indexer_version = $13
	`

	_, err := i.db.ExecContext(i.ctx, query,
		blob.NetworkID, blob.BlockNumber, blob.BlobIndex, blob.TxHash, blob.FromAddress, blob.UserAttribution,
		blob.BlobSizeBytes, blob.BaseFeePerBlobGas, blob.TipPerBlobGas, blob.TotalCostETH,
		blob.Timestamp, blob.Confirmed, blob.IndexerVersion,
	)

	return err
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

	// Get the latest block number
	latestBlock, err := i.ethClient.GetLatestBlockNumber(i.ctx)
	if err != nil {
		return fmt.Errorf("failed to get latest block number: %w", err)
	}

	// Get the blob base fee for the latest block
	blobBaseFee, err := i.ethClient.GetBlobBaseFee(i.ctx, latestBlock)
	if err != nil {
		return fmt.Errorf("failed to get blob base fee for latest block: %w", err)
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

		// Calculate the tip per blob gas
		maxFeePerBlobGas := tx.BlobGasFeeCap()
		tipPerBlobGas := new(big.Int).Sub(maxFeePerBlobGas, blobBaseFee)
		if tipPerBlobGas.Sign() < 0 {
			tipPerBlobGas = big.NewInt(0)
		}

		// Calculate the total cost
		blobGasUsed := tx.BlobGas()
		totalCost := new(big.Int).Mul(
			new(big.Int).Add(blobBaseFee, tipPerBlobGas),
			new(big.Int).SetUint64(blobGasUsed),
		)

		// Create the blob record
		blob := models.Blob{
			NetworkID:         i.network.ChainID,
			BlockNumber:       -1, // Pending transaction
			BlobIndex:         0,  // Placeholder
			TxHash:            tx.Hash().Hex(),
			FromAddress:       from,
			UserAttribution:   userAttribution,
			BlobSizeBytes:     int64(blobGasUsed * 128), // Approximate size
			BaseFeePerBlobGas: blobBaseFee.String(),
			TipPerBlobGas:     tipPerBlobGas.String(),
			TotalCostETH:      totalCost.String(),
			Timestamp:         time.Now(),
			Confirmed:         false,
			IndexerVersion:    i.indexerVersion,
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
					indexer_version = $11
				WHERE id = $1 AND tx_hash = $2
			`
			_, err = i.db.ExecContext(i.ctx, query,
				existingID, blob.TxHash, blob.FromAddress, blob.UserAttribution,
				blob.BlobSizeBytes, blob.BaseFeePerBlobGas, blob.TipPerBlobGas, blob.TotalCostETH,
				blob.Timestamp, blob.Confirmed, blob.IndexerVersion,
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
	} else if err != sql.ErrNoRows {
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
				indexer_version = $12
			WHERE network_id = $1 AND tx_hash = $2 AND block_number < 0
		`
		_, err = i.db.ExecContext(i.ctx, query,
			blob.NetworkID, blob.TxHash, blob.BlobIndex, blob.FromAddress, blob.UserAttribution,
			blob.BlobSizeBytes, blob.BaseFeePerBlobGas, blob.TipPerBlobGas, blob.TotalCostETH,
			blob.Timestamp, blob.Confirmed, blob.IndexerVersion,
		)
	} else {
		// Insert a new record
		query := `
			INSERT INTO blobs (
				network_id, block_number, blob_index, tx_hash, from_address, user_attribution,
				blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_eth,
				timestamp, confirmed, indexer_version
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13
			)
		`
		_, err = i.db.ExecContext(i.ctx, query,
			blob.NetworkID, blob.BlockNumber, blob.BlobIndex, blob.TxHash, blob.FromAddress, blob.UserAttribution,
			blob.BlobSizeBytes, blob.BaseFeePerBlobGas, blob.TipPerBlobGas, blob.TotalCostETH,
			blob.Timestamp, blob.Confirmed, blob.IndexerVersion,
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

	// Process the block range
	return i.processBlockRange(startBlock, endBlock)
}

// GetLastIndexedBlock gets the last indexed block
func (i *Indexer) GetLastIndexedBlock() uint64 {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.lastIndexedBlock
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
func (i *Indexer) GetTopBlobUsers(ctx context.Context, limit int) ([]struct {
	Address       string    `db:"from_address"`
	Name          string    `db:"user_attribution"`
	BlobCount     int       `db:"blob_count"`
	TotalCostETH  string    `db:"total_cost_eth"`
	LastTimestamp time.Time `db:"last_timestamp"`
}, error) {
	// This is a simplified implementation
	// In a real implementation, you would filter by network ID
	return i.attribution.GetTopBlobUsers(ctx, limit)
}
