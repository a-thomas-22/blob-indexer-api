package indexer

import (
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
)

// expectBounds queues the indexed-block bounds probe the backfill starts with.
func expectBounds(mock sqlmock.Sqlmock, minBlock, maxBlock interface{}) {
	mock.ExpectQuery("SELECT MIN\\(block_number\\) AS min_block").
		WillReturnRows(sqlmock.NewRows([]string{"min_block", "max_block"}).AddRow(minBlock, maxBlock))
}

// expectWatermarkRead queues the resume-point lookup. Pass nil for "not set".
func expectWatermarkRead(mock sqlmock.Sqlmock, value interface{}) {
	query := mock.ExpectQuery("SELECT value FROM indexer_metadata")
	if value == nil {
		query.WillReturnRows(sqlmock.NewRows([]string{"value"}))
		return
	}
	query.WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(value))
}

func expectChunk(mock sqlmock.Sqlmock, chainID int, from, to int64) {
	mock.ExpectExec("SELECT blob_block_streaks_recompute_all").
		WithArgs(chainID, from, to).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO indexer_metadata").
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestRunStreakBackfill_WalksAllHistoryInChunks(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB

	// Two full chunks plus a partial tail.
	maxBlock := 2*streakBackfillChunkBlocks + 5
	expectBounds(mock, 1, maxBlock)
	expectWatermarkRead(mock, nil)
	expectChunk(mock, idx.network.ChainID, 1, streakBackfillChunkBlocks)
	expectChunk(mock, idx.network.ChainID, streakBackfillChunkBlocks+1, 2*streakBackfillChunkBlocks)
	expectChunk(mock, idx.network.ChainID, 2*streakBackfillChunkBlocks+1, maxBlock)

	idx.runStreakBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestRunStreakBackfill_ResumesFromWatermark(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB

	expectBounds(mock, 100, 400)
	expectWatermarkRead(mock, "250")
	// Resume redoes the checkpointed block itself: one idempotent recompute is
	// cheaper than trusting a checkpoint whose chunk may not have landed.
	expectChunk(mock, idx.network.ChainID, 250, 400)

	idx.runStreakBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestRunStreakBackfill_WatermarkBelowHistoryStartsFromEarliestBlock(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB

	// A watermark from before a history prune (or a reset) must not skip the
	// blocks that do exist.
	expectBounds(mock, 1000, 1200)
	expectWatermarkRead(mock, "10")
	expectChunk(mock, idx.network.ChainID, 1000, 1200)

	idx.runStreakBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestRunStreakBackfill_WatermarkPastTipIsNoOp(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB

	expectBounds(mock, 100, 400)
	expectWatermarkRead(mock, "401")

	idx.runStreakBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestRunStreakBackfill_UnparsableWatermarkRebuildsFromStart(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB

	expectBounds(mock, 100, 200)
	expectWatermarkRead(mock, "not-a-number")
	expectChunk(mock, idx.network.ChainID, 100, 200)

	idx.runStreakBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestRunStreakBackfill_NoIndexedBlocks(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB

	expectBounds(mock, nil, nil)

	idx.runStreakBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestRunStreakBackfill_BoundsProbeFailureAborts(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB

	mock.ExpectQuery("SELECT MIN\\(block_number\\) AS min_block").
		WillReturnError(errors.New("probe failed"))

	idx.runStreakBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// A failed chunk stops the run rather than leaving later chunks to write rows
// around a hole; the next restart resumes from the last checkpoint.
func TestRunStreakBackfill_ChunkFailureStopsRun(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB

	expectBounds(mock, 1, 3*streakBackfillChunkBlocks)
	expectWatermarkRead(mock, nil)
	mock.ExpectExec("SELECT blob_block_streaks_recompute_all").
		WillReturnError(errors.New("recompute failed"))

	idx.runStreakBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// Checkpointing is best effort: losing it costs redundant work on the next
// start, so it must not abort the backfill in progress.
func TestRunStreakBackfill_CheckpointFailureContinues(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB

	maxBlock := streakBackfillChunkBlocks + 1
	expectBounds(mock, 1, maxBlock)
	expectWatermarkRead(mock, nil)
	mock.ExpectExec("SELECT blob_block_streaks_recompute_all").
		WithArgs(idx.network.ChainID, int64(1), streakBackfillChunkBlocks).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO indexer_metadata").
		WillReturnError(errors.New("checkpoint failed"))
	expectChunk(mock, idx.network.ChainID, streakBackfillChunkBlocks+1, maxBlock)

	idx.runStreakBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestStreakBackfillMetadataKey(t *testing.T) {
	if models.MetadataStreakBackfillBlock != "records_streak_backfill_block" {
		t.Fatalf("streak backfill metadata key changed to %q, which would silently restart every backfill",
			models.MetadataStreakBackfillBlock)
	}
}
