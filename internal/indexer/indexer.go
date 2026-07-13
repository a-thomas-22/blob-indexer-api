package indexer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	geth "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/misc/eip4844"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
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

	// maxGapScanRetries is the normal retry budget before a block moves to slow safety-net retries.
	maxGapScanRetries = 10

	// failedBlockSafetyRetryInterval keeps failed blocks recoverable without retrying them every scan forever.
	failedBlockSafetyRetryInterval = time.Hour

	// reindexRequestPollInterval controls how often the indexer checks for
	// operator-requested reindex ranges in block_reindex_requests.
	reindexRequestPollInterval = 30 * time.Second

	// reindexRequestCompletionPollInterval controls how often a queued reindex
	// request is checked for completion in indexed_blocks.
	reindexRequestCompletionPollInterval = 5 * time.Second

	// reindexRequestStaleAfter allows a new pod to reclaim a request that was
	// left processing by a crashed or force-deleted indexer.
	reindexRequestStaleAfter = 15 * time.Minute

	// bytesPerBlob is the on-wire blob size in bytes (EIP-4844 fixes blobs at
	// 4096 field elements of 32 bytes each = 131072 bytes). It is numerically
	// equal to params.BlobTxBlobGasPerBlob but expresses a byte quantity, not
	// a gas amount — keep the two units separate so a future protocol change
	// to one does not silently corrupt the other.
	bytesPerBlob = 4096 * 32

	// pendingTxProcessingQueueSize keeps websocket subscription reads fast. The
	// subscription emits every pending tx hash, but only a tiny fraction are blob
	// transactions; when processing falls behind, dropping hashes is preferable
	// to letting the RPC subscription queue overflow and die.
	pendingTxProcessingQueueSize = 8192

	// pendingTxResubscribeMaxAttempts bounds how many times we retry the pending
	// transaction websocket resubscribe before giving up and falling back to
	// polling. A transient RPC blip should recover via retry; a persistent
	// failure must not spin forever.
	pendingTxResubscribeMaxAttempts = 5

	// pendingTxResubscribeBaseBackoff is the initial delay between resubscribe
	// attempts. Each subsequent attempt doubles the delay (capped at
	// pendingTxResubscribeMaxBackoff) so we back off a struggling RPC endpoint.
	pendingTxResubscribeBaseBackoff = 500 * time.Millisecond

	// pendingTxResubscribeMaxBackoff caps the exponential backoff between
	// resubscribe attempts.
	pendingTxResubscribeMaxBackoff = 8 * time.Second

	// blobReplacementRetention bounds how long blob_replacements rows are
	// kept; pruned on the mempool cleanup ticker. A week keeps the log
	// useful for resolving a stale hash pasted days later while bounding
	// the table to a trickle of rows (fee bumps are occasional events).
	blobReplacementRetention = 7 * 24 * time.Hour

	// defaultMempoolReconcileInterval paces the slow reconciliation poll
	// that runs alongside the websocket pending-tx subscription. The
	// subscription announces a tx only once, on entry to the pool, so a
	// WS-mode row's timestamp is its first-seen time and the TTL sweep
	// would purge a still-pending tx after mempool_ttl. Re-upserting the
	// pool's blob txs refreshes their timestamps, making the sweep mean
	// "gone from the pool for mempool_ttl" in both modes. Slower than the
	// fallback poll: the subscription already delivers additions instantly,
	// so this only needs to outpace the TTL by a wide margin.
	defaultMempoolReconcileInterval = 2 * time.Minute
)

// errReorgDetected is returned when a chain reorganization is detected and handled
var errReorgDetected = errors.New("chain reorganization detected")

// errStaleBlockFetch is returned when a block's RPC fetch predates a reorg or
// reindex cleanup that committed while the fetch was in flight. The data in
// hand may describe the abandoned fork, so the insert is refused; the worker
// retry loop re-runs processBlock, which refetches the now-canonical block.
var errStaleBlockFetch = errors.New("block fetched before reorg cleanup")

// BlockTask represents a task to process a block
type BlockTask struct {
	BlockNumber uint64
}

type indexerMetadataRow struct {
	Key   string `db:"key"`
	Value string `db:"value"`
}

type backfillCursorState struct {
	active       bool
	activeSet    bool
	currentBlock *uint64
	targetBlock  *uint64
}

type blockReindexRequest struct {
	ID         int64  `db:"id"`
	ChainID    int    `db:"chain_id"`
	StartBlock uint64 `db:"start_block"`
	EndBlock   uint64 `db:"end_block"`
	Attempts   int    `db:"attempts"`
	ClaimedBy  string `db:"claimed_by"`
}

type blobMetrics struct {
	blobSizeBytes     int64
	baseFeePerBlobGas string
	tipPerBlobGas     string
	totalCostETH      string
	maxFeePerBlobGas  *string
	blobGasUsed       *int64
}

