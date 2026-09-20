package indexer

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"

	"github.com/a-thomas-22/blob-indexer-api/internal/attribution"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
)

// expectPredecessorsCommitted queues the snapshot gate's indexed_blocks read
// for blockNumber, answering that every block in the window is committed
// (or, with committed=false, that one is missing). The earliest indexed
// block is reported as the window's own start, so nothing is floored away.
func expectPredecessorsCommitted(mock sqlmock.Sqlmock, idx *Indexer, blockNumber int64, committed bool) {
	from, to, ok := idx.snapshotPredecessorRange(blockNumber)
	if !ok {
		return
	}
	count := to - from + 1
	if !committed {
		count--
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT MIN(block_number) FROM indexed_blocks")).
		WithArgs(idx.network.ChainID, from, to).
		WillReturnRows(sqlmock.NewRows([]string{"earliest", "indexed"}).AddRow(from, count))
}

// expectCandidateRepair queues one run of the maintenance repair. removed
// lists the block numbers of the rows the DELETE returns; nil means nothing
// to repair, so no UPDATE follows.
func expectCandidateRepair(mock sqlmock.Sqlmock, idx *Indexer, removed []int64) {
	mock.ExpectBegin()
	rows := sqlmock.NewRows([]string{"block_number"})
	for _, block := range removed {
		rows.AddRow(block)
	}
	mock.ExpectQuery(regexp.QuoteMeta("DELETE FROM blob_inclusion_candidates c")).
		WithArgs(idx.network.ChainID).
		WillReturnRows(rows)
	if len(removed) > 0 {
		distinct := make([]int64, 0, len(removed))
		seen := map[int64]bool{}
		for _, block := range removed {
			if !seen[block] {
				seen[block] = true
				distinct = append(distinct, block)
			}
		}
		mock.ExpectExec(regexp.QuoteMeta("UPDATE block_builders AS bb SET")).
			WithArgs(idx.network.ChainID, pq.Array(distinct), models.CandidateEligible).
			WillReturnResult(sqlmock.NewResult(0, int64(len(distinct))))
	}
	mock.ExpectCommit()
}

func TestCandidateSnapshotPredecessors(t *testing.T) {
	idx := newTestIndexer()
	// newTestIndexer polls the mempool every 20s with no reconcile interval,
	// so the liveness window is 2×20s = 40s: four 12s slots, plus one.
	if got := idx.candidateSnapshotPredecessors(); got != 5 {
		t.Fatalf("predecessors at 12s slots = %d, want 5", got)
	}
	idx.network.SecondsPerSlot = 5
	if got := idx.candidateSnapshotPredecessors(); got != 9 {
		t.Fatalf("predecessors at 5s slots = %d, want 9", got)
	}
	idx.mempoolReconcileInterval = 60 * time.Second
	if got := idx.candidateSnapshotPredecessors(); got != 25 {
		t.Fatalf("predecessors with a 120s window at 5s slots = %d, want 25", got)
	}
}

func TestSnapshotPredecessorRange(t *testing.T) {
	idx := newTestIndexer() // 5 predecessors
	cases := []struct {
		block    int64
		from, to int64
		ok       bool
	}{
		{block: 1000, from: 995, to: 999, ok: true},
		{block: 5, from: 0, to: 4, ok: true},
		{block: 3, from: 0, to: 2, ok: true}, // floored at zero, whatever start_block says
		{block: 0, ok: false},                // nothing below
	}
	for _, start := range []string{"100", "LATEST", "LATEST-2", ""} {
		idx.network.StartBlock = start
		for _, tc := range cases {
			from, to, ok := idx.snapshotPredecessorRange(tc.block)
			if ok != tc.ok || (ok && (from != tc.from || to != tc.to)) {
				t.Errorf("start=%q range(%d) = [%d, %d] ok=%t, want [%d, %d] ok=%t", start, tc.block, from, to, ok, tc.from, tc.to, tc.ok)
			}
		}
	}
}

// The gate floors the window at the earliest indexed block, which is how a
// LATEST-k start (resolved against the node, never a parsable number) and a
// numeric start both stop the gate from demanding blocks that will never be
// indexed.
func TestPredecessorsCommittedFloorsAtTheEarliestIndexedBlock(t *testing.T) {
	idx := newTestIndexer() // 5 predecessors: block 1000's window is [995, 999]
	idx.network.StartBlock = "LATEST-2"

	cases := []struct {
		name     string
		earliest interface{}
		indexed  int64
		want     bool
	}{
		{name: "nothing indexed yet: the first block", earliest: nil, indexed: 0, want: true},
		{name: "earliest above the window: nothing to wait for", earliest: int64(1000), indexed: 0, want: true},
		{name: "earliest inside the window, rest present", earliest: int64(998), indexed: 2, want: true},
		{name: "earliest inside the window, one missing", earliest: int64(997), indexed: 2, want: false},
		{name: "earliest below the window, all present", earliest: int64(10), indexed: 5, want: true},
		{name: "earliest below the window, one missing", earliest: int64(10), indexed: 4, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idxDB, mock := newMockIndexerDB(t)
			idx.db = idxDB
			mock.ExpectBegin()
			mock.ExpectQuery(regexp.QuoteMeta("SELECT MIN(block_number) FROM indexed_blocks")).
				WithArgs(idx.network.ChainID, int64(995), int64(999)).
				WillReturnRows(sqlmock.NewRows([]string{"earliest", "indexed"}).AddRow(tc.earliest, tc.indexed))
			mock.ExpectRollback()

			tx, err := idxDB.BeginTxx(context.Background(), nil)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			got, err := idx.predecessorsCommitted(context.Background(), tx, 1000)
			_ = tx.Rollback()
			if err != nil {
				t.Fatalf("predecessorsCommitted() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("predecessorsCommitted() = %t, want %t", got, tc.want)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("expectations not met: %v", err)
			}
		})
	}
}

func TestEnqueueBlockTracksInFlight(t *testing.T) {
	idx := newTestIndexer()

	for _, block := range []uint64{10, 11, 11} {
		if !idx.enqueueBlock(block) {
			t.Fatalf("enqueueBlock(%d) = false", block)
		}
	}
	if len(idx.blockTaskCh) != 3 {
		t.Fatalf("queued %d tasks, want 3", len(idx.blockTaskCh))
	}
	if lowest, ok := idx.lowestInFlightBelow(12, 0); !ok || lowest != 10 {
		t.Fatalf("lowest in flight below 12 = %d ok=%t, want 10", lowest, ok)
	}
	if _, ok := idx.lowestInFlightBelow(12, 11); !ok {
		t.Fatal("block 11 is in flight and inside [11, 12)")
	}
	if _, ok := idx.lowestInFlightBelow(10, 0); ok {
		t.Fatal("nothing below 10 is in flight")
	}

	// The doubly-queued height stays in flight until both passes finish.
	idx.finishBlock(10)
	idx.finishBlock(11)
	if lowest, ok := idx.lowestInFlightBelow(12, 0); !ok || lowest != 11 {
		t.Fatalf("after one pass of 11 finished, lowest = %d ok=%t, want 11", lowest, ok)
	}
	idx.finishBlock(11)
	if _, ok := idx.lowestInFlightBelow(12, 0); ok {
		t.Fatal("expected nothing in flight once every pass finished")
	}
	// Finishing a task that was never enqueued is a no-op.
	idx.finishBlock(99)
	if len(idx.inFlight) != 0 {
		t.Fatalf("in-flight set = %v, want empty", idx.inFlight)
	}

	// A shutting-down indexer does not queue and does not leak a reference.
	idx.cancel()
	full := newTestIndexer()
	full.blockTaskCh = make(chan BlockTask) // unbuffered: the send would block
	full.cancel()
	if full.enqueueBlock(5) {
		t.Fatal("enqueueBlock must report failure once the context is done")
	}
	if len(full.inFlight) != 0 {
		t.Fatalf("a refused enqueue left %v in flight", full.inFlight)
	}
}

func TestAwaitPredecessors(t *testing.T) {
	t.Run("returns once the earlier block finishes", func(t *testing.T) {
		idx := newTestIndexer()
		idx.predecessorWait = 2 * time.Second
		if !idx.enqueueBlock(200) || !idx.enqueueBlock(201) {
			t.Fatal("enqueue failed")
		}

		var wg sync.WaitGroup
		wg.Add(1)
		start := time.Now()
		var waited time.Duration
		go func() {
			defer wg.Done()
			idx.awaitPredecessors(201)
			waited = time.Since(start)
		}()
		time.Sleep(60 * time.Millisecond)
		idx.finishBlock(200)
		wg.Wait()
		if waited < 50*time.Millisecond || waited > time.Second {
			t.Fatalf("waited %v, want roughly until block 200 finished", waited)
		}
	})

	t.Run("gives up at the deadline", func(t *testing.T) {
		idx := newTestIndexer()
		idx.predecessorWait = 80 * time.Millisecond
		if !idx.enqueueBlock(200) || !idx.enqueueBlock(201) {
			t.Fatal("enqueue failed")
		}
		start := time.Now()
		idx.awaitPredecessors(201)
		if waited := time.Since(start); waited < 80*time.Millisecond || waited > time.Second {
			t.Fatalf("waited %v, want about the 80ms deadline", waited)
		}
	})

	t.Run("does not wait for blocks outside the window or above", func(t *testing.T) {
		idx := newTestIndexer()
		idx.predecessorWait = time.Second
		span := idx.candidateSnapshotPredecessors()
		if !idx.enqueueBlock(1000-span-1) || !idx.enqueueBlock(1000) || !idx.enqueueBlock(1001) {
			t.Fatal("enqueue failed")
		}
		start := time.Now()
		idx.awaitPredecessors(1000)
		if waited := time.Since(start); waited > 200*time.Millisecond {
			t.Fatalf("waited %v for blocks the snapshot does not depend on", waited)
		}
	})

	t.Run("disabled by a zero wait", func(t *testing.T) {
		idx := newTestIndexer()
		if !idx.enqueueBlock(200) {
			t.Fatal("enqueue failed")
		}
		start := time.Now()
		idx.awaitPredecessors(201)
		if waited := time.Since(start); waited > 200*time.Millisecond {
			t.Fatalf("waited %v with the wait disabled", waited)
		}
	})

	t.Run("returns on shutdown", func(t *testing.T) {
		idx := newTestIndexer()
		idx.predecessorWait = 5 * time.Second
		if !idx.enqueueBlock(200) {
			t.Fatal("enqueue failed")
		}
		done := make(chan struct{})
		go func() {
			idx.awaitPredecessors(201)
			close(done)
		}()
		time.Sleep(30 * time.Millisecond)
		idx.cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("awaitPredecessors did not return after the context was canceled")
		}
	})
}

