package indexer

import (
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
)

// testStreakFingerprint is what the canned catalog/version rows below
// fingerprint to.
const testStreakFingerprint = "v2:above_target,below_target,drought,full"

// expectBounds queues the indexed-block bounds probe the backfill starts with.
func expectBounds(mock sqlmock.Sqlmock, minBlock, maxBlock interface{}) {
	mock.ExpectQuery("SELECT MIN\\(block_number\\) AS min_block").
		WillReturnRows(sqlmock.NewRows([]string{"min_block", "max_block"}).AddRow(minBlock, maxBlock))
}

// expectFingerprintRead queues the two-step streak definition probe: the kind
// catalog plus, when the schema has one, the definition version.
func expectFingerprintRead(mock sqlmock.Sqlmock) {
	mock.ExpectQuery("SELECT\\s+COALESCE").
		WillReturnRows(sqlmock.NewRows([]string{"kinds", "has_version"}).
			AddRow("above_target,below_target,drought,full", true))
	mock.ExpectQuery("SELECT blob_record_streak_definition_version").
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow("2"))
}

// expectStoredFingerprintRead queues the lookup of the definitions the last
// backfill ran under. Pass nil for "never recorded", which forces a rebuild.
func expectStoredFingerprintRead(mock sqlmock.Sqlmock, value interface{}) {
	expectMetadataRead(mock, value)
}

// expectWatermarkRead queues the resume-point lookup. Pass nil for "not set".
func expectWatermarkRead(mock sqlmock.Sqlmock, value interface{}) {
	expectMetadataRead(mock, value)
}

func expectMetadataRead(mock sqlmock.Sqlmock, value interface{}) {
	query := mock.ExpectQuery("SELECT value FROM indexer_metadata")
	if value == nil {
		query.WillReturnRows(sqlmock.NewRows([]string{"value"}))
		return
	}
	query.WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(value))
}

