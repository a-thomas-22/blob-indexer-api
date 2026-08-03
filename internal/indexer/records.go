package indexer

import (
	"database/sql"
	"errors"
	"strconv"
	"time"

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

// streakBackfillChunkAttempts is how many times one chunk is tried before the
// backfill gives up, and defaultStreakBackfillRetryBackoff scales the wait
// between attempts. A chunk is one idempotent statement, so retrying it is
// always safe.
const (
	streakBackfillChunkAttempts       = 3
	defaultStreakBackfillRetryBackoff = 2 * time.Second
)

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

	// A checkpoint records how far a backfill got, never which definitions it
	// got there with. Pair it with the database's current streak fingerprint
	// so that adding a kind or changing a predicate rebuilds history instead
	// of applying only to blocks indexed from then on.
	fingerprint, fingerprintOK := i.streakDefinitionFingerprint()
	definitionsChanged := !fingerprintOK || fingerprint != i.storedStreakFingerprint()

	from := bounds.Min
	if definitionsChanged {
		logger.Info("Rebuilding blob block streaks from the earliest indexed block",
			zap.String("network", i.network.Name),
			zap.String("definitions", fingerprint))
		// Claim the new definitions before the first chunk lands. A crash
		// mid-backfill then resumes from the chunk checkpoint under the same
		// definitions rather than restarting the whole walk; a crash before
		// any chunk leaves no checkpoint, which reads as "start from the
		// beginning" anyway.
		i.setStreakBackfillWatermark(bounds.Min)
		if fingerprintOK {
			i.setStreakDefinitionFingerprint(fingerprint)
		}
	} else if resume, ok := i.streakBackfillWatermark(); ok && resume >= bounds.Min {
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

		if err := i.backfillStreakChunk(chunkStart, chunkEnd); err != nil {
			if i.ctx.Err() == nil {
				// Giving up leaves /records serving the records found so far,
				// which for an unfinished first backfill understates the
				// all-time maxima. The checkpoint means the next start
				// resumes rather than rewalks.
				logger.Error("Blob block streak backfill aborted; records stay partial until the next start",
					zap.String("network", i.network.Name),
					zap.Int64("chunk_start", chunkStart),
					zap.Int64("through_block", chunkStart-1),
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

// backfillStreakChunk rebuilds one chunk, retrying a failure a few times
// before giving up. A chunk is a single idempotent statement, so a transient
// database error (a failover, a statement timeout under load) should not cost
// the whole backfill and leave /records reporting partial all-time records
// until the process happens to restart.
func (i *Indexer) backfillStreakChunk(chunkStart, chunkEnd int64) error {
	var err error
	for attempt := 1; attempt <= streakBackfillChunkAttempts; attempt++ {
		unlockWrites := i.lockDBWrites()
		err = i.db.BackfillBlobBlockStreaksChunk(i.ctx, i.network.ChainID, chunkStart, chunkEnd)
		unlockWrites()
		if err == nil || i.ctx.Err() != nil {
			return err
		}

		if attempt < streakBackfillChunkAttempts {
			logger.Warn("Retrying blob block streak backfill chunk",
				zap.String("network", i.network.Name),
				zap.Int64("chunk_start", chunkStart),
				zap.Int("attempt", attempt),
				zap.Error(err))
			select {
			case <-i.ctx.Done():
				return err
			case <-time.After(time.Duration(attempt) * i.streakBackfillRetryBackoff):
			}
		}
	}
	return err
}

// streakDefinitionFingerprint reads the database's current streak definitions.
// The bool is false when the read fails, which callers treat as "assume they
// changed": a redundant rebuild is cheap, a skipped one leaves history wrong.
func (i *Indexer) streakDefinitionFingerprint() (string, bool) {
	fingerprint, err := i.db.StreakDefinitionFingerprint(i.ctx)
	if err != nil {
		if i.ctx.Err() == nil {
			logger.Warn("Failed to read streak definitions; rebuilding history to be safe",
				zap.String("network", i.network.Name),
				zap.Error(err))
		}
		return "", false
	}
	return fingerprint, true
}

// storedStreakFingerprint reads the definitions the last backfill ran under.
// An absent value means the checkpoint predates fingerprinting, which must
// force a rebuild.
func (i *Indexer) storedStreakFingerprint() string {
	value, err := i.db.GetNetworkMetadata(i.ctx, i.network.ChainID, models.MetadataStreakBackfillKinds)
	if err != nil {
		return ""
	}
	return value
}

func (i *Indexer) setStreakDefinitionFingerprint(fingerprint string) {
	err := i.db.SetNetworkMetadata(i.ctx, i.network.ChainID,
		models.MetadataStreakBackfillKinds, fingerprint)
	if err != nil && i.ctx.Err() == nil {
		logger.Warn("Failed to record streak definitions; the next start will rebuild history again",
			zap.String("network", i.network.Name),
			zap.Error(err))
	}
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
