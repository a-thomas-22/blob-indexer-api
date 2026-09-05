package indexer

import (
	"errors"
	"math/big"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/lib/pq"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
)

// newPriorityFeeTestIndexer builds an indexer whose backfill walks two-block
// windows with no pauses or retry waits, against a fake RPC whose blocks
// carry blobTx and a 10 wei execution base fee.
func newPriorityFeeTestIndexer(t *testing.T, latest uint64) (*Indexer, sqlmock.Sqlmock, *testEthRPC) {
	t.Helper()
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB
	ethClient, rpcSvc := newMockEthClient(t, latest)
	idx.ethClient = ethClient
	rpcSvc.baseFee = big.NewInt(10)
	idx.priorityFeeBackfill = priorityFeeBackfillSettings{
		enabled:      true,
		windowBlocks: 2,
		updateBatch:  1,
		fetchWorkers: 2,
	}
	return idx, mock, rpcSvc
}

func expectPriorityFeeWatermarkRead(mock sqlmock.Sqlmock, value interface{}) {
	expectMetadataRead(mock, value)
}

func expectMissingBlocks(mock sqlmock.Sqlmock, from, to int64, blocks ...int64) {
	rows := sqlmock.NewRows([]string{"block_number"})
	for _, block := range blocks {
		rows.AddRow(block)
	}
	mock.ExpectQuery("SELECT DISTINCT block_number").
		WithArgs(testIndexerChainID, from, to).
		WillReturnRows(rows)
}

