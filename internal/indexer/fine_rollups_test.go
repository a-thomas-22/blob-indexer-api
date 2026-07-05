package indexer

import (
	"database/sql/driver"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/a-thomas-22/blob-indexer-api/internal/db"
)

func TestRunFineRollupMaintenance_BackfillsThenPrunes(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB
	idx.fineRollupPruneInterval = 5 * time.Millisecond

	// Startup backfill covers the retention window in one-hour chunks, two
	// statements per chunk.
	chunks := int(db.FineChartRollupRetention / fineRollupBackfillChunk)
	for i := 0; i < chunks; i++ {
		mock.ExpectExec("INSERT INTO blob_chart_rollups").
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec("INSERT INTO block_metrics_rollups").
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
	// First prune tick deletes expired fine buckets from both tables.
	mock.ExpectExec("DELETE FROM blob_chart_rollups").
		WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectExec("DELETE FROM block_metrics_rollups").
		WillReturnResult(sqlmock.NewResult(0, 2))

	done := make(chan struct{})
	go func() {
		idx.runFineRollupMaintenance()
		close(done)
	}()

	deadline := time.After(3 * time.Second)
	for mock.ExpectationsWereMet() != nil {
		select {
		case <-deadline:
			idx.cancel()
			<-done
			t.Fatalf("expectations not met in time: %v", mock.ExpectationsWereMet())
		case <-time.After(2 * time.Millisecond):
		}
	}

	idx.cancel()
	<-done
}

func TestRunFineRollupMaintenance_PruneErrorKeepsLoopAlive(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB
	idx.fineRollupPruneInterval = 5 * time.Millisecond

	// Abort the backfill immediately so the test exercises the prune loop.
	mock.ExpectExec("INSERT INTO blob_chart_rollups").
		WillReturnError(errors.New("backfill failed"))
	// A failed prune tick must not stop the loop: a later tick still runs.
	mock.ExpectExec("DELETE FROM blob_chart_rollups").
		WillReturnError(errors.New("prune failed"))
	mock.ExpectExec("DELETE FROM blob_chart_rollups").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM block_metrics_rollups").
		WillReturnResult(sqlmock.NewResult(0, 1))

	done := make(chan struct{})
	go func() {
		idx.runFineRollupMaintenance()
		close(done)
	}()

	deadline := time.After(3 * time.Second)
	for mock.ExpectationsWereMet() != nil {
		select {
		case <-deadline:
			idx.cancel()
			<-done
			t.Fatalf("expectations not met in time: %v", mock.ExpectationsWereMet())
		case <-time.After(2 * time.Millisecond):
		}
	}

	idx.cancel()
	<-done
}

// timeRecorder is a sqlmock argument matcher that captures time arguments so
// the test can assert on chunk bounds computed from time.Now().
type timeRecorder struct {
	captured *[]time.Time
}

func (r timeRecorder) Match(v driver.Value) bool {
	tm, ok := v.(time.Time)
	if !ok {
		return false
	}
	*r.captured = append(*r.captured, tm)
	return true
}

func TestBackfillFineRollups_ChunksAreAlignedAndContiguous(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB

	var starts, ends []time.Time
	total := int(db.FineChartRollupRetention / fineRollupBackfillChunk)
	for i := 0; i < total; i++ {
		mock.ExpectExec("INSERT INTO blob_chart_rollups").
			WithArgs(idx.network.ChainID, timeRecorder{&starts}, timeRecorder{&ends}, db.FineChartRollupBucketSeconds).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec("INSERT INTO block_metrics_rollups").
			WithArgs(idx.network.ChainID, sqlmock.AnyArg(), sqlmock.AnyArg(), db.FineChartRollupBucketSeconds).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}

	idx.backfillFineRollups()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
	if len(starts) != total || len(ends) != total {
		t.Fatalf("expected %d chunks, got %d starts / %d ends", total, len(starts), len(ends))
	}
	for i := range starts {
		if !starts[i].Truncate(db.FineChartRollupBucketDuration).Equal(starts[i]) {
			t.Fatalf("chunk %d start %s is not bucket-aligned", i, starts[i])
		}
		if !ends[i].Equal(starts[i].Add(fineRollupBackfillChunk)) {
			t.Fatalf("chunk %d spans [%s, %s), want one backfill chunk", i, starts[i], ends[i])
		}
		if i > 0 && !starts[i].Equal(ends[i-1]) {
			t.Fatalf("chunk %d start %s does not continue from previous end %s", i, starts[i], ends[i-1])
		}
	}
	if got := ends[total-1].Sub(starts[0]); got != db.FineChartRollupRetention {
		t.Fatalf("backfill covered %s, want %s", got, db.FineChartRollupRetention)
	}
}
