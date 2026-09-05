package indexer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

const (
	// defaultPriorityFeeBackfillWindowBlocks is how many blocks one walk step
	// covers. A step is one keyed range scan for unpriced blocks plus the
	// fetches and updates for whatever it finds; the checkpoint advances per
	// step, so the width bounds how much a restart repeats.
	defaultPriorityFeeBackfillWindowBlocks int64 = 2000
	// defaultPriorityFeeBackfillUpdateBatch is how many blocks' transactions
	// one UPDATE statement carries. The blobs update triggers refresh chart
	// rollups per (bucket, sender) touched by a statement, so batching blocks
	// per statement is what keeps that refresh from running once per block.
	defaultPriorityFeeBackfillUpdateBatch = 100
	// defaultPriorityFeeBackfillFetchWorkers bounds concurrent block fetches
	// within a batch; the RPC client's own rate limit applies on top.
	defaultPriorityFeeBackfillFetchWorkers = 4
	// priorityFeeBackfillFetchAttempts is how many times one block fetch is
	// retried before the walk gives up for this process. The checkpoint means
	// the next start resumes at the failed window.
	priorityFeeBackfillFetchAttempts = 3
	// defaultPriorityFeeBackfillRetryBackoff scales the wait between fetch
	// and update retries.
	defaultPriorityFeeBackfillRetryBackoff = 2 * time.Second
	// priorityFeeBackfillProgressEvery is how many windows pass between
	// progress log lines; per-window lines would be thousands per network.
	priorityFeeBackfillProgressEvery = 50
)

// priorityFeeBackfillSettings tunes runPriorityFeeBackfill.
type priorityFeeBackfillSettings struct {
	enabled      bool
	pause        time.Duration
	windowBlocks int64
	updateBatch  int
	fetchWorkers int
	retryBackoff time.Duration
}

func newPriorityFeeBackfillSettings(cfg config.IndexerConfig) priorityFeeBackfillSettings {
	pause := cfg.PriorityFeeBackfillPause
	if pause < 0 {
		pause = 0
	}
	return priorityFeeBackfillSettings{
		enabled:      cfg.PriorityFeeBackfillEnabled,
		pause:        pause,
		windowBlocks: defaultPriorityFeeBackfillWindowBlocks,
		updateBatch:  defaultPriorityFeeBackfillUpdateBatch,
		fetchWorkers: defaultPriorityFeeBackfillFetchWorkers,
		retryBackoff: defaultPriorityFeeBackfillRetryBackoff,
	}
}