// The worker releases the in-flight reference however the task ends, and a
// block that was handed to the gap scanner is no longer waited for.
func TestBlockProcessingWorkerReleasesInFlight(t *testing.T) {
	idx := newTestIndexer()
	idx.maxBlockRetries = 0
	idx.blockTaskCh = make(chan BlockTask, 4)
	idx.ethClient = nil // processBlock panics on the nil client; the worker recovers

	if !idx.enqueueBlock(300) {
		t.Fatal("enqueue failed")
	}
	done := make(chan struct{})
	go func() {
		idx.blockProcessingWorker(1)
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := idx.lowestInFlightBelow(301, 0); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the worker did not release the in-flight reference")
		}
		time.Sleep(5 * time.Millisecond)
	}
	idx.cancel()
	<-done
	idx.failedBlocksMu.Lock()
	defer idx.failedBlocksMu.Unlock()
	if _, tracked := idx.failedBlocks[300]; !tracked {
		t.Fatal("the failed block must still reach the gap scanner")
	}
}

// A live block whose window below it is not fully committed keeps its
// builder identity and takes no snapshot: the pool it would read still
// holds what the uncommitted block included.
func TestInsertBlockDataSkipsSnapshotWhenPredecessorsUncommitted(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB
	idx.candidateSnapshotMaxLag = time.Minute

	blockTime := time.Now().UTC()
	metrics := &models.BlockMetrics{
		ChainID: idx.network.ChainID, BlockNumber: 903, BlockTimestamp: blockTime,
		BlobBaseFee: "100", BaseFeeWei: "1000", BlobGasLimit: 6 * 131072,
	}
	builder := &models.BlockBuilder{
		ChainID: idx.network.ChainID, BlockNumber: 903, BlockTimestamp: blockTime,
		FeeRecipient: "0xabc", ExtraData: "0x", BuilderKey: "titan", BuilderName: "Titan",
	}

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO block_metrics").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery("candidate_snapshot FROM block_builders").
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(false))
	expectPredecessorsCommitted(mock, idx, 903, false)
	// No pool read, no candidate insert: the upsert carries no snapshot.
	mock.ExpectExec("INSERT INTO block_builders").
		WithArgs(builder.ChainID, builder.BlockNumber, blockTime, builder.FeeRecipient, builder.ExtraData,
			builder.BuilderKey, builder.BuilderName, 0, nil, nil,
			false, nil, nil, nil, nil).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO indexed_blocks").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := idx.insertBlockData(nil, models.IndexedBlock{ChainID: idx.network.ChainID, BlockNumber: 903}, metrics, builder, 0); err != nil {
		t.Fatalf("insertBlockData() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

func TestInsertBlockDataPredecessorCheckFailureAbortsTheBlock(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB
	idx.candidateSnapshotMaxLag = time.Minute

	blockTime := time.Now().UTC()
	metrics := &models.BlockMetrics{
		ChainID: idx.network.ChainID, BlockNumber: 904, BlockTimestamp: blockTime,
		BlobBaseFee: "100", BaseFeeWei: "1000", BlobGasLimit: 6 * 131072,
	}
	builder := &models.BlockBuilder{
		ChainID: idx.network.ChainID, BlockNumber: 904, BlockTimestamp: blockTime,
		FeeRecipient: "0xabc", ExtraData: "0x", BuilderKey: "titan", BuilderName: "Titan",
	}

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO block_metrics").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery("candidate_snapshot FROM block_builders").
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(false))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT MIN(block_number) FROM indexed_blocks")).
		WillReturnError(errors.New("gate read failed"))
	mock.ExpectRollback()

	err := idx.insertBlockData(nil, models.IndexedBlock{ChainID: idx.network.ChainID, BlockNumber: 904}, metrics, builder, 0)
	if err == nil || !strings.Contains(err.Error(), "failed to check committed predecessors") {
		t.Fatalf("expected a predecessor check error, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations not met: %v", err)
	}
}

