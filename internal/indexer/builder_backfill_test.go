package indexer

import (
	"context"
	"errors"
	"math/big"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/lib/pq"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
)

// fakeBlockSource serves canned blocks for the builder backfill without an
// RPC endpoint: the walk only needs a block's header and transactions.
type fakeBlockSource struct {
	// txs are the transactions every served block carries.
	txs []*types.Transaction
	// extra is the header extra data every served block carries.
	extra []byte
	// coinbase is the header fee recipient every served block carries.
	coinbase common.Address
	// failBlocks maps a block number to the number of fetches that fail
	// before it starts succeeding.
	failBlocks map[uint64]int
	// fetched counts fetches per block number.
	fetched map[uint64]int
	// order lists every fetch in the order it arrived, so a test can assert
	// which end of history the walk started from.
	order []uint64
	mu    sync.Mutex
}

func (f *fakeBlockSource) fetch(_ context.Context, number uint64) (*types.Block, error) {
	f.mu.Lock()
	if f.fetched == nil {
		f.fetched = make(map[uint64]int)
	}
	f.fetched[number]++
	f.order = append(f.order, number)
	remaining := f.failBlocks[number]
	if remaining > 0 {
		f.failBlocks[number]--
	}
	f.mu.Unlock()
	if remaining > 0 {
		return nil, errors.New("rpc failure")
	}
	return f.blockAt(number), nil
}

// blockAt renders the canned block for a height. Tests advertise its hash in
// the missing-row listing, which the backfill compares against what the
// source answers, so both sides must come from here.
func (f *fakeBlockSource) blockAt(number uint64) *types.Block {
	header := &types.Header{
		Number:   new(big.Int).SetUint64(number),
		Time:     number * 12,
		Extra:    f.extra,
		Coinbase: f.coinbase,
		BaseFee:  big.NewInt(10),
	}
	return types.NewBlockWithHeader(header).WithBody(types.Body{Transactions: f.txs})
}

// hashAt is the canned block's hash as indexed_blocks would hold it.
func (f *fakeBlockSource) hashAt(number int64) string {
	return f.blockAt(uint64(number)).Hash().Hex()
}

func (f *fakeBlockSource) count(number uint64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetched[number]
}

// fetchOrder is every fetch so far, oldest request first.
func (f *fakeBlockSource) fetchOrder() []uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uint64(nil), f.order...)
}

// newBuilderBackfillTestIndexer builds an indexer whose backfill walks
// two-block windows with no pauses or retry waits, against a fake block
// source rather than an RPC client.
func newBuilderBackfillTestIndexer(t *testing.T) (*Indexer, sqlmock.Sqlmock, *fakeBlockSource) {
	t.Helper()
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB
	source := &fakeBlockSource{extra: []byte("beaverbuild.org"), coinbase: common.HexToAddress("0xbeef")}
	idx.builderBackfill = builderBackfillSettings{
		enabled:      true,
		windowBlocks: 2,
		insertBatch:  1,
		fetchWorkers: 2,
		blockSource:  source.fetch,
	}
	return idx, mock, source
}

func expectBuilderlessBlocks(mock sqlmock.Sqlmock, source *fakeBlockSource, from, to int64, blocks ...int64) {
	rows := sqlmock.NewRows([]string{"block_number", "block_hash"})
	for _, block := range blocks {
		rows.AddRow(block, source.hashAt(block))
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM indexed_blocks ib")).
		WithArgs(testIndexerChainID, from, to).
		WillReturnRows(rows)
}

// expectBuilderInsert queues the two-statement write transaction: the
// builder insert, then the tx_index update. Pass withTxIndex=false for a
// block whose fetched body carries no blob transaction.
func expectBuilderInsert(mock sqlmock.Sqlmock, blocks []int64, withTxIndex bool) {
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO block_builders")).
		WithArgs(testIndexerChainID, pq.Array(blocks),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, int64(len(blocks))))
	if withTxIndex {
		mock.ExpectExec(regexp.QuoteMeta("UPDATE blobs b")).
			WillReturnResult(sqlmock.NewResult(0, int64(len(blocks))))
	}
	mock.ExpectCommit()
}