func expectPriorityFeeWatermarkWrite(mock sqlmock.Sqlmock, block int64) {
	mock.ExpectExec("INSERT INTO indexer_metadata").
		WithArgs(testIndexerChainID, models.MetadataPriorityFeeBackfillBlock, big.NewInt(block).String()).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestPriorityFeeBackfill_FillsUnpricedBlocksAndCheckpoints(t *testing.T) {
	idx, mock, rpcSvc := newPriorityFeeTestIndexer(t, 4)
	blobTx := newSignedBlobTx(t, int64(idx.network.ChainID), 7)
	rpcSvc.blockTxs = []*types.Transaction{blobTx, newSignedDynamicTx(t, int64(idx.network.ChainID), 8)}

	expectBounds(mock, 1, 4)
	expectPriorityFeeWatermarkRead(mock, nil)
	// Window [1,2]: only block 2 is unpriced; after the write it is rechecked
	// and found complete.
	expectMissingBlocks(mock, 1, 2, 2)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE blobs b")).
		WithArgs(testIndexerChainID,
			pq.Array([]int64{2}),
			pq.Array([]string{blobTx.Hash().Hex()}),
			// GasTipCap 1, GasFeeCap 2, base fee 10: the fee cap sits below
			// the base fee, so the paid tip floors at zero.
			pq.Array([]string{"1"}), pq.Array([]string{"2"}), pq.Array([]string{"0"})).
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectMissingBlocks(mock, 1, 2)
	expectPriorityFeeWatermarkWrite(mock, 2)
	// Window [3,4]: nothing to do, so no recheck, but the checkpoint advances.
	expectMissingBlocks(mock, 3, 4)
	expectPriorityFeeWatermarkWrite(mock, 4)

	idx.runPriorityFeeBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
	if rpcSvc.fetched[2] != 1 || rpcSvc.fetched[1] != 0 || rpcSvc.fetched[3] != 0 {
		t.Fatalf("expected only the unpriced block to be fetched, got %v", rpcSvc.fetched)
	}
}

func TestPriorityFeeBackfill_FetchesBatchesConcurrentlyInBlockOrder(t *testing.T) {
	idx, mock, rpcSvc := newPriorityFeeTestIndexer(t, 8)
	idx.priorityFeeBackfill.windowBlocks = 8
	idx.priorityFeeBackfill.updateBatch = 3
	idx.priorityFeeBackfill.fetchWorkers = 2
	blobTx := newSignedBlobTx(t, int64(idx.network.ChainID), 7)
	rpcSvc.blockTxs = []*types.Transaction{blobTx}

	expectBounds(mock, 1, 8)
	expectPriorityFeeWatermarkRead(mock, nil)
	expectMissingBlocks(mock, 1, 8, 1, 2, 3, 5, 8)
	// Five blocks in batches of three: [1,2,3] then [5,8], each written in
	// ascending block order regardless of which worker fetched what.
	mock.ExpectExec(regexp.QuoteMeta("UPDATE blobs b")).
		WithArgs(testIndexerChainID, pq.Array([]int64{1, 2, 3}), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE blobs b")).
		WithArgs(testIndexerChainID, pq.Array([]int64{5, 8}), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 2))
	expectMissingBlocks(mock, 1, 8)
	expectPriorityFeeWatermarkWrite(mock, 8)

	idx.runPriorityFeeBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
	for _, block := range []uint64{1, 2, 3, 5, 8} {
		if rpcSvc.fetched[block] != 1 {
			t.Fatalf("expected block %d fetched once, got %v", block, rpcSvc.fetched)
		}
	}
}

func TestPriorityFeeBackfill_ResumesPastWatermark(t *testing.T) {
	idx, mock, _ := newPriorityFeeTestIndexer(t, 6)

	expectBounds(mock, 1, 6)
	expectPriorityFeeWatermarkRead(mock, "4")
	expectMissingBlocks(mock, 5, 6)
	expectPriorityFeeWatermarkWrite(mock, 6)

	idx.runPriorityFeeBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestPriorityFeeBackfill_SkipsWhenCaughtUpOrEmpty(t *testing.T) {
	t.Run("watermark at the tip", func(t *testing.T) {
		idx, mock, _ := newPriorityFeeTestIndexer(t, 6)
		expectBounds(mock, 1, 6)
		expectPriorityFeeWatermarkRead(mock, "6")

		idx.runPriorityFeeBackfill()

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
	})

	t.Run("no indexed blocks", func(t *testing.T) {
		idx, mock, _ := newPriorityFeeTestIndexer(t, 6)
		expectBounds(mock, nil, nil)

		idx.runPriorityFeeBackfill()

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
	})

	t.Run("bounds read fails", func(t *testing.T) {
		idx, mock, _ := newPriorityFeeTestIndexer(t, 6)
		mock.ExpectQuery("SELECT MIN\\(block_number\\) AS min_block").WillReturnError(errors.New("down"))

		idx.runPriorityFeeBackfill()

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
	})

	t.Run("unparsable watermark restarts the walk", func(t *testing.T) {
		idx, mock, _ := newPriorityFeeTestIndexer(t, 2)
		expectBounds(mock, 1, 2)
		expectPriorityFeeWatermarkRead(mock, "not-a-number")
		expectMissingBlocks(mock, 1, 2)
		expectPriorityFeeWatermarkWrite(mock, 2)

		idx.runPriorityFeeBackfill()

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
	})
}

func TestPriorityFeeBackfill_SkipsFailingBlockAndHoldsCheckpoint(t *testing.T) {
	idx, mock, rpcSvc := newPriorityFeeTestIndexer(t, 4)
	rpcSvc.blockTxs = []*types.Transaction{newSignedBlobTx(t, int64(idx.network.ChainID), 7)}
	// Block 1 fails once then succeeds; block 2 fails every attempt.
	rpcSvc.failBlocks = map[uint64]int{1: 1, 2: priorityFeeBackfillFetchAttempts}

	expectBounds(mock, 1, 4)
	expectPriorityFeeWatermarkRead(mock, nil)
	expectMissingBlocks(mock, 1, 2, 1, 2)
	// Block 1 lands; block 2's batch has nothing to write. The recheck still
	// finds block 2, so the window is incomplete and no checkpoint is written.
	mock.ExpectExec(regexp.QuoteMeta("UPDATE blobs b")).
		WithArgs(testIndexerChainID, pq.Array([]int64{1}), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectMissingBlocks(mock, 1, 2, 2)
	// The walk continues into the next window, but the checkpoint stays put.
	expectMissingBlocks(mock, 3, 4)

	idx.runPriorityFeeBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
	if rpcSvc.fetched[1] != 2 {
		t.Fatalf("expected block 1 to be fetched twice (one retry), got %d", rpcSvc.fetched[1])
	}
	if rpcSvc.fetched[2] != priorityFeeBackfillFetchAttempts {
		t.Fatalf("expected block 2 to exhaust %d attempts, got %d", priorityFeeBackfillFetchAttempts, rpcSvc.fetched[2])
	}
}

func TestPriorityFeeBackfill_HoldsCheckpointWhenRowsStayUnpriced(t *testing.T) {
	idx, mock, rpcSvc := newPriorityFeeTestIndexer(t, 4)
	// The fetched block carries no blob transaction for the unpriced row (a
	// transaction the canonical block does not hold), so the update matches
	// nothing and the recheck still lists the block.
	rpcSvc.blockTxs = nil

	expectBounds(mock, 1, 4)
	expectPriorityFeeWatermarkRead(mock, nil)
	expectMissingBlocks(mock, 1, 2, 2)
	expectMissingBlocks(mock, 1, 2, 2)
	expectMissingBlocks(mock, 3, 4)

	idx.runPriorityFeeBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestPriorityFeeBackfill_RetriesWriteThenAborts(t *testing.T) {
	idx, mock, rpcSvc := newPriorityFeeTestIndexer(t, 2)
	rpcSvc.blockTxs = []*types.Transaction{newSignedBlobTx(t, int64(idx.network.ChainID), 7)}

	expectBounds(mock, 1, 2)
	expectPriorityFeeWatermarkRead(mock, nil)
	expectMissingBlocks(mock, 1, 2, 1)
	for attempt := 0; attempt < priorityFeeBackfillFetchAttempts; attempt++ {
		mock.ExpectExec(regexp.QuoteMeta("UPDATE blobs b")).WillReturnError(errors.New("deadlock"))
	}

	idx.runPriorityFeeBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestPriorityFeeBackfill_RetriesListingThenAborts(t *testing.T) {
	idx, mock, _ := newPriorityFeeTestIndexer(t, 2)

	expectBounds(mock, 1, 2)
	expectPriorityFeeWatermarkRead(mock, nil)
	for attempt := 0; attempt < priorityFeeBackfillFetchAttempts; attempt++ {
		mock.ExpectQuery("SELECT DISTINCT block_number").WillReturnError(errors.New("timeout"))
	}

	idx.runPriorityFeeBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestPriorityFeeBackfill_StopsWhenIndexerStops(t *testing.T) {
	idx, mock, _ := newPriorityFeeTestIndexer(t, 6)
	idx.priorityFeeBackfill.pause = time.Hour

	expectBounds(mock, 1, 6)
	expectPriorityFeeWatermarkRead(mock, nil)
	expectMissingBlocks(mock, 1, 2)
	expectPriorityFeeWatermarkWrite(mock, 2)

	done := make(chan struct{})
	go func() {
		idx.runPriorityFeeBackfill()
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	idx.cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("backfill did not stop on cancel")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestBlockPriorityFeeUpdates(t *testing.T) {
	blobTx := newSignedBlobTx(t, 42, 1)
	plain := newSignedDynamicTx(t, 42, 2)
	isBlob := func(tx *types.Transaction) bool { return tx.Type() == types.BlobTxType }

	t.Run("derives fees against the block base fee", func(t *testing.T) {
		header := &types.Header{Number: big.NewInt(100), BaseFee: big.NewInt(1)}
		block := types.NewBlockWithHeader(header).WithBody(types.Body{Transactions: []*types.Transaction{plain, blobTx}})

		updates := blockPriorityFeeUpdates(block, isBlob)

		if len(updates) != 1 {
			t.Fatalf("expected 1 update, got %+v", updates)
		}
		want := db.BlobPriorityFeeUpdate{BlockNumber: 100, TxHash: blobTx.Hash().Hex(), MaxPriorityFeePerGas: "1", MaxFeePerGas: "2", PriorityFeePerGas: "1"}
		if updates[0] != want {
			t.Fatalf("update = %+v, want %+v", updates[0], want)
		}
	})

	t.Run("records only the caps without a base fee", func(t *testing.T) {
		header := &types.Header{Number: big.NewInt(100)}
		block := types.NewBlockWithHeader(header).WithBody(types.Body{Transactions: []*types.Transaction{blobTx}})

		updates := blockPriorityFeeUpdates(block, isBlob)

		if len(updates) != 1 || updates[0].PriorityFeePerGas != "" || updates[0].MaxFeePerGas != "2" {
			t.Fatalf("unexpected updates: %+v", updates)
		}
	})
}

func TestNewPriorityFeeBackfillSettings(t *testing.T) {
	settings := newPriorityFeeBackfillSettings(configIndexerFor(true, 3*time.Second))
	if !settings.enabled || settings.pause != 3*time.Second {
		t.Fatalf("unexpected settings: %+v", settings)
	}
	if settings.windowBlocks != defaultPriorityFeeBackfillWindowBlocks || settings.updateBatch != defaultPriorityFeeBackfillUpdateBatch || settings.fetchWorkers != defaultPriorityFeeBackfillFetchWorkers {
		t.Fatalf("expected defaults, got %+v", settings)
	}
	if newPriorityFeeBackfillSettings(configIndexerFor(false, 0)).enabled {
		t.Fatal("expected the backfill to be disabled")
	}
	if pause := newPriorityFeeBackfillSettings(configIndexerFor(true, -time.Second)).pause; pause != 0 {
		t.Fatalf("expected a negative pause to clamp to zero, got %s", pause)
	}
}

func configIndexerFor(enabled bool, pause time.Duration) config.IndexerConfig {
	return config.IndexerConfig{PriorityFeeBackfillEnabled: enabled, PriorityFeeBackfillPause: pause}
}