func TestRepairIncludedCandidates(t *testing.T) {
	t.Run("removes rows and recomputes the affected blocks", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		expectCandidateRepair(mock, idx, []int64{5, 5, 7})

		if !idx.repairIncludedCandidates(idx.ctx) {
			t.Fatal("a successful repair must report success")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations not met: %v", err)
		}
	})

	t.Run("nothing to repair skips the recompute", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		expectCandidateRepair(mock, idx, nil)

		idx.repairIncludedCandidates(idx.ctx)
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations not met: %v", err)
		}
	})

	t.Run("failures roll back and are logged, not fatal", func(t *testing.T) {
		for name, arrange := range map[string]func(mock sqlmock.Sqlmock){
			"begin": func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin().WillReturnError(errors.New("begin failed"))
			},
			"delete": func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectQuery(regexp.QuoteMeta("DELETE FROM blob_inclusion_candidates c")).
					WillReturnError(errors.New("delete failed"))
				mock.ExpectRollback()
			},
			"scan": func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectQuery(regexp.QuoteMeta("DELETE FROM blob_inclusion_candidates c")).
					WillReturnRows(sqlmock.NewRows([]string{"block_number"}).AddRow("not-a-number"))
				mock.ExpectRollback()
			},
			"rows": func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectQuery(regexp.QuoteMeta("DELETE FROM blob_inclusion_candidates c")).
					WillReturnRows(sqlmock.NewRows([]string{"block_number"}).AddRow(int64(5)).RowError(0, errors.New("rows failed")))
				mock.ExpectRollback()
			},
			"update": func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectQuery(regexp.QuoteMeta("DELETE FROM blob_inclusion_candidates c")).
					WillReturnRows(sqlmock.NewRows([]string{"block_number"}).AddRow(int64(5)))
				mock.ExpectExec(regexp.QuoteMeta("UPDATE block_builders AS bb SET")).
					WillReturnError(errors.New("update failed"))
				mock.ExpectRollback()
			},
			"commit": func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectQuery(regexp.QuoteMeta("DELETE FROM blob_inclusion_candidates c")).
					WillReturnRows(sqlmock.NewRows([]string{"block_number"}))
				mock.ExpectCommit().WillReturnError(errors.New("commit failed"))
			},
		} {
			t.Run(name, func(t *testing.T) {
				idx := newTestIndexer()
				idxDB, mock := newMockIndexerDB(t)
				idx.db = idxDB
				arrange(mock)

				if idx.repairIncludedCandidates(idx.ctx) {
					t.Fatal("a failed repair must report failure so the prunes are held back")
				}
				if err := mock.ExpectationsWereMet(); err != nil {
					t.Fatalf("expectations not met: %v", err)
				}
			})
		}
	})

	t.Run("a canceled context is not an error", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		// database/sql refuses to begin on a canceled context before the
		// driver sees it, so the repair must return without touching the
		// mock at all.
		idx.repairIncludedCandidates(ctx)
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations not met: %v", err)
		}
	})
}