func expectBuilderFloorWrite(mock sqlmock.Sqlmock, block int64) {
	mock.ExpectExec("INSERT INTO indexer_metadata").
		WithArgs(testIndexerChainID, models.MetadataBlockBuilderBackfillFloor, big.NewInt(block).String()).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// expectNoFloor queues the delete of the oldest-first checkpoint earlier
// releases kept, which every start attempts, followed by the floor lookup
// finding nothing. Pass legacyRows=1 to simulate a network that still
// carried that checkpoint.
func expectNoFloor(mock sqlmock.Sqlmock, legacyRows int64) {
	expectLegacyCheckpointRetired(mock, legacyRows)
	expectMetadataRead(mock, nil)
}

// expectFloor queues the same delete followed by a floor lookup that finds
// the given value.
func expectFloor(mock sqlmock.Sqlmock, value string) {
	expectLegacyCheckpointRetired(mock, 0)
	expectMetadataRead(mock, value)
}

func expectLegacyCheckpointRetired(mock sqlmock.Sqlmock, legacyRows int64) {
	mock.ExpectExec("DELETE FROM indexer_metadata").
		WithArgs(testIndexerChainID, models.MetadataBlockBuilderBackfillBlock).
		WillReturnResult(sqlmock.NewResult(0, legacyRows))
}

func TestBuilderBackfill_InsertsMissingRowsAndCheckpoints(t *testing.T) {
	idx, mock, source := newBuilderBackfillTestIndexer(t)
	blobTx := newSignedBlobTx(t, int64(idx.network.ChainID), 7)
	source.txs = []*types.Transaction{newSignedDynamicTx(t, int64(idx.network.ChainID), 8), blobTx}

	expectBounds(mock, 1, 4)
	expectNoFloor(mock, 0)
	// Window [3,4] comes first: nothing to do, so no recheck, but the floor
	// descends.
	expectBuilderlessBlocks(mock, source, 3, 4)
	expectBuilderFloorWrite(mock, 3)
	// Window [1,2]: only block 2 lacks a builder row; after the write it is
	// rechecked and found complete.
	expectBuilderlessBlocks(mock, source, 1, 2, 2)
	expectBuilderInsert(mock, []int64{2}, true)
	expectBuilderlessBlocks(mock, source, 1, 2)
	expectBuilderFloorWrite(mock, 1)

	idx.runBuilderBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
	if source.count(2) != 1 || source.count(1) != 0 || source.count(3) != 0 {
		t.Fatalf("expected only the builder-less block to be fetched, got %v", source.fetched)
	}
}

func TestBuilderBackfill_FetchesBatchesConcurrentlyInBlockOrder(t *testing.T) {
	idx, mock, source := newBuilderBackfillTestIndexer(t)
	idx.builderBackfill.windowBlocks = 8
	idx.builderBackfill.insertBatch = 3
	source.txs = []*types.Transaction{newSignedBlobTx(t, int64(idx.network.ChainID), 7)}

	expectBounds(mock, 1, 8)
	expectNoFloor(mock, 0)
	expectBuilderlessBlocks(mock, source, 1, 8, 1, 2, 3, 5, 8)
	// Five blocks in batches of three: [1,2,3] then [5,8], each written in
	// ascending block order regardless of which worker fetched what.
	expectBuilderInsert(mock, []int64{1, 2, 3}, true)
	expectBuilderInsert(mock, []int64{5, 8}, true)
	expectBuilderlessBlocks(mock, source, 1, 8)
	expectBuilderFloorWrite(mock, 1)

	idx.runBuilderBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
	for _, block := range []uint64{1, 2, 3, 5, 8} {
		if source.count(block) != 1 {
			t.Fatalf("expected block %d fetched once, got %v", block, source.fetched)
		}
	}
}

// The walk starts at the newest indexed block and works down, so the blocks
// the recent leaderboard windows read are filled before older history.
func TestBuilderBackfill_WalksNewestFirst(t *testing.T) {
	idx, mock, source := newBuilderBackfillTestIndexer(t)
	idx.builderBackfill.fetchWorkers = 1
	source.txs = []*types.Transaction{newSignedBlobTx(t, int64(idx.network.ChainID), 7)}

	expectBounds(mock, 1, 5)
	expectNoFloor(mock, 0)
	// Windows [4,5], [2,3] and [1,1]: the last is narrower because the walk
	// stops at the earliest indexed block rather than below it.
	for _, window := range [][]int64{{4, 5}, {2, 3}, {1, 1}} {
		blocks := make([]int64, 0, 2)
		for block := window[0]; block <= window[1]; block++ {
			blocks = append(blocks, block)
		}
		expectBuilderlessBlocks(mock, source, window[0], window[1], blocks...)
		for _, block := range blocks {
			expectBuilderInsert(mock, []int64{block}, true)
		}
		expectBuilderlessBlocks(mock, source, window[0], window[1])
		expectBuilderFloorWrite(mock, window[0])
	}

	idx.runBuilderBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
	// Within a window the listing is ascending, which is immaterial at
	// window granularity; across windows the walk descends.
	want := []uint64{4, 5, 2, 3, 1}
	got := source.fetchOrder()
	if len(got) != len(want) {
		t.Fatalf("fetch order = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("fetch order = %v, want %v", got, want)
		}
	}
}

// A restart resumes just below the floor: the windows above it, including
// every block live indexing wrote meanwhile, are neither listed nor fetched.
func TestBuilderBackfill_ResumesBelowFloor(t *testing.T) {
	idx, mock, source := newBuilderBackfillTestIndexer(t)

	expectBounds(mock, 1, 6)
	expectFloor(mock, "3")
	expectBuilderlessBlocks(mock, source, 1, 2)
	expectBuilderFloorWrite(mock, 1)

	idx.runBuilderBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
	if len(source.fetched) != 0 {
		t.Fatalf("expected no fetches above the floor, got %v", source.fetched)
	}
}

// A floor above the indexed tip describes history a reindex removed since;
// the walk starts over from the tip and lowers the floor again.
func TestBuilderBackfill_RestartsFromTipWhenFloorIsAboveIt(t *testing.T) {
	idx, mock, source := newBuilderBackfillTestIndexer(t)

	expectBounds(mock, 1, 4)
	expectFloor(mock, "9")
	expectBuilderlessBlocks(mock, source, 3, 4)
	expectBuilderFloorWrite(mock, 3)
	expectBuilderlessBlocks(mock, source, 1, 2)
	expectBuilderFloorWrite(mock, 1)

	idx.runBuilderBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// The oldest-first checkpoint earlier releases kept means the opposite of
// the floor. It is deleted on every start, never trusted: a network that
// still carries it walks from the tip, and the prefix the old walk covered
// is listed again (cheaply, every block there has its row).
func TestBuilderBackfill_RetiresAscendingCheckpoint(t *testing.T) {
	t.Run("deletes the old key and walks from the tip", func(t *testing.T) {
		idx, mock, source := newBuilderBackfillTestIndexer(t)

		expectBounds(mock, 1, 4)
		expectNoFloor(mock, 1)
		expectBuilderlessBlocks(mock, source, 3, 4)
		expectBuilderFloorWrite(mock, 3)
		expectBuilderlessBlocks(mock, source, 1, 2)
		expectBuilderFloorWrite(mock, 1)

		idx.runBuilderBackfill()

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
	})

	t.Run("a failed delete does not stop the walk", func(t *testing.T) {
		idx, mock, source := newBuilderBackfillTestIndexer(t)

		expectBounds(mock, 1, 2)
		mock.ExpectExec("DELETE FROM indexer_metadata").
			WithArgs(testIndexerChainID, models.MetadataBlockBuilderBackfillBlock).
			WillReturnError(errors.New("down"))
		expectMetadataRead(mock, nil)
		expectBuilderlessBlocks(mock, source, 1, 2)
		expectBuilderFloorWrite(mock, 1)

		idx.runBuilderBackfill()

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
	})

	// A delete that failed on the start that first wrote the floor must not
	// leave the key behind for good: the next start, floor and all, retries.
	t.Run("a network with a floor still retires the old key", func(t *testing.T) {
		idx, mock, source := newBuilderBackfillTestIndexer(t)

		expectBounds(mock, 1, 4)
		expectLegacyCheckpointRetired(mock, 1)
		expectMetadataRead(mock, "3")
		expectBuilderlessBlocks(mock, source, 1, 2)
		expectBuilderFloorWrite(mock, 1)

		idx.runBuilderBackfill()

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
	})
}

func TestBuilderBackfill_SkipsWhenCaughtUpOrEmpty(t *testing.T) {
	t.Run("floor at the earliest indexed block", func(t *testing.T) {
		idx, mock, _ := newBuilderBackfillTestIndexer(t)
		expectBounds(mock, 1, 6)
		expectFloor(mock, "1")

		idx.runBuilderBackfill()

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
	})

	t.Run("no indexed blocks", func(t *testing.T) {
		idx, mock, _ := newBuilderBackfillTestIndexer(t)
		expectBounds(mock, nil, nil)

		idx.runBuilderBackfill()

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
	})

	t.Run("bounds read fails", func(t *testing.T) {
		idx, mock, _ := newBuilderBackfillTestIndexer(t)
		mock.ExpectQuery("SELECT MIN\\(block_number\\) AS min_block").WillReturnError(errors.New("down"))

		idx.runBuilderBackfill()

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
	})

	t.Run("unparsable floor restarts the walk", func(t *testing.T) {
		idx, mock, source := newBuilderBackfillTestIndexer(t)
		expectBounds(mock, 1, 2)
		expectFloor(mock, "not-a-number")
		expectBuilderlessBlocks(mock, source, 1, 2)
		expectBuilderFloorWrite(mock, 1)

		idx.runBuilderBackfill()

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
	})

	// A floor read that fails outright skips the run: walking from the tip
	// would overwrite a floor that may sit millions of blocks lower with
	// the first window's start, and the next start can simply read again.
	t.Run("floor read fails", func(t *testing.T) {
		idx, mock, source := newBuilderBackfillTestIndexer(t)
		expectBounds(mock, 1, 2)
		expectLegacyCheckpointRetired(mock, 0)
		mock.ExpectQuery("SELECT value FROM indexer_metadata").WillReturnError(errors.New("down"))

		idx.runBuilderBackfill()

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
		if len(source.fetched) != 0 {
			t.Fatalf("expected no fetches after a failed floor read, got %v", source.fetched)
		}
	})
}

// A block the RPC never serves leaves its window incomplete: the walk moves
// on to lower windows but the floor stays above it, so the next start
// resumes exactly at that window.
func TestBuilderBackfill_SkipsFailingBlockAndHoldsFloor(t *testing.T) {
	idx, mock, source := newBuilderBackfillTestIndexer(t)
	source.txs = []*types.Transaction{newSignedBlobTx(t, int64(idx.network.ChainID), 7)}
	// Block 3 fails once then succeeds; block 4 fails every attempt.
	source.failBlocks = map[uint64]int{3: 1, 4: builderBackfillFetchAttempts}

	expectBounds(mock, 1, 6)
	expectNoFloor(mock, 0)
	expectBuilderlessBlocks(mock, source, 5, 6)
	expectBuilderFloorWrite(mock, 5)
	expectBuilderlessBlocks(mock, source, 3, 4, 3, 4)
	// Block 3 lands; block 4's batch has nothing to write at all.
	expectBuilderInsert(mock, []int64{3}, true)
	expectBuilderlessBlocks(mock, source, 3, 4, 4)
	// The walk continues into the window below, but the floor stays at 5.
	expectBuilderlessBlocks(mock, source, 1, 2)

	idx.runBuilderBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
	if source.count(3) != 2 {
		t.Fatalf("expected block 3 to be fetched twice (one retry), got %d", source.count(3))
	}
	if source.count(4) != builderBackfillFetchAttempts {
		t.Fatalf("expected block 4 to exhaust %d attempts, got %d", builderBackfillFetchAttempts, source.count(4))
	}
}

// A block whose fetched body carries no blob transaction still gets its
// builder row; only the tx_index statement is skipped.
func TestBuilderBackfill_WritesBuilderRowWithoutBlobTransactions(t *testing.T) {
	idx, mock, source := newBuilderBackfillTestIndexer(t)
	source.txs = []*types.Transaction{newSignedDynamicTx(t, int64(idx.network.ChainID), 3)}

	expectBounds(mock, 1, 2)
	expectNoFloor(mock, 0)
	expectBuilderlessBlocks(mock, source, 1, 2, 1)
	expectBuilderInsert(mock, []int64{1}, false)
	expectBuilderlessBlocks(mock, source, 1, 2)
	expectBuilderFloorWrite(mock, 1)

	idx.runBuilderBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestBuilderBackfill_RetriesWriteThenAborts(t *testing.T) {
	idx, mock, source := newBuilderBackfillTestIndexer(t)
	source.txs = []*types.Transaction{newSignedBlobTx(t, int64(idx.network.ChainID), 7)}

	expectBounds(mock, 1, 2)
	expectNoFloor(mock, 0)
	expectBuilderlessBlocks(mock, source, 1, 2, 1)
	for attempt := 0; attempt < builderBackfillFetchAttempts; attempt++ {
		mock.ExpectBegin()
		mock.ExpectExec(regexp.QuoteMeta("INSERT INTO block_builders")).WillReturnError(errors.New("deadlock"))
		mock.ExpectRollback()
	}

	idx.runBuilderBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestBuilderBackfill_RetriesListingThenAborts(t *testing.T) {
	idx, mock, _ := newBuilderBackfillTestIndexer(t)

	expectBounds(mock, 1, 2)
	expectNoFloor(mock, 0)
	for attempt := 0; attempt < builderBackfillFetchAttempts; attempt++ {
		mock.ExpectQuery(regexp.QuoteMeta("FROM indexed_blocks ib")).WillReturnError(errors.New("timeout"))
	}

	idx.runBuilderBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// A floor write that fails is only a repeated window next start, so the
// walk carries on.
func TestBuilderBackfill_ContinuesWhenFloorWriteFails(t *testing.T) {
	idx, mock, source := newBuilderBackfillTestIndexer(t)

	expectBounds(mock, 1, 4)
	expectNoFloor(mock, 0)
	expectBuilderlessBlocks(mock, source, 3, 4)
	mock.ExpectExec("INSERT INTO indexer_metadata").WillReturnError(errors.New("down"))
	expectBuilderlessBlocks(mock, source, 1, 2)
	expectBuilderFloorWrite(mock, 1)

	idx.runBuilderBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// A reorg or reindex cleanup committing while a batch is in flight discards
// that batch rather than resurrecting rows for an abandoned fork. The window
// then rechecks as incomplete and the floor holds.
func TestBuilderBackfill_DiscardsBatchInvalidatedByCleanup(t *testing.T) {
	idx, mock, source := newBuilderBackfillTestIndexer(t)
	source.txs = []*types.Transaction{newSignedBlobTx(t, int64(idx.network.ChainID), 7)}

	expectBounds(mock, 1, 2)
	expectNoFloor(mock, 0)
	expectBuilderlessBlocks(mock, source, 1, 2, 1)
	// No write is expected: the epoch moves between the listing and the
	// write, which the fetch's stale sample detects.
	expectBuilderlessBlocks(mock, source, 1, 2, 1)

	origin := idx.builderBackfill.blockSource
	idx.builderBackfill.blockSource = func(ctx context.Context, number uint64) (*types.Block, error) {
		atomic.AddUint64(&idx.reorgEpoch, 1)
		return origin(ctx, number)
	}

	idx.runBuilderBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// The walk is throttled by the configured pause between windows, and the
// pause is interruptible: canceling the indexer stops it mid-wait.
func TestBuilderBackfill_StopsWhenIndexerStops(t *testing.T) {
	idx, mock, source := newBuilderBackfillTestIndexer(t)
	idx.builderBackfill.pause = time.Hour

	expectBounds(mock, 1, 6)
	expectNoFloor(mock, 0)
	expectBuilderlessBlocks(mock, source, 5, 6)
	expectBuilderFloorWrite(mock, 5)

	done := make(chan struct{})
	go func() {
		idx.runBuilderBackfill()
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

func TestNewBuilderBackfillSettings(t *testing.T) {
	t.Run("defaults and enablement come from config", func(t *testing.T) {
		settings := newBuilderBackfillSettings(config.IndexerConfig{
			BuilderBackfillEnabled: true,
			BuilderBackfillPause:   250 * time.Millisecond,
		})
		if !settings.enabled || settings.pause != 250*time.Millisecond {
			t.Fatalf("settings = %+v", settings)
		}
		if settings.windowBlocks != defaultBuilderBackfillWindowBlocks ||
			settings.insertBatch != defaultBuilderBackfillInsertBatch ||
			settings.fetchWorkers != defaultBuilderBackfillFetchWorkers ||
			settings.retryBackoff != defaultBuilderBackfillRetryBackoff {
			t.Fatalf("expected package defaults, got %+v", settings)
		}
		if settings.blockSource != nil {
			t.Fatal("expected production settings to use the RPC client")
		}
	})

	t.Run("a negative pause is clamped to none", func(t *testing.T) {
		settings := newBuilderBackfillSettings(config.IndexerConfig{BuilderBackfillPause: -time.Second})
		if settings.pause != 0 {
			t.Fatalf("pause = %s, want 0", settings.pause)
		}
		if settings.enabled {
			t.Fatal("expected the backfill to be disabled")
		}
	})
}

// builder_backfill_enabled = false short-circuits before the walk touches
// the database or the RPC at all, not just before it writes.
func TestBuilderBackfill_DisabledShortCircuits(t *testing.T) {
	idx, mock, source := newBuilderBackfillTestIndexer(t)
	idx.builderBackfill.enabled = false

	idx.runBuilderBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
	if len(source.fetched) != 0 {
		t.Fatalf("expected no fetches, got %v", source.fetched)
	}
}

// The configured pause is taken between windows, so the walk stays a
// background trickle next to live indexing rather than a burst of RPC load.
func TestBuilderBackfill_HonorsPauseBetweenWindows(t *testing.T) {
	idx, mock, source := newBuilderBackfillTestIndexer(t)
	idx.builderBackfill.pause = 80 * time.Millisecond

	// Three windows of two blocks each: two pauses, no pause after the last.
	expectBounds(mock, 1, 6)
	expectNoFloor(mock, 0)
	for _, window := range [][2]int64{{5, 6}, {3, 4}, {1, 2}} {
		expectBuilderlessBlocks(mock, source, window[0], window[1])
		expectBuilderFloorWrite(mock, window[0])
	}

	began := time.Now()
	idx.runBuilderBackfill()
	elapsed := time.Since(began)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
	if elapsed < 2*idx.builderBackfill.pause {
		t.Fatalf("walk took %s, expected at least two %s pauses", elapsed, idx.builderBackfill.pause)
	}
}

// Zero-width or unset tuning falls back to the package defaults rather than
// spinning on an empty batch.
func TestBuilderBackfill_FallsBackToDefaultTuning(t *testing.T) {
	idx, mock, source := newBuilderBackfillTestIndexer(t)
	idx.builderBackfill.insertBatch = 0
	idx.builderBackfill.fetchWorkers = 0
	source.txs = []*types.Transaction{newSignedBlobTx(t, int64(idx.network.ChainID), 7)}

	expectBounds(mock, 1, 2)
	expectNoFloor(mock, 0)
	expectBuilderlessBlocks(mock, source, 1, 2, 1, 2)
	expectBuilderInsert(mock, []int64{1, 2}, true)
	expectBuilderlessBlocks(mock, source, 1, 2)
	expectBuilderFloorWrite(mock, 1)

	idx.runBuilderBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestBlobTxIndexUpdates(t *testing.T) {
	blobTx := newSignedBlobTx(t, 42, 1)
	plain := newSignedDynamicTx(t, 42, 2)
	header := &types.Header{Number: big.NewInt(100), Time: 1200}
	block := types.NewBlockWithHeader(header).
		WithBody(types.Body{Transactions: []*types.Transaction{plain, blobTx, plain}})

	updates := blobTxIndexUpdates(block)

	if len(updates) != 1 {
		t.Fatalf("expected 1 update, got %+v", updates)
	}
	want := db.BlobTxIndexUpdate{BlockNumber: 100, TxHash: blobTx.Hash().Hex(), TxIndex: 1}
	if updates[0] != want {
		t.Fatalf("update = %+v, want %+v", updates[0], want)
	}
	if got := builderBackfillBlockTimestamp(block); !got.Equal(time.Unix(1200, 0).UTC()) {
		t.Fatalf("timestamp = %s, want %s", got, time.Unix(1200, 0).UTC())
	}
}

// A nil body from the block source is a failed fetch, not a builder row with
// zeroed header fields.
func TestBuilderBackfill_RejectsEmptyBlockBody(t *testing.T) {
	idx, _, _ := newBuilderBackfillTestIndexer(t)
	idx.builderBackfill.blockSource = func(context.Context, uint64) (*types.Block, error) {
		return nil, nil
	}

	if _, err := idx.fetchBuilderBackfillBlock(idx.ctx, 7); err == nil {
		t.Fatal("expected an error for a block with no body")
	}
}

// An empty batch writes nothing at all rather than opening a transaction.
func TestBuilderBackfill_WriteSkipsEmptyBatch(t *testing.T) {
	idx, mock, _ := newBuilderBackfillTestIndexer(t)

	inserted, indexed, err := idx.writeBuilderBackfillBatch(nil, nil, 0)

	if err != nil || inserted != 0 || indexed != 0 {
		t.Fatalf("writeBuilderBackfillBatch(nil) = (%d, %d, %v)", inserted, indexed, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// The backfill builds its rows with the same helper the live path uses, so a
// registry label and a proposer payment are derived identically.
func TestBuilderBackfill_RowMatchesLivePath(t *testing.T) {
	idx, _, source := newBuilderBackfillTestIndexer(t)
	source.extra = []byte("Titan (titanbuilder.xyz)")

	block, err := idx.fetchBuilderBackfillBlock(idx.ctx, 11)
	if err != nil {
		t.Fatalf("fetchBuilderBackfillBlock: %v", err)
	}
	row := idx.blockBuilderRow(block, builderBackfillBlockTimestamp(block))

	if row.BuilderKey != "titan" || row.BuilderName != "Titan" {
		t.Fatalf("labels = (%q, %q), want (titan, Titan)", row.BuilderKey, row.BuilderName)
	}
	if row.CandidateSnapshot || row.PendingCandidateTxs != nil || row.EligibleSkippedMaxTip != nil {
		t.Fatalf("expected no candidate snapshot on a backfilled row, got %+v", row)
	}
	if row.BlockNumber != 11 || row.ChainID != testIndexerChainID {
		t.Fatalf("row = %+v", row)
	}
}

// The backfill addresses blocks by number. When the node answers with a
// different block than the one indexed_blocks holds for that height — a
// reorg the live path has not cleaned up yet — the block is skipped like a
// failed fetch: nothing is written, the window rechecks as incomplete, and
// the floor stays above so the next start retries it.
func TestBuilderBackfill_SkipsBlockWhoseHashDoesNotMatchTheIndexedFork(t *testing.T) {
	idx, mock, source := newBuilderBackfillTestIndexer(t)
	source.txs = []*types.Transaction{newSignedBlobTx(t, int64(idx.network.ChainID), 7)}

	expectBounds(mock, 1, 2)
	expectNoFloor(mock, 0)
	// Block 1 is listed under a hash the source never serves: the stored
	// data for that height belongs to another fork.
	mock.ExpectQuery(regexp.QuoteMeta("FROM indexed_blocks ib")).
		WithArgs(testIndexerChainID, int64(1), int64(2)).
		WillReturnRows(sqlmock.NewRows([]string{"block_number", "block_hash"}).AddRow(int64(1), "0xanotherfork"))
	// No write transaction at all, and the recheck still finds block 1
	// builder-less, so the floor is never written.
	mock.ExpectQuery(regexp.QuoteMeta("FROM indexed_blocks ib")).
		WithArgs(testIndexerChainID, int64(1), int64(2)).
		WillReturnRows(sqlmock.NewRows([]string{"block_number", "block_hash"}).AddRow(int64(1), "0xanotherfork"))

	idx.runBuilderBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
	if source.count(1) != 1 {
		t.Fatalf("expected one fetch of block 1, got %d", source.count(1))
	}
}

// The hash comparison ignores checksum casing: indexed_blocks and a node's
// answer are both hex, but nothing guarantees the same case.
func TestBuilderBackfill_AcceptsAHashDifferingOnlyInCase(t *testing.T) {
	idx, mock, source := newBuilderBackfillTestIndexer(t)
	source.txs = []*types.Transaction{newSignedBlobTx(t, int64(idx.network.ChainID), 7)}

	expectBounds(mock, 1, 2)
	expectNoFloor(mock, 0)
	mock.ExpectQuery(regexp.QuoteMeta("FROM indexed_blocks ib")).
		WithArgs(testIndexerChainID, int64(1), int64(2)).
		WillReturnRows(sqlmock.NewRows([]string{"block_number", "block_hash"}).
			AddRow(int64(1), strings.ToUpper(source.hashAt(1))))
	expectBuilderInsert(mock, []int64{1}, true)
	expectBuilderlessBlocks(mock, source, 1, 2)
	expectBuilderFloorWrite(mock, 1)

	idx.runBuilderBackfill()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}
