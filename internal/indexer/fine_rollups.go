package indexer

import (
	"time"

	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/db"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

const (
	// fineRollupBackfillChunk is how much raw history one backfill statement
	// covers. Chunks keep each statement fast and interruptible, per the
	// no-heavy-backfills-in-migrations rule (internal/db/migrations/README.md).
	fineRollupBackfillChunk = time.Hour

	// defaultFineRollupPruneInterval is how often expired fine rollup buckets
	// are pruned. Retention is db.FineChartRollupRetention.
	defaultFineRollupPruneInterval = 15 * time.Minute
)

// runFineRollupMaintenance backfills the fine (60s) chart rollup buckets for
// the retention window on startup, then prunes expired fine buckets on a
// timer. The statement triggers maintain fine buckets for new writes; this
// loop covers rows indexed before the fine bucket existed (or while this
// indexer was down) and enforces retention.
func (i *Indexer) runFineRollupMaintenance() {
	logger.Info("Fine chart rollup maintenance starting",
		zap.String("network", i.network.Name),
		zap.Duration("retention", db.FineChartRollupRetention),
		zap.Duration("prune_interval", i.fineRollupPruneInterval))

	i.backfillFineRollups()

	ticker := time.NewTicker(i.fineRollupPruneInterval)
	defer ticker.Stop()

	for {
		select {
		case <-i.ctx.Done():
			logger.Info("Fine chart rollup maintenance stopped", zap.String("network", i.network.Name))
			return
		case <-ticker.C:
			cutoff := time.Now().UTC().Add(-db.FineChartRollupRetention)
			deleted, err := i.db.PruneFineChartRollups(i.ctx, i.network.ChainID, cutoff)
			if err != nil {
				if i.ctx.Err() == nil {
					logger.Error("Failed to prune fine chart rollups",
						zap.String("network", i.network.Name),
						zap.Error(err))
				}
				continue
			}
			if deleted > 0 {
				logger.Debug("Pruned expired fine chart rollups",
					zap.String("network", i.network.Name),
					zap.Int64("deleted_rows", deleted))
			}
		}
	}
}

// backfillFineRollups recomputes fine rollup buckets across the retention
// window in bucket-aligned chunks, oldest first. Each chunk holds the network
// write lock so the full-replace upserts cannot race this indexer's own
// trigger-driven rollup increments. Re-running is safe (recompute-and-replace),
// so a failed run just leaves the remaining coverage gap for the API's raw
// fallback until the next restart.
func (i *Indexer) backfillFineRollups() {
	bucket := db.FineChartRollupBucketDuration
	// End past the in-progress bucket so backfill and live triggers meet with
	// no gap; the write lock keeps the two from interleaving on that bucket.
	end := time.Now().UTC().Truncate(bucket).Add(bucket)
	start := end.Add(-db.FineChartRollupRetention)

	logger.Info("Backfilling fine chart rollups",
		zap.String("network", i.network.Name),
		zap.Time("start", start),
		zap.Time("end", end))

	began := time.Now()
	for chunkStart := start; chunkStart.Before(end); {
		chunkEnd := chunkStart.Add(fineRollupBackfillChunk)
		if chunkEnd.After(end) {
			chunkEnd = end
		}

		unlockWrites := i.lockDBWrites()
		err := i.db.BackfillFineChartRollupsChunk(i.ctx, i.network.ChainID, chunkStart, chunkEnd)
		unlockWrites()
		if err != nil {
			if i.ctx.Err() == nil {
				logger.Error("Fine chart rollup backfill aborted",
					zap.String("network", i.network.Name),
					zap.Time("chunk_start", chunkStart),
					zap.Error(err))
			}
			return
		}
		chunkStart = chunkEnd

		select {
		case <-i.ctx.Done():
			return
		default:
		}
	}

	logger.Info("Fine chart rollup backfill complete",
		zap.String("network", i.network.Name),
		zap.Duration("took", time.Since(began)))
}