// runPriorityFeeBackfill fills max_priority_fee_per_gas, max_fee_per_gas, and
// priority_fee_per_gas onto blob rows indexed before migration 000015 stored
// them. It walks indexed history oldest first in fixed block windows, lists
// the blocks in each window that still hold an unpriced row, refetches those
// blocks with their transactions, and updates the rows in place.
//
// Unlike a reindex, nothing is deleted: charts and block pages keep serving
// the range while it is being filled, and the fee-only UPDATE is a no-op
// for the aggregate triggers (migration 000016 makes them return before any
// work when no column they read changed). The walk pauses after every
// update batch and every window so the
// extra RPC load stays modest next to live indexing.
//
// Progress checkpoints in indexer_metadata. The checkpoint only advances
// over a contiguous prefix of windows proven complete: after a window is
// processed it is listed again, and any block still unpriced (a fetch that
// failed every attempt, or a row whose transaction the fetched block did not
// carry) leaves the window incomplete. The walk carries on through later
// windows so one bad block cannot wedge the rest of history, but the
// checkpoint stays at the incomplete window so the next start retries it.
// A block whose blobs were reorged or reindexed between the listing and the
// update already carries fees and is skipped by the update's own guard.
func (i *Indexer) runPriorityFeeBackfill() {
	bounds, err := i.db.IndexedBlockBounds(i.ctx, i.network.ChainID)
	if err != nil {
		if i.ctx.Err() == nil {
			logger.Error("Failed to read indexed block bounds for priority fee backfill",
				zap.String("network", i.network.Name),
				zap.Error(err))
		}
		return
	}
	if !bounds.HasBlocks {
		logger.Debug("Skipping priority fee backfill: no indexed blocks",
			zap.String("network", i.network.Name))
		return
	}

	from := bounds.Min
	if resume, ok := i.priorityFeeBackfillWatermark(); ok && resume >= from {
		from = resume + 1
	}
	if from > bounds.Max {
		logger.Debug("Priority fee backfill already covers indexed history",
			zap.String("network", i.network.Name),
			zap.Int64("through_block", bounds.Max))
		return
	}

	logger.Info("Backfilling blob priority fees",
		zap.String("network", i.network.Name),
		zap.Int64("from_block", from),
		zap.Int64("to_block", bounds.Max),
		zap.Int64("window_blocks", i.priorityFeeBackfill.windowBlocks),
		zap.Duration("pause", i.priorityFeeBackfill.pause))

	began := time.Now()
	var windows, blocksFilled, rowsUpdated, incompleteWindows int64
	checkpoint := from - 1
	checkpointStalled := false
	for windowStart := from; windowStart <= bounds.Max; windowStart += i.priorityFeeBackfill.windowBlocks {
		windowEnd := windowStart + i.priorityFeeBackfill.windowBlocks - 1
		if windowEnd > bounds.Max {
			windowEnd = bounds.Max
		}

		complete, filled, updated, err := i.backfillPriorityFeeWindow(windowStart, windowEnd)
		blocksFilled += filled
		rowsUpdated += updated
		if err != nil {
			if i.ctx.Err() == nil {
				logger.Error("Blob priority fee backfill aborted; tip history stays partial until the next start",
					zap.String("network", i.network.Name),
					zap.Int64("window_start", windowStart),
					zap.Int64("checkpoint", checkpoint),
					zap.Error(err))
			}
			return
		}
		if !complete {
			incompleteWindows++
			if !checkpointStalled {
				checkpointStalled = true
				logger.Warn("Blob priority fee backfill window left unpriced rows; checkpoint holds here while the walk continues",
					zap.String("network", i.network.Name),
					zap.Int64("window_start", windowStart),
					zap.Int64("window_end", windowEnd))
			}
		}
		if !checkpointStalled {
			checkpoint = windowEnd
			i.setPriorityFeeBackfillWatermark(windowEnd)
		}

		windows++
		if windows%priorityFeeBackfillProgressEvery == 0 {
			logger.Info("Blob priority fee backfill progress",
				zap.String("network", i.network.Name),
				zap.Int64("through_block", windowEnd),
				zap.Int64("checkpoint", checkpoint),
				zap.Int64("to_block", bounds.Max),
				zap.Int64("blocks_filled", blocksFilled),
				zap.Int64("rows_updated", rowsUpdated),
				zap.Int64("incomplete_windows", incompleteWindows),
				zap.Duration("elapsed", time.Since(began)))
		}

		if windowEnd < bounds.Max && !i.pausePriorityFeeBackfill() {
			return
		}
	}

	if incompleteWindows > 0 {
		logger.Warn("Blob priority fee backfill walked all indexed history but left unpriced rows; the next start retries from the checkpoint",
			zap.String("network", i.network.Name),
			zap.Int64("checkpoint", checkpoint),
			zap.Int64("to_block", bounds.Max),
			zap.Int64("incomplete_windows", incompleteWindows),
			zap.Int64("blocks_filled", blocksFilled),
			zap.Int64("rows_updated", rowsUpdated),
			zap.Duration("took", time.Since(began)))
		return
	}
	logger.Info("Blob priority fee backfill complete",
		zap.String("network", i.network.Name),
		zap.Int64("through_block", bounds.Max),
		zap.Int64("blocks_filled", blocksFilled),
		zap.Int64("rows_updated", rowsUpdated),
		zap.Duration("took", time.Since(began)))
}

// backfillPriorityFeeWindow fills one window and reports whether it is now
// fully priced. A window that had nothing to fill is complete by
// construction; one that had work is listed again afterwards, so a block
// that could not be fetched or a row the fetched block did not account for
// is detected rather than assumed filled. The error return is reserved for
// database failures, which abort the walk; RPC failures only leave the
// window incomplete.
func (i *Indexer) backfillPriorityFeeWindow(windowStart, windowEnd int64) (complete bool, filled, updated int64, err error) {
	blocks, err := i.blocksMissingPriorityFees(windowStart, windowEnd)
	if err != nil {
		return false, 0, 0, err
	}
	if len(blocks) == 0 {
		return true, 0, 0, nil
	}

	updated, err = i.backfillPriorityFeeBlocks(blocks)
	if err != nil {
		return false, int64(len(blocks)), updated, err
	}

	remaining, err := i.blocksMissingPriorityFees(windowStart, windowEnd)
	if err != nil {
		return false, int64(len(blocks)), updated, err
	}
	filled = int64(len(blocks) - len(remaining))
	if len(remaining) > 0 {
		logger.Warn("Blob priority fee backfill left blocks unpriced",
			zap.String("network", i.network.Name),
			zap.Int64("window_start", windowStart),
			zap.Int64("window_end", windowEnd),
			zap.Int64s("blocks", remaining))
	}
	return len(remaining) == 0, filled, updated, nil
}

