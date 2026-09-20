package indexer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

const (
	// defaultBuilderBackfillWindowBlocks is how many blocks one walk step
	// covers. A step is one anti-join over indexed_blocks plus the fetches
	// and inserts for whatever it finds; the floor descends per step, so
	// the width bounds how much a restart repeats.
	defaultBuilderBackfillWindowBlocks int64 = 2000
	// defaultBuilderBackfillInsertBatch is how many blocks one write
	// transaction carries. Builder rows fire no triggers, but the blob
	// tx_index update in the same transaction touches the blobs table, so
	// batching keeps the number of statements down either way.
	defaultBuilderBackfillInsertBatch = 100
	// defaultBuilderBackfillFetchWorkers bounds concurrent block fetches
	// within a batch; the RPC client's own rate limit applies on top.
	defaultBuilderBackfillFetchWorkers = 4
	// builderBackfillFetchAttempts is how many times one fetch or write is
	// retried before the walk gives up on it for this process. The floor
	// means the next start resumes at the failed window.
	builderBackfillFetchAttempts = 3
	// defaultBuilderBackfillRetryBackoff scales the wait between retries.
	defaultBuilderBackfillRetryBackoff = 2 * time.Second
	// builderBackfillProgressEvery is how many windows pass between progress
	// log lines; per-window lines would be thousands per network.
	builderBackfillProgressEvery = 50
)

// builderBackfillSettings tunes runBuilderBackfill.
type builderBackfillSettings struct {
	enabled      bool
	pause        time.Duration
	windowBlocks int64
	insertBatch  int
	fetchWorkers int
	retryBackoff time.Duration
	// blockSource fetches one block with its transactions. A field so tests
	// can serve canned blocks; nil means the indexer's RPC client, which is
	// what production always uses.
	blockSource func(context.Context, uint64) (*types.Block, error)
}

func newBuilderBackfillSettings(cfg config.IndexerConfig) builderBackfillSettings {
	pause := cfg.BuilderBackfillPause
	if pause < 0 {
		pause = 0
	}
	return builderBackfillSettings{
		enabled:      cfg.BuilderBackfillEnabled,
		pause:        pause,
		windowBlocks: defaultBuilderBackfillWindowBlocks,
		insertBatch:  defaultBuilderBackfillInsertBatch,
		fetchWorkers: defaultBuilderBackfillFetchWorkers,
		retryBackoff: defaultBuilderBackfillRetryBackoff,
	}
}

