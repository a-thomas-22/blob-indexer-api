package indexer

import (
	"database/sql"
	"errors"
	"strconv"

	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

// streakBackfillChunkBlocks is how many blocks one backfill statement rebuilds.
// Each chunk is a keyed block_metrics range scan plus the streak rows it
// produces, so the chunk size trades statement duration against round trips;
// keeping statements short matters because the backfill holds the network write
// lock for each one.
const streakBackfillChunkBlocks int64 = 50000

// runStreakBackfill rebuilds the /records streak leaderboards across all
// indexed history that predates this process, then returns. New blocks are
// maintained by the statement triggers on block_metrics, so this only covers
// rows indexed before the streak table existed (first deploy) or while no
// indexer with this code was running.
//
// Progress is checkpointed per chunk in indexer_metadata, so a restart resumes
// instead of rewalking history. The recompute is idempotent and widens each
// range to whole runs, so re-running a chunk (or overlapping the live triggers
// at the tip) converges on the same rows.
func (i *Indexer) runStreakBackfill() {
	bounds, err := i.db.IndexedBlockBounds(i.ctx, i.network.ChainID)
	if err != nil {
		if i.ctx.Err() == nil {
			logger.Error("Failed to read indexed block bounds for streak backfill",
				zap.String("network", i.network.Name),
				zap.Error(err))
		}
		return
	}
	if !bounds.HasBlocks {
		logger.Debug("Skipping streak backfill: no indexed blocks",
			zap.String("network", i.network.Name))
		return
	}

	from := bounds.Min
	if resume, ok := i.streakBackfillWatermark(); ok && resume >= bounds.Min {
		// Resume at the checkpointed block rather than past it: redoing one
		// block is free (the recompute is idempotent) and it keeps the run
		// straddling the checkpoint from depending on the previous run's
		// last statement having fully landed.
		from = resume
	}
	if from > bounds.Max {
		return
	}

	logger.Info("Backfilling blob block streaks",
		zap.String("network", i.network.Name),
		zap.Int64("from_block", from),
		zap.Int64("to_block", bounds.Max))

	for chunkStart := from; chunkStart <= bounds.Max; chunkStart += streakBackfillChunkBlocks {
		chunkEnd := chunkStart + streakBackfillChunkBlocks - 1
		if chunkEnd > bounds.Max {
			chunkEnd = bounds.Max
		}

		unlockWrites := i.lockDBWrites()
		err := i.db.BackfillBlobBlockStreaksChunk(i.ctx, i.network.ChainID, chunkStart, chunkEnd)
		unlockWrites()
		if err != nil {
			if i.ctx.Err() == nil {
				logger.Error("Blob block streak backfill aborted",
					zap.String("network", i.network.Name),
					zap.Int64("chunk_start", chunkStart),
					zap.Error(err))
			}
			return
		}

		i.setStreakBackfillWatermark(chunkEnd)

		select {
		case <-i.ctx.Done():
			return
		default:
		}
	}

	logger.Info("Blob block streak backfill complete",
		zap.String("network", i.network.Name),
		zap.Int64("through_block", bounds.Max))
}

// streakBackfillWatermark reads the highest block a previous backfill run
// completed. A missing or unparsable value means "start from the beginning",
// which costs a full rebuild but never leaves a hole.
func (i *Indexer) streakBackfillWatermark() (int64, bool) {
	value, err := i.db.GetNetworkMetadata(i.ctx, i.network.ChainID, models.MetadataStreakBackfillBlock)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) && i.ctx.Err() == nil {
			logger.Warn("Failed to read streak backfill watermark; rebuilding from the earliest indexed block",
				zap.String("network", i.network.Name),
				zap.Error(err))
		}
		return 0, false
	}
	block, parseErr := strconv.ParseInt(value, 10, 64)
	if parseErr != nil {
		logger.Warn("Ignoring unparsable streak backfill watermark",
			zap.String("network", i.network.Name),
			zap.String("value", value),
			zap.Error(parseErr))
		return 0, false
	}
	return block, true
}

// setStreakBackfillWatermark records backfill progress. A failed write only
// costs redundant work on the next start, so it is logged and the backfill
// continues.
func (i *Indexer) setStreakBackfillWatermark(block int64) {
	err := i.db.SetNetworkMetadata(i.ctx, i.network.ChainID,
		models.MetadataStreakBackfillBlock, strconv.FormatInt(block, 10))
	if err != nil && i.ctx.Err() == nil {
		logger.Warn("Failed to checkpoint streak backfill progress",
			zap.String("network", i.network.Name),
			zap.Int64("block", block),
			zap.Error(err))
	}
}