// Start runs the repair once whatever the maintenance settings: with both
// the mempool sweep and the candidate prune disabled the maintenance loop
// never starts, and the rows an earlier run corrupted must still be fixed.
func TestStartRunsTheCandidateRepairWithoutMaintenance(t *testing.T) {
	idx := newTestIndexer()
	idx.mempoolTTL = 0
	idx.mempoolCleanupInterval = 0
	idx.candidateRetention = -time.Second
	if idx.maintenanceDue() {
		t.Fatal("fixture must have maintenance disabled")
	}
	idx.pollingInterval = 5 * time.Millisecond
	idx.mempoolPollingInterval = 5 * time.Millisecond
	idx.network.StartBlock = "1000" // keep runBlockIndexer from queueing work

	idxDB, mock := newMockIndexerDB(t)
	mock.MatchExpectationsInOrder(false)
	idx.db = idxDB
	idx.ethClient, _ = newMockEthClient(t, 10)
	idx.attribution = attribution.NewService(idxDB)
	idx.attribution.SetChainID(idx.network.ChainID)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT value FROM indexer_metadata WHERE chain_id = $1 AND key = $2")).
		WithArgs(idx.network.ChainID, models.MetadataLastIndexedBlock).
		WillReturnError(sql.ErrNoRows)
	expectCandidateRepair(mock, idx, []int64{5})

	if err := idx.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for mock.ExpectationsWereMet() != nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	idx.Stop()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the startup repair did not run: %v", err)
	}
}