// runBuilderBackfill gives blocks indexed before migration 000017 the
// block_builders row live indexing now writes, and fills blobs.tx_index for
// their blob rows from the same fetch. It walks indexed history newest first
// in fixed block windows, lists the blocks in each window with no builder
// row, refetches them with their transactions, and inserts.
//
// Newest first because the API only serves builder statistics over bounded
// recent windows (30 days at most): filling the tail of history first makes
// those windows complete within hours of a deploy, while the years of older
// blocks nobody queries follow behind. The walk can start at the indexed tip
// without a search for where the gap begins: live indexing writes a block's
// builder row in the same transaction as the block itself, and every reorg
// or reindex cleanup deletes both together, so a window of live blocks lists
// nothing and costs one primary-key anti-join.
//
// Nothing is deleted and no existing row is overwritten: the insert is
// ON CONFLICT DO NOTHING, so a live insert that raced ahead keeps its
// candidate snapshot, and the tx_index update only fills rows that have
// none. Backfilled rows carry candidate_snapshot = false with NULL
// aggregates, because the pending pool a historical block faced is gone —
// "not observed" rather than "nothing was pending". The candidates table is
// never touched.
//
// Progress checkpoints in indexer_metadata as a floor: the lowest block such
// that every indexed block from it up to the tip has been verified to carry
// a builder row. The floor only descends over a contiguous run of windows
// proven complete: after a window is processed it is listed again, and any
// block still without a builder row (a fetch that failed every attempt, or a
// batch a reorg cleanup invalidated in flight) leaves the window incomplete.
// The walk carries on through lower windows so one bad block cannot wedge
// the rest of history, but the floor stays above the incomplete window so
// the next start resumes exactly there. A restart never walks above the
// floor: the blocks live indexing wrote meanwhile already have rows and are
// neither listed nor fetched again.
//
// The floor is a different metadata key from the checkpoint the oldest-first
// walk of earlier releases kept (the highest block of a complete prefix).
// The two mean opposite things, so the old key is deleted the first time a
// network starts without a floor rather than reinterpreted; the prefix it
// covered is walked again at one empty listing per window.
//
// indexer.builder_backfill_enabled turns the whole walk off; the gate lives
// here rather than at the call site because the walk is chained onto the
// relabel pass's goroutine, which is not itself optional.
func (i *Indexer) runBuilderBackfill() {
	if !i.builderBackfill.enabled {
		logger.Debug("Skipping builder backfill: disabled by configuration",
			zap.String("network", i.network.Name))
		return
	}

	bounds, err := i.db.IndexedBlockBounds(i.ctx, i.network.ChainID)
	if err != nil {
		if i.ctx.Err() == nil {
			logger.Error("Failed to read indexed block bounds for builder backfill",
				zap.String("network", i.network.Name),
				zap.Error(err))
		}
		return
	}
	if !bounds.HasBlocks {
		logger.Debug("Skipping builder backfill: no indexed blocks",
			zap.String("network", i.network.Name))
		return
	}

	// A zero or negative window would never advance the walk's loop
	// variable, so unset tuning falls back to the package default.
	windowBlocks := i.builderBackfill.windowBlocks
	if windowBlocks <= 0 {
		windowBlocks = defaultBuilderBackfillWindowBlocks
	}

	top := bounds.Max
	if floor, ok := i.builderBackfillFloor(); ok {
		// A floor above the tip describes history a reindex has since
		// removed; the walk starts over from the tip and lowers it.
		if floor <= bounds.Max {
			top = floor - 1
		}
	} else {
		i.retireAscendingBuilderBackfillCheckpoint()
	}
	if top < bounds.Min {
		logger.Debug("Builder backfill already covers indexed history",
			zap.String("network", i.network.Name),
			zap.Int64("down_to_block", bounds.Min))
		return
	}

	logger.Info("Backfilling block builders newest first",
		zap.String("network", i.network.Name),
		zap.Int64("from_block", top),
		zap.Int64("down_to_block", bounds.Min),
		zap.Int64("window_blocks", windowBlocks),
		zap.Duration("pause", i.builderBackfill.pause))

	began := time.Now()
	var windows, blocksFilled, rowsInserted, blobsIndexed, incompleteWindows int64
	floor := top + 1
	floorStalled := false
	for windowEnd := top; windowEnd >= bounds.Min; windowEnd -= windowBlocks {
		windowStart := windowEnd - windowBlocks + 1
		if windowStart < bounds.Min {
			windowStart = bounds.Min
		}

		complete, filled, inserted, indexed, err := i.backfillBuilderWindow(windowStart, windowEnd)
		blocksFilled += filled
		rowsInserted += inserted
		blobsIndexed += indexed
		if err != nil {
			if i.ctx.Err() == nil {
				logger.Error("Block builder backfill aborted; history stays partial until the next start",
					zap.String("network", i.network.Name),
					zap.Int64("window_end", windowEnd),
					zap.Int64("floor", floor),
					zap.Error(err))
			}
			return
		}
		if !complete {
			incompleteWindows++
			if !floorStalled {
				floorStalled = true
				logger.Warn("Block builder backfill window left blocks without a builder row; floor holds here while the walk continues",
					zap.String("network", i.network.Name),
					zap.Int64("window_start", windowStart),
					zap.Int64("window_end", windowEnd))
			}
		}
		if !floorStalled {
			floor = windowStart
			i.setBuilderBackfillFloor(windowStart)
		}

		windows++
		if windows%builderBackfillProgressEvery == 0 {
			logger.Info("Block builder backfill progress",
				zap.String("network", i.network.Name),
				zap.Int64("reached_block", windowStart),
				zap.Int64("floor", floor),
				zap.Int64("down_to_block", bounds.Min),
				zap.Int64("blocks_filled", blocksFilled),
				zap.Int64("rows_inserted", rowsInserted),
				zap.Int64("blobs_indexed", blobsIndexed),
				zap.Int64("incomplete_windows", incompleteWindows),
				zap.Duration("elapsed", time.Since(began)))
		}

		if windowStart > bounds.Min && !i.pauseBuilderBackfill() {
			return
		}
	}

	if incompleteWindows > 0 {
		logger.Warn("Block builder backfill walked all indexed history but left blocks without a builder row; the next start resumes below the floor",
			zap.String("network", i.network.Name),
			zap.Int64("floor", floor),
			zap.Int64("down_to_block", bounds.Min),
			zap.Int64("incomplete_windows", incompleteWindows),
			zap.Int64("blocks_filled", blocksFilled),
			zap.Int64("rows_inserted", rowsInserted),
			zap.Int64("blobs_indexed", blobsIndexed),
			zap.Duration("took", time.Since(began)))
		return
	}
	logger.Info("Block builder backfill complete",
		zap.String("network", i.network.Name),
		zap.Int64("down_to_block", bounds.Min),
		zap.Int64("blocks_filled", blocksFilled),
		zap.Int64("rows_inserted", rowsInserted),
		zap.Int64("blobs_indexed", blobsIndexed),
		zap.Duration("took", time.Since(began)))
}

