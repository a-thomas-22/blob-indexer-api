package indexer

import (
	"context"
	"fmt"
	"math/big"
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
)

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
	ctx                    context.Context
	cancel                 context.CancelFunc
	wg                     sync.WaitGroup
	lastIndexedBlock       uint64
	indexerVersion         string
	mu                     sync.Mutex
}

// New creates a new indexer
func New(ctx context.Context, db *db.DB, ethClient *ethereum.Client, cfg *config.Config, network config.NetworkConfig) *Indexer {
	indexerCtx, cancel := context.WithCancel(ctx)

	// Create attribution service and set network ID
	attributionSvc := attribution.NewService(db)
	attributionSvc.SetNetworkID(network.ChainID)

	return &Indexer{
		db:                     db,
		ethClient:              ethClient,
		attribution:            attributionSvc,
		config:                 cfg,
		network:                network,
		batchSize:              cfg.Indexer.BatchSize,
		pollingInterval:        cfg.Indexer.PollingInterval,
		mempoolPollingInterval: cfg.Indexer.MempoolPollingInterval,
		ctx:                    indexerCtx,
		cancel:                 cancel,
		indexerVersion:         cfg.Indexer.Version,
	}
}

// Start starts the indexer
func (i *Indexer) Start() error {
	logger.Info("Starting indexer...",
		zap.String("network", i.network.Name),
		zap.Int("chain_id", i.network.ChainID))

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

	// Start the block indexer
	i.wg.Add(1)
	go func() {
		defer i.wg.Done()
		i.runBlockIndexer(startBlock)
	}()

	// Start the mempool indexer
	i.wg.Add(1)
	go func() {
		defer i.wg.Done()
		i.runMempoolIndexer()
	}()

	logger.Info("Indexer started",
		zap.String("network", i.network.Name),
		zap.Uint64("start_block", startBlock))
	return nil
}

// Stop stops the indexer
func (i *Indexer) Stop() {
	logger.Info("Stopping indexer...", zap.String("network", i.network.Name))
	i.cancel()
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

			if err := i.processBlockRange(currentBlock, endBlock); err != nil {
				logger.Error("Failed to process block range",
					zap.String("network", i.network.Name),
					zap.Uint64("start_block", currentBlock),
					zap.Uint64("end_block", endBlock),
					zap.Error(err))
				// Continue with the next batch
			}

			// Update the current block
			currentBlock = endBlock + 1
		}
	}
}

// processBlockRange processes a range of blocks
func (i *Indexer) processBlockRange(startBlock, endBlock uint64) error {
	for blockNumber := startBlock; blockNumber <= endBlock; blockNumber++ {
		// Check if the context is cancelled
		select {
		case <-i.ctx.Done():
			return i.ctx.Err()
		default:
		}

		// Process the block
		if err := i.processBlock(blockNumber); err != nil {
			logger.Error("Failed to process block",
				zap.String("network", i.network.Name),
				zap.Uint64("block", blockNumber),
				zap.Error(err))
			// Continue with the next block
			continue
		}

		// Update the last indexed block
		i.mu.Lock()
		i.lastIndexedBlock = blockNumber
		i.mu.Unlock()

		// Update the metadata with network-specific key
		if err := i.db.SetNetworkMetadata(i.ctx, i.network.ChainID, models.MetadataLastIndexedBlock, strconv.FormatUint(blockNumber, 10)); err != nil {
			logger.Error("Failed to update last indexed block metadata",
				zap.String("network", i.network.Name),
				zap.Error(err))
		}
	}

	return nil
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
	query := `
		INSERT INTO blobs (
			network_id, block_number, blob_index, tx_hash, from_address, user_attribution,
			blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_eth,
			timestamp, confirmed, indexer_version
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13
		)
		ON CONFLICT (network_id, tx_hash) DO UPDATE SET
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