// pausePriorityFeeBackfill waits the configured pause, returning false when
// the indexer stopped meanwhile.
func (i *Indexer) pausePriorityFeeBackfill() bool {
	if i.priorityFeeBackfill.pause <= 0 {
		return i.ctx.Err() == nil
	}
	select {
	case <-i.ctx.Done():
		return false
	case <-time.After(i.priorityFeeBackfill.pause):
		return true
	}
}

// blocksMissingPriorityFees lists the window's unpriced blocks, retrying a
// transient database error a few times before giving up on the walk.
func (i *Indexer) blocksMissingPriorityFees(windowStart, windowEnd int64) ([]int64, error) {
	var (
		blocks []int64
		err    error
	)
	for attempt := 1; attempt <= priorityFeeBackfillFetchAttempts; attempt++ {
		blocks, err = i.db.BlocksMissingPriorityFees(i.ctx, i.network.ChainID, windowStart, windowEnd)
		if err == nil || i.ctx.Err() != nil {
			return blocks, err
		}
		if !i.waitPriorityFeeBackfillRetry(attempt, "listing unpriced blocks", windowStart, err) {
			return nil, err
		}
	}
	return nil, err
}

// backfillPriorityFeeBlocks refetches the given blocks in batches and writes
// their blob transactions' fees, pausing between batches. It returns the
// number of rows updated. A block whose fetch fails every attempt is skipped
// (the window's recheck reports it); a database failure is returned.
func (i *Indexer) backfillPriorityFeeBlocks(blocks []int64) (int64, error) {
	var total int64
	batchSize := i.priorityFeeBackfill.updateBatch
	if batchSize <= 0 {
		batchSize = defaultPriorityFeeBackfillUpdateBatch
	}
	for start := 0; start < len(blocks); start += batchSize {
		end := start + batchSize
		if end > len(blocks) {
			end = len(blocks)
		}
		updates := i.fetchPriorityFeeUpdates(blocks[start:end])
		if i.ctx.Err() != nil {
			return total, i.ctx.Err()
		}
		updated, err := i.writePriorityFeeUpdates(updates)
		if err != nil {
			return total, err
		}
		total += updated
		if end < len(blocks) && !i.pausePriorityFeeBackfill() {
			return total, i.ctx.Err()
		}
	}
	return total, nil
}

// fetchPriorityFeeUpdates fetches a batch of blocks concurrently and derives
// the fee columns for every blob transaction they carry, in block order. A
// block that fails every fetch attempt contributes nothing; its rows stay
// unpriced and the window recheck holds the checkpoint on it, so the batch
// still lands for the blocks that did fetch rather than losing them all.
func (i *Indexer) fetchPriorityFeeUpdates(blocks []int64) []db.BlobPriorityFeeUpdate {
	workers := i.priorityFeeBackfill.fetchWorkers
	if workers <= 0 {
		workers = 1
	}
	if workers > len(blocks) {
		workers = len(blocks)
	}

	ordered := make([][]db.BlobPriorityFeeUpdate, len(blocks))
	tasks := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range tasks {
				updates, err := i.fetchBlockPriorityFees(i.ctx, uint64(blocks[index]))
				if err != nil {
					if i.ctx.Err() == nil {
						logger.Warn("Skipping block in priority fee backfill after repeated fetch failures",
							zap.String("network", i.network.Name),
							zap.Int64("block", blocks[index]),
							zap.Error(err))
					}
					continue
				}
				ordered[index] = updates
			}
		}()
	}
	for index := range blocks {
		select {
		case tasks <- index:
		case <-i.ctx.Done():
		}
	}
	close(tasks)
	wg.Wait()

	var updates []db.BlobPriorityFeeUpdate
	for _, blockUpdates := range ordered {
		updates = append(updates, blockUpdates...)
	}
	return updates
}