// expectDefinitionsClaimed queues the two writes a rebuild makes before its
// first chunk: reset the checkpoint, then record the definitions it is
// rebuilding under.
func expectDefinitionsClaimed(mock sqlmock.Sqlmock) {
	mock.ExpectExec("INSERT INTO indexer_metadata").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO indexer_metadata").
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func expectChunk(mock sqlmock.Sqlmock, chainID int, from, to int64) {
	mock.ExpectExec("SELECT blob_block_streaks_recompute_all").
		WithArgs(chainID, from, to).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO indexer_metadata").
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// newStreakTestIndexer builds an indexer whose chunk retries do not sleep, so
// failure paths stay fast.
func newStreakTestIndexer(t *testing.T) (*Indexer, sqlmock.Sqlmock) {
	t.Helper()
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB
	idx.streakBackfillRetryBackoff = time.Millisecond
	return idx, mock
}

func TestRunStreakBackfill_WalksAllHistoryInChunks(t *testing.T) {
	idx, mock := newStreakTestIndexer(t)

	// Two full chunks plus a partial tail.
	maxBlock := 2*streakBackfillChunkBlocks + 5
	expectBounds(mock, 1, maxBlock)
	expectFingerprintRead(mock)
	expectStoredFingerprintRead(mock, nil)
	expectDefinitionsClaimed(mock)
	expectChunk(mock, idx.network.ChainID, 1, streakBackfillChunkBlocks)
	expectChunk(mock, idx.network.ChainID, streakBackfillChunkBlocks+1, 2*streakBackfillChunkBlocks)
	expectChunk(mock, idx.network.ChainID, 2*streakBackfillChunkBlocks+1, maxBlock)

	idx.runStreakBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestRunStreakBackfill_ResumesFromWatermark(t *testing.T) {
	idx, mock := newStreakTestIndexer(t)

	expectBounds(mock, 100, 400)
	expectFingerprintRead(mock)
	expectStoredFingerprintRead(mock, testStreakFingerprint)
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
	idx, mock := newStreakTestIndexer(t)

	// A watermark from before a history prune (or a reset) must not skip the
	// blocks that do exist.
	expectBounds(mock, 1000, 1200)
	expectFingerprintRead(mock)
	expectStoredFingerprintRead(mock, testStreakFingerprint)
	expectWatermarkRead(mock, "10")
	expectChunk(mock, idx.network.ChainID, 1000, 1200)

	idx.runStreakBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestRunStreakBackfill_WatermarkPastTipIsNoOp(t *testing.T) {
	idx, mock := newStreakTestIndexer(t)

	expectBounds(mock, 100, 400)
	expectFingerprintRead(mock)
	expectStoredFingerprintRead(mock, testStreakFingerprint)
	expectWatermarkRead(mock, "401")

	idx.runStreakBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestRunStreakBackfill_UnparsableWatermarkRebuildsFromStart(t *testing.T) {
	idx, mock := newStreakTestIndexer(t)

	expectBounds(mock, 100, 200)
	expectFingerprintRead(mock)
	expectStoredFingerprintRead(mock, testStreakFingerprint)
	expectWatermarkRead(mock, "not-a-number")
	expectChunk(mock, idx.network.ChainID, 100, 200)

	idx.runStreakBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestRunStreakBackfill_NoIndexedBlocks(t *testing.T) {
	idx, mock := newStreakTestIndexer(t)

	expectBounds(mock, nil, nil)

	idx.runStreakBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestRunStreakBackfill_BoundsProbeFailureAborts(t *testing.T) {
	idx, mock := newStreakTestIndexer(t)

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
	idx, mock := newStreakTestIndexer(t)

	expectBounds(mock, 1, 3*streakBackfillChunkBlocks)
	expectFingerprintRead(mock)
	expectStoredFingerprintRead(mock, nil)
	expectDefinitionsClaimed(mock)
	// Every attempt fails, so the chunk exhausts its retries and the run stops
	// without touching the later chunks.
	for attempt := 0; attempt < streakBackfillChunkAttempts; attempt++ {
		mock.ExpectExec("SELECT blob_block_streaks_recompute_all").
			WillReturnError(errors.New("recompute failed"))
	}

	idx.runStreakBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// Checkpointing is best effort: losing it costs redundant work on the next
// start, so it must not abort the backfill in progress.
func TestRunStreakBackfill_CheckpointFailureContinues(t *testing.T) {
	idx, mock := newStreakTestIndexer(t)

	maxBlock := streakBackfillChunkBlocks + 1
	expectBounds(mock, 1, maxBlock)
	expectFingerprintRead(mock)
	expectStoredFingerprintRead(mock, nil)
	expectDefinitionsClaimed(mock)
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

// A changed streak definition must rebuild history rather than resume from a
// checkpoint that was reached under the old definitions, which would leave the
// added or changed predicate covering only blocks indexed from then on.
func TestRunStreakBackfill_DefinitionChangeRebuildsHistory(t *testing.T) {
	idx, mock := newStreakTestIndexer(t)

	expectBounds(mock, 100, 200)
	expectFingerprintRead(mock)
	// The stored fingerprint is the pre-000014 two-kind set, so it no longer
	// matches and the checkpoint at block 190 must be discarded.
	expectStoredFingerprintRead(mock, "v1:above_target,full")
	expectDefinitionsClaimed(mock)
	expectChunk(mock, idx.network.ChainID, 100, 200)

	idx.runStreakBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// A failed definition read must not be taken as "unchanged": a redundant
// rebuild is cheap, a skipped one leaves history permanently wrong.
func TestRunStreakBackfill_FingerprintReadFailureRebuilds(t *testing.T) {
	idx, mock := newStreakTestIndexer(t)

	expectBounds(mock, 100, 200)
	mock.ExpectQuery("SELECT\\s+COALESCE").
		WillReturnError(errors.New("catalog probe failed"))
	// The checkpoint is reset, but no fingerprint is recorded: the next start
	// must try to read the definitions again rather than trust an empty one.
	mock.ExpectExec("INSERT INTO indexer_metadata").
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectChunk(mock, idx.network.ChainID, 100, 200)

	idx.runStreakBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// A schema with no definition version function fingerprints as unknown, which
// is what makes an upgrade from 000013 rebuild exactly once.
func TestRunStreakBackfill_MissingVersionFunctionFingerprintsUnknown(t *testing.T) {
	idx, mock := newStreakTestIndexer(t)

	expectBounds(mock, 100, 200)
	mock.ExpectQuery("SELECT\\s+COALESCE").
		WillReturnRows(sqlmock.NewRows([]string{"kinds", "has_version"}).
			AddRow("above_target,full", false))
	expectStoredFingerprintRead(mock, "vunknown:above_target,full")
	// The fingerprint matches what was stored, so this resumes rather than
	// rebuilding: an unknown version is still a stable identity.
	expectWatermarkRead(mock, "150")
	expectChunk(mock, idx.network.ChainID, 150, 200)

	idx.runStreakBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// A transient chunk failure must be retried rather than costing the whole
// backfill, since /records would otherwise serve partial all-time records
// until the process happened to restart.
func TestRunStreakBackfill_ChunkRetrySucceeds(t *testing.T) {
	idx, mock := newStreakTestIndexer(t)

	expectBounds(mock, 1, 100)
	expectFingerprintRead(mock)
	expectStoredFingerprintRead(mock, testStreakFingerprint)
	expectWatermarkRead(mock, nil)
	mock.ExpectExec("SELECT blob_block_streaks_recompute_all").
		WillReturnError(errors.New("transient failure"))
	expectChunk(mock, idx.network.ChainID, 1, 100)

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
	if models.MetadataStreakBackfillKinds != "records_streak_backfill_kinds" {
		t.Fatalf("streak definition metadata key changed to %q, which would rebuild history on every start",
			models.MetadataStreakBackfillKinds)
	}
}