// backfillBuilderWindow fills one window and reports whether every block in
// it now has a builder row. A window that had nothing to fill is complete by
// construction; one that had work is listed again afterwards, so a block
// that could not be fetched is detected rather than assumed filled. The
// error return is reserved for database failures, which abort the walk; RPC
// failures only leave the window incomplete.
func (i *Indexer) backfillBuilderWindow(windowStart, windowEnd int64) (complete bool, filled, inserted, indexed int64, err error) {
	blocks, err := i.blocksMissingBlockBuilders(windowStart, windowEnd)
	if err != nil {
		return false, 0, 0, 0, err
	}
	if len(blocks) == 0 {
		return true, 0, 0, 0, nil
	}

	inserted, indexed, err = i.backfillBuilderBlocks(blocks)
	if err != nil {
		return false, int64(len(blocks)), inserted, indexed, err
	}

	remaining, err := i.blocksMissingBlockBuilders(windowStart, windowEnd)
	if err != nil {
		return false, int64(len(blocks)), inserted, indexed, err
	}
	filled = int64(len(blocks) - len(remaining))
	if len(remaining) > 0 {
		logger.Warn("Block builder backfill left blocks without a builder row",
			zap.String("network", i.network.Name),
			zap.Int64("window_start", windowStart),
			zap.Int64("window_end", windowEnd),
			zap.Int64s("blocks", missingBlockNumbers(remaining)))
	}
	return len(remaining) == 0, filled, inserted, indexed, nil
}

// pauseBuilderBackfill waits the configured pause, returning false when the
// indexer stopped meanwhile.
func (i *Indexer) pauseBuilderBackfill() bool {
	if i.builderBackfill.pause <= 0 {
		return i.ctx.Err() == nil
	}
	select {
	case <-i.ctx.Done():
		return false
	case <-time.After(i.builderBackfill.pause):
		return true
	}
}

// blocksMissingBlockBuilders lists the window's builder-less blocks, retrying
// a transient database error a few times before giving up on the walk.
func (i *Indexer) blocksMissingBlockBuilders(windowStart, windowEnd int64) ([]db.MissingBuilderBlock, error) {
	var (
		blocks []db.MissingBuilderBlock
		err    error
	)
	for attempt := 1; attempt <= builderBackfillFetchAttempts; attempt++ {
		blocks, err = i.db.BlocksMissingBlockBuilders(i.ctx, i.network.ChainID, windowStart, windowEnd)
		if err == nil || i.ctx.Err() != nil {
			return blocks, err
		}
		if !i.waitBuilderBackfillRetry(attempt, "listing blocks without a builder row", windowStart, err) {
			return nil, err
		}
	}
	return nil, err
}