// fetchBlockPriorityFees fetches one block, retrying transient failures, and
// returns the fee columns for each blob transaction in it. The derivation is
// the one processBlock applies to new blocks, so backfilled rows match rows
// indexed live.
func (i *Indexer) fetchBlockPriorityFees(ctx context.Context, blockNumber uint64) ([]db.BlobPriorityFeeUpdate, error) {
	var (
		block *types.Block
		err   error
	)
	for attempt := 1; attempt <= priorityFeeBackfillFetchAttempts; attempt++ {
		block, err = i.ethClient.GetBlockByNumber(ctx, blockNumber)
		if err == nil || ctx.Err() != nil {
			break
		}
		if !i.waitPriorityFeeBackfillRetry(attempt, "fetching block", int64(blockNumber), err) {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("failed to fetch block %d for priority fee backfill: %w", blockNumber, err)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	return blockPriorityFeeUpdates(block, i.ethClient.IsBlobTransaction), nil
}

// blockPriorityFeeUpdates derives one update per blob transaction in block.
// A block without an execution base fee cannot carry blobs (blobs postdate
// London), but is handled by recording only the caps and leaving the paid
// fee empty, which UpdateBlobPriorityFees stores as NULL, matching what the
// live path stores for such a transaction.
func blockPriorityFeeUpdates(block *types.Block, isBlobTx func(*types.Transaction) bool) []db.BlobPriorityFeeUpdate {
	header := block.Header()
	var updates []db.BlobPriorityFeeUpdate
	for _, tx := range block.Transactions() {
		if !isBlobTx(tx) || len(tx.BlobHashes()) == 0 {
			continue
		}
		update := db.BlobPriorityFeeUpdate{
			BlockNumber:          header.Number.Int64(),
			TxHash:               tx.Hash().Hex(),
			MaxPriorityFeePerGas: tx.GasTipCap().String(),
			MaxFeePerGas:         tx.GasFeeCap().String(),
		}
		if header.BaseFee != nil {
			update.PriorityFeePerGas = effectivePriorityFeePerGas(tx, header.BaseFee).String()
		}
		updates = append(updates, update)
	}
	return updates
}

// writePriorityFeeUpdates applies one batch under the network write lock,
// so the rollup refresh its triggers run cannot interleave with this
// indexer's own block inserts on the same buckets.
func (i *Indexer) writePriorityFeeUpdates(updates []db.BlobPriorityFeeUpdate) (int64, error) {
	if len(updates) == 0 {
		return 0, nil
	}
	var (
		updated int64
		err     error
	)
	for attempt := 1; attempt <= priorityFeeBackfillFetchAttempts; attempt++ {
		unlockWrites := i.lockDBWrites()
		updated, err = i.db.UpdateBlobPriorityFees(i.ctx, i.network.ChainID, updates)
		unlockWrites()
		if err == nil || i.ctx.Err() != nil {
			return updated, err
		}
		if !i.waitPriorityFeeBackfillRetry(attempt, "writing priority fees", updates[0].BlockNumber, err) {
			break
		}
	}
	return 0, err
}

// waitPriorityFeeBackfillRetry logs a failed attempt and waits before the
// next one. It returns false when no attempt remains or the indexer is
// stopping.
func (i *Indexer) waitPriorityFeeBackfillRetry(attempt int, step string, block int64, err error) bool {
	if attempt >= priorityFeeBackfillFetchAttempts {
		return false
	}
	logger.Warn("Retrying priority fee backfill step",
		zap.String("network", i.network.Name),
		zap.String("step", step),
		zap.Int64("block", block),
		zap.Int("attempt", attempt),
		zap.Error(err))
	select {
	case <-i.ctx.Done():
		return false
	case <-time.After(time.Duration(attempt) * i.priorityFeeBackfill.retryBackoff):
		return true
	}
}

// priorityFeeBackfillWatermark reads the highest block a previous run
// completed. Absent or unparsable means "start from the beginning", which
// repeats cheap window scans but never leaves rows behind.
func (i *Indexer) priorityFeeBackfillWatermark() (int64, bool) {
	value, err := i.db.GetNetworkMetadata(i.ctx, i.network.ChainID, models.MetadataPriorityFeeBackfillBlock)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) && i.ctx.Err() == nil {
			logger.Warn("Failed to read priority fee backfill watermark; walking from the earliest indexed block",
				zap.String("network", i.network.Name),
				zap.Error(err))
		}
		return 0, false
	}
	block, parseErr := strconv.ParseInt(value, 10, 64)
	if parseErr != nil {
		logger.Warn("Ignoring unparsable priority fee backfill watermark",
			zap.String("network", i.network.Name),
			zap.String("value", value),
			zap.Error(parseErr))
		return 0, false
	}
	return block, true
}

// setPriorityFeeBackfillWatermark checkpoints a completed window. A failed
// write only costs a repeated window on the next start.
func (i *Indexer) setPriorityFeeBackfillWatermark(block int64) {
	// Under Indexer.mu like every other metadata write.
	i.mu.Lock()
	err := i.db.SetNetworkMetadata(i.ctx, i.network.ChainID,
		models.MetadataPriorityFeeBackfillBlock, strconv.FormatInt(block, 10))
	i.mu.Unlock()
	if err != nil && i.ctx.Err() == nil {
		logger.Warn("Failed to checkpoint priority fee backfill; the next start repeats this window",
			zap.String("network", i.network.Name),
			zap.Int64("block", block),
			zap.Error(err))
	}
}