// calculateBlobMetrics returns per-blob metric values for any blob carried by
// tx, evaluated against blobBaseFee. Every blob in an EIP-4844 transaction
// consumes the same blob gas and is charged the same blob base fee, so callers
// emit one Blob row per BlobHashes() entry using these identical values.
func calculateBlobMetrics(tx *types.Transaction, blobBaseFee *big.Int) blobMetrics {
	maxFeePerBlobGas := tx.BlobGasFeeCap()
	tipPerBlobGas := new(big.Int).Sub(maxFeePerBlobGas, blobBaseFee)
	if tipPerBlobGas.Sign() < 0 {
		tipPerBlobGas = big.NewInt(0)
	}

	totalCost := new(big.Int).Mul(blobBaseFee, new(big.Int).SetUint64(params.BlobTxBlobGasPerBlob))

	maxFeeStr := maxFeePerBlobGas.String()
	blobGasUsedInt := int64(params.BlobTxBlobGasPerBlob)

	return blobMetrics{
		blobSizeBytes:     int64(bytesPerBlob),
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
	mempoolTTL             time.Duration
	mempoolCleanupInterval time.Duration
	// mempoolReconcileInterval paces the WS-mode reconciliation poll; a
	// field so tests can shrink it. Defaults to
	// defaultMempoolReconcileInterval.
	mempoolReconcileInterval time.Duration
	workerCount              int
	maxBlockRetries          int
	gapScanInterval          time.Duration
	maxReorgDepth            int
	startupGapScanBlocks     int
	// pendingTxResubBaseBackoff is the initial resubscribe backoff; a field so
	// tests can shrink it. Defaults to pendingTxResubscribeBaseBackoff.
	pendingTxResubBaseBackoff time.Duration
	// fineRollupPruneInterval is how often expired fine chart rollup buckets
	// are pruned; a field so tests can shrink it. Defaults to
	// defaultFineRollupPruneInterval.
	fineRollupPruneInterval time.Duration
	ctx                     context.Context
	cancel                  context.CancelFunc
	wg                      sync.WaitGroup
	lastIndexedBlock        uint64 // accessed with sync/atomic
	indexerVersion          string
	mu                      sync.Mutex // protects DB metadata writes
	dbWriteMu               sync.Mutex // serializes same-network writes that fire summary rollup triggers
	blockTaskCh             chan BlockTask
	useWebsocket            bool
	blockSub                *ethereum.BlockSubscription
	pendingTxSub            *ethereum.PendingTxSubscription
	mempoolPollingStarted   uint32
	failedBlocks            map[uint64]int // block number -> cumulative failure count
	failedBlockNextRetry    map[uint64]time.Time
	failedBlocksMu          sync.Mutex
	reorgDetected           uint32 // atomic flag: 1 = reorg detected, main loop should reset
	// reorgRangeMu guards reorgRewindFrom/reorgInvalidatedThrough, which are
	// only meaningful while reorgDetected == 1. Reorgs signaled before the main
	// loop consumes the flag merge into the widest invalidated range.
	reorgRangeMu            sync.Mutex
	reorgRewindFrom         uint64 // first invalidated block (fork point + 1)
	reorgInvalidatedThrough uint64 // highest block deleted by the reorg
	// reorgRecoverySignaled records whether any reorg recovery signal has been
	// raised in this process (atomic; 1 = signaled). It distinguishes "the
	// persisted marker's range is already queued here" from "the marker was
	// only recovered from disk and its signal was lost": startup seeding is
	// best-effort, so after a failed marker read the full marker range must be
	// (re-)signaled — by the next reorg or the gap scanner — or it would stay
	// inert until the next restart.
	reorgRecoverySignaled uint32
	// reorgEpoch counts destructive block-range cleanups (reorg rewinds,
	// reindex deletes). processBlock samples it before fetching a block via
	// RPC and insertBlockData rejects the insert if it changed: a worker
	// holding a block fetched before the cleanup's DELETEs committed would
	// otherwise re-insert abandoned-fork data, and checkForReorg cannot catch
	// that (the deleted parent row reads as a benign gap). Incremented under
	// dbWriteMu; accessed atomically because the sample happens outside it.
	reorgEpoch  uint64
	chainConfig *params.ChainConfig // go-ethereum chain config for fork-aware blob math
}

// New creates a new indexer
func New(ctx context.Context, database *db.DB, ethClient *ethereum.Client, cfg *config.Config, network config.NetworkConfig) *Indexer {
	indexerCtx, cancel := context.WithCancel(ctx)

	// Create attribution service with network ID context.
	attributionSvc := attribution.NewService(database, network.ChainID)
	attributionSvc.ConfigureBlobList(attribution.BlobListConfig{
		Enabled:         cfg.Attribution.BlobListEnabled,
		BaseURL:         cfg.Attribution.BlobListBaseURL,
		RefreshInterval: cfg.Attribution.BlobListRefreshInterval,
		RequestTimeout:  cfg.Attribution.BlobListRequestTimeout,
	})

	// Determine the number of workers. The configured/default value is honored
	// directly so pods do not silently fan out based on node CPU count.
	workerCount := cfg.Indexer.WorkerCount
	if workerCount <= 0 {
		workerCount = DefaultWorkerCount
	}

	// Check if the client supports websockets
	useWebsocket := ethClient.IsWebsocket()

	return &Indexer{
		db:                        database,
		ethClient:                 ethClient,
		attribution:               attributionSvc,
		config:                    cfg,
		network:                   network,
		batchSize:                 cfg.Indexer.BatchSize,
		pollingInterval:           cfg.Indexer.PollingInterval,
		mempoolPollingInterval:    cfg.Indexer.MempoolPollingInterval,
		mempoolTTL:                cfg.Indexer.MempoolTTL,
		mempoolCleanupInterval:    cfg.Indexer.MempoolCleanupInterval,
		mempoolReconcileInterval:  defaultMempoolReconcileInterval,
		workerCount:               workerCount,
		maxBlockRetries:           cfg.Indexer.MaxBlockRetries,
		gapScanInterval:           cfg.Indexer.GapScanInterval,
		maxReorgDepth:             cfg.Indexer.MaxReorgDepth,
		startupGapScanBlocks:      cfg.Indexer.StartupGapScanBlocks,
		pendingTxResubBaseBackoff: pendingTxResubscribeBaseBackoff,
		fineRollupPruneInterval:   defaultFineRollupPruneInterval,
		ctx:                       indexerCtx,
		cancel:                    cancel,
		indexerVersion:            cfg.Indexer.Version,
		blockTaskCh:               make(chan BlockTask, 1000), // Buffer for block tasks
		useWebsocket:              useWebsocket,
		failedBlocks:              make(map[uint64]int),
		failedBlockNextRetry:      make(map[uint64]time.Time),
		chainConfig:               blobparams.ChainConfigForID(network.ChainID),
	}
}

func (i *Indexer) getBlobBaseFeeFromBlock(block *types.Block) *big.Int {
	header := block.Header()
	if header.ExcessBlobGas != nil {
		return eip4844.CalcBlobFee(i.chainConfig, header)
	}
	return big.NewInt(1)
}

type metadataUpdate struct {
	key   string
	value string
}

func (i *Indexer) setNetworkMetadataValues(updates ...metadataUpdate) {
	if i.db == nil {
		return
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	i.setNetworkMetadataValuesLocked(updates...)
}

func (i *Indexer) setNetworkMetadataValuesLocked(updates ...metadataUpdate) {
	if i.db == nil || len(updates) == 0 {
		return
	}

	entries := make([]db.MetadataKV, len(updates))
	keys := make([]string, len(updates))
	for idx, update := range updates {
		entries[idx] = db.MetadataKV{Key: update.key, Value: update.value}
		keys[idx] = update.key
	}
	if err := i.db.SetNetworkMetadataBatch(i.ctx, i.network.ChainID, entries); err != nil {
		logger.Error("Failed to update indexer metadata",
			zap.String("network", i.network.Name),
			zap.Strings("metadata_keys", keys),
			zap.Error(err))
	}
}

func (i *Indexer) lockDBWrites() func() {
	i.dbWriteMu.Lock()
	return i.dbWriteMu.Unlock
}

func (i *Indexer) updateCurrentChainHead(blockNumber uint64, observedAt time.Time) {
	i.setNetworkMetadataValues(
		metadataUpdate{key: models.MetadataCurrentChainHead, value: strconv.FormatUint(blockNumber, 10)},
		metadataUpdate{key: models.MetadataChainHeadUpdatedAt, value: models.FormatMetadataTimestamp(observedAt)},
	)
}

func (i *Indexer) updateWebSocketFreshness(observedAt time.Time) {
	i.setNetworkMetadataValues(
		metadataUpdate{key: models.MetadataWebSocketFreshnessAt, value: models.FormatMetadataTimestamp(observedAt)},
	)
}

func (i *Indexer) updateBackfillStatus(active bool, startBlock, currentBlock, targetBlock uint64, observedAt time.Time) {
	updates := []metadataUpdate{
		{key: models.MetadataBackfillActive, value: strconv.FormatBool(active)},
		{key: models.MetadataBackfillStartBlock, value: strconv.FormatUint(startBlock, 10)},
		{key: models.MetadataBackfillCurrentBlock, value: strconv.FormatUint(currentBlock, 10)},
		{key: models.MetadataBackfillTargetBlock, value: strconv.FormatUint(targetBlock, 10)},
		{key: models.MetadataBackfillUpdatedAt, value: models.FormatMetadataTimestamp(observedAt)},
	}
	if !active {
		updates = append(updates, metadataUpdate{key: models.MetadataBackfillCompletedAt, value: models.FormatMetadataTimestamp(observedAt)})
	}
	i.setNetworkMetadataValues(updates...)
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

	// Parallel workers commit out of order, so a crash can persist a
	// last_indexed_block watermark above blocks that never committed. When
	// the resume point skips past the watermark those blocks would stay
	// orphaned forever — the gap scanner only tracks in-memory failures. A
	// resume at or below the watermark (active backfill) re-walks through any
	// gaps on its own.
	if lastBlock > 0 && startBlock > lastBlock {
		i.seedStartupGapRecovery(lastBlock)
	}

	// A reorg persisted its invalidated range but the process died before the
	// re-queued blocks committed. The startup gap scan above cannot see this
	// hole: it only looks below the watermark, and the reorg rewound the
	// watermark to the fork point — the deleted range sits entirely above it.
	i.seedReorgRecoveryFromMarker()

	// Pending rows live in mempool_blobs; purge any legacy block_number < 0
	// sentinel rows an old binary may have written into blobs between the
	// mempool_blobs migration and this binary taking over. One-shot and
	// best-effort: leftovers only skew blob_user_stats slightly.
	i.cleanupLegacyPendingBlobs()

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

	// Start the manual reindex request scanner.
	i.wg.Add(1)
	go func() {
		defer i.wg.Done()
		i.runReindexRequestScanner()
	}()

	// If websocket is available, subscribe to new blocks and pending transactions
	if i.useWebsocket {
		// Subscribe to new blocks
		blockSub, err := i.subscribeToNewBlocks()
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
		pendingTxSub, err := i.subscribeToPendingTransactions()
		if err != nil {
			logger.Warn("Failed to subscribe to pending transactions, falling back to polling",
				zap.String("network", i.network.Name),
				zap.Error(err))

			i.startMempoolIndexer()
		} else {
			i.pendingTxSub = pendingTxSub
			i.wg.Add(1)
			go func() {
				defer i.wg.Done()
				i.handlePendingTransactionSubscription()
			}()
			logger.Info("Subscribed to pending transactions via websocket",
				zap.String("network", i.network.Name))

			// Slow reconciliation poll: the subscription announces a tx
			// only once, so without this a still-pending blob's row keeps
			// its first-seen timestamp and the TTL sweep purges it from
			// the pending view after mempool_ttl while it is still in the
			// node's pool. Re-upserting refreshes timestamps so the sweep
			// only reaps txs that actually left the pool. If the
			// subscription later dies and the fast polling fallback
			// starts, both tickers run the same idempotent poll — wasteful
			// only, never wrong.
			if i.mempoolReconcileInterval > 0 {
				i.wg.Add(1)
				go func() {
					defer i.wg.Done()
					i.runMempoolReconciler()
				}()
			}
		}
	} else {
		i.startMempoolIndexer()
	}

	// Backfill fine chart rollups for the retention window, then prune expired
	// fine buckets periodically.
	if i.db != nil && i.fineRollupPruneInterval > 0 {
		i.wg.Add(1)
		go func() {
			defer i.wg.Done()
			i.runFineRollupMaintenance()
		}()
	}

	// Start periodic cleanup of stale pending blobs
	if i.mempoolTTL > 0 && i.mempoolCleanupInterval > 0 {
		i.wg.Add(1)
		go func() {
			defer i.wg.Done()
			i.runMempoolCleanup()
		}()
	}

	logger.Info("Indexer started",
		zap.String("network", i.network.Name),
		zap.Uint64("start_block", startBlock))
	return nil
}

func (i *Indexer) subscribeToNewBlocks() (*ethereum.BlockSubscription, error) {
	return i.ethClient.SubscribeToNewHeads(i.ctx, fmt.Sprintf("indexer-%s", i.network.Name))
}

func (i *Indexer) subscribeToPendingTransactions() (*ethereum.PendingTxSubscription, error) {
	return i.ethClient.SubscribeToPendingTransactions(i.ctx, fmt.Sprintf("indexer-%s", i.network.Name))
}

func (i *Indexer) resubscribeToPendingTransactions() (*ethereum.PendingTxSubscription, error) {
	return i.ethClient.ResubscribeToPendingTransactions(i.ctx, fmt.Sprintf("indexer-%s", i.network.Name))
}

func (i *Indexer) resubscribeToNewBlocks() (*ethereum.BlockSubscription, error) {
	return i.ethClient.ResubscribeToNewHeads(i.ctx, fmt.Sprintf("indexer-%s", i.network.Name))
}

// resubscribeToNewBlocksWithBackoff retries the new-heads websocket resubscribe
// with the same retry budget as the pending-transaction path. It returns the
// live subscription on success, or the last error once the attempt budget is
// exhausted (signaling the caller to fall back to batch polling).
func (i *Indexer) resubscribeToNewBlocksWithBackoff() (*ethereum.BlockSubscription, error) {
	var lastErr error
	backoff := i.pendingTxResubBaseBackoff
	if backoff <= 0 {
		backoff = pendingTxResubscribeBaseBackoff
	}
	for attempt := 1; attempt <= pendingTxResubscribeMaxAttempts; attempt++ {
		if i.ctx.Err() != nil {
			return nil, i.ctx.Err()
		}

		sub, err := i.resubscribeToNewBlocks()
		if err == nil {
			if attempt > 1 {
				logger.Info("Recovered new-heads subscription after retry",
					zap.String("event", "new_heads_resubscribe_recovered"),
					zap.String("network", i.network.Name),
					zap.Int("attempt", attempt))
			}
			return sub, nil
		}
		lastErr = err

		logger.Warn("New-heads resubscribe attempt failed",
			zap.String("event", "new_heads_resubscribe_retry"),
			zap.String("network", i.network.Name),
			zap.Int("attempt", attempt),
			zap.Int("max_attempts", pendingTxResubscribeMaxAttempts),
			zap.Error(err))

		if attempt == pendingTxResubscribeMaxAttempts {
			break
		}

		select {
		case <-i.ctx.Done():
			return nil, i.ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > pendingTxResubscribeMaxBackoff {
			backoff = pendingTxResubscribeMaxBackoff
		}
	}
	return nil, fmt.Errorf("new-heads resubscribe exhausted %d attempts: %w", pendingTxResubscribeMaxAttempts, lastErr)
}

// resubscribeToPendingTransactionsWithBackoff retries the pending-transaction
// websocket resubscribe up to pendingTxResubscribeMaxAttempts times with
// exponential backoff. It returns the live subscription on success, or the last
// error once the attempt budget is exhausted (signaling the caller to fall
// back to polling). A canceled context aborts immediately.
func (i *Indexer) resubscribeToPendingTransactionsWithBackoff() (*ethereum.PendingTxSubscription, error) {
	var lastErr error
	backoff := i.pendingTxResubBaseBackoff
	if backoff <= 0 {
		backoff = pendingTxResubscribeBaseBackoff
	}
	for attempt := 1; attempt <= pendingTxResubscribeMaxAttempts; attempt++ {
		if i.ctx.Err() != nil {
			return nil, i.ctx.Err()
		}

		sub, err := i.resubscribeToPendingTransactions()
		if err == nil {
			if attempt > 1 {
				logger.Info("Recovered pending transaction subscription after retry",
					zap.String("event", "pending_tx_resubscribe_recovered"),
					zap.String("network", i.network.Name),
					zap.Int("attempt", attempt))
			}
			return sub, nil
		}
		lastErr = err

		logger.Warn("Pending transaction resubscribe attempt failed",
			zap.String("event", "pending_tx_resubscribe_retry"),
			zap.String("network", i.network.Name),
			zap.Int("attempt", attempt),
			zap.Int("max_attempts", pendingTxResubscribeMaxAttempts),
			zap.Error(err))

		if attempt == pendingTxResubscribeMaxAttempts {
			break
		}

		select {
		case <-i.ctx.Done():
			return nil, i.ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > pendingTxResubscribeMaxBackoff {
			backoff = pendingTxResubscribeMaxBackoff
		}
	}

	return nil, fmt.Errorf("pending transaction resubscribe exhausted %d attempts: %w", pendingTxResubscribeMaxAttempts, lastErr)
}

func (i *Indexer) unsubscribeFromPendingTransactions() {
	if i.ethClient == nil {
		return
	}
	i.ethClient.UnsubscribeFromPendingTransactions(fmt.Sprintf("indexer-%s", i.network.Name))
}

func (i *Indexer) startMempoolIndexer() {
	if !atomic.CompareAndSwapUint32(&i.mempoolPollingStarted, 0, 1) {
		return
	}

	i.wg.Add(1)
	go func() {
		defer i.wg.Done()
		i.runMempoolIndexer()
	}()
}

// Stop stops the indexer
func (i *Indexer) Stop() {
	logger.Info("Stopping indexer...", zap.String("network", i.network.Name))

	// Cancel the context to signal all goroutines to stop
	i.cancel()

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
	lastBlock := atomic.LoadUint64(&i.lastIndexedBlock)

	if i.network.StartBlock != "" {
		configuredStart, targetBlock, targetKnown, err := i.resolveConfiguredStartBlock()
		if err != nil {
			return 0, err
		}

		if lastBlock > 0 {
			resumeBlock, ok, err := i.backfillResumeBlock(configuredStart, targetBlock, targetKnown)
			if err != nil {
				return 0, err
			}
			if ok {
				return resumeBlock, nil
			}

			nextBlock := lastBlock + 1
			if nextBlock < configuredStart {
				return configuredStart, nil
			}
			return nextBlock, nil
		}

		return configuredStart, nil
	}

	// If we have a last indexed block, start from the next block
	if lastBlock > 0 {
		return lastBlock + 1, nil
	}

	// Otherwise, start from the EIP-4844 activation block (this is a placeholder)
	// In a real implementation, you would use the actual EIP-4844 activation block
	return 0, nil
}

func (i *Indexer) resolveConfiguredStartBlock() (startBlock, targetBlock uint64, targetKnown bool, err error) {
	if strings.HasPrefix(i.network.StartBlock, "LATEST") {
		latestBlock, err := i.ethClient.GetLatestBlockNumber(i.ctx)
		if err != nil {
			return 0, 0, false, fmt.Errorf("failed to get latest block number: %w", err)
		}
		i.updateCurrentChainHead(latestBlock, time.Now())

		parts := strings.Split(i.network.StartBlock, "-")
		if len(parts) == 2 {
			offset, err := strconv.ParseUint(parts[1], 10, 64)
			if err != nil {
				return 0, 0, false, fmt.Errorf("failed to parse offset in StartBlock: %w", err)
			}

			if offset > latestBlock {
				return 0, latestBlock, true, nil
			}
			return latestBlock - offset, latestBlock, true, nil
		}

		return latestBlock, latestBlock, true, nil
	}

	blockNumber, err := strconv.ParseUint(i.network.StartBlock, 10, 64)
	if err != nil {
		return 0, 0, false, fmt.Errorf("failed to parse StartBlock: %w", err)
	}
	return blockNumber, 0, false, nil
}

func (i *Indexer) backfillResumeBlock(configuredStart, targetBlock uint64, targetKnown bool) (resumeBlock uint64, ok bool, err error) {
	if i.db == nil {
		return 0, false, nil
	}

	state, err := i.getBackfillCursorState()
	if err != nil {
		return 0, false, err
	}
	if !state.activeSet || !state.active {
		return 0, false, nil
	}

	if state.currentBlock != nil {
		if *state.currentBlock < configuredStart {
			logger.Info("Resuming active backfill from configured start",
				zap.String("network", i.network.Name),
				zap.Uint64("configured_start_block", configuredStart),
				zap.Uint64("backfill_current_block", *state.currentBlock))
			return configuredStart, true, nil
		}

		// The cursor is only a hint, not the source of truth: a tip-gap
		// catch-up shares these metadata keys with the historical backfill and
		// can overwrite the cursor with a tip-region block while the range
		// below is still unindexed. Trusting cursor+1 here would silently
		// orphan that range, so resume from the first indexed_blocks gap at or
		// above the configured start instead. When coverage below the cursor
		// is complete this returns cursor+1, matching the old fast path.
		resumeBlock, err = i.db.GetFirstUnindexedBlock(i.ctx, i.network.ChainID, configuredStart, *state.currentBlock)
		if err != nil {
			return 0, false, err
		}
		if resumeBlock <= *state.currentBlock {
			logger.Warn("Backfill cursor is ahead of indexed coverage, resuming from first unindexed block",
				zap.String("network", i.network.Name),
				zap.Uint64("configured_start_block", configuredStart),
				zap.Uint64("backfill_current_block", *state.currentBlock),
				zap.Uint64("resume_block", resumeBlock))
		} else {
			logger.Info("Resuming active backfill from metadata cursor",
				zap.String("network", i.network.Name),
				zap.Uint64("backfill_current_block", *state.currentBlock),
				zap.Uint64("resume_block", resumeBlock))
		}
		return resumeBlock, true, nil
	}

	if state.targetBlock != nil {
		targetBlock = *state.targetBlock
		targetKnown = true
	}
	if !targetKnown {
		latestBlock, err := i.ethClient.GetLatestBlockNumber(i.ctx)
		if err != nil {
			return 0, false, fmt.Errorf("failed to get latest block number for backfill resume: %w", err)
		}
		targetBlock = latestBlock
		i.updateCurrentChainHead(latestBlock, time.Now())
	}
	if configuredStart > targetBlock {
		return targetBlock + 1, true, nil
	}

	resumeBlock, err = i.db.GetFirstUnindexedBlock(i.ctx, i.network.ChainID, configuredStart, targetBlock)
	if err != nil {
		return 0, false, err
	}
	logger.Info("Resuming active backfill from first unindexed block",
		zap.String("network", i.network.Name),
		zap.Uint64("configured_start_block", configuredStart),
		zap.Uint64("target_block", targetBlock),
		zap.Uint64("resume_block", resumeBlock))
	return resumeBlock, true, nil
}

func (i *Indexer) getBackfillCursorState() (backfillCursorState, error) {
	var rows []indexerMetadataRow
	query := `
		SELECT key, value
		FROM indexer_metadata
		WHERE chain_id = $1
			AND key IN ($2, $3, $4)
	`
	if err := i.db.SelectContext(
		i.ctx,
		&rows,
		query,
		i.network.ChainID,
		models.MetadataBackfillActive,
		models.MetadataBackfillCurrentBlock,
		models.MetadataBackfillTargetBlock,
	); err != nil {
		return backfillCursorState{}, fmt.Errorf("failed to get backfill cursor metadata: %w", err)
	}

	var state backfillCursorState
	for _, row := range rows {
		switch row.Key {
		case models.MetadataBackfillActive:
			active, err := strconv.ParseBool(row.Value)
			if err != nil {
				return backfillCursorState{}, fmt.Errorf("failed to parse backfill active metadata: %w", err)
			}
			state.active = active
			state.activeSet = true
		case models.MetadataBackfillCurrentBlock:
			block, err := strconv.ParseUint(row.Value, 10, 64)
			if err != nil {
				return backfillCursorState{}, fmt.Errorf("failed to parse backfill current block metadata: %w", err)
			}
			state.currentBlock = &block
		case models.MetadataBackfillTargetBlock:
			block, err := strconv.ParseUint(row.Value, 10, 64)
			if err != nil {
				return backfillCursorState{}, fmt.Errorf("failed to parse backfill target block metadata: %w", err)
			}
			state.targetBlock = &block
		}
	}

	return state, nil
}

// blockProcessingWorker processes blocks from the task channel with inline retries
func (i *Indexer) blockProcessingWorker(workerID int) {
	logger.Info("Starting block processing worker",
		zap.String("network", i.network.Name),
		zap.Int("worker_id", workerID))

	for {
		select {
		case <-i.ctx.Done():
			logger.Info("Block processing worker stopped",
				zap.String("network", i.network.Name),
				zap.Int("worker_id", workerID))
			return
		case task := <-i.blockTaskCh:
			func(task BlockTask) {
				defer func() {
					if recovered := recover(); recovered != nil {
						logger.Error("Recovered panic in block processing worker",
							zap.String("network", i.network.Name),
							zap.Int("worker_id", workerID),
							zap.Uint64("block", task.BlockNumber),
							zap.Any("panic", recovered))
						i.trackFailedBlock(task.BlockNumber)
					}
				}()

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

					// Reorg was detected and handled — don't retry, the main loop
					// re-queues the invalidated range (which includes this block).
					// The block was never inserted, so bail out without advancing
					// the watermark: persisting an uninserted block as the
					// high-water mark leaves a hole on crash and lets the
					// WebSocket follower skip the rewound range.
					if errors.Is(lastErr, errReorgDetected) {
						return
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
					return
				}

				// Clear from failed blocks tracking on success
				i.failedBlocksMu.Lock()
				delete(i.failedBlocks, task.BlockNumber)
				delete(i.failedBlockNextRetry, task.BlockNumber)
				i.failedBlocksMu.Unlock()

				// Update the last indexed block
				i.updateLastIndexedBlock(task.BlockNumber)
			}(task)
		}
	}
}

// updateLastIndexedBlock atomically updates the last indexed block and persists to DB
func (i *Indexer) updateLastIndexedBlock(blockNumber uint64) {
	for {
		current := atomic.LoadUint64(&i.lastIndexedBlock)
		if blockNumber <= current {
			return
		}
		if atomic.CompareAndSwapUint64(&i.lastIndexedBlock, current, blockNumber) {
			i.mu.Lock()

			if atomic.LoadUint64(&i.lastIndexedBlock) != blockNumber {
				i.mu.Unlock()
				return
			}

			i.setNetworkMetadataValuesLocked(
				metadataUpdate{key: models.MetadataLastIndexedBlock, value: strconv.FormatUint(blockNumber, 10)},
				metadataUpdate{key: models.MetadataLastIndexedAt, value: models.FormatMetadataTimestamp(time.Now())},
			)
			i.mu.Unlock()
			return
		}
	}
}

// trackFailedBlock records a block that failed processing for later retry by the gap scanner
func (i *Indexer) trackFailedBlock(blockNumber uint64) {
	i.failedBlocksMu.Lock()
	if i.failedBlocks == nil {
		i.failedBlocks = make(map[uint64]int)
	}
	i.failedBlocks[blockNumber]++
	i.failedBlocksMu.Unlock()
}

// seedStartupGapRecovery hands blocks orphaned below the persisted
// last_indexed_block watermark to the gap scanner. Parallel workers commit
// out of order, so a crash can leave the watermark above blocks that never
// committed; a steady-state resume from watermark+1 would skip them forever.
// Only a bounded recent window is scanned: crash fallout sits close to the
// watermark (task channel capacity plus lingering failed-block retries),
// while older gaps are operator territory (manual reindex requests).
func (i *Indexer) seedStartupGapRecovery(lastBlock uint64) {
	if i.db == nil || i.startupGapScanBlocks <= 0 {
		return
	}

	window := uint64(i.startupGapScanBlocks)
	var windowStart uint64
	if lastBlock >= window {
		windowStart = lastBlock - window + 1
	}

	// When the intended start of coverage is knowable, gaps right down to it
	// are real — a bootstrap crash can persist a watermark before any lower
	// block commits, leaving no indexed row at the start. Only LATEST-style
	// start blocks fall back to flooring at the earliest indexed row, because
	// the tip they resolved to at first boot is not persisted anywhere.
	floorAtEarliestIndexed := true
	if i.network.StartBlock == "" {
		floorAtEarliestIndexed = false
	} else if configuredStart, parseErr := strconv.ParseUint(i.network.StartBlock, 10, 64); parseErr == nil {
		floorAtEarliestIndexed = false
		if configuredStart > windowStart {
			windowStart = configuredStart
		}
	}

	missing, err := i.db.GetUnindexedBlocksInRange(i.ctx, i.network.ChainID, windowStart, lastBlock, i.startupGapScanBlocks, floorAtEarliestIndexed)
	if err != nil {
		// Best-effort: startup must not fail on a recovery scan. The blocks
		// stay orphaned exactly as they would without it.
		logger.Error("Failed to scan for blocks orphaned below the watermark",
			zap.String("network", i.network.Name),
			zap.Uint64("window_start", windowStart),
			zap.Uint64("last_indexed_block", lastBlock),
			zap.Error(err))
		return
	}
	if len(missing) == 0 {
		return
	}

	logger.Warn("Recovering blocks orphaned below the persisted watermark",
		zap.String("event", "startup_gap_recovery"),
		zap.String("network", i.network.Name),
		zap.Int("chain_id", i.network.ChainID),
		zap.Int("count", len(missing)),
		zap.Uint64("first_block", missing[0]),
		zap.Uint64("last_block", missing[len(missing)-1]),
		zap.Uint64("watermark", lastBlock))
	for _, blockNumber := range missing {
		i.trackFailedBlock(blockNumber)
	}
}

// backfillRangeFullyIndexed reports whether indexed_blocks covers
// [startBlock, endBlock] with no gaps. Errors count as not covered: staying
// backfill_active=true is the safe state — it keeps the coverage-verified
// resume path armed across restarts.
func (i *Indexer) backfillRangeFullyIndexed(startBlock, endBlock uint64) bool {
	if i.db == nil {
		return true
	}

	firstGap, err := i.db.GetFirstUnindexedBlock(i.ctx, i.network.ChainID, startBlock, endBlock)
	if err != nil {
		logger.Error("Failed to verify backfill coverage, keeping backfill active",
			zap.String("network", i.network.Name),
			zap.Uint64("start_block", startBlock),
			zap.Uint64("end_block", endBlock),
			zap.Error(err))
		return false
	}
	if firstGap <= endBlock {
		logger.Info("Backfill range not fully indexed yet, deferring completion",
			zap.String("network", i.network.Name),
			zap.Uint64("start_block", startBlock),
			zap.Uint64("end_block", endBlock),
			zap.Uint64("first_unindexed_block", firstGap))
		return false
	}
	return true
}

// runBlockIndexer runs the block indexer
func (i *Indexer) runBlockIndexer(startBlock uint64) {
	logger.Info("Block indexer starting",
		zap.String("network", i.network.Name),
		zap.Uint64("start_block", startBlock))

	currentBlock := startBlock
	var backfillStartBlock uint64
	backfillActive := false
	// Throttles repeated coverage scans while a completion attempt is deferred
	// by in-flight or failed blocks: the phase range can span millions of rows
	// right after a historical walk, so don't re-scan it every tick.
	var nextCompletionCheck time.Time
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

		// Check if a reorg was handled and we need to re-index invalidated blocks
		if atomic.LoadUint32(&i.reorgDetected) == 1 {
			rewindFrom, invalidatedThrough := i.consumeReorgReset()
			if rewindFrom <= currentBlock {
				logger.Info("Resetting indexer position after chain reorganization",
					zap.String("network", i.network.Name),
					zap.Uint64("old_position", currentBlock),
					zap.Uint64("new_position", rewindFrom))
				currentBlock = rewindFrom
			} else {
				// The reorg happened entirely above this walker's position — a
				// tip reorg while the historical backfill is still running.
				// Moving the walker forward would overwrite the backfill cursor
				// with a tip-region block and silently orphan the unindexed
				// range below (mainnet incident, 2026-07-05), so re-queue the
				// invalidated tip range directly and keep walking history.
				logger.Info("Re-queueing reorged blocks above walker position",
					zap.String("network", i.network.Name),
					zap.Uint64("walker_position", currentBlock),
					zap.Uint64("rewind_from", rewindFrom),
					zap.Uint64("invalidated_through", invalidatedThrough))
				for blockNumber := rewindFrom; blockNumber <= invalidatedThrough; blockNumber++ {
					select {
					case <-i.ctx.Done():
						return
					case i.blockTaskCh <- BlockTask{BlockNumber: blockNumber}:
					}
				}
			}
		}

		// Get the latest block number
		latestBlock, err := i.ethClient.GetLatestBlockNumber(i.ctx)
		if err != nil {
			logger.Error("Failed to get latest block number",
				zap.String("network", i.network.Name),
				zap.Error(err))
			continue
		}
		observedAt := time.Now()
		i.updateCurrentChainHead(latestBlock, observedAt)

		// If we're caught up, wait for the next tick
		if currentBlock > latestBlock {
			if backfillActive && observedAt.After(nextCompletionCheck) {
				// Completion means every block was enqueued, not that every
				// block committed. Only record it once indexed_blocks actually
				// covers the phase range: with backfill_active=false a crash
				// here would skip the coverage-verified resume and orphan any
				// queued-but-uncommitted blocks.
				if i.backfillRangeFullyIndexed(backfillStartBlock, latestBlock) {
					i.updateBackfillStatus(false, backfillStartBlock, latestBlock, latestBlock, observedAt)
					backfillActive = false
				} else {
					nextCompletionCheck = observedAt.Add(i.gapScanInterval)
				}
			}
			continue
		}

		if !backfillActive {
			backfillStartBlock = currentBlock
			backfillActive = true
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
		i.updateBackfillStatus(true, backfillStartBlock, endBlock, latestBlock, observedAt)

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

			// Try to resubscribe with a fresh subscription — SubscribeToNewHeads
			// would hand back the cached dead one and leave head-following
			// permanently degraded to batch polling.
			blockSub, err := i.resubscribeToNewBlocksWithBackoff()
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
			observedAt := time.Now()
			i.updateCurrentChainHead(blockNumber, observedAt)
			i.updateWebSocketFreshness(observedAt)

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

	processCh := make(chan common.Hash, pendingTxProcessingQueueSize)
	workerCount := i.workerCount
	if workerCount <= 0 {
		workerCount = DefaultWorkerCount
	}

	var workers sync.WaitGroup
	workers.Add(workerCount)
	for w := 0; w < workerCount; w++ {
		go func() {
			defer workers.Done()
			for {
				select {
				case <-i.ctx.Done():
					return
				case hash, ok := <-processCh:
					if !ok {
						return
					}
					i.processPendingTransaction(hash)
				}
			}
		}()
	}
	defer func() {
		close(processCh)
		workers.Wait()
	}()

	droppedHashes := 0
	for {
		select {
		case <-i.ctx.Done():
			logger.Info("Pending transaction subscription handler stopped",
				zap.String("network", i.network.Name))
			return
		case err, ok := <-i.pendingTxSub.Subscription.Err():
			if !ok {
				logger.Warn("Pending transaction subscription error channel closed, falling back to polling",
					zap.String("network", i.network.Name))
				i.unsubscribeFromPendingTransactions()
				i.startMempoolIndexer()
				return
			}
			logger.Error("Pending transaction subscription error, reconnecting...",
				zap.String("network", i.network.Name),
				zap.Error(err))

			// Try to resubscribe with bounded exponential backoff before
			// giving up and falling back to polling.
			pendingTxSub, err := i.resubscribeToPendingTransactionsWithBackoff()
			if err != nil {
				logger.Error("Failed to resubscribe to pending transactions, falling back to polling",
					zap.String("network", i.network.Name),
					zap.Error(err))
				i.startMempoolIndexer()
				return
			}
			i.pendingTxSub = pendingTxSub

		case hash, ok := <-i.pendingTxSub.Hashes:
			if !ok {
				logger.Warn("Pending transaction hash channel closed, falling back to polling",
					zap.String("network", i.network.Name))
				i.unsubscribeFromPendingTransactions()
				i.startMempoolIndexer()
				return
			}

			select {
			case processCh <- hash:
			default:
				droppedHashes++
				if droppedHashes == 1 || droppedHashes%1000 == 0 {
					logger.Warn("Dropping pending transaction hash because processing queue is full",
						zap.String("network", i.network.Name),
						zap.Int("dropped_hashes", droppedHashes),
						zap.Int("queue_size", pendingTxProcessingQueueSize))
				}
			}
		}
	}
}

// processPendingTransaction processes a single pending transaction by its hash
func (i *Indexer) processPendingTransaction(hash common.Hash) {
	// Get the transaction details
	tx, isPending, err := i.ethClient.GetTransactionByHash(i.ctx, hash)
	if err != nil {
		if errors.Is(err, geth.NotFound) {
			logger.Debug("Pending transaction no longer available",
				zap.String("network", i.network.Name),
				zap.String("tx_hash", hash.Hex()))
			return
		}
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

	blobBaseFee := i.getBlobBaseFeeFromBlock(latestBlock)

	// Get the sender address
	from, err := i.getSender(tx)
	if err != nil {
		logger.Error("Failed to get sender for pending transaction",
			zap.String("network", i.network.Name),
			zap.String("tx_hash", hash.Hex()),
			zap.Error(err))
		return
	}

	// Get the user attribution at the latest known head for the pending transaction.
	userAttribution := i.attribution.GetUserAttributionForBlock(from, int64(latestBlockNum))

	pendingBlobs := buildPendingBlobs(tx, blobBaseFee, i.network.ChainID, from, userAttribution)
	if len(pendingBlobs) == 0 {
		return
	}

	if err := i.insertPendingBlobs(pendingBlobs); err != nil {
		logger.Error("Failed to insert pending blob records",
			zap.String("network", i.network.Name),
			zap.String("tx_hash", hash.Hex()),
			zap.Error(err))
	}
}

// buildPendingBlobs constructs one Blob row per blob hash carried by tx. The
// BlobIndex field is left at zero; insertPendingBlobs assigns final values
// when it allocates indices from the pending pool.
func buildPendingBlobs(tx *types.Transaction, blobBaseFee *big.Int, networkID int, from, userAttribution string) []models.Blob {
	blobHashes := tx.BlobHashes()
	if len(blobHashes) == 0 {
		return nil
	}
	metrics := calculateBlobMetrics(tx, blobBaseFee)
	now := time.Now()
	rows := make([]models.Blob, 0, len(blobHashes))
	for _, blobHash := range blobHashes {
		versionedHash := blobHash.Hex()
		rows = append(rows, models.Blob{
			ChainID:           networkID,
			BlockNumber:       -1,
			TxHash:            tx.Hash().Hex(),
			FromAddress:       from,
			UserAttribution:   userAttribution,
			BlobSizeBytes:     metrics.blobSizeBytes,
			BaseFeePerBlobGas: metrics.baseFeePerBlobGas,
			TipPerBlobGas:     metrics.tipPerBlobGas,
			TotalCostWei:      metrics.totalCostETH,
			Timestamp:         now,
			Confirmed:         false,
			MaxFeePerBlobGas:  metrics.maxFeePerBlobGas,
			BlobGasUsed:       metrics.blobGasUsed,
			VersionedHash:     &versionedHash,
			Nonce:             tx.Nonce(),
		})
	}
	return rows
}

// processBlock processes a single block with reorg detection and batch inserts
func (i *Indexer) processBlock(blockNumber uint64) error {
	// Sample the cleanup epoch before the RPC fetch so insertBlockData can
	// tell whether a reorg/reindex cleanup invalidated this fetch in flight.
	fetchEpoch := atomic.LoadUint64(&i.reorgEpoch)

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
	blobBaseFee := i.getBlobBaseFeeFromBlock(block)

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

	// Collect all blob records for this block. Each EIP-4844 blob — not each
	// blob transaction — is one row. blobIndex is the block-wide blob ordinal,
	// shared by no other row in the same (chain_id, block_number).
	blobs := make([]models.Blob, 0, len(block.Transactions()))
	var attributedUsers []string
	blobIndex := 0

	for _, tx := range block.Transactions() {
		if !i.ethClient.IsBlobTransaction(tx) {
			continue
		}

		blobHashes := tx.BlobHashes()
		if len(blobHashes) == 0 {
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

		// Get the user attribution for this block's validity window.
		userAttribution := i.attribution.GetUserAttributionForBlock(from, int64(blockNumber))

		metrics := calculateBlobMetrics(tx, blobBaseFee)

		for _, blobHash := range blobHashes {
			versionedHash := blobHash.Hex()
			blobs = append(blobs, models.Blob{
				ChainID:           i.network.ChainID,
				BlockNumber:       int64(blockNumber),
				BlobIndex:         blobIndex,
				TxHash:            tx.Hash().Hex(),
				FromAddress:       from,
				UserAttribution:   userAttribution,
				BlobSizeBytes:     metrics.blobSizeBytes,
				BaseFeePerBlobGas: metrics.baseFeePerBlobGas,
				TipPerBlobGas:     metrics.tipPerBlobGas,
				TotalCostWei:      metrics.totalCostETH,
				Timestamp:         timestamp,
				Confirmed:         true,
				MaxFeePerBlobGas:  metrics.maxFeePerBlobGas,
				BlobGasUsed:       metrics.blobGasUsed,
				VersionedHash:     &versionedHash,
				Nonce:             tx.Nonce(),
			})
			blobIndex++
		}

		if userAttribution != "" {
			attributedUsers = append(attributedUsers, from)
		}
	}

	// Build block-level metrics. BlobCount is the actual blob count, not the
	// blob-tx count — a single EIP-4844 tx may carry multiple blobs.
	blockMetrics := &models.BlockMetrics{
		ChainID:          i.network.ChainID,
		BlockNumber:      int64(blockNumber),
		BlockTimestamp:   timestamp,
		BlobCount:        blobIndex,
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
		ChainID:     i.network.ChainID,
		BlockNumber: int64(blockNumber),
		BlockHash:   block.Hash().Hex(),
		ParentHash:  block.ParentHash().Hex(),
	}

	if err := i.insertBlockData(blobs, indexedBlock, blockMetrics, fetchEpoch); err != nil {
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
		// Previous block not in our index — can't check (initial sync, gap, or first block).
		if errors.Is(err, sql.ErrNoRows) {
			// If the parent is at or below our committed high-water mark it should
			// already be persisted. A missing row there means the worker pool
			// processed this block before its parent committed (out-of-order race)
			// or an earlier block silently failed — either way reorg detection is
			// running blind for this block, so surface it instead of skipping
			// quietly.
			if parent := blockNumber - 1; parent <= atomic.LoadUint64(&i.lastIndexedBlock) {
				logger.Warn("Skipping reorg check: parent block not committed yet",
					zap.String("event", "reorg_check_skipped_parent_uncommitted"),
					zap.String("network", i.network.Name),
					zap.Int("chain_id", i.network.ChainID),
					zap.Uint64("block", blockNumber),
					zap.Uint64("missing_parent", parent),
					zap.Uint64("last_indexed_block", atomic.LoadUint64(&i.lastIndexedBlock)))
			}
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
	// Walk back to find the fork point. forkFound stays false if we never see a
	// block whose stored hash still matches the canonical chain — i.e. we either
	// walked off the start of our indexed range, off block 0, or exhausted the
	// depth cap. Only an exhausted depth cap is dangerous: in that case forkBlock
	// is an arbitrary truncation point, not a verified common ancestor, so the
	// subsequent DELETE may keep blocks that are actually on the abandoned fork.
	forkBlock := fromBlock - 1
	forkFound := false
	depth := 0
	for ; depth < i.maxReorgDepth && forkBlock > 0; depth++ {
		block, err := i.ethClient.GetBlockByNumber(i.ctx, forkBlock)
		if err != nil {
			return fmt.Errorf("failed to get block %d during reorg scan: %w", forkBlock, err)
		}

		storedHash, err := i.db.GetIndexedBlockHash(i.ctx, i.network.ChainID, forkBlock)
		if errors.Is(err, sql.ErrNoRows) {
			// Past our indexed range — treat the earliest indexed block as the
			// fork point. This is a benign exit, not a depth-cap exhaustion.
			forkFound = true
			break
		}
		if err != nil {
			// A transient DB error or context cancellation must not be mistaken
			// for the boundary condition above: abort and leave indexed data
			// intact rather than rewinding to an unverified fork point.
			return fmt.Errorf("failed to read stored hash for block %d during reorg scan: %w", forkBlock, err)
		}

		if storedHash == block.Hash().Hex() {
			// Found the fork point — this block is still valid
			forkFound = true
			break
		}

		forkBlock--
	}

	// The walk hit the depth cap without confirming a common ancestor. Surface
	// this loudly: blocks below forkBlock may still belong to the orphaned fork,
	// and an operator-driven reindex is the only safe recovery.
	if !forkFound && depth >= i.maxReorgDepth {
		logger.Error("Reorg scan exhausted max depth without finding fork point",
			zap.String("event", "reorg_depth_cap_exhausted"),
			zap.String("network", i.network.Name),
			zap.Int("chain_id", i.network.ChainID),
			zap.Uint64("from_block", fromBlock),
			zap.Uint64("truncated_fork_point", forkBlock),
			zap.Int("max_reorg_depth", i.maxReorgDepth))
	}

	logger.Warn("Handling chain reorganization",
		zap.String("network", i.network.Name),
		zap.Uint64("from_block", fromBlock),
		zap.Uint64("fork_point", forkBlock),
		zap.Uint64("invalidated_blocks", fromBlock-forkBlock-1))

	unlockWrites := i.lockDBWrites()
	defer unlockWrites()

	tx, err := i.db.BeginTxx(i.ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin reorg transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Bound the range this reorg invalidates before deleting. dbWriteMu is held,
	// so every committed insert is visible and the bound is exact — the in-memory
	// lastIndexedBlock watermark can lag a just-committed block and would
	// undercount.
	var maxIndexed sql.NullInt64
	if err := tx.GetContext(i.ctx, &maxIndexed, "SELECT MAX(block_number) FROM indexed_blocks WHERE chain_id = $1", i.network.ChainID); err != nil {
		return fmt.Errorf("failed to determine reorg invalidated range: %w", err)
	}
	invalidatedThrough := forkBlock
	if maxIndexed.Valid && maxIndexed.Int64 > int64(forkBlock) {
		invalidatedThrough = uint64(maxIndexed.Int64)
	}
	// fromBlock aborted without being inserted, so it can sit above every
	// indexed row — include it, or the re-queue would drop the one block that
	// exposed the reorg.
	if fromBlock > invalidatedThrough {
		invalidatedThrough = fromBlock
	}

	// Delete invalidated data atomically.
	if _, err := tx.ExecContext(i.ctx, "DELETE FROM blobs WHERE chain_id = $1 AND block_number >= $2", i.network.ChainID, int64(forkBlock+1)); err != nil {
		return fmt.Errorf("failed to delete reorged blobs: %w", err)
	}
	if _, err := tx.ExecContext(i.ctx, "DELETE FROM block_metrics WHERE chain_id = $1 AND block_number >= $2", i.network.ChainID, int64(forkBlock+1)); err != nil {
		return fmt.Errorf("failed to delete reorged block metrics: %w", err)
	}
	if _, err := tx.ExecContext(i.ctx, "DELETE FROM indexed_blocks WHERE chain_id = $1 AND block_number >= $2", i.network.ChainID, forkBlock+1); err != nil {
		return fmt.Errorf("failed to delete reorged indexed blocks: %w", err)
	}

	// Persist the invalidated range in the same transaction as the deletions.
	// The in-memory reorgDetected signal dies with the process: a crash before
	// the re-queued blocks commit would otherwise lose the range, and a
	// LATEST-start network resumes at the current tip, skipping it forever.
	// Failure aborts the whole transaction — deleting without the marker
	// recreates exactly that hole.
	markerFrom, markerThrough, err := i.mergeReorgRecoveryMarker(tx, forkBlock+1, invalidatedThrough)
	if err != nil {
		return err
	}

	// Reset lastIndexedBlock to the fork point
	atomic.StoreUint64(&i.lastIndexedBlock, forkBlock)
	i.mu.Lock()
	if _, err := tx.ExecContext(i.ctx, `
		INSERT INTO indexer_metadata (chain_id, key, value)
		VALUES ($1, $2, $3)
		ON CONFLICT (chain_id, key) DO UPDATE SET value = $3
	`, i.network.ChainID, models.MetadataLastIndexedBlock, strconv.FormatUint(forkBlock, 10)); err != nil {
		logger.Error("Failed to update last indexed block after reorg",
			zap.String("network", i.network.Name),
			zap.Error(err))
	}
	i.mu.Unlock()
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit reorg transaction: %w", err)
	}

	// Invalidate in-flight fetches while dbWriteMu is still held: a worker
	// that fetched an old-fork block before the DELETEs above committed would
	// otherwise land it as soon as it acquires the lock.
	atomic.AddUint64(&i.reorgEpoch, 1)

	// Signal the main indexer loop to re-index the invalidated range. If no
	// recovery signal has been raised in this process yet, a prior marker's
	// range may never have been queued (startup seeding is best-effort), so
	// widen the signal to the merged marker. Otherwise the prior range is
	// already queued here and the fresh range suffices — blindly re-signaling
	// a large mostly-recovered marker would restart a long re-walk from
	// scratch on every subsequent tip reorg.
	signalFrom, signalThrough := forkBlock+1, invalidatedThrough
	if atomic.CompareAndSwapUint32(&i.reorgRecoverySignaled, 0, 1) {
		signalFrom, signalThrough = markerFrom, markerThrough
	}
	i.signalReorgReset(signalFrom, signalThrough)

	return fmt.Errorf("reorg handled, rewound from %d to %d: %w", fromBlock, forkBlock, errReorgDetected)
}

// signalReorgReset records the block range invalidated by a reorg and raises
// the reorgDetected flag for the main loop. If a prior signal has not been
// consumed yet, the ranges merge so no invalidated block is lost.
func (i *Indexer) signalReorgReset(from, through uint64) {
	i.reorgRangeMu.Lock()
	defer i.reorgRangeMu.Unlock()

	if atomic.LoadUint32(&i.reorgDetected) == 1 {
		if from < i.reorgRewindFrom {
			i.reorgRewindFrom = from
		}
		if through > i.reorgInvalidatedThrough {
			i.reorgInvalidatedThrough = through
		}
		return
	}

	i.reorgRewindFrom = from
	i.reorgInvalidatedThrough = through
	atomic.StoreUint32(&i.reorgDetected, 1)
}

// consumeReorgReset returns the pending invalidated range and clears the
// reorgDetected flag. Only meaningful after observing reorgDetected == 1.
func (i *Indexer) consumeReorgReset() (from, through uint64) {
	i.reorgRangeMu.Lock()
	defer i.reorgRangeMu.Unlock()

	from = i.reorgRewindFrom
	through = i.reorgInvalidatedThrough
	atomic.StoreUint32(&i.reorgDetected, 0)
	return from, through
}

// mergeReorgRecoveryMarker upserts the persisted reorg recovery marker inside
// the reorg deletion transaction, widening any existing unrecovered range so
// back-to-back reorgs (or a crash loop) never narrow it. Unparseable existing
// values cannot widen anything and are overwritten. Returns the merged range
// that was persisted.
func (i *Indexer) mergeReorgRecoveryMarker(tx *sqlx.Tx, rewindFrom, invalidatedThrough uint64) (markerFrom, markerThrough uint64, err error) {
	markerFrom, markerThrough = rewindFrom, invalidatedThrough

	var rows []indexerMetadataRow
	if err := tx.SelectContext(i.ctx, &rows, `
		SELECT key, value
		FROM indexer_metadata
		WHERE chain_id = $1
			AND key IN ($2, $3)
	`, i.network.ChainID, models.MetadataReorgRewindFrom, models.MetadataReorgInvalidatedThrough); err != nil {
		return 0, 0, fmt.Errorf("failed to read reorg recovery marker: %w", err)
	}
	for _, row := range rows {
		prev, parseErr := strconv.ParseUint(row.Value, 10, 64)
		if parseErr != nil {
			logger.Error("Overwriting unparseable reorg recovery marker value",
				zap.String("network", i.network.Name),
				zap.String("metadata_key", row.Key),
				zap.String("metadata_value", row.Value),
				zap.Error(parseErr))
			continue
		}
		switch row.Key {
		case models.MetadataReorgRewindFrom:
			if prev < markerFrom {
				markerFrom = prev
			}
		case models.MetadataReorgInvalidatedThrough:
			if prev > markerThrough {
				markerThrough = prev
			}
		}
	}

	if _, err := tx.ExecContext(i.ctx, `
		INSERT INTO indexer_metadata (chain_id, key, value)
		VALUES ($1, $2, $3), ($1, $4, $5)
		ON CONFLICT (chain_id, key) DO UPDATE SET value = EXCLUDED.value
	`, i.network.ChainID,
		models.MetadataReorgRewindFrom, strconv.FormatUint(markerFrom, 10),
		models.MetadataReorgInvalidatedThrough, strconv.FormatUint(markerThrough, 10)); err != nil {
		return 0, 0, fmt.Errorf("failed to persist reorg recovery marker: %w", err)
	}
	return markerFrom, markerThrough, nil
}

// getReorgRecoveryMarker reads the persisted reorg recovery marker. ok is
// false when no marker exists; a half-written, unparseable, or inverted
// marker (impossible from mergeReorgRecoveryMarker, so operator damage) is
// returned as an error so callers surface it instead of acting on it.
func (i *Indexer) getReorgRecoveryMarker() (from, through uint64, ok bool, err error) {
	var rows []indexerMetadataRow
	query := `
		SELECT key, value
		FROM indexer_metadata
		WHERE chain_id = $1
			AND key IN ($2, $3)
	`
	if err := i.db.SelectContext(i.ctx, &rows, query,
		i.network.ChainID, models.MetadataReorgRewindFrom, models.MetadataReorgInvalidatedThrough); err != nil {
		return 0, 0, false, fmt.Errorf("failed to read reorg recovery marker: %w", err)
	}
	if len(rows) == 0 {
		return 0, 0, false, nil
	}

	var haveFrom, haveThrough bool
	for _, row := range rows {
		value, parseErr := strconv.ParseUint(row.Value, 10, 64)
		if parseErr != nil {
			return 0, 0, false, fmt.Errorf("failed to parse reorg recovery marker %s=%q: %w", row.Key, row.Value, parseErr)
		}
		switch row.Key {
		case models.MetadataReorgRewindFrom:
			from = value
			haveFrom = true
		case models.MetadataReorgInvalidatedThrough:
			through = value
			haveThrough = true
		}
	}
	if !haveFrom || !haveThrough {
		return 0, 0, false, fmt.Errorf("reorg recovery marker is half-written: have rewind_from=%t invalidated_through=%t", haveFrom, haveThrough)
	}
	if from > through {
		return 0, 0, false, fmt.Errorf("reorg recovery marker range is inverted: [%d %d]", from, through)
	}
	return from, through, true, nil
}

// seedReorgRecoveryFromMarker re-raises the reorg signal for a persisted
// invalidated range whose re-indexing a crash interrupted. The main loop then
// re-walks it exactly like a live reorg; without this, a LATEST-start network
// resumes at the current tip and the rewound range stays lost forever.
// Best-effort like seedStartupGapRecovery: a broken marker must not block
// startup, and it is left in place for operator inspection.
func (i *Indexer) seedReorgRecoveryFromMarker() {
	if i.db == nil {
		return
	}

	from, through, ok, err := i.getReorgRecoveryMarker()
	if err != nil {
		logger.Error("Failed to read reorg recovery marker at startup",
			zap.String("network", i.network.Name),
			zap.Int("chain_id", i.network.ChainID),
			zap.Error(err))
		return
	}
	if !ok {
		return
	}

	logger.Warn("Recovering reorg-invalidated range persisted before a crash",
		zap.String("event", "reorg_marker_recovery"),
		zap.String("network", i.network.Name),
		zap.Int("chain_id", i.network.ChainID),
		zap.Uint64("rewind_from", from),
		zap.Uint64("invalidated_through", through))
	i.signalReorgReset(from, through)
	atomic.StoreUint32(&i.reorgRecoverySignaled, 1)
}

// maybeCompleteReorgRecovery clears the persisted reorg recovery marker once
// indexed_blocks provably covers the invalidated range again. Runs on the gap
// scanner tick; a marker is present only in the window between a reorg and
// the re-indexing of its range, so the common case is one cheap metadata read.
func (i *Indexer) maybeCompleteReorgRecovery() {
	i.completeReorgRecoveryIfCovered(atomic.LoadUint64(&i.reorgEpoch))
}

// completeReorgRecoveryIfCovered verifies coverage of the marker range and
// deletes the marker. fetchEpoch is the reorgEpoch sampled before the marker
// was read: a destructive cleanup (reorg rewind, reindex delete) committing
// between the coverage scan and the delete re-opens holes the scan never saw,
// so the delete is fenced the same way insertBlockData fences stale fetches.
// The coverage scan itself runs outside dbWriteMu — it can span millions of
// rows and must not stall block inserts. Best-effort: on any error the marker
// stays put and the next gap-scan tick retries.
func (i *Indexer) completeReorgRecoveryIfCovered(fetchEpoch uint64) {
	if i.db == nil {
		return
	}

	from, through, ok, err := i.getReorgRecoveryMarker()
	if err != nil {
		logger.Error("Failed to read reorg recovery marker",
			zap.String("network", i.network.Name),
			zap.Int("chain_id", i.network.ChainID),
			zap.Error(err))
		return
	}
	if !ok {
		return
	}

	firstGap, err := i.db.GetFirstUnindexedBlock(i.ctx, i.network.ChainID, from, through)
	if err != nil {
		logger.Error("Failed to verify reorg recovery coverage, keeping marker",
			zap.String("network", i.network.Name),
			zap.Uint64("rewind_from", from),
			zap.Uint64("invalidated_through", through),
			zap.Error(err))
		return
	}
	if firstGap <= through {
		// If no recovery signal was ever raised in this process, the marker
		// range was never queued — startup seeding is best-effort and can lose
		// it to a transient read error, which would otherwise leave the marker
		// inert until the next restart. Re-raise it exactly once.
		if atomic.CompareAndSwapUint32(&i.reorgRecoverySignaled, 0, 1) {
			logger.Warn("Re-raising recovery for a reorg marker that was never signaled in this process",
				zap.String("event", "reorg_marker_recovery_reraised"),
				zap.String("network", i.network.Name),
				zap.Int("chain_id", i.network.ChainID),
				zap.Uint64("rewind_from", from),
				zap.Uint64("invalidated_through", through))
			i.signalReorgReset(from, through)
			return
		}
		logger.Debug("Reorg-invalidated range not fully re-indexed yet, keeping marker",
			zap.String("network", i.network.Name),
			zap.Uint64("rewind_from", from),
			zap.Uint64("invalidated_through", through),
			zap.Uint64("first_unindexed_block", firstGap))
		return
	}

	unlockWrites := i.lockDBWrites()
	defer unlockWrites()

	if atomic.LoadUint64(&i.reorgEpoch) != fetchEpoch {
		// A cleanup committed while the coverage scan ran; its handler already
		// re-persisted (and possibly widened) the marker. Re-verify next tick.
		return
	}

	if _, err := i.db.ExecContext(i.ctx, `
		DELETE FROM indexer_metadata
		WHERE chain_id = $1
			AND key IN ($2, $3)
	`, i.network.ChainID, models.MetadataReorgRewindFrom, models.MetadataReorgInvalidatedThrough); err != nil {
		logger.Error("Failed to clear reorg recovery marker",
			zap.String("network", i.network.Name),
			zap.Error(err))
		return
	}

	logger.Info("Reorg-invalidated range fully re-indexed, cleared recovery marker",
		zap.String("event", "reorg_marker_recovered"),
		zap.String("network", i.network.Name),
		zap.Int("chain_id", i.network.ChainID),
		zap.Uint64("rewind_from", from),
		zap.Uint64("invalidated_through", through))
}

// blobInsertColumns is the number of columns written per row when inserting
// into blobs.
const blobInsertColumns = 14

// mempoolBlobInsertColumns is the number of columns written per row when
// upserting into mempool_blobs.
const mempoolBlobInsertColumns = 15

// valuesPlaceholders builds a multi-row VALUES clause "($1,$2,...),($n,...),..."
// for rows rows of width columns. casts, when non-nil, must have width entries
// and appends "::type" to each placeholder — required when the VALUES list has
// no INSERT target to infer parameter types from (e.g. UPDATE ... FROM (VALUES ...)).
func valuesPlaceholders(rows, width int, casts []string) string {
	if casts != nil && len(casts) != width {
		panic(fmt.Sprintf("valuesPlaceholders: %d casts for width %d", len(casts), width))
	}
	var b strings.Builder
	n := 1
	for r := 0; r < rows; r++ {
		if r > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('(')
		for c := 0; c < width; c++ {
			if c > 0 {
				b.WriteByte(',')
			}
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			if casts != nil {
				b.WriteString("::")
				b.WriteString(casts[c])
			}
			n++
		}
		b.WriteByte(')')
	}
	return b.String()
}

// insertBlockData inserts all blobs, block metrics, and records the indexed block in a single
// database transaction. This ensures atomicity — either the entire block is recorded or nothing is.
// fetchEpoch is the reorgEpoch value sampled before the block was fetched via
// RPC; the insert is refused if a cleanup committed in between.
func (i *Indexer) insertBlockData(blobs []models.Blob, indexedBlock models.IndexedBlock, blockMetrics *models.BlockMetrics, fetchEpoch uint64) error {
	unlockWrites := i.lockDBWrites()
	defer unlockWrites()

	if atomic.LoadUint64(&i.reorgEpoch) != fetchEpoch {
		return fmt.Errorf("discarding block %d: %w", indexedBlock.BlockNumber, errStaleBlockFetch)
	}

	tx, err := i.db.BeginTxx(i.ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Remove blob rows this block no longer has. The upsert below overwrites
	// blob_index 0..n-1, but if a stale-fork version of the block landed with
	// more blobs, its surplus rows would survive every reprocess — nothing
	// else deletes them and the stats triggers keep counting them.
	if _, err := tx.ExecContext(i.ctx,
		"DELETE FROM blobs WHERE chain_id = $1 AND block_number = $2 AND blob_index >= $3",
		indexedBlock.ChainID, indexedBlock.BlockNumber, len(blobs),
	); err != nil {
		return fmt.Errorf("failed to trim surplus blob rows (block: %d): %w", indexedBlock.BlockNumber, err)
	}

	// Insert blobs using a prepared statement within the transaction
	if len(blobs) > 0 {
		// Collect unique tx hashes to delete their pending counterparts, and
		// each tx's (sender, nonce) to delete any pending tx it replaced.
		txHashSet := make(map[string]struct{}, len(blobs))
		txHashes := make([]string, 0, len(blobs))
		senders := make([]string, 0, len(blobs))
		nonces := make([]int64, 0, len(blobs))
		for _, blob := range blobs {
			if _, seen := txHashSet[blob.TxHash]; seen {
				continue
			}
			txHashSet[blob.TxHash] = struct{}{}
			txHashes = append(txHashes, blob.TxHash)
			senders = append(senders, blob.FromAddress)
			nonces = append(nonces, int64(blob.Nonce))
		}

		// Delete pending blob rows that are now being confirmed
		if len(txHashes) > 0 {
			deleteQuery, deleteArgs, err := sqlx.In(
				"DELETE FROM mempool_blobs WHERE chain_id = ? AND tx_hash IN (?)",
				i.network.ChainID, txHashes,
			)
			if err != nil {
				return fmt.Errorf("failed to build pending blob delete query: %w", err)
			}
			deleteQuery = tx.Rebind(deleteQuery)
			res, err := tx.ExecContext(i.ctx, deleteQuery, deleteArgs...)
			if err != nil {
				return fmt.Errorf("failed to delete pending blobs: %w", err)
			}
			if promoted, _ := res.RowsAffected(); promoted > 0 {
				logger.Debug("Promoted pending blobs to confirmed",
					zap.String("network", i.network.Name),
					zap.Int64("promoted_count", promoted))
			}

			// A confirmed tx also invalidates any pending tx it replaced:
			// same sender and nonce under a different hash. The replaced
			// hash never confirms, so the hash-based delete above cannot
			// see it and only the TTL sweep would remove it. Legacy rows
			// with NULL nonce never match and still age out via the sweep.
			// Each evicted hash is recorded in blob_replacements in the
			// same statement — the hash-based delete ran first, so only
			// genuinely replaced hashes reach the log.
			supersededRes, err := tx.ExecContext(i.ctx,
				`WITH superseded AS (
					DELETE FROM mempool_blobs m
					USING unnest($2::text[], $3::bigint[], $4::text[]) AS t(from_address, nonce, replacement_tx_hash)
					WHERE m.chain_id = $1 AND m.from_address = t.from_address AND m.nonce = t.nonce
					RETURNING m.tx_hash, m.from_address, m.nonce, t.replacement_tx_hash
				)
				INSERT INTO blob_replacements (chain_id, replaced_tx_hash, replacement_tx_hash, from_address, nonce, replaced_at)
				SELECT DISTINCT $1, tx_hash, replacement_tx_hash, from_address, nonce, $5::timestamp FROM superseded
				ON CONFLICT (chain_id, replaced_tx_hash) DO UPDATE SET
					replacement_tx_hash = EXCLUDED.replacement_tx_hash,
					from_address = EXCLUDED.from_address,
					nonce = EXCLUDED.nonce,
					replaced_at = EXCLUDED.replaced_at`,
				i.network.ChainID, pq.Array(senders), pq.Array(nonces), pq.Array(txHashes), blobs[0].Timestamp,
			)
			if err != nil {
				return fmt.Errorf("failed to delete superseded pending blobs: %w", err)
			}
			if superseded, _ := supersededRes.RowsAffected(); superseded > 0 {
				logger.Debug("Recorded pending blobs superseded by confirmed transactions",
					zap.String("network", i.network.Name),
					zap.Int64("superseded_tx_count", superseded))
			}
		}

		// Insert all blobs in one multi-row statement so the statement-level
		// aggregate triggers on blobs (network_blob_stats, blob_user_stats,
		// blob_chart_rollups) fire once per block instead of once per row.
		insertQuery := `
			INSERT INTO blobs (
				chain_id, block_number, blob_index, tx_hash, from_address, user_attribution,
				blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
				timestamp, max_fee_per_blob_gas, blob_gas_used, versioned_hash
			) VALUES ` + valuesPlaceholders(len(blobs), blobInsertColumns, nil) + `
			ON CONFLICT (chain_id, block_number, blob_index) DO UPDATE SET
				tx_hash = EXCLUDED.tx_hash,
				from_address = EXCLUDED.from_address,
				user_attribution = EXCLUDED.user_attribution,
				blob_size_bytes = EXCLUDED.blob_size_bytes,
				base_fee_per_blob_gas = EXCLUDED.base_fee_per_blob_gas,
				tip_per_blob_gas = EXCLUDED.tip_per_blob_gas,
				total_cost_wei = EXCLUDED.total_cost_wei,
				timestamp = EXCLUDED.timestamp,
				max_fee_per_blob_gas = EXCLUDED.max_fee_per_blob_gas,
				blob_gas_used = EXCLUDED.blob_gas_used,
				versioned_hash = EXCLUDED.versioned_hash
		`
		insertArgs := make([]interface{}, 0, len(blobs)*blobInsertColumns)
		for _, blob := range blobs {
			insertArgs = append(insertArgs,
				blob.ChainID, blob.BlockNumber, blob.BlobIndex, blob.TxHash, blob.FromAddress, blob.UserAttribution,
				blob.BlobSizeBytes, blob.BaseFeePerBlobGas, blob.TipPerBlobGas, blob.TotalCostWei,
				blob.Timestamp, blob.MaxFeePerBlobGas, blob.BlobGasUsed, blob.VersionedHash)
		}
		if _, err := tx.ExecContext(i.ctx, insertQuery, insertArgs...); err != nil {
			return fmt.Errorf("failed to insert blobs (block: %d): %w", indexedBlock.BlockNumber, err)
		}
	}

	// Insert block-level blob metrics
	if blockMetrics != nil {
		_, err = tx.ExecContext(i.ctx, `
			INSERT INTO block_metrics (
				chain_id, block_number, block_timestamp, blob_count,
				blob_gas_used, blob_gas_target, blob_gas_limit,
				excess_blob_gas, blob_base_fee, utilization_ratio,
				blob_params_target, blob_params_max, update_fraction
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
			ON CONFLICT (chain_id, block_number) DO UPDATE SET
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
		`, blockMetrics.ChainID, blockMetrics.BlockNumber, blockMetrics.BlockTimestamp, blockMetrics.BlobCount,
			blockMetrics.BlobGasUsed, blockMetrics.BlobGasTarget, blockMetrics.BlobGasLimit,
			blockMetrics.ExcessBlobGas, blockMetrics.BlobBaseFee, blockMetrics.UtilizationRatio,
			blockMetrics.BlobParamsTarget, blockMetrics.BlobParamsMax, blockMetrics.UpdateFraction)
		if err != nil {
			return fmt.Errorf("failed to insert block metrics: %w", err)
		}
	}

	// Record the indexed block for reorg detection
	_, err = tx.ExecContext(i.ctx, `
		INSERT INTO indexed_blocks (chain_id, block_number, block_hash, parent_hash)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (chain_id, block_number) DO UPDATE SET
			block_hash = EXCLUDED.block_hash,
			parent_hash = EXCLUDED.parent_hash,
			indexed_at = NOW()
	`, indexedBlock.ChainID, indexedBlock.BlockNumber, indexedBlock.BlockHash, indexedBlock.ParentHash)
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
			// The template poll above only re-observes competitively priced
			// txs; tracked txs that fell out of the template still need
			// their liveness confirmed or they age out while pending.
			i.refreshPendingBlobLiveness()
		}
	}
}

// runMempoolReconciler re-polls the node's pending pool on a slow ticker
// while the websocket pending-tx subscription is active. It runs the same
// poll body as the fallback indexer: still-pending blob txs are re-upserted
// (refreshing their timestamps so the TTL sweep only reaps txs that actually
// left the pool), newly appeared ones are picked up, and confirmed or
// replaced ones are suppressed by insertPendingBlobs' guards.
func (i *Indexer) runMempoolReconciler() {
	logger.Info("Mempool reconciler starting",
		zap.String("network", i.network.Name),
		zap.Duration("interval", i.mempoolReconcileInterval))

	ticker := time.NewTicker(i.mempoolReconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-i.ctx.Done():
			logger.Info("Mempool reconciler stopped", zap.String("network", i.network.Name))
			return
		case <-ticker.C:
			if err := i.processPendingTransactions(); err != nil {
				logger.Error("Mempool reconciliation poll failed",
					zap.String("network", i.network.Name),
					zap.Error(err))
			}
			i.refreshPendingBlobLiveness()
		}
	}
}

// refreshPendingBlobLiveness bumps last_seen for every tracked pending blob
// tx the node still reports as pending. The pending-block template the
// mempool poll reads (eth_getBlockByNumber("pending")) is fee-filtered and
// blob-capped, so an underpriced blob tx waiting out a fee spike never
// appears there — exactly the tx the pending view must not lose. Asking the
// node for each tracked hash directly keeps the TTL sweep honest in both
// directions: a still-pooled tx survives indefinitely, and a tx the node
// dropped stops being bumped and is reaped mempool_ttl after its last
// sighting. Cost is bounded by our own row count (the live blob mempool),
// not the node's pool, so this stays cheap at every cadence it runs at.
func (i *Indexer) refreshPendingBlobLiveness() {
	var hashes []string
	if err := i.db.SelectContext(i.ctx, &hashes,
		"SELECT DISTINCT tx_hash FROM mempool_blobs WHERE chain_id = $1", i.network.ChainID); err != nil {
		logger.Error("Failed to list pending blobs for liveness refresh",
			zap.String("network", i.network.Name),
			zap.Error(err))
		return
	}
	if len(hashes) == 0 {
		return
	}

	live := make([]string, 0, len(hashes))
	for _, hash := range hashes {
		select {
		case <-i.ctx.Done():
			return
		default:
		}
		_, isPending, err := i.ethClient.GetTransactionByHash(i.ctx, common.HexToHash(hash))
		if err != nil {
			// NotFound means the node no longer knows the tx; anything else
			// is a transient RPC failure. Either way last_seen stays put —
			// the TTL sweep reaps the row only if the condition persists.
			if !errors.Is(err, geth.NotFound) {
				logger.Debug("Pending blob liveness check failed",
					zap.String("network", i.network.Name),
					zap.String("tx_hash", hash),
					zap.Error(err))
			}
			continue
		}
		if isPending {
			live = append(live, hash)
		}
	}
	if len(live) == 0 {
		return
	}

	unlockWrites := i.lockDBWrites()
	defer unlockWrites()
	if _, err := i.db.ExecContext(i.ctx,
		"UPDATE mempool_blobs SET last_seen = $2 WHERE chain_id = $1 AND tx_hash = ANY($3)",
		i.network.ChainID, time.Now(), pq.Array(live)); err != nil {
		logger.Error("Failed to refresh pending blob liveness",
			zap.String("network", i.network.Name),
			zap.Error(err))
	}
}

// cleanupLegacyPendingBlobs removes pending rows written into blobs under the
// old block_number < 0 sentinel scheme. The mempool_blobs migration deletes
// them, but an old binary still running during the deploy window can write a
// few more before this binary takes over; without this sweep those rows would
// sit in blobs forever, inflating blob_user_stats. Best-effort — failures are
// logged, not fatal.
func (i *Indexer) cleanupLegacyPendingBlobs() {
	unlockWrites := i.lockDBWrites()
	defer unlockWrites()

	res, err := i.db.ExecContext(i.ctx,
		"DELETE FROM blobs WHERE chain_id = $1 AND block_number < 0", i.network.ChainID)
	if err != nil {
		logger.Error("Failed to clean up legacy pending blob rows",
			zap.String("network", i.network.Name),
			zap.Error(err))
		return
	}
	if deleted, _ := res.RowsAffected(); deleted > 0 {
		logger.Info("Removed legacy pending blob rows from blobs",
			zap.String("network", i.network.Name),
			zap.Int64("deleted_count", deleted))
	}
}

// runMempoolCleanup periodically removes pending blobs that have exceeded the configured TTL.
func (i *Indexer) runMempoolCleanup() {
	logger.Info("Mempool cleanup starting",
		zap.String("network", i.network.Name),
		zap.Duration("ttl", i.mempoolTTL),
		zap.Duration("interval", i.mempoolCleanupInterval))

	ticker := time.NewTicker(i.mempoolCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-i.ctx.Done():
			logger.Info("Mempool cleanup stopped", zap.String("network", i.network.Name))
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-i.mempoolTTL)
			unlockWrites := i.lockDBWrites()
			deleted, err := i.db.DeleteStalePendingBlobs(i.ctx, i.network.ChainID, cutoff)
			unlockWrites()
			if err != nil {
				logger.Error("Failed to clean up stale pending blobs",
					zap.String("network", i.network.Name),
					zap.Error(err))
				continue
			}
			if deleted > 0 {
				logger.Info("Cleaned up stale pending blobs",
					zap.String("network", i.network.Name),
					zap.Int64("deleted_count", deleted))
			}

			replacementCutoff := time.Now().Add(-blobReplacementRetention)
			unlockWrites = i.lockDBWrites()
			pruned, err := i.db.DeleteStaleBlobReplacements(i.ctx, i.network.ChainID, replacementCutoff)
			unlockWrites()
			if err != nil {
				logger.Error("Failed to prune stale blob replacements",
					zap.String("network", i.network.Name),
					zap.Error(err))
				continue
			}
			if pruned > 0 {
				logger.Info("Pruned stale blob replacements",
					zap.String("network", i.network.Name),
					zap.Int64("pruned_count", pruned))
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

	blobBaseFee := i.getBlobBaseFeeFromBlock(latestBlock)

	// Process each pending transaction
	for _, tx := range pendingTxs {
		if !i.ethClient.IsBlobTransaction(tx) {
			continue
		}

		from, err := i.getSender(tx)
		if err != nil {
			logger.Error("Failed to get sender for pending transaction",
				zap.String("network", i.network.Name),
				zap.String("tx_hash", tx.Hash().Hex()),
				zap.Error(err))
			continue
		}

		// Get the user attribution at the latest known head for the pending transaction.
		userAttribution := i.attribution.GetUserAttributionForBlock(from, int64(latestBlockNum))

		pendingBlobs := buildPendingBlobs(tx, blobBaseFee, i.network.ChainID, from, userAttribution)
		if len(pendingBlobs) == 0 {
			continue
		}

		if err := i.insertPendingBlobs(pendingBlobs); err != nil {
			logger.Error("Failed to insert pending blob records",
				zap.String("network", i.network.Name),
				zap.String("tx_hash", tx.Hash().Hex()),
				zap.Error(err))
			continue
		}
	}

	return nil
}

// insertPendingBlobs upserts the per-blob pending rows for a single transaction
// into mempool_blobs. All blobs in the slice must share the same ChainID,
// TxHash, FromAddress, and Nonce. mempool_blobs.blob_index is the
// per-transaction blob ordinal (0..N-1), so a re-poll of an already-tracked tx
// updates the same rows in place via ON CONFLICT; rows of a pending tx this
// one replaces (same sender and nonce, different hash) are deleted, and if the
// tx's blob count shrank, surplus rows are trimmed first. If the tx is already
// confirmed, or already recorded as replaced in blob_replacements, the call is
// a no-op.
func (i *Indexer) insertPendingBlobs(blobs []models.Blob) error {
	if len(blobs) == 0 {
		return nil
	}
	networkID := blobs[0].ChainID
	txHash := blobs[0].TxHash

	unlockWrites := i.lockDBWrites()
	defer unlockWrites()

	tx, err := i.db.BeginTxx(i.ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin pending blob transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// If the tx is already confirmed, or already observed replaced, do not
	// (re)create pending rows. The block_number >= 0 filter keeps legacy
	// pending sentinel rows — which an old binary can still write into blobs
	// during the deploy window — from suppressing mempool_blobs writes. The
	// blob_replacements check treats the eviction record as a tombstone: a
	// pending fetch races the block path, so the block confirming this tx's
	// replacement (same sender and nonce, different hash) can commit between
	// this tx being fetched and stored here, and without the tombstone the
	// dead tx would be resurrected into the pending view until the TTL
	// sweep. Suppressing on a tombstone is safe: a node only re-admits a
	// replaced tx via an explicit re-broadcast after its replacement
	// vanished from the pool, which is rare and self-corrects on inclusion.
	var suppressed bool
	if err := tx.QueryRowContext(i.ctx,
		`SELECT EXISTS (SELECT 1 FROM blobs WHERE chain_id = $1 AND tx_hash = $2 AND block_number >= 0)
			OR EXISTS (SELECT 1 FROM blob_replacements WHERE chain_id = $1 AND replaced_tx_hash = $2)`,
		networkID, txHash,
	).Scan(&suppressed); err != nil {
		return fmt.Errorf("failed to check confirmed blobs for pending tx: %w", err)
	}
	if suppressed {
		return tx.Commit()
	}

	// A fee-bumped replacement reuses the sender's nonce under a new hash.
	// The superseded hash never confirms, so without this delete its rows
	// would sit in the pending view until the TTL sweep. Legacy rows with
	// NULL nonce never match and still age out via the sweep. Each evicted
	// hash is recorded in blob_replacements in the same statement — once the
	// rows are gone the observation is otherwise lost.
	supersededRes, err := tx.ExecContext(i.ctx,
		`WITH superseded AS (
			DELETE FROM mempool_blobs WHERE chain_id = $1 AND from_address = $2 AND nonce = $3 AND tx_hash <> $4
			RETURNING tx_hash
		)
		INSERT INTO blob_replacements (chain_id, replaced_tx_hash, replacement_tx_hash, from_address, nonce, replaced_at)
		SELECT DISTINCT $1, tx_hash, $4, $2, $3, $5::timestamp FROM superseded
		ON CONFLICT (chain_id, replaced_tx_hash) DO UPDATE SET
			replacement_tx_hash = EXCLUDED.replacement_tx_hash,
			from_address = EXCLUDED.from_address,
			nonce = EXCLUDED.nonce,
			replaced_at = EXCLUDED.replaced_at`,
		networkID, blobs[0].FromAddress, int64(blobs[0].Nonce), txHash, blobs[0].Timestamp,
	)
	if err != nil {
		return fmt.Errorf("failed to delete superseded pending blobs (tx: %s): %w", txHash, err)
	}
	if superseded, _ := supersededRes.RowsAffected(); superseded > 0 {
		logger.Debug("Recorded pending blobs superseded by replacement transaction",
			zap.String("network", i.network.Name),
			zap.String("tx_hash", txHash),
			zap.Int64("superseded_tx_count", superseded))
	}

	// Trim surplus rows if the tx's blob count shrank under us.
	if _, err := tx.ExecContext(i.ctx,
		`DELETE FROM mempool_blobs WHERE chain_id = $1 AND tx_hash = $2 AND blob_index >= $3`,
		networkID, txHash, len(blobs),
	); err != nil {
		return fmt.Errorf("failed to trim surplus pending blobs: %w", err)
	}

	// One multi-row upsert per poll keeps this a single round-trip; rows are
	// keyed by the per-tx blob ordinal, so a re-poll updates the same rows in
	// place. timestamp is deliberately absent from the update list: it is the
	// first-seen instant serving API ordering and pending-age metrics, while
	// last_seen is the liveness watermark the TTL sweep reaps on — a
	// re-observation bumps only the latter.
	upsertQuery := `
		INSERT INTO mempool_blobs (
			chain_id, tx_hash, blob_index, from_address, user_attribution,
			blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
			timestamp, max_fee_per_blob_gas, blob_gas_used, versioned_hash, nonce, last_seen
		) VALUES ` + valuesPlaceholders(len(blobs), mempoolBlobInsertColumns, nil) + `
		ON CONFLICT (chain_id, tx_hash, blob_index) DO UPDATE SET
			from_address = EXCLUDED.from_address,
			user_attribution = EXCLUDED.user_attribution,
			blob_size_bytes = EXCLUDED.blob_size_bytes,
			base_fee_per_blob_gas = EXCLUDED.base_fee_per_blob_gas,
			tip_per_blob_gas = EXCLUDED.tip_per_blob_gas,
			total_cost_wei = EXCLUDED.total_cost_wei,
			max_fee_per_blob_gas = EXCLUDED.max_fee_per_blob_gas,
			blob_gas_used = EXCLUDED.blob_gas_used,
			versioned_hash = EXCLUDED.versioned_hash,
			nonce = EXCLUDED.nonce,
			last_seen = EXCLUDED.last_seen
	`
	upsertArgs := make([]interface{}, 0, len(blobs)*mempoolBlobInsertColumns)
	for offset, b := range blobs {
		upsertArgs = append(upsertArgs,
			b.ChainID, b.TxHash, offset, b.FromAddress, b.UserAttribution,
			b.BlobSizeBytes, b.BaseFeePerBlobGas, b.TipPerBlobGas, b.TotalCostWei,
			b.Timestamp, b.MaxFeePerBlobGas, b.BlobGasUsed, b.VersionedHash, int64(b.Nonce), b.Timestamp)
	}
	if _, err := tx.ExecContext(i.ctx, upsertQuery, upsertArgs...); err != nil {
		return fmt.Errorf("failed to insert pending blobs (tx: %s): %w", txHash, err)
	}

	return tx.Commit()
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

	if err := i.deleteReindexRange(startBlock, endBlock); err != nil {
		return err
	}

	// Process the block range
	return i.processBlockRange(startBlock, endBlock)
}

func (i *Indexer) deleteReindexRange(startBlock, endBlock uint64) error {
	unlockWrites := i.lockDBWrites()
	defer unlockWrites()

	tx, err := i.db.BeginTxx(i.ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin reindex transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Delete existing blob records in the range.
	query := "DELETE FROM blobs WHERE chain_id = $1 AND block_number >= $2 AND block_number <= $3"
	if _, err := tx.ExecContext(i.ctx, query, i.network.ChainID, startBlock, endBlock); err != nil {
		return fmt.Errorf("failed to delete existing blob records: %w", err)
	}

	// Delete existing block metrics in the range.
	query = "DELETE FROM block_metrics WHERE chain_id = $1 AND block_number >= $2 AND block_number <= $3"
	if _, err := tx.ExecContext(i.ctx, query, i.network.ChainID, startBlock, endBlock); err != nil {
		return fmt.Errorf("failed to delete existing block metrics: %w", err)
	}

	// Delete existing indexed block records in the range.
	query = "DELETE FROM indexed_blocks WHERE chain_id = $1 AND block_number >= $2 AND block_number <= $3"
	if _, err := tx.ExecContext(i.ctx, query, i.network.ChainID, startBlock, endBlock); err != nil {
		return fmt.Errorf("failed to delete existing indexed block records: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit reindex cleanup: %w", err)
	}

	// Same fence as handleReorg: a worker mid-flight with a pre-cleanup fetch
	// must not repopulate the range with the data this delete just purged.
	atomic.AddUint64(&i.reorgEpoch, 1)

	return nil
}

func (i *Indexer) runReindexRequestScanner() {
	logger.Info("Reindex request scanner starting", zap.String("network", i.network.Name))

	ticker := time.NewTicker(reindexRequestPollInterval)
	defer ticker.Stop()

	for {
		if !i.processNextReindexRequest() {
			select {
			case <-i.ctx.Done():
				logger.Info("Reindex request scanner stopped", zap.String("network", i.network.Name))
				return
			case <-ticker.C:
			}
		}
	}
}

func (i *Indexer) processNextReindexRequest() bool {
	request, err := i.claimNextReindexRequest()
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false
		}
		logger.Error("Failed to claim reindex request",
			zap.String("network", i.network.Name),
			zap.Error(err))
		return false
	}

	logger.Info("Processing reindex request",
		zap.String("network", i.network.Name),
		zap.Int64("request_id", request.ID),
		zap.Uint64("start_block", request.StartBlock),
		zap.Uint64("end_block", request.EndBlock),
		zap.Int("attempts", request.Attempts))

	stopHeartbeat := i.startReindexRequestHeartbeat(request)
	if err := i.Reindex(request.StartBlock, request.EndBlock); err != nil {
		stopHeartbeat()
		i.failReindexRequest(request, err)
		return true
	}
	stopHeartbeat()

	if err := i.waitForReindexRequestCompletion(request); err != nil {
		i.failReindexRequest(request, err)
		return true
	}

	if err := i.completeReindexRequest(request); err != nil {
		logger.Error("Failed to complete reindex request",
			zap.String("network", i.network.Name),
			zap.Int64("request_id", request.ID),
			zap.Error(err))
		return true
	}

	logger.Info("Reindex request completed",
		zap.String("network", i.network.Name),
		zap.Int64("request_id", request.ID),
		zap.Uint64("start_block", request.StartBlock),
		zap.Uint64("end_block", request.EndBlock))

	return true
}

func (i *Indexer) claimNextReindexRequest() (blockReindexRequest, error) {
	var request blockReindexRequest
	query := `
		WITH next_request AS (
			SELECT id
			FROM block_reindex_requests
			WHERE chain_id = $1
				AND (
					status = 'pending'
					OR (
						status = 'processing'
						AND updated_at < NOW() - ($3 * INTERVAL '1 second')
					)
				)
			ORDER BY requested_at, id
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE block_reindex_requests r
		SET
			status = 'processing',
			attempts = attempts + 1,
			claimed_by = $2,
			started_at = COALESCE(started_at, NOW()),
			completed_at = NULL,
			last_error = NULL,
			updated_at = NOW()
		FROM next_request
		WHERE r.id = next_request.id
		RETURNING r.id, r.chain_id, r.start_block, r.end_block, r.attempts, r.claimed_by
	`
	err := i.db.GetContext(
		i.ctx,
		&request,
		query,
		i.network.ChainID,
		i.reindexRequestClaimer(),
		int(reindexRequestStaleAfter.Seconds()),
	)
	if err != nil {
		return blockReindexRequest{}, err
	}
	return request, nil
}

func (i *Indexer) waitForReindexRequestCompletion(request blockReindexRequest) error {
	ticker := time.NewTicker(reindexRequestCompletionPollInterval)
	defer ticker.Stop()

	for {
		missing, err := i.countMissingIndexedBlocks(request.StartBlock, request.EndBlock)
		if err != nil {
			return err
		}
		if missing == 0 {
			return nil
		}

		if err := i.heartbeatReindexRequest(request); err != nil {
			return err
		}

		select {
		case <-i.ctx.Done():
			return i.ctx.Err()
		case <-ticker.C:
		}
	}
}

func (i *Indexer) startReindexRequestHeartbeat(request blockReindexRequest) func() {
	done := make(chan struct{})
	stopped := make(chan struct{})

	go func() {
		defer close(stopped)

		ticker := time.NewTicker(reindexRequestCompletionPollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-i.ctx.Done():
				return
			case <-ticker.C:
				if err := i.heartbeatReindexRequest(request); err != nil {
					logger.Error("Failed to heartbeat reindex request",
						zap.String("network", i.network.Name),
						zap.Int64("request_id", request.ID),
						zap.Error(err))
					return
				}
			}
		}
	}()

	return func() {
		close(done)
		<-stopped
	}
}

func (i *Indexer) countMissingIndexedBlocks(startBlock, endBlock uint64) (int64, error) {
	var missing int64
	query := `
		SELECT ($3::bigint - $2::bigint + 1) - COUNT(*)
		FROM indexed_blocks
		WHERE chain_id = $1
			AND block_number >= $2
			AND block_number <= $3
	`
	if err := i.db.GetContext(i.ctx, &missing, query, i.network.ChainID, startBlock, endBlock); err != nil {
		return 0, fmt.Errorf("failed to count missing reindex blocks: %w", err)
	}
	return missing, nil
}

func (i *Indexer) completeReindexRequest(request blockReindexRequest) error {
	query := `
		UPDATE block_reindex_requests
		SET status = 'completed',
			completed_at = NOW(),
			updated_at = NOW()
		WHERE id = $1
			AND chain_id = $2
			AND claimed_by = $3
			AND status = 'processing'
	`
	res, err := i.db.ExecContext(i.ctx, query, request.ID, i.network.ChainID, request.ClaimedBy)
	if err != nil {
		return fmt.Errorf("failed to mark reindex request complete: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to inspect completed reindex request update: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("failed to mark reindex request %d complete: request is no longer claimed by %q", request.ID, request.ClaimedBy)
	}
	return nil
}

func (i *Indexer) failReindexRequest(request blockReindexRequest, requestErr error) {
	query := `
		UPDATE block_reindex_requests
		SET status = 'failed',
			last_error = $3,
			completed_at = NOW(),
			updated_at = NOW()
		WHERE id = $1
			AND chain_id = $2
			AND claimed_by = $4
			AND status = 'processing'
	`
	res, err := i.db.ExecContext(i.ctx, query, request.ID, i.network.ChainID, requestErr.Error(), request.ClaimedBy)
	if err != nil {
		logger.Error("Failed to mark reindex request failed",
			zap.String("network", i.network.Name),
			zap.Int64("request_id", request.ID),
			zap.Error(err))
		return
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		logger.Warn("Reindex request failure ignored because claim was lost",
			zap.String("network", i.network.Name),
			zap.Int64("request_id", request.ID),
			zap.String("claimed_by", request.ClaimedBy))
	}
}

func (i *Indexer) heartbeatReindexRequest(request blockReindexRequest) error {
	query := `
		UPDATE block_reindex_requests
		SET updated_at = NOW()
		WHERE id = $1
			AND chain_id = $2
			AND claimed_by = $3
			AND status = 'processing'
	`
	res, err := i.db.ExecContext(i.ctx, query, request.ID, i.network.ChainID, request.ClaimedBy)
	if err != nil {
		return fmt.Errorf("failed to heartbeat reindex request: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to inspect reindex request heartbeat: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("reindex request %d is no longer claimed by %q", request.ID, request.ClaimedBy)
	}
	return nil
}

func (i *Indexer) reindexRequestClaimer() string {
	return fmt.Sprintf("blob-indexer/%s/%s", i.network.Name, i.indexerVersion)
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
			i.maybeCompleteReorgRecovery()
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
	if i.failedBlockNextRetry == nil {
		i.failedBlockNextRetry = make(map[uint64]time.Time)
	}

	now := time.Now()
	var toRetry []uint64
	var safetyRetryCount int
	var deferredCount int
	for block, count := range i.failedBlocks {
		if count <= maxGapScanRetries {
			toRetry = append(toRetry, block)
			continue
		}

		nextRetry, ok := i.failedBlockNextRetry[block]
		if !ok {
			nextRetry = now.Add(failedBlockSafetyRetryInterval)
			i.failedBlockNextRetry[block] = nextRetry
			deferredCount++
			logger.Warn("Block exceeded retry budget; scheduling safety-net retry",
				zap.String("network", i.network.Name),
				zap.Uint64("block", block),
				zap.Int("total_attempts", count),
				zap.Time("next_retry", nextRetry))
			continue
		}

		if now.Before(nextRetry) {
			deferredCount++
			continue
		}

		toRetry = append(toRetry, block)
		safetyRetryCount++
		i.failedBlockNextRetry[block] = now.Add(failedBlockSafetyRetryInterval)
	}
	i.failedBlocksMu.Unlock()

	if len(toRetry) == 0 {
		return
	}

	logger.Info("Gap scanner re-queuing failed blocks",
		zap.String("network", i.network.Name),
		zap.Int("count", len(toRetry)),
		zap.Int("safety_retry_count", safetyRetryCount),
		zap.Int("deferred_count", deferredCount))

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
			(SELECT COUNT(*) FROM blobs WHERE chain_id = $1) as confirmed_count,
			(SELECT COUNT(*) FROM mempool_blobs WHERE chain_id = $1) as pending_count
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