// backfillBuilderBlocks refetches the given blocks in batches and writes
// their builder rows and blob transaction positions, pausing between
// batches. A block whose fetch fails every attempt is skipped (the window's
// recheck reports it); a database failure is returned.
func (i *Indexer) backfillBuilderBlocks(blocks []db.MissingBuilderBlock) (inserted, indexed int64, err error) {
	batchSize := i.builderBackfill.insertBatch
	if batchSize <= 0 {
		batchSize = defaultBuilderBackfillInsertBatch
	}
	for start := 0; start < len(blocks); start += batchSize {
		end := start + batchSize
		if end > len(blocks) {
			end = len(blocks)
		}
		// Sample the cleanup epoch before the fetches, as processBlock does:
		// a reorg rewind or reindex delete committing while this batch is in
		// flight invalidates it, and the write is refused rather than
		// resurrecting rows for an abandoned fork.
		fetchEpoch := atomic.LoadUint64(&i.reorgEpoch)
		rows, txIndexes := i.fetchBuilderBackfillBatch(blocks[start:end])
		if i.ctx.Err() != nil {
			return inserted, indexed, i.ctx.Err()
		}
		batchInserted, batchIndexed, writeErr := i.writeBuilderBackfillBatch(rows, txIndexes, fetchEpoch)
		inserted += batchInserted
		indexed += batchIndexed
		if writeErr != nil {
			return inserted, indexed, writeErr
		}
		if end < len(blocks) && !i.pauseBuilderBackfill() {
			return inserted, indexed, i.ctx.Err()
		}
	}
	return inserted, indexed, nil
}

// missingBlockNumbers projects the block numbers out of a missing-row
// listing, for log fields that name the blocks rather than their hashes.
func missingBlockNumbers(blocks []db.MissingBuilderBlock) []int64 {
	numbers := make([]int64, 0, len(blocks))
	for _, block := range blocks {
		numbers = append(numbers, block.BlockNumber)
	}
	return numbers
}

// fetchBuilderBackfillBatch fetches a batch of blocks concurrently and
// derives their builder rows and blob transaction positions, in block order.
// A block that fails every fetch attempt contributes nothing; it stays
// builder-less and the window recheck holds the floor above it, so the
// batch still lands for the blocks that did fetch rather than losing them
// all.
//
// A block the node answers with a different hash than indexed_blocks holds
// is treated exactly like a failed fetch. The backfill addresses blocks by
// number, so a reorg the live path has not yet cleaned up would otherwise
// pair one fork's builder — and its transaction positions — with another
// fork's stored metrics and blobs. Skipping leaves the window incomplete,
// the floor where it is, and the block to the next pass, by which time
// the live reorg handling has resolved which fork is canonical.
func (i *Indexer) fetchBuilderBackfillBatch(blocks []db.MissingBuilderBlock) ([]models.BlockBuilder, []db.BlobTxIndexUpdate) {
	workers := i.builderBackfill.fetchWorkers
	if workers <= 0 {
		workers = 1
	}
	if workers > len(blocks) {
		workers = len(blocks)
	}

	rows := make([]*models.BlockBuilder, len(blocks))
	positions := make([][]db.BlobTxIndexUpdate, len(blocks))
	tasks := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range tasks {
				want := blocks[index]
				block, err := i.fetchBuilderBackfillBlock(i.ctx, uint64(want.BlockNumber))
				if err != nil {
					if i.ctx.Err() == nil {
						logger.Warn("Skipping block in builder backfill after repeated fetch failures",
							zap.String("network", i.network.Name),
							zap.Int64("block", want.BlockNumber),
							zap.Error(err))
					}
					continue
				}
				if got := block.Hash().Hex(); !strings.EqualFold(got, want.BlockHash) {
					logger.Debug("Skipping block in builder backfill: the node answered with a different hash than the one indexed",
						zap.String("network", i.network.Name),
						zap.Int64("block", want.BlockNumber),
						zap.String("indexed_hash", want.BlockHash),
						zap.String("fetched_hash", got))
					continue
				}
				rows[index] = i.blockBuilderRow(block, builderBackfillBlockTimestamp(block))
				positions[index] = blobTxIndexUpdates(block)
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

	builderRows := make([]models.BlockBuilder, 0, len(blocks))
	var txIndexes []db.BlobTxIndexUpdate
	for index, row := range rows {
		if row == nil {
			continue
		}
		builderRows = append(builderRows, *row)
		txIndexes = append(txIndexes, positions[index]...)
	}
	return builderRows, txIndexes
}

// fetchBuilderBackfillBlock fetches one block with its transactions,
// retrying transient failures. The full block is needed, not just the
// header: the proposer payment heuristic reads the last transaction and its
// recovered sender, and tx_index needs every transaction's position.
func (i *Indexer) fetchBuilderBackfillBlock(ctx context.Context, blockNumber uint64) (*types.Block, error) {
	fetch := i.builderBackfill.blockSource
	if fetch == nil {
		fetch = i.ethClient.GetBlockByNumber
	}
	var (
		block *types.Block
		err   error
	)
	for attempt := 1; attempt <= builderBackfillFetchAttempts; attempt++ {
		block, err = fetch(ctx, blockNumber)
		if err == nil || ctx.Err() != nil {
			break
		}
		if !i.waitBuilderBackfillRetry(attempt, "fetching block", int64(blockNumber), err) {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("failed to fetch block %d for builder backfill: %w", blockNumber, err)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if block == nil {
		return nil, fmt.Errorf("block %d returned no body for builder backfill", blockNumber)
	}
	return block, nil
}

// builderBackfillBlockTimestamp renders a block's time the way
// ethereum.Client.GetBlockTimestamp does, without needing a client: the
// backfill's block source is injectable, so a test can run the whole walk
// with no RPC endpoint at all.
func builderBackfillBlockTimestamp(block *types.Block) time.Time {
	return time.Unix(int64(block.Time()), 0).UTC()
}

// blobTxIndexUpdates records the position of every blob transaction in the
// block. Only blob transactions have blobs rows to update; the predicate
// matches ethereum.Client.IsBlobTransaction, with the same empty-hash guard
// the fee backfill applies.
func blobTxIndexUpdates(block *types.Block) []db.BlobTxIndexUpdate {
	number := block.Number().Int64()
	var updates []db.BlobTxIndexUpdate
	for index, tx := range block.Transactions() {
		if tx.Type() != types.BlobTxType || len(tx.BlobHashes()) == 0 {
			continue
		}
		updates = append(updates, db.BlobTxIndexUpdate{
			BlockNumber: number,
			TxHash:      tx.Hash().Hex(),
			TxIndex:     index,
		})
	}
	return updates
}

// writeBuilderBackfillBatch applies one batch under the network write lock,
// like every other write path, so the blobs update cannot interleave with
// this indexer's own block inserts. A batch the epoch check rejects is
// dropped without error: the window recheck sees those blocks again and
// holds the floor above them.
func (i *Indexer) writeBuilderBackfillBatch(rows []models.BlockBuilder, txIndexes []db.BlobTxIndexUpdate, fetchEpoch uint64) (inserted, indexed int64, err error) {
	if len(rows) == 0 {
		return 0, 0, nil
	}
	for attempt := 1; attempt <= builderBackfillFetchAttempts; attempt++ {
		unlockWrites := i.lockDBWrites()
		if atomic.LoadUint64(&i.reorgEpoch) != fetchEpoch {
			unlockWrites()
			logger.Warn("Discarding builder backfill batch invalidated by a reorg cleanup",
				zap.String("network", i.network.Name),
				zap.Int64("first_block", rows[0].BlockNumber),
				zap.Int("blocks", len(rows)))
			return 0, 0, nil
		}
		inserted, indexed, err = i.db.InsertBackfilledBlockBuilders(i.ctx, i.network.ChainID, rows, txIndexes)
		unlockWrites()
		if err == nil {
			return inserted, indexed, nil
		}
		if i.ctx.Err() != nil {
			return 0, 0, err
		}
		if !i.waitBuilderBackfillRetry(attempt, "writing builder rows", rows[0].BlockNumber, err) {
			break
		}
	}
	return 0, 0, err
}

// waitBuilderBackfillRetry logs a failed attempt and waits before the next
// one. It returns false when no attempt remains or the indexer is stopping.
func (i *Indexer) waitBuilderBackfillRetry(attempt int, step string, block int64, err error) bool {
	if attempt >= builderBackfillFetchAttempts {
		return false
	}
	logger.Warn("Retrying builder backfill step",
		zap.String("network", i.network.Name),
		zap.String("step", step),
		zap.Int64("block", block),
		zap.Int("attempt", attempt),
		zap.Error(err))
	select {
	case <-i.ctx.Done():
		return false
	case <-time.After(time.Duration(attempt) * i.builderBackfill.retryBackoff):
		return true
	}
}

// builderBackfillFloor reads the lowest block a previous run verified,
// together with everything above it. Absent or unparsable means "start from
// the tip", which repeats cheap window listings but never leaves blocks
// behind.
func (i *Indexer) builderBackfillFloor() (int64, bool) {
	value, err := i.db.GetNetworkMetadata(i.ctx, i.network.ChainID, models.MetadataBlockBuilderBackfillFloor)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) && i.ctx.Err() == nil {
			logger.Warn("Failed to read builder backfill floor; walking from the newest indexed block",
				zap.String("network", i.network.Name),
				zap.Error(err))
		}
		return 0, false
	}
	block, parseErr := strconv.ParseInt(value, 10, 64)
	if parseErr != nil {
		logger.Warn("Ignoring unparsable builder backfill floor",
			zap.String("network", i.network.Name),
			zap.String("value", value),
			zap.Error(parseErr))
		return 0, false
	}
	return block, true
}

// setBuilderBackfillFloor checkpoints a completed window by its lowest
// block. A failed write only costs a repeated window on the next start.
func (i *Indexer) setBuilderBackfillFloor(block int64) {
	// Under Indexer.mu like every other metadata write.
	i.mu.Lock()
	err := i.db.SetNetworkMetadata(i.ctx, i.network.ChainID,
		models.MetadataBlockBuilderBackfillFloor, strconv.FormatInt(block, 10))
	i.mu.Unlock()
	if err != nil && i.ctx.Err() == nil {
		logger.Warn("Failed to checkpoint builder backfill floor; the next start repeats this window",
			zap.String("network", i.network.Name),
			zap.Int64("block", block),
			zap.Error(err))
	}
}

// retireAscendingBuilderBackfillCheckpoint deletes the checkpoint the
// oldest-first walk of earlier releases kept. That key recorded the highest
// block of a complete prefix of history — the opposite of the floor — so it
// cannot seed the descending walk and must not be mistaken for it. It is
// only consulted when no floor exists yet, so a network pays the delete
// once; a failure is logged and the stale key is retried next start.
func (i *Indexer) retireAscendingBuilderBackfillCheckpoint() {
	i.mu.Lock()
	deleted, err := i.db.DeleteNetworkMetadata(i.ctx, i.network.ChainID, models.MetadataBlockBuilderBackfillBlock)
	i.mu.Unlock()
	if err != nil {
		if i.ctx.Err() == nil {
			logger.Warn("Failed to retire the oldest-first builder backfill checkpoint",
				zap.String("network", i.network.Name),
				zap.String("key", models.MetadataBlockBuilderBackfillBlock),
				zap.Error(err))
		}
		return
	}
	if deleted {
		logger.Info("Retired the oldest-first builder backfill checkpoint; the walk now runs newest first from the tip",
			zap.String("network", i.network.Name),
			zap.String("key", models.MetadataBlockBuilderBackfillBlock))
	}
}
