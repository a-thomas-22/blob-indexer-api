package indexer

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"math/big"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/holiman/uint256"
	"github.com/jmoiron/sqlx"

	"github.com/a-thomas-22/blob-indexer-api/internal/attribution"
	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	"github.com/a-thomas-22/blob-indexer-api/internal/ethereum"
)

type testEthRPC struct {
	latest         uint64
	failBlock      bool
	txByHash       *types.Transaction
	txPending      bool
	txErr          error
	blockTxs       []*types.Transaction
	blobBaseFeeHex string
	blobBaseFeeErr error
}

func (e *testEthRPC) GetBlockByNumber(_ context.Context, blockNum string, _ bool) (interface{}, error) {
	if e.failBlock {
		return nil, errors.New("rpc failure")
	}

	number := e.latest
	if blockNum != "latest" {
		parsed, err := strconv.ParseUint(strings.TrimPrefix(blockNum, "0x"), 16, 64)
		if err == nil {
			number = parsed
		}
	}

	excessBlobGas := uint64(0)
	blobGasUsed := uint64(0)
	header := &types.Header{
		ParentHash:    common.BigToHash(big.NewInt(int64(number))),
		UncleHash:     types.EmptyUncleHash,
		Root:          common.BigToHash(big.NewInt(2)),
		TxHash:        types.EmptyTxsHash,
		ReceiptHash:   types.EmptyReceiptsHash,
		Difficulty:    big.NewInt(1),
		Number:        big.NewInt(int64(number)),
		GasLimit:      30_000_000,
		GasUsed:       0,
		Time:          number * 12, // deterministic: 12-second slot time per block number
		Extra:         []byte{},
		ExcessBlobGas: &excessBlobGas,
		BlobGasUsed:   &blobGasUsed,
	}
	if len(e.blockTxs) > 0 {
		header.TxHash = common.BigToHash(big.NewInt(999))
	}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		return nil, err
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(headerJSON, &payload); err != nil {
		return nil, err
	}
	payload["hash"] = common.BigToHash(big.NewInt(int64(number + 1000))).Hex()

	txs := make([]interface{}, 0, len(e.blockTxs))
	for _, tx := range e.blockTxs {
		txJSON, err := tx.MarshalJSON()
		if err != nil {
			return nil, err
		}
		var txPayload map[string]interface{}
		if err := json.Unmarshal(txJSON, &txPayload); err != nil {
			return nil, err
		}
		txs = append(txs, txPayload)
	}
	payload["transactions"] = txs
	payload["uncles"] = []interface{}{}

	return payload, nil
}

func (e *testEthRPC) BlobBaseFee(_ context.Context) (string, error) {
	if e.blobBaseFeeErr != nil {
		return "", e.blobBaseFeeErr
	}
	if e.blobBaseFeeHex != "" {
		return e.blobBaseFeeHex, nil
	}
	return "0x3b9aca00", nil
}

func (e *testEthRPC) GetTransactionByHash(_ context.Context, _ common.Hash) (interface{}, error) {
	if e.txErr != nil {
		return nil, e.txErr
	}
	if e.txByHash == nil {
		return nil, nil
	}
	data, err := e.txByHash.MarshalJSON()
	if err != nil {
		return nil, err
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	if !e.txPending {
		payload["blockNumber"] = "0x1"
		payload["blockHash"] = common.BigToHash(big.NewInt(1)).Hex()
	}
	return payload, nil
}

// newMockEthClient serves the eth namespace only. GetPendingTransactions
// reads the pending block (eth_getBlockByNumber("pending", true)), which the
// mock builds from testEthRPC.blockTxs — the client no longer calls
// txpool_content, so no txpool service is registered.
func newMockEthClient(t *testing.T, latest uint64) (*ethereum.Client, *testEthRPC) {
	t.Helper()

	rpcServer := rpc.NewServer()
	ethSvc := &testEthRPC{latest: latest}
	if err := rpcServer.RegisterName("eth", ethSvc); err != nil {
		t.Fatalf("failed to register eth rpc service: %v", err)
	}

	httpServer := httptest.NewServer(rpcServer)
	t.Cleanup(httpServer.Close)

	client, err := ethereum.NewClient(httpServer.URL)
	if err != nil {
		t.Fatalf("failed to create ethereum client: %v", err)
	}
	t.Cleanup(client.Close)

	return client, ethSvc
}

func newMockIndexerDB(t *testing.T) (*db.DB, sqlmock.Sqlmock) {
	t.Helper()

	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	t.Cleanup(func() {
		_ = sqlDB.Close()
	})

	return &db.DB{DB: sqlx.NewDb(sqlDB, "sqlmock")}, mock
}

func newBlobFixture() models.Blob {
	return models.Blob{
		ChainID:           42,
		BlockNumber:       -1,
		BlobIndex:         0,
		TxHash:            "0xabc",
		FromAddress:       "0xfrom",
		UserAttribution:   "alice",
		BlobSizeBytes:     1024,
		BaseFeePerBlobGas: "10",
		TipPerBlobGas:     "2",
		TotalCostWei:      "12",
		Timestamp:         time.Unix(1, 0),
		Confirmed:         false,
		VersionedHash:     &fixtureVersionedHash,
	}
}

var fixtureVersionedHash = "0x0100000000000000000000000000000000000000000000000000000000000001"

func newSignedBlobTx(t *testing.T, chainID int64, nonce uint64) *types.Transaction {
	t.Helper()

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate private key: %v", err)
	}
	signer := types.LatestSignerForChainID(big.NewInt(chainID))

	return types.MustSignNewTx(key, signer, &types.BlobTx{
		ChainID:    uint256.NewInt(uint64(chainID)),
		Nonce:      nonce,
		GasTipCap:  uint256.NewInt(1),
		GasFeeCap:  uint256.NewInt(2),
		Gas:        21_000,
		To:         common.Address{},
		Value:      uint256.NewInt(0),
		BlobFeeCap: uint256.NewInt(3),
		BlobHashes: []common.Hash{{byte(nonce + 1)}},
	})
}

func newSignedDynamicTx(t *testing.T, chainID int64, nonce uint64) *types.Transaction {
	t.Helper()

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate private key: %v", err)
	}
	signer := types.LatestSignerForChainID(big.NewInt(chainID))

	return types.MustSignNewTx(key, signer, &types.DynamicFeeTx{
		ChainID:   big.NewInt(chainID),
		Nonce:     nonce,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(2),
		Gas:       21_000,
		To:        &common.Address{},
	})
}

func TestNew_InitializesIndexer(t *testing.T) {
	cfg := &config.Config{
		Indexer: config.IndexerConfig{
			Version:                "v1",
			BatchSize:              10,
			PollingInterval:        5 * time.Second,
			MempoolPollingInterval: 7 * time.Second,
		},
	}
	network := config.NetworkConfig{Name: "test", ChainID: 1, StartBlock: "1", Enabled: true}

	idx := New(context.Background(), nil, &ethereum.Client{}, cfg, network)
	if idx == nil {
		t.Fatal("expected non-nil indexer")
	}
	if idx.batchSize != 10 {
		t.Fatalf("expected batch size 10, got %d", idx.batchSize)
	}
	if idx.indexerVersion != "v1" {
		t.Fatalf("expected version v1, got %q", idx.indexerVersion)
	}
	if idx.workerCount != DefaultWorkerCount {
		t.Fatalf("expected workerCount %d, got %d", DefaultWorkerCount, idx.workerCount)
	}
}

func TestNewForTest_InitializesIndexer(t *testing.T) {
	cfg := &config.Config{}
	network := config.NetworkConfig{Name: "test", ChainID: 10, Enabled: true}

	idx := NewForTest(nil, cfg, network, 99)
	if idx.GetLastIndexedBlock() != 99 {
		t.Fatalf("expected last block 99, got %d", idx.GetLastIndexedBlock())
	}
	if idx.GetNetworkInfo().ChainID != 10 {
		t.Fatalf("expected chain id 10, got %d", idx.GetNetworkInfo().ChainID)
	}

	dbConn, _ := newMockIndexerDB(t)
	idxWithDB := NewForTest(dbConn, cfg, network, 7)
	if idxWithDB.attribution == nil {
		t.Fatal("expected attribution service to be initialized when database is provided")
	}
}

func TestDetermineStartBlock_LatestVariants(t *testing.T) {
	client, rpcSvc := newMockEthClient(t, 120)
	idx := newTestIndexer()
	idx.ethClient = client

	idx.network.StartBlock = "LATEST-20"
	start, err := idx.determineStartBlock()
	if err != nil {
		t.Fatalf("determineStartBlock latest-offset error = %v", err)
	}
	if start != 100 {
		t.Fatalf("expected 100, got %d", start)
	}

	idx.network.StartBlock = "LATEST"
	start, err = idx.determineStartBlock()
	if err != nil {
		t.Fatalf("determineStartBlock latest error = %v", err)
	}
	if start != 120 {
		t.Fatalf("expected 120, got %d", start)
	}

	idx.network.StartBlock = "LATEST-9999"
	start, err = idx.determineStartBlock()
	if err != nil {
		t.Fatalf("determineStartBlock huge offset error = %v", err)
	}
	if start != 0 {
		t.Fatalf("expected 0 for offset > latest, got %d", start)
	}

	idx.network.StartBlock = "LATEST-nope"
	_, err = idx.determineStartBlock()
	if err == nil {
		t.Fatal("expected parse error for invalid offset")
	}

	rpcSvc.failBlock = true
	idx.network.StartBlock = "LATEST"
	_, err = idx.determineStartBlock()
	if err == nil {
		t.Fatal("expected error when latest block lookup fails")
	}
}

func TestDetermineStartBlock_ResumesActiveBackfillCursor(t *testing.T) {
	idx := newTestIndexer()
	idx.network.StartBlock = "100"
	atomic.StoreUint64(&idx.lastIndexedBlock, 1_000)
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB

	rows := sqlmock.NewRows([]string{"key", "value"}).
		AddRow(models.MetadataBackfillActive, "true").
		AddRow(models.MetadataBackfillCurrentBlock, "250").
		AddRow(models.MetadataBackfillTargetBlock, "1000")
	mock.ExpectQuery("SELECT key, value").
		WithArgs(idx.network.ChainID, models.MetadataBackfillActive, models.MetadataBackfillCurrentBlock, models.MetadataBackfillTargetBlock).
		WillReturnRows(rows)
	// Coverage below the cursor is complete, so the verified resume point is
	// cursor+1 — the same block the old blind cursor+1 fast path used.
	mock.ExpectQuery("WITH indexed AS").
		WithArgs(idx.network.ChainID, uint64(100), uint64(250)).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(uint64(251)))

	start, err := idx.determineStartBlock()
	if err != nil {
		t.Fatalf("determineStartBlock() error = %v", err)
	}
	if start != 251 {
		t.Fatalf("expected backfill resume block 251, got %d", start)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// Regression test for the 2026-07-05 mainnet incident: the tip-gap catch-up
// shares the backfill_* metadata keys with the historical backfill, so a
// restart can observe a cursor that jumped from the historical range
// (~19.56M) to the tip (25,465,056). Resume must continue from the first
// unindexed block above the configured start, not trust the tip cursor —
// trusting it orphaned ~97% of the historical range.
func TestDetermineStartBlock_CursorJumpedToTipResumesFromFirstGap(t *testing.T) {
	idx := newTestIndexer()
	idx.network.StartBlock = "19426587"
	atomic.StoreUint64(&idx.lastIndexedBlock, 25_465_060)
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB

	rows := sqlmock.NewRows([]string{"key", "value"}).
		AddRow(models.MetadataBackfillActive, "true").
		AddRow(models.MetadataBackfillCurrentBlock, "25465056").
		AddRow(models.MetadataBackfillTargetBlock, "25465060")
	mock.ExpectQuery("SELECT key, value").
		WithArgs(idx.network.ChainID, models.MetadataBackfillActive, models.MetadataBackfillCurrentBlock, models.MetadataBackfillTargetBlock).
		WillReturnRows(rows)
	// indexed_blocks covers the configured start up to 19,562,586 plus a band
	// at the tip; the first gap is where the historical backfill really was.
	mock.ExpectQuery("WITH indexed AS").
		WithArgs(idx.network.ChainID, uint64(19_426_587), uint64(25_465_056)).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(uint64(19_562_587)))

	start, err := idx.determineStartBlock()
	if err != nil {
		t.Fatalf("determineStartBlock() error = %v", err)
	}
	if start != 19_562_587 {
		t.Fatalf("expected resume from first unindexed block 19562587, got %d", start)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestDetermineStartBlock_InfersLegacyBackfillCursor(t *testing.T) {
	idx := newTestIndexer()
	idx.network.StartBlock = "100"
	atomic.StoreUint64(&idx.lastIndexedBlock, 1_000)
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB

	rows := sqlmock.NewRows([]string{"key", "value"}).
		AddRow(models.MetadataBackfillActive, "true").
		AddRow(models.MetadataBackfillTargetBlock, "1000")
	mock.ExpectQuery("SELECT key, value").
		WithArgs(idx.network.ChainID, models.MetadataBackfillActive, models.MetadataBackfillCurrentBlock, models.MetadataBackfillTargetBlock).
		WillReturnRows(rows)
	mock.ExpectQuery("WITH indexed AS").
		WithArgs(idx.network.ChainID, uint64(100), uint64(1000)).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(uint64(275)))

	start, err := idx.determineStartBlock()
	if err != nil {
		t.Fatalf("determineStartBlock() error = %v", err)
	}
	if start != 275 {
		t.Fatalf("expected inferred resume block 275, got %d", start)
	}
}

func TestDetermineStartBlock_CompletedBackfillUsesLiveProgress(t *testing.T) {
	idx := newTestIndexer()
	idx.network.StartBlock = "100"
	atomic.StoreUint64(&idx.lastIndexedBlock, 1_000)
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB

	rows := sqlmock.NewRows([]string{"key", "value"}).
		AddRow(models.MetadataBackfillActive, "false").
		AddRow(models.MetadataBackfillCurrentBlock, "1000").
		AddRow(models.MetadataBackfillTargetBlock, "1000")
	mock.ExpectQuery("SELECT key, value").
		WithArgs(idx.network.ChainID, models.MetadataBackfillActive, models.MetadataBackfillCurrentBlock, models.MetadataBackfillTargetBlock).
		WillReturnRows(rows)

	start, err := idx.determineStartBlock()
	if err != nil {
		t.Fatalf("determineStartBlock() error = %v", err)
	}
	if start != 1001 {
		t.Fatalf("expected live resume block 1001, got %d", start)
	}
}

func TestBackfillResumeBlock_Branches(t *testing.T) {
	t.Run("no database", func(t *testing.T) {
		idx := newTestIndexer()
		idx.db = nil

		resumeBlock, ok, err := idx.backfillResumeBlock(100, 200, true)
		if err != nil {
			t.Fatalf("backfillResumeBlock() error = %v", err)
		}
		if ok || resumeBlock != 0 {
			t.Fatalf("expected no backfill resume without database, got block=%d ok=%t", resumeBlock, ok)
		}
	})

	t.Run("active cursor before configured start", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		rows := sqlmock.NewRows([]string{"key", "value"}).
			AddRow(models.MetadataBackfillActive, "true").
			AddRow(models.MetadataBackfillCurrentBlock, "50")
		mock.ExpectQuery("SELECT key, value").
			WithArgs(idx.network.ChainID, models.MetadataBackfillActive, models.MetadataBackfillCurrentBlock, models.MetadataBackfillTargetBlock).
			WillReturnRows(rows)

		resumeBlock, ok, err := idx.backfillResumeBlock(100, 200, true)
		if err != nil {
			t.Fatalf("backfillResumeBlock() error = %v", err)
		}
		if !ok || resumeBlock != 100 {
			t.Fatalf("expected configured-start resume, got block=%d ok=%t", resumeBlock, ok)
		}
	})

	t.Run("inactive metadata", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		rows := sqlmock.NewRows([]string{"key", "value"}).
			AddRow(models.MetadataBackfillActive, "false")
		mock.ExpectQuery("SELECT key, value").
			WithArgs(idx.network.ChainID, models.MetadataBackfillActive, models.MetadataBackfillCurrentBlock, models.MetadataBackfillTargetBlock).
			WillReturnRows(rows)

		resumeBlock, ok, err := idx.backfillResumeBlock(100, 200, true)
		if err != nil {
			t.Fatalf("backfillResumeBlock() error = %v", err)
		}
		if ok || resumeBlock != 0 {
			t.Fatalf("expected inactive metadata to skip resume, got block=%d ok=%t", resumeBlock, ok)
		}
	})

	t.Run("target unknown uses latest block", func(t *testing.T) {
		idx := newTestIndexer()
		idx.ethClient, _ = newMockEthClient(t, 120)
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		rows := sqlmock.NewRows([]string{"key", "value"}).
			AddRow(models.MetadataBackfillActive, "true")
		mock.ExpectQuery("SELECT key, value").
			WithArgs(idx.network.ChainID, models.MetadataBackfillActive, models.MetadataBackfillCurrentBlock, models.MetadataBackfillTargetBlock).
			WillReturnRows(rows)
		mock.ExpectExec("INSERT INTO indexer_metadata").
			WithArgs(idx.network.ChainID, models.MetadataCurrentChainHead, "120", models.MetadataChainHeadUpdatedAt, sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(1, 2))
		mock.ExpectQuery("WITH indexed AS").
			WithArgs(idx.network.ChainID, uint64(100), uint64(120)).
			WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(uint64(117)))

		resumeBlock, ok, err := idx.backfillResumeBlock(100, 0, false)
		if err != nil {
			t.Fatalf("backfillResumeBlock() error = %v", err)
		}
		if !ok || resumeBlock != 117 {
			t.Fatalf("expected inferred resume block 117, got block=%d ok=%t", resumeBlock, ok)
		}
	})

	t.Run("configured start after target", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		rows := sqlmock.NewRows([]string{"key", "value"}).
			AddRow(models.MetadataBackfillActive, "true").
			AddRow(models.MetadataBackfillTargetBlock, "90")
		mock.ExpectQuery("SELECT key, value").
			WithArgs(idx.network.ChainID, models.MetadataBackfillActive, models.MetadataBackfillCurrentBlock, models.MetadataBackfillTargetBlock).
			WillReturnRows(rows)

		resumeBlock, ok, err := idx.backfillResumeBlock(100, 0, false)
		if err != nil {
			t.Fatalf("backfillResumeBlock() error = %v", err)
		}
		if !ok || resumeBlock != 91 {
			t.Fatalf("expected target+1 resume, got block=%d ok=%t", resumeBlock, ok)
		}
	})

	t.Run("cursor coverage check error", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		rows := sqlmock.NewRows([]string{"key", "value"}).
			AddRow(models.MetadataBackfillActive, "true").
			AddRow(models.MetadataBackfillCurrentBlock, "150")
		mock.ExpectQuery("SELECT key, value").
			WithArgs(idx.network.ChainID, models.MetadataBackfillActive, models.MetadataBackfillCurrentBlock, models.MetadataBackfillTargetBlock).
			WillReturnRows(rows)
		mock.ExpectQuery("WITH indexed AS").
			WithArgs(idx.network.ChainID, uint64(100), uint64(150)).
			WillReturnError(errors.New("coverage scan failed"))

		_, _, err := idx.backfillResumeBlock(100, 200, true)
		if err == nil || !strings.Contains(err.Error(), "failed to get first unindexed block") {
			t.Fatalf("expected coverage scan error, got %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet sqlmock expectations: %v", err)
		}
	})

	t.Run("metadata lookup error", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectQuery("SELECT key, value").
			WithArgs(idx.network.ChainID, models.MetadataBackfillActive, models.MetadataBackfillCurrentBlock, models.MetadataBackfillTargetBlock).
			WillReturnError(errors.New("metadata failed"))

		_, _, err := idx.backfillResumeBlock(100, 200, true)
		if err == nil || !strings.Contains(err.Error(), "failed to get backfill cursor metadata") {
			t.Fatalf("expected metadata error, got %v", err)
		}
	})
}

func TestGetBackfillCursorState_ParseErrors(t *testing.T) {
	tests := []struct {
		name      string
		key       string
		value     string
		wantError string
	}{
		{
			name:      "active",
			key:       models.MetadataBackfillActive,
			value:     "not-bool",
			wantError: "failed to parse backfill active metadata",
		},
		{
			name:      "current block",
			key:       models.MetadataBackfillCurrentBlock,
			value:     "not-uint",
			wantError: "failed to parse backfill current block metadata",
		},
		{
			name:      "target block",
			key:       models.MetadataBackfillTargetBlock,
			value:     "not-uint",
			wantError: "failed to parse backfill target block metadata",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idx := newTestIndexer()
			idxDB, mock := newMockIndexerDB(t)
			idx.db = idxDB

			rows := sqlmock.NewRows([]string{"key", "value"}).AddRow(tt.key, tt.value)
			mock.ExpectQuery("SELECT key, value").
				WithArgs(idx.network.ChainID, models.MetadataBackfillActive, models.MetadataBackfillCurrentBlock, models.MetadataBackfillTargetBlock).
				WillReturnRows(rows)

			_, err := idx.getBackfillCursorState()
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("expected %q error, got %v", tt.wantError, err)
			}
		})
	}
}

func TestGetCurrentBlock(t *testing.T) {
	client, _ := newMockEthClient(t, 77)
	idx := newTestIndexer()
	idx.ethClient = client

	block, err := idx.GetCurrentBlock(context.Background())
	if err != nil {
		t.Fatalf("GetCurrentBlock() error = %v", err)
	}
	if block != 77 {
		t.Fatalf("expected current block 77, got %d", block)
	}
}

func TestRunBlockIndexer_QueuesBlocks(t *testing.T) {
	client, _ := newMockEthClient(t, 5)
	idx := newTestIndexer()
	idx.ethClient = client
	idx.pollingInterval = 5 * time.Millisecond
	idx.batchSize = 3
	idx.blockTaskCh = make(chan BlockTask, 100)

	done := make(chan struct{})
	go func() {
		idx.runBlockIndexer(1)
		close(done)
	}()

	got := make([]uint64, 0, 5)
	timeout := time.After(300 * time.Millisecond)
	for len(got) < 5 {
		select {
		case task := <-idx.blockTaskCh:
			got = append(got, task.BlockNumber)
		case <-timeout:
			t.Fatalf("timed out waiting for queued blocks; got %v", got)
		}
	}

	idx.cancel()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("runBlockIndexer did not stop after cancel")
	}

	want := []uint64{1, 2, 3, 4, 5}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("queued block %d: got %d, want %d", i, got[i], w)
		}
	}
}

func TestRunBlockIndexer_ResetsOnReorgSignal(t *testing.T) {
	collectTasks := func(t *testing.T, idx *Indexer, n int) []uint64 {
		t.Helper()
		got := make([]uint64, 0, n)
		timeout := time.After(300 * time.Millisecond)
		for len(got) < n {
			select {
			case task := <-idx.blockTaskCh:
				got = append(got, task.BlockNumber)
			case <-timeout:
				t.Fatalf("timed out waiting for reorg-queued blocks; got %v", got)
			}
		}
		return got
	}

	t.Run("rewinds when fork point is below walker position", func(t *testing.T) {
		client, _ := newMockEthClient(t, 10)
		idx := newTestIndexer()
		idx.ethClient = client
		idx.pollingInterval = 5 * time.Millisecond
		idx.batchSize = 2
		idx.blockTaskCh = make(chan BlockTask, 10)
		atomic.StoreUint64(&idx.lastIndexedBlock, 8)
		idx.signalReorgReset(9, 10)

		done := make(chan struct{})
		go func() {
			// The walker had already caught up past the fork point.
			idx.runBlockIndexer(11)
			close(done)
		}()

		got := collectTasks(t, idx, 2)

		idx.cancel()
		<-done

		if got[0] != 9 || got[1] != 10 {
			t.Fatalf("expected blocks [9 10], got %v", got)
		}
	})

	t.Run("heals tip reorg above walker without abandoning historical walk", func(t *testing.T) {
		client, _ := newMockEthClient(t, 10)
		idx := newTestIndexer()
		idx.ethClient = client
		idx.pollingInterval = 5 * time.Millisecond
		idx.batchSize = 2
		idx.blockTaskCh = make(chan BlockTask, 10)
		atomic.StoreUint64(&idx.lastIndexedBlock, 8)
		idx.signalReorgReset(9, 10)

		done := make(chan struct{})
		go func() {
			// The walker is deep in a historical backfill, far below the fork
			// point. It must re-queue the invalidated tip range directly and
			// keep walking from its own position — teleporting to the tip is
			// what orphaned the historical range in the 2026-07-05 incident.
			idx.runBlockIndexer(1)
			close(done)
		}()

		got := collectTasks(t, idx, 4)

		idx.cancel()
		<-done

		want := []uint64{9, 10, 1, 2}
		for i, w := range want {
			if got[i] != w {
				t.Fatalf("expected tasks %v, got %v", want, got)
			}
		}
	})
}

func TestSignalReorgReset_MergesPendingRanges(t *testing.T) {
	idx := newTestIndexer()

	idx.signalReorgReset(50, 60)
	// A second reorg lands before the main loop consumes the first — the
	// invalidated ranges must merge, not clobber each other.
	idx.signalReorgReset(40, 55)

	if atomic.LoadUint32(&idx.reorgDetected) != 1 {
		t.Fatal("expected reorgDetected flag to be set")
	}
	from, through := idx.consumeReorgReset()
	if from != 40 || through != 60 {
		t.Fatalf("expected merged range [40 60], got [%d %d]", from, through)
	}
	if atomic.LoadUint32(&idx.reorgDetected) != 0 {
		t.Fatal("expected reorgDetected flag to be cleared after consume")
	}
}

func TestStart_ErrorsAndSuccessPath(t *testing.T) {
	t.Run("last indexed block lookup error", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB
		idx.ethClient, _ = newMockEthClient(t, 0)
		idx.attribution = attribution.NewService(idxDB)
		idx.attribution.SetChainID(idx.network.ChainID)

		mock.ExpectQuery(regexp.QuoteMeta("SELECT value FROM indexer_metadata WHERE chain_id = $1 AND key = $2")).
			WithArgs(idx.network.ChainID, models.MetadataLastIndexedBlock).
			WillReturnError(errors.New("metadata lookup failed"))

		err := idx.Start()
		if err == nil || !strings.Contains(err.Error(), "failed to get last indexed block") {
			t.Fatalf("expected get last block error, got %v", err)
		}
	})

	t.Run("determine start block parse error", func(t *testing.T) {
		idx := newTestIndexer()
		idx.network.StartBlock = "bad"
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB
		idx.ethClient, _ = newMockEthClient(t, 0)
		idx.attribution = attribution.NewService(idxDB)
		idx.attribution.SetChainID(idx.network.ChainID)

		mock.ExpectQuery(regexp.QuoteMeta("SELECT value FROM indexer_metadata WHERE chain_id = $1 AND key = $2")).
			WithArgs(idx.network.ChainID, models.MetadataLastIndexedBlock).
			WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("0"))

		err := idx.Start()
		if err == nil || !strings.Contains(err.Error(), "failed to determine start block") {
			t.Fatalf("expected determineStartBlock error, got %v", err)
		}
	})

	t.Run("success and stop", func(t *testing.T) {
		idx := newTestIndexer()
		idx.pollingInterval = 5 * time.Millisecond
		idx.mempoolPollingInterval = 5 * time.Millisecond
		idx.network.StartBlock = "1000" // keep runBlockIndexer from queueing work

		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB
		idx.ethClient, _ = newMockEthClient(t, 10)
		idx.attribution = attribution.NewService(idxDB)
		idx.attribution.SetChainID(idx.network.ChainID)

		mock.ExpectQuery(regexp.QuoteMeta("SELECT value FROM indexer_metadata WHERE chain_id = $1 AND key = $2")).
			WithArgs(idx.network.ChainID, models.MetadataLastIndexedBlock).
			WillReturnError(sql.ErrNoRows)

		if err := idx.Start(); err != nil {
			t.Fatalf("Start() error = %v", err)
		}

		time.Sleep(30 * time.Millisecond)
		idx.Stop()
	})

	t.Run("websocket enabled with subscription fallback", func(t *testing.T) {
		idx := newTestIndexer()
		idx.pollingInterval = 5 * time.Millisecond
		idx.mempoolPollingInterval = 5 * time.Millisecond
		idx.network.StartBlock = "1000" // keep runBlockIndexer from queueing work
		idx.useWebsocket = true

		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB
		idx.ethClient, _ = newMockEthClient(t, 10) // HTTP client, subscribe calls will fail
		idx.attribution = attribution.NewService(idxDB)
		idx.attribution.SetChainID(idx.network.ChainID)

		mock.ExpectQuery(regexp.QuoteMeta("SELECT value FROM indexer_metadata WHERE chain_id = $1 AND key = $2")).
			WithArgs(idx.network.ChainID, models.MetadataLastIndexedBlock).
			WillReturnError(sql.ErrNoRows)

		if err := idx.Start(); err != nil {
			t.Fatalf("Start() error = %v", err)
		}

		time.Sleep(30 * time.Millisecond)
		idx.Stop()
	})
}

func TestSeedStartupGapRecovery_SeedsAndRequeuesMissingBlocks(t *testing.T) {
	idx := newTestIndexer()
	idx.startupGapScanBlocks = 1000
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB

	// lastBlock 500 < window 1000, so the scan window clamps to the numeric
	// configured start: [100, 500], with no earliest-indexed floor.
	rows := sqlmock.NewRows([]string{"block_number"}).AddRow(uint64(495)).AddRow(uint64(498))
	mock.ExpectQuery("SELECT gs.block_number\\s+FROM generate_series").
		WithArgs(idx.network.ChainID, uint64(100), uint64(500), 1000).
		WillReturnRows(rows)

	idx.seedStartupGapRecovery(500)

	idx.failedBlocksMu.Lock()
	seeded495 := idx.failedBlocks[495]
	seeded498 := idx.failedBlocks[498]
	total := len(idx.failedBlocks)
	idx.failedBlocksMu.Unlock()
	if seeded495 != 1 || seeded498 != 1 || total != 2 {
		t.Fatalf("expected blocks 495 and 498 seeded once, got %v", idx.failedBlocks)
	}

	// The existing gap scanner machinery must pick the seeds up and re-queue
	// them like any other failed block.
	idx.retryFailedBlocks()
	requeued := map[uint64]bool{}
	for range 2 {
		select {
		case task := <-idx.blockTaskCh:
			requeued[task.BlockNumber] = true
		default:
			t.Fatalf("expected 2 re-queued tasks, got %v", requeued)
		}
	}
	if !requeued[495] || !requeued[498] {
		t.Fatalf("expected blocks 495 and 498 re-queued, got %v", requeued)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestSeedStartupGapRecovery_WindowClampedBelowWatermark(t *testing.T) {
	idx := newTestIndexer()
	idx.startupGapScanBlocks = 1000
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB

	// lastBlock 10000 with a 1000-block window scans [9001, 10000] only —
	// the numeric configured start (100) is below the window and irrelevant.
	mock.ExpectQuery("SELECT gs.block_number\\s+FROM generate_series").
		WithArgs(idx.network.ChainID, uint64(9001), uint64(10_000), 1000).
		WillReturnRows(sqlmock.NewRows([]string{"block_number"}))

	idx.seedStartupGapRecovery(10_000)

	idx.failedBlocksMu.Lock()
	total := len(idx.failedBlocks)
	idx.failedBlocksMu.Unlock()
	if total != 0 {
		t.Fatalf("expected no seeds for a gap-free window, got %v", idx.failedBlocks)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestSeedStartupGapRecovery_Disabled(t *testing.T) {
	t.Run("no database", func(t *testing.T) {
		idx := newTestIndexer()
		idx.startupGapScanBlocks = 1000
		idx.db = nil

		idx.seedStartupGapRecovery(500)

		if len(idx.failedBlocks) != 0 {
			t.Fatalf("expected no seeds without a database, got %v", idx.failedBlocks)
		}
	})

	t.Run("zero window", func(t *testing.T) {
		idx := newTestIndexer()
		idx.startupGapScanBlocks = 0
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		idx.seedStartupGapRecovery(500)

		if len(idx.failedBlocks) != 0 {
			t.Fatalf("expected no seeds with the scan disabled, got %v", idx.failedBlocks)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unexpected query with the scan disabled: %v", err)
		}
	})
}

func TestSeedStartupGapRecovery_QueryErrorIsNonFatal(t *testing.T) {
	idx := newTestIndexer()
	idx.startupGapScanBlocks = 1000
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB

	mock.ExpectQuery("SELECT gs.block_number\\s+FROM generate_series").
		WithArgs(idx.network.ChainID, uint64(100), uint64(500), 1000).
		WillReturnError(errors.New("scan failed"))

	idx.seedStartupGapRecovery(500)

	if len(idx.failedBlocks) != 0 {
		t.Fatalf("expected no seeds on query error, got %v", idx.failedBlocks)
	}
}

// Regression test for the bootstrap-crash review finding: with a knowable
// intended start there must be no earliest-indexed floor, or a crash that
// commits only the highest queued block would hide the missing prefix behind
// MIN(block_number) forever.
func TestSeedStartupGapRecovery_FloorSelection(t *testing.T) {
	t.Run("empty start block scans from zero unfloored", func(t *testing.T) {
		idx := newTestIndexer()
		idx.network.StartBlock = ""
		idx.startupGapScanBlocks = 1000
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectQuery("SELECT gs.block_number\\s+FROM generate_series").
			WithArgs(idx.network.ChainID, uint64(0), uint64(500), 1000).
			WillReturnRows(sqlmock.NewRows([]string{"block_number"}).AddRow(uint64(0)))

		idx.seedStartupGapRecovery(500)

		idx.failedBlocksMu.Lock()
		seeded := idx.failedBlocks[0]
		idx.failedBlocksMu.Unlock()
		if seeded != 1 {
			t.Fatalf("expected genesis-adjacent gap seeded, got %v", idx.failedBlocks)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet sqlmock expectations: %v", err)
		}
	})

	t.Run("latest start block floors at earliest indexed row", func(t *testing.T) {
		idx := newTestIndexer()
		idx.network.StartBlock = "LATEST-20"
		idx.startupGapScanBlocks = 1000
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		// The tip LATEST resolved to at first boot is not persisted, so the
		// scan must keep the earliest-indexed floor.
		mock.ExpectQuery("WITH bounds AS").
			WithArgs(idx.network.ChainID, uint64(0), uint64(500), 1000).
			WillReturnRows(sqlmock.NewRows([]string{"block_number"}).AddRow(uint64(495)))

		idx.seedStartupGapRecovery(500)

		idx.failedBlocksMu.Lock()
		seeded := idx.failedBlocks[495]
		idx.failedBlocksMu.Unlock()
		if seeded != 1 {
			t.Fatalf("expected block 495 seeded, got %v", idx.failedBlocks)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet sqlmock expectations: %v", err)
		}
	})

	t.Run("configured start above window clamps scan away entirely", func(t *testing.T) {
		idx := newTestIndexer()
		idx.network.StartBlock = "600"
		idx.startupGapScanBlocks = 1000
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		// windowStart clamps to 600 > lastBlock 500: nothing at or above the
		// configured start can be missing below the watermark, and no query
		// is issued.
		idx.seedStartupGapRecovery(500)

		if len(idx.failedBlocks) != 0 {
			t.Fatalf("expected no seeds, got %v", idx.failedBlocks)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unexpected query: %v", err)
		}
	})
}

// Regression test for the crash-recovery hole where updateLastIndexedBlock
// persisted a watermark above uncommitted blocks (parallel workers commit out
// of order) and a steady-state restart resumed from watermark+1, orphaning
// them: Start must scan the recent window below the watermark and hand any
// gaps to the gap scanner.
func TestStart_SteadyStateResumeSeedsOrphanedBlocks(t *testing.T) {
	idx := newTestIndexer()
	idx.pollingInterval = 5 * time.Millisecond
	idx.mempoolPollingInterval = 5 * time.Millisecond
	idx.network.StartBlock = "100"
	idx.startupGapScanBlocks = 1000

	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB
	idx.ethClient, _ = newMockEthClient(t, 10)
	idx.attribution = attribution.NewService(idxDB)
	idx.attribution.SetChainID(idx.network.ChainID)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT value FROM indexer_metadata WHERE chain_id = $1 AND key = $2")).
		WithArgs(idx.network.ChainID, models.MetadataLastIndexedBlock).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("500"))
	// Backfill inactive: determineStartBlock falls through to watermark+1.
	mock.ExpectQuery("SELECT key, value").
		WithArgs(idx.network.ChainID, models.MetadataBackfillActive, models.MetadataBackfillCurrentBlock, models.MetadataBackfillTargetBlock).
		WillReturnRows(sqlmock.NewRows([]string{"key", "value"}).AddRow(models.MetadataBackfillActive, "false"))
	// Block 497 committed nothing before the crash even though the watermark
	// reached 500. Numeric start 100 clamps the window and drops the floor.
	mock.ExpectQuery("SELECT gs.block_number\\s+FROM generate_series").
		WithArgs(idx.network.ChainID, uint64(100), uint64(500), 1000).
		WillReturnRows(sqlmock.NewRows([]string{"block_number"}).AddRow(uint64(497)))

	if err := idx.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer idx.Stop()

	idx.failedBlocksMu.Lock()
	seeded := idx.failedBlocks[497]
	idx.failedBlocksMu.Unlock()
	if seeded != 1 {
		t.Fatalf("expected orphaned block 497 seeded for the gap scanner, got %v", idx.failedBlocks)
	}
}

func TestGetLastIndexedBlock_DBPaths(t *testing.T) {
	t.Run("no rows", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectQuery(regexp.QuoteMeta("SELECT value FROM indexer_metadata WHERE chain_id = $1 AND key = $2")).
			WithArgs(idx.network.ChainID, models.MetadataLastIndexedBlock).
			WillReturnError(sql.ErrNoRows)

		block, err := idx.getLastIndexedBlock()
		if err != nil {
			t.Fatalf("expected nil error for missing metadata, got %v", err)
		}
		if block != 0 {
			t.Fatalf("expected 0 block for missing metadata, got %d", block)
		}
	})

	t.Run("parse error", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectQuery(regexp.QuoteMeta("SELECT value FROM indexer_metadata WHERE chain_id = $1 AND key = $2")).
			WithArgs(idx.network.ChainID, models.MetadataLastIndexedBlock).
			WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("not-a-number"))

		_, err := idx.getLastIndexedBlock()
		if err == nil || !strings.Contains(err.Error(), "failed to parse last indexed block") {
			t.Fatalf("expected parse error, got %v", err)
		}
	})

	t.Run("success", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectQuery(regexp.QuoteMeta("SELECT value FROM indexer_metadata WHERE chain_id = $1 AND key = $2")).
			WithArgs(idx.network.ChainID, models.MetadataLastIndexedBlock).
			WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("123"))

		block, err := idx.getLastIndexedBlock()
		if err != nil {
			t.Fatalf("getLastIndexedBlock() error = %v", err)
		}
		if block != 123 {
			t.Fatalf("expected 123, got %d", block)
		}
	})
}

func TestUpdateLastIndexedBlock(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB
	atomic.StoreUint64(&idx.lastIndexedBlock, 10)

	mock.ExpectExec("INSERT INTO indexer_metadata").
		WithArgs(idx.network.ChainID, models.MetadataLastIndexedBlock, "12", models.MetadataLastIndexedAt, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 2))

	idx.updateLastIndexedBlock(12)
	if got := idx.GetLastIndexedBlock(); got != 12 {
		t.Fatalf("expected last indexed block 12, got %d", got)
	}

	idx.updateLastIndexedBlock(11) // should no-op
	if got := idx.GetLastIndexedBlock(); got != 12 {
		t.Fatalf("expected last indexed block to remain 12, got %d", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestUpdateCurrentChainHead(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB

	observedAt := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	mock.ExpectExec("INSERT INTO indexer_metadata").
		WithArgs(idx.network.ChainID,
			models.MetadataCurrentChainHead, "99",
			models.MetadataChainHeadUpdatedAt, models.FormatMetadataTimestamp(observedAt)).
		WillReturnResult(sqlmock.NewResult(1, 2))

	idx.updateCurrentChainHead(99, observedAt)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestUpdateBackfillStatus(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB

	observedAt := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	mock.ExpectExec("INSERT INTO indexer_metadata").
		WithArgs(idx.network.ChainID,
			models.MetadataBackfillActive, "true",
			models.MetadataBackfillStartBlock, "10",
			models.MetadataBackfillCurrentBlock, "15",
			models.MetadataBackfillTargetBlock, "20",
			models.MetadataBackfillUpdatedAt, models.FormatMetadataTimestamp(observedAt)).
		WillReturnResult(sqlmock.NewResult(1, 5))

	idx.updateBackfillStatus(true, 10, 15, 20, observedAt)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestReindex(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB
		idx.blockTaskCh = make(chan BlockTask, 10)

		mock.ExpectBegin()
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE chain_id = $1 AND block_number >= $2 AND block_number <= $3")).
			WithArgs(idx.network.ChainID, uint64(5), uint64(7)).
			WillReturnResult(sqlmock.NewResult(0, 3))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM block_metrics WHERE chain_id = $1 AND block_number >= $2 AND block_number <= $3")).
			WithArgs(idx.network.ChainID, uint64(5), uint64(7)).
			WillReturnResult(sqlmock.NewResult(0, 3))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM indexed_blocks WHERE chain_id = $1 AND block_number >= $2 AND block_number <= $3")).
			WithArgs(idx.network.ChainID, uint64(5), uint64(7)).
			WillReturnResult(sqlmock.NewResult(0, 3))
		mock.ExpectCommit()

		if err := idx.Reindex(5, 7); err != nil {
			t.Fatalf("Reindex() error = %v", err)
		}
		if got := atomic.LoadUint64(&idx.reorgEpoch); got != 1 {
			t.Fatalf("expected reindex cleanup to bump reorgEpoch to 1, got %d", got)
		}

		var got []uint64
		for {
			select {
			case task := <-idx.blockTaskCh:
				got = append(got, task.BlockNumber)
			default:
				goto done
			}
		}
	done:
		if len(got) != 3 || got[0] != 5 || got[1] != 6 || got[2] != 7 {
			t.Fatalf("unexpected queued blocks: %v", got)
		}
	})

	t.Run("delete blobs error", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectBegin()
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE chain_id = $1 AND block_number >= $2 AND block_number <= $3")).
			WithArgs(idx.network.ChainID, uint64(5), uint64(7)).
			WillReturnError(errors.New("delete blobs failed"))
		mock.ExpectRollback()

		err := idx.Reindex(5, 7)
		if err == nil || !strings.Contains(err.Error(), "failed to delete existing blob records") {
			t.Fatalf("expected delete blobs error, got %v", err)
		}
	})

	t.Run("delete indexed blocks error", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectBegin()
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE chain_id = $1 AND block_number >= $2 AND block_number <= $3")).
			WithArgs(idx.network.ChainID, uint64(5), uint64(7)).
			WillReturnResult(sqlmock.NewResult(0, 3))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM block_metrics WHERE chain_id = $1 AND block_number >= $2 AND block_number <= $3")).
			WithArgs(idx.network.ChainID, uint64(5), uint64(7)).
			WillReturnResult(sqlmock.NewResult(0, 3))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM indexed_blocks WHERE chain_id = $1 AND block_number >= $2 AND block_number <= $3")).
			WithArgs(idx.network.ChainID, uint64(5), uint64(7)).
			WillReturnError(errors.New("delete indexed failed"))
		mock.ExpectRollback()

		err := idx.Reindex(5, 7)
		if err == nil || !strings.Contains(err.Error(), "failed to delete existing indexed block records") {
			t.Fatalf("expected delete indexed error, got %v", err)
		}
	})
}

func TestClaimNextReindexRequest(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB

	rows := sqlmock.NewRows([]string{"id", "chain_id", "start_block", "end_block", "attempts", "claimed_by"}).
		AddRow(int64(12), idx.network.ChainID, int64(100), int64(105), 2, "blob-indexer/testnet/test-v1")
	mock.ExpectQuery("UPDATE block_reindex_requests").
		WithArgs(idx.network.ChainID, "blob-indexer/testnet/test-v1", int(reindexRequestStaleAfter.Seconds())).
		WillReturnRows(rows)

	request, err := idx.claimNextReindexRequest()
	if err != nil {
		t.Fatalf("claimNextReindexRequest() error = %v", err)
	}
	if request.ID != 12 || request.ChainID != idx.network.ChainID || request.StartBlock != 100 || request.EndBlock != 105 || request.Attempts != 2 || request.ClaimedBy != "blob-indexer/testnet/test-v1" {
		t.Fatalf("unexpected request: %+v", request)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestReindexRequestStatusUpdates(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB
	request := blockReindexRequest{
		ID:        12,
		ChainID:   idx.network.ChainID,
		ClaimedBy: "blob-indexer/testnet/test-v1",
	}

	mock.ExpectExec("UPDATE block_reindex_requests").
		WithArgs(request.ID, idx.network.ChainID, request.ClaimedBy).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := idx.completeReindexRequest(request); err != nil {
		t.Fatalf("completeReindexRequest() error = %v", err)
	}

	request.ID = 13
	mock.ExpectExec("UPDATE block_reindex_requests").
		WithArgs(request.ID, idx.network.ChainID, "boom", request.ClaimedBy).
		WillReturnResult(sqlmock.NewResult(0, 1))
	idx.failReindexRequest(request, errors.New("boom"))

	request.ID = 14
	mock.ExpectExec("UPDATE block_reindex_requests").
		WithArgs(request.ID, idx.network.ChainID, request.ClaimedBy).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := idx.heartbeatReindexRequest(request); err != nil {
		t.Fatalf("heartbeatReindexRequest() error = %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestCountMissingIndexedBlocks(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB

	rows := sqlmock.NewRows([]string{"missing"}).AddRow(int64(3))
	mock.ExpectQuery("COUNT").
		WithArgs(idx.network.ChainID, uint64(100), uint64(105)).
		WillReturnRows(rows)

	missing, err := idx.countMissingIndexedBlocks(100, 105)
	if err != nil {
		t.Fatalf("countMissingIndexedBlocks() error = %v", err)
	}
	if missing != 3 {
		t.Fatalf("expected 3 missing blocks, got %d", missing)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestGetBlobCountsAndTopUsers(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB
	idx.attribution = attribution.NewService(idxDB)
	idx.attribution.SetChainID(idx.network.ChainID)

	countRows := sqlmock.NewRows([]string{"confirmed_count", "pending_count"}).AddRow(3, 2)
	mock.ExpectQuery("SELECT").WithArgs(idx.network.ChainID).WillReturnRows(countRows)

	confirmed, pending, err := idx.GetBlobCounts(context.Background())
	if err != nil {
		t.Fatalf("GetBlobCounts() error = %v", err)
	}
	if confirmed != 3 || pending != 2 {
		t.Fatalf("expected confirmed=3 pending=2, got %d/%d", confirmed, pending)
	}

	now := time.Now()
	userRows := sqlmock.NewRows([]string{"from_address", "user_attribution", "blob_count", "total_cost_wei", "last_timestamp"}).
		AddRow("0xabc", "alice", 5, "10", now)
	mock.ExpectQuery("SELECT").WithArgs(idx.network.ChainID, 5, 0).WillReturnRows(userRows)

	users, err := idx.GetTopBlobUsers(context.Background(), 5, 0)
	if err != nil {
		t.Fatalf("GetTopBlobUsers() error = %v", err)
	}
	if len(users) != 1 || users[0].Address != "0xabc" || users[0].BlobCount != 5 {
		t.Fatalf("unexpected top users result: %+v", users)
	}

	mock.ExpectQuery("SELECT").WithArgs(idx.network.ChainID).WillReturnError(errors.New("count failed"))
	_, _, err = idx.GetBlobCounts(context.Background())
	if err == nil {
		t.Fatal("expected GetBlobCounts error")
	}
}

func TestInsertPendingBlobs(t *testing.T) {
	t.Run("inserts new pending blobs", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB
		blob := newBlobFixture()

		mock.ExpectBegin()
		mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS (SELECT 1 FROM blobs WHERE chain_id = $1 AND tx_hash = $2 AND block_number >= 0)")).
			WithArgs(blob.ChainID, blob.TxHash).
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM mempool_blobs WHERE chain_id = $1 AND from_address = $2 AND nonce = $3 AND tx_hash <> $4")).
			WithArgs(blob.ChainID, blob.FromAddress, int64(blob.Nonce), blob.TxHash, blob.Timestamp).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM mempool_blobs WHERE chain_id = $1 AND tx_hash = $2 AND blob_index >= $3")).
			WithArgs(blob.ChainID, blob.TxHash, 1).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("INSERT INTO mempool_blobs").
			WithArgs(blob.ChainID, blob.TxHash, 0, blob.FromAddress, blob.UserAttribution,
				blob.BlobSizeBytes, blob.BaseFeePerBlobGas, blob.TipPerBlobGas, blob.TotalCostWei,
				blob.Timestamp, blob.MaxFeePerBlobGas, blob.BlobGasUsed, blob.VersionedHash, int64(blob.Nonce), blob.Timestamp).
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit()

		if err := idx.insertPendingBlobs([]models.Blob{blob}); err != nil {
			t.Fatalf("insertPendingBlobs() error = %v", err)
		}
	})

	t.Run("upserts multiple blobs at per-tx ordinals in one statement", func(t *testing.T) {
		// blob_index is the per-transaction ordinal (0..N-1): a re-poll upserts
		// the same rows in place via a single multi-row statement, and the
		// leading DELETE trims rows past the current blob count if the tx
		// shrank. Nothing here can grow unbounded, unlike the old pool-wide
		// index counter in blobs.
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB
		blob := newBlobFixture()
		blobs := []models.Blob{blob, blob, blob}

		mock.ExpectBegin()
		mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS")).
			WithArgs(blob.ChainID, blob.TxHash).
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM mempool_blobs WHERE chain_id = $1 AND from_address = $2 AND nonce = $3 AND tx_hash <> $4")).
			WithArgs(blob.ChainID, blob.FromAddress, int64(blob.Nonce), blob.TxHash, blob.Timestamp).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM mempool_blobs")).
			WithArgs(blob.ChainID, blob.TxHash, 3).
			WillReturnResult(sqlmock.NewResult(0, 0))
		upsertArgs := make([]driver.Value, 0, len(blobs)*mempoolBlobInsertColumns)
		for offset := 0; offset < len(blobs); offset++ {
			upsertArgs = append(upsertArgs,
				blob.ChainID, blob.TxHash, offset, blob.FromAddress, blob.UserAttribution,
				blob.BlobSizeBytes, blob.BaseFeePerBlobGas, blob.TipPerBlobGas, blob.TotalCostWei,
				blob.Timestamp, blob.MaxFeePerBlobGas, blob.BlobGasUsed, blob.VersionedHash, int64(blob.Nonce), blob.Timestamp)
		}
		mock.ExpectExec("INSERT INTO mempool_blobs").
			WithArgs(upsertArgs...).
			WillReturnResult(sqlmock.NewResult(int64(len(blobs)), int64(len(blobs))))
		mock.ExpectCommit()

		if err := idx.insertPendingBlobs(blobs); err != nil {
			t.Fatalf("insertPendingBlobs() error = %v", err)
		}
	})

	t.Run("skips when tx already confirmed", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB
		blob := newBlobFixture()

		mock.ExpectBegin()
		mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS")).
			WithArgs(blob.ChainID, blob.TxHash).
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
		mock.ExpectCommit()

		if err := idx.insertPendingBlobs([]models.Blob{blob}); err != nil {
			t.Fatalf("insertPendingBlobs() error = %v", err)
		}
	})

	t.Run("wraps superseded delete error", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB
		blob := newBlobFixture()

		mock.ExpectBegin()
		mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS")).
			WithArgs(blob.ChainID, blob.TxHash).
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM mempool_blobs")).
			WithArgs(blob.ChainID, blob.FromAddress, int64(blob.Nonce), blob.TxHash, blob.Timestamp).
			WillReturnError(errors.New("superseded delete failed"))
		mock.ExpectRollback()

		err := idx.insertPendingBlobs([]models.Blob{blob})
		if err == nil || !strings.Contains(err.Error(), "failed to delete superseded pending blobs") {
			t.Fatalf("expected wrapped superseded delete error, got %v", err)
		}
	})

	t.Run("wraps trim error", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB
		blob := newBlobFixture()

		mock.ExpectBegin()
		mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS")).
			WithArgs(blob.ChainID, blob.TxHash).
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM mempool_blobs")).
			WithArgs(blob.ChainID, blob.FromAddress, int64(blob.Nonce), blob.TxHash, blob.Timestamp).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM mempool_blobs")).
			WithArgs(blob.ChainID, blob.TxHash, 1).
			WillReturnError(errors.New("trim failed"))
		mock.ExpectRollback()

		err := idx.insertPendingBlobs([]models.Blob{blob})
		if err == nil || !strings.Contains(err.Error(), "failed to trim surplus pending blobs") {
			t.Fatalf("expected wrapped trim error, got %v", err)
		}
	})

	t.Run("wraps insert error", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB
		blob := newBlobFixture()

		mock.ExpectBegin()
		mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS")).
			WithArgs(blob.ChainID, blob.TxHash).
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM mempool_blobs")).
			WithArgs(blob.ChainID, blob.FromAddress, int64(blob.Nonce), blob.TxHash, blob.Timestamp).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM mempool_blobs")).
			WithArgs(blob.ChainID, blob.TxHash, 1).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("INSERT INTO mempool_blobs").
			WillReturnError(errors.New("insert failed"))
		mock.ExpectRollback()

		err := idx.insertPendingBlobs([]models.Blob{blob})
		if err == nil || !strings.Contains(err.Error(), "failed to insert pending blobs") {
			t.Fatalf("expected wrapped insert error, got %v", err)
		}
	})
}

func TestValuesPlaceholders(t *testing.T) {
	if got := valuesPlaceholders(2, 3, nil); got != "($1,$2,$3), ($4,$5,$6)" {
		t.Fatalf("unexpected placeholders: %q", got)
	}
	if got := valuesPlaceholders(1, 2, []string{"text", "int"}); got != "($1::text,$2::int)" {
		t.Fatalf("unexpected cast placeholders: %q", got)
	}

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on casts/width mismatch")
		}
	}()
	valuesPlaceholders(1, 3, []string{"text"})
}

func TestInsertBlockData(t *testing.T) {
	indexedBlock := models.IndexedBlock{ChainID: 42, BlockNumber: 10, BlockHash: "0xhash", ParentHash: "0xparent"}
	blob := newBlobFixture()

	t.Run("success", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectBegin()
		// Expect surplus-row trim for the block, then pending blob promotion
		// and superseded-replacement cleanup before the confirmed insert
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE chain_id = $1 AND block_number = $2 AND blob_index >= $3")).
			WithArgs(indexedBlock.ChainID, indexedBlock.BlockNumber, 1).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("DELETE FROM mempool_blobs WHERE").
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("DELETE FROM mempool_blobs m").
			WithArgs(blob.ChainID, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), blob.Timestamp).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("INSERT INTO blobs").
			WithArgs(blob.ChainID, blob.BlockNumber, blob.BlobIndex, blob.TxHash, blob.FromAddress, blob.UserAttribution,
				blob.BlobSizeBytes, blob.BaseFeePerBlobGas, blob.TipPerBlobGas, blob.TotalCostWei,
				blob.Timestamp, blob.MaxFeePerBlobGas, blob.BlobGasUsed, blob.VersionedHash).
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectExec("INSERT INTO indexed_blocks").
			WithArgs(indexedBlock.ChainID, indexedBlock.BlockNumber, indexedBlock.BlockHash, indexedBlock.ParentHash).
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit()

		if err := idx.insertBlockData([]models.Blob{blob}, indexedBlock, nil, 0); err != nil {
			t.Fatalf("insertBlockData() error = %v", err)
		}
	})

	t.Run("surplus trim error", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectBegin()
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE chain_id = $1 AND block_number = $2 AND blob_index >= $3")).
			WithArgs(indexedBlock.ChainID, indexedBlock.BlockNumber, 1).
			WillReturnError(errors.New("trim failed"))
		mock.ExpectRollback()

		err := idx.insertBlockData([]models.Blob{blob}, indexedBlock, nil, 0)
		if err == nil || !strings.Contains(err.Error(), "failed to trim surplus blob rows") {
			t.Fatalf("expected surplus trim error, got %v", err)
		}
	})

	t.Run("stale fetch rejected after reorg cleanup", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		// A cleanup committed after the caller fetched its block.
		atomic.StoreUint64(&idx.reorgEpoch, 1)

		err := idx.insertBlockData([]models.Blob{blob}, indexedBlock, nil, 0)
		if !errors.Is(err, errStaleBlockFetch) {
			t.Fatalf("expected errStaleBlockFetch, got %v", err)
		}
		// The fence must reject before any statement reaches the database.
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("stale insert touched the database: %v", err)
		}
	})

	t.Run("blob insert error", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectBegin()
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE chain_id = $1 AND block_number = $2 AND blob_index >= $3")).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("DELETE FROM mempool_blobs WHERE").
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("DELETE FROM mempool_blobs m").
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("INSERT INTO blobs").
			WillReturnError(errors.New("insert failed"))
		mock.ExpectRollback()

		err := idx.insertBlockData([]models.Blob{blob}, indexedBlock, nil, 0)
		if err == nil || !strings.Contains(err.Error(), "failed to insert blob") {
			t.Fatalf("expected blob insert error, got %v", err)
		}
	})

	t.Run("indexed block insert error", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectBegin()
		// A zero-blob block still trims: the canonical block may have replaced
		// a stale-fork version that carried blobs.
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE chain_id = $1 AND block_number = $2 AND blob_index >= $3")).
			WithArgs(indexedBlock.ChainID, indexedBlock.BlockNumber, 0).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("INSERT INTO indexed_blocks").
			WithArgs(indexedBlock.ChainID, indexedBlock.BlockNumber, indexedBlock.BlockHash, indexedBlock.ParentHash).
			WillReturnError(errors.New("indexed insert failed"))
		mock.ExpectRollback()

		err := idx.insertBlockData(nil, indexedBlock, nil, 0)
		if err == nil || !strings.Contains(err.Error(), "failed to record indexed block") {
			t.Fatalf("expected indexed block error, got %v", err)
		}
	})

	t.Run("commit error", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectBegin()
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE chain_id = $1 AND block_number = $2 AND blob_index >= $3")).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("INSERT INTO indexed_blocks").
			WithArgs(indexedBlock.ChainID, indexedBlock.BlockNumber, indexedBlock.BlockHash, indexedBlock.ParentHash).
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit().WillReturnError(errors.New("commit failed"))
		mock.ExpectRollback()

		err := idx.insertBlockData(nil, indexedBlock, nil, 0)
		if err == nil {
			t.Fatal("expected commit error")
		}
	})

	t.Run("with block metrics success", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		metrics := &models.BlockMetrics{
			ChainID:          42,
			BlockNumber:      10,
			BlockTimestamp:   time.Now(),
			BlobCount:        3,
			BlobGasUsed:      393216,
			BlobGasTarget:    393216,
			BlobGasLimit:     786432,
			ExcessBlobGas:    100000,
			BlobBaseFee:      "1",
			UtilizationRatio: "1.000000",
			BlobParamsTarget: 3,
			BlobParamsMax:    6,
			UpdateFraction:   3338477,
		}

		mock.ExpectBegin()
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE chain_id = $1 AND block_number = $2 AND blob_index >= $3")).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("INSERT INTO block_metrics").
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectExec("INSERT INTO indexed_blocks").
			WithArgs(indexedBlock.ChainID, indexedBlock.BlockNumber, indexedBlock.BlockHash, indexedBlock.ParentHash).
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit()

		if err := idx.insertBlockData(nil, indexedBlock, metrics, 0); err != nil {
			t.Fatalf("insertBlockData() error = %v", err)
		}
	})

	t.Run("block metrics insert error", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		metrics := &models.BlockMetrics{
			ChainID:     42,
			BlockNumber: 10,
		}

		mock.ExpectBegin()
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE chain_id = $1 AND block_number = $2 AND blob_index >= $3")).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("INSERT INTO block_metrics").
			WillReturnError(errors.New("metrics insert failed"))
		mock.ExpectRollback()

		err := idx.insertBlockData(nil, indexedBlock, metrics, 0)
		if err == nil || !strings.Contains(err.Error(), "failed to insert block metrics") {
			t.Fatalf("expected block metrics error, got %v", err)
		}
	})
}

func TestProcessBlock_NoBlobTransactions(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB
	idx.ethClient, _ = newMockEthClient(t, 10)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT block_hash FROM indexed_blocks WHERE chain_id = $1 AND block_number = $2")).
		WithArgs(idx.network.ChainID, uint64(0)).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE chain_id = $1 AND block_number = $2 AND blob_index >= $3")).
		WithArgs(idx.network.ChainID, int64(1), 0).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO block_metrics").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO indexed_blocks").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := idx.processBlock(1); err != nil {
		t.Fatalf("processBlock() error = %v", err)
	}
}

func TestProcessBlock_WithBlobTransaction(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB
	ethClient, rpcSvc := newMockEthClient(t, 10)
	idx.ethClient = ethClient

	blobTx := newSignedBlobTx(t, int64(idx.network.ChainID), 7)
	rpcSvc.blockTxs = []*types.Transaction{blobTx}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT block_hash FROM indexed_blocks WHERE chain_id = $1 AND block_number = $2")).
		WithArgs(idx.network.ChainID, uint64(0)).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectBegin()
	// Expect surplus-row trim for the block
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE chain_id = $1 AND block_number = $2 AND blob_index >= $3")).
		WithArgs(idx.network.ChainID, int64(1), 1).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Expect pending blob promotion cleanup, then superseded-replacement
	// cleanup keyed on the confirmed tx's (sender, nonce)
	mock.ExpectExec("DELETE FROM mempool_blobs WHERE").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("DELETE FROM mempool_blobs m").
		WithArgs(idx.network.ChainID, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO blobs").
		WithArgs(
			idx.network.ChainID,
			int64(1),
			0,
			blobTx.Hash().Hex(),
			sqlmock.AnyArg(),
			"",
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),             // max_fee_per_blob_gas
			sqlmock.AnyArg(),             // blob_gas_used
			blobTx.BlobHashes()[0].Hex(), // versioned_hash
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO block_metrics").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO indexed_blocks").
		WithArgs(idx.network.ChainID, int64(1), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := idx.processBlock(1); err != nil {
		t.Fatalf("processBlock() error = %v", err)
	}
}

// TestProcessBlock_BlobBaseFeeError is no longer applicable because blob base fee
// is now computed from the block header (ExcessBlobGas) instead of calling the
// eth_blobBaseFee RPC. The RPC-based error path no longer exists in processBlock.

func TestCheckForReorg_Branches(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB
	idx.ethClient, _ = newMockEthClient(t, 10)

	block, err := idx.ethClient.GetBlockByNumber(context.Background(), 1)
	if err != nil {
		t.Fatalf("failed to build test block: %v", err)
	}

	if err := idx.checkForReorg(0, block); err != nil {
		t.Fatalf("block 0 should skip reorg checks: %v", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT block_hash FROM indexed_blocks WHERE chain_id = $1 AND block_number = $2")).
		WithArgs(idx.network.ChainID, uint64(0)).
		WillReturnError(sql.ErrNoRows)
	if err := idx.checkForReorg(1, block); err != nil {
		t.Fatalf("sql.ErrNoRows should be treated as no-reorg: %v", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT block_hash FROM indexed_blocks WHERE chain_id = $1 AND block_number = $2")).
		WithArgs(idx.network.ChainID, uint64(0)).
		WillReturnRows(sqlmock.NewRows([]string{"block_hash"}).AddRow(block.ParentHash().Hex()))
	if err := idx.checkForReorg(1, block); err != nil {
		t.Fatalf("matching parent hash should not trigger reorg: %v", err)
	}
}

func TestCheckForReorg_DetectsMismatchAndRewinds(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB
	idx.ethClient, _ = newMockEthClient(t, 10)

	block, err := idx.ethClient.GetBlockByNumber(context.Background(), 5)
	if err != nil {
		t.Fatalf("failed to get test block: %v", err)
	}
	forkBlock := uint64(4)
	forkBlockHash, err := idx.ethClient.GetBlockByNumber(context.Background(), forkBlock)
	if err != nil {
		t.Fatalf("failed to get fork block: %v", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT block_hash FROM indexed_blocks WHERE chain_id = $1 AND block_number = $2")).
		WithArgs(idx.network.ChainID, uint64(4)).
		WillReturnRows(sqlmock.NewRows([]string{"block_hash"}).AddRow("0xdeadbeef"))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT block_hash FROM indexed_blocks WHERE chain_id = $1 AND block_number = $2")).
		WithArgs(idx.network.ChainID, uint64(4)).
		WillReturnRows(sqlmock.NewRows([]string{"block_hash"}).AddRow(forkBlockHash.Hash().Hex()))
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT MAX(block_number) FROM indexed_blocks WHERE chain_id = $1")).
		WithArgs(idx.network.ChainID).
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(int64(8)))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE chain_id = $1 AND block_number >= $2")).
		WithArgs(idx.network.ChainID, int64(5)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM block_metrics WHERE chain_id = $1 AND block_number >= $2")).
		WithArgs(idx.network.ChainID, int64(5)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM indexed_blocks WHERE chain_id = $1 AND block_number >= $2")).
		WithArgs(idx.network.ChainID, uint64(5)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT key, value").
		WithArgs(idx.network.ChainID, models.MetadataReorgRewindFrom, models.MetadataReorgInvalidatedThrough).
		WillReturnRows(sqlmock.NewRows([]string{"key", "value"}))
	mock.ExpectExec("INSERT INTO indexer_metadata").
		WithArgs(idx.network.ChainID, models.MetadataReorgRewindFrom, "5", models.MetadataReorgInvalidatedThrough, "8").
		WillReturnResult(sqlmock.NewResult(1, 2))
	mock.ExpectExec("INSERT INTO indexer_metadata").
		WithArgs(idx.network.ChainID, models.MetadataLastIndexedBlock, "4").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	err = idx.checkForReorg(5, block)
	if err == nil || !errors.Is(err, errReorgDetected) {
		t.Fatalf("expected errReorgDetected, got %v", err)
	}
}

func TestHandleReorg_SuccessAndError(t *testing.T) {
	t.Run("success path rewinds state", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB
		idx.ethClient, _ = newMockEthClient(t, 10)

		forkBlock := uint64(4)
		block, err := idx.ethClient.GetBlockByNumber(context.Background(), forkBlock)
		if err != nil {
			t.Fatalf("failed to get block for expected hash: %v", err)
		}
		expectedHash := block.Hash().Hex()

		mock.ExpectQuery(regexp.QuoteMeta("SELECT block_hash FROM indexed_blocks WHERE chain_id = $1 AND block_number = $2")).
			WithArgs(idx.network.ChainID, forkBlock).
			WillReturnRows(sqlmock.NewRows([]string{"block_hash"}).AddRow(expectedHash))
		mock.ExpectBegin()
		mock.ExpectQuery(regexp.QuoteMeta("SELECT MAX(block_number) FROM indexed_blocks WHERE chain_id = $1")).
			WithArgs(idx.network.ChainID).
			WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(int64(7)))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE chain_id = $1 AND block_number >= $2")).
			WithArgs(idx.network.ChainID, int64(forkBlock+1)).
			WillReturnResult(sqlmock.NewResult(0, 2))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM block_metrics WHERE chain_id = $1 AND block_number >= $2")).
			WithArgs(idx.network.ChainID, int64(forkBlock+1)).
			WillReturnResult(sqlmock.NewResult(0, 2))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM indexed_blocks WHERE chain_id = $1 AND block_number >= $2")).
			WithArgs(idx.network.ChainID, forkBlock+1).
			WillReturnResult(sqlmock.NewResult(0, 2))
		mock.ExpectQuery("SELECT key, value").
			WithArgs(idx.network.ChainID, models.MetadataReorgRewindFrom, models.MetadataReorgInvalidatedThrough).
			WillReturnRows(sqlmock.NewRows([]string{"key", "value"}))
		mock.ExpectExec("INSERT INTO indexer_metadata").
			WithArgs(idx.network.ChainID, models.MetadataReorgRewindFrom, "5", models.MetadataReorgInvalidatedThrough, "7").
			WillReturnResult(sqlmock.NewResult(1, 2))
		mock.ExpectExec("INSERT INTO indexer_metadata").
			WithArgs(idx.network.ChainID, models.MetadataLastIndexedBlock, strconv.FormatUint(forkBlock, 10)).
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit()

		err = idx.handleReorg(5)
		if err == nil || !errors.Is(err, errReorgDetected) {
			t.Fatalf("expected errReorgDetected wrapper, got %v", err)
		}
		if got := idx.GetLastIndexedBlock(); got != forkBlock {
			t.Fatalf("expected lastIndexedBlock=%d, got %d", forkBlock, got)
		}
		if atomic.LoadUint32(&idx.reorgDetected) != 1 {
			t.Fatal("expected reorgDetected flag to be set")
		}
		if got := atomic.LoadUint64(&idx.reorgEpoch); got != 1 {
			t.Fatalf("expected reorgEpoch=1 after cleanup, got %d", got)
		}
		from, through := idx.consumeReorgReset()
		if from != forkBlock+1 || through != 7 {
			t.Fatalf("expected invalidated range [%d 7], got [%d %d]", forkBlock+1, from, through)
		}
	})

	t.Run("invalidated range lookup error", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB
		idx.ethClient, _ = newMockEthClient(t, 10)

		forkBlock := uint64(4)
		block, _ := idx.ethClient.GetBlockByNumber(context.Background(), forkBlock)
		expectedHash := block.Hash().Hex()

		mock.ExpectQuery(regexp.QuoteMeta("SELECT block_hash FROM indexed_blocks WHERE chain_id = $1 AND block_number = $2")).
			WithArgs(idx.network.ChainID, forkBlock).
			WillReturnRows(sqlmock.NewRows([]string{"block_hash"}).AddRow(expectedHash))
		mock.ExpectBegin()
		mock.ExpectQuery(regexp.QuoteMeta("SELECT MAX(block_number) FROM indexed_blocks WHERE chain_id = $1")).
			WithArgs(idx.network.ChainID).
			WillReturnError(errors.New("max lookup failed"))
		mock.ExpectRollback()

		err := idx.handleReorg(5)
		if err == nil || !strings.Contains(err.Error(), "failed to determine reorg invalidated range") {
			t.Fatalf("expected invalidated range error, got %v", err)
		}
	})

	t.Run("block lookup error", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, _ := newMockIndexerDB(t)
		idx.db = idxDB
		ethClient, rpcSvc := newMockEthClient(t, 10)
		rpcSvc.failBlock = true
		idx.ethClient = ethClient

		err := idx.handleReorg(5)
		if err == nil || !strings.Contains(err.Error(), "failed to get block") {
			t.Fatalf("expected block lookup error, got %v", err)
		}
	})

	t.Run("delete block metrics error", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB
		idx.ethClient, _ = newMockEthClient(t, 10)

		forkBlock := uint64(4)
		block, _ := idx.ethClient.GetBlockByNumber(context.Background(), forkBlock)
		expectedHash := block.Hash().Hex()

		mock.ExpectQuery(regexp.QuoteMeta("SELECT block_hash FROM indexed_blocks WHERE chain_id = $1 AND block_number = $2")).
			WithArgs(idx.network.ChainID, forkBlock).
			WillReturnRows(sqlmock.NewRows([]string{"block_hash"}).AddRow(expectedHash))
		mock.ExpectBegin()
		mock.ExpectQuery(regexp.QuoteMeta("SELECT MAX(block_number) FROM indexed_blocks WHERE chain_id = $1")).
			WithArgs(idx.network.ChainID).
			WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(int64(7)))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE chain_id = $1 AND block_number >= $2")).
			WithArgs(idx.network.ChainID, int64(forkBlock+1)).
			WillReturnResult(sqlmock.NewResult(0, 2))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM block_metrics WHERE chain_id = $1 AND block_number >= $2")).
			WithArgs(idx.network.ChainID, int64(forkBlock+1)).
			WillReturnError(errors.New("metrics delete failed"))
		mock.ExpectRollback()

		err := idx.handleReorg(5)
		if err == nil || !strings.Contains(err.Error(), "failed to delete reorged block metrics") {
			t.Fatalf("expected block metrics delete error, got %v", err)
		}
	})
}

// Regression test for the fetch/cleanup race: worker A fetches a soon-to-be-
// orphaned fork block via RPC, worker B's handleReorg then deletes every row
// past the fork point, and A's insertBlockData lands only after the cleanup.
// checkForReorg cannot catch A's late insert — the deleted parent row reads as
// a benign gap — so the reorg-epoch fence must reject it. The interleaving is
// serialized here: each step below is one side of the race in commit order.
func TestInsertBlockData_ReorgFencesStaleFetch(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB
	idx.ethClient, _ = newMockEthClient(t, 10)

	// Worker A samples the epoch and fetches block 6 from the old fork.
	fetchEpoch := atomic.LoadUint64(&idx.reorgEpoch)
	staleBlob := newBlobFixture()
	staleBlob.BlockNumber = 6
	staleBlock := models.IndexedBlock{ChainID: 42, BlockNumber: 6, BlockHash: "0xforkhash", ParentHash: "0xforkparent"}

	// Worker B hits the reorg at block 5 and rewinds to fork point 4,
	// deleting everything >= 5 — including the rows block 6 would join.
	forkBlock := uint64(4)
	canonical, err := idx.ethClient.GetBlockByNumber(context.Background(), forkBlock)
	if err != nil {
		t.Fatalf("failed to get canonical fork block: %v", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT block_hash FROM indexed_blocks WHERE chain_id = $1 AND block_number = $2")).
		WithArgs(idx.network.ChainID, forkBlock).
		WillReturnRows(sqlmock.NewRows([]string{"block_hash"}).AddRow(canonical.Hash().Hex()))
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT MAX(block_number) FROM indexed_blocks WHERE chain_id = $1")).
		WithArgs(idx.network.ChainID).
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(int64(8)))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE chain_id = $1 AND block_number >= $2")).
		WithArgs(idx.network.ChainID, int64(forkBlock+1)).
		WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM block_metrics WHERE chain_id = $1 AND block_number >= $2")).
		WithArgs(idx.network.ChainID, int64(forkBlock+1)).
		WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM indexed_blocks WHERE chain_id = $1 AND block_number >= $2")).
		WithArgs(idx.network.ChainID, forkBlock+1).
		WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectQuery("SELECT key, value").
		WithArgs(idx.network.ChainID, models.MetadataReorgRewindFrom, models.MetadataReorgInvalidatedThrough).
		WillReturnRows(sqlmock.NewRows([]string{"key", "value"}))
	mock.ExpectExec("INSERT INTO indexer_metadata").
		WithArgs(idx.network.ChainID, models.MetadataReorgRewindFrom, "5", models.MetadataReorgInvalidatedThrough, "8").
		WillReturnResult(sqlmock.NewResult(1, 2))
	mock.ExpectExec("INSERT INTO indexer_metadata").
		WithArgs(idx.network.ChainID, models.MetadataLastIndexedBlock, strconv.FormatUint(forkBlock, 10)).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := idx.handleReorg(5); !errors.Is(err, errReorgDetected) {
		t.Fatalf("expected errReorgDetected from handleReorg, got %v", err)
	}

	// Worker A's insert lands after the cleanup: the fence must reject it
	// before any statement reaches the database.
	err = idx.insertBlockData([]models.Blob{staleBlob}, staleBlock, nil, fetchEpoch)
	if !errors.Is(err, errStaleBlockFetch) {
		t.Fatalf("expected errStaleBlockFetch for post-cleanup insert, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("stale insert touched the database: %v", err)
	}
}

func TestHandleReorg_DepthCapExhausted(t *testing.T) {
	idx := newTestIndexer()
	idx.maxReorgDepth = 2
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB
	idx.ethClient, _ = newMockEthClient(t, 10)

	fromBlock := uint64(10)
	// Walk back over forkBlock 9 then 8; both stored hashes mismatch the
	// canonical chain, so the scan never confirms a common ancestor and exhausts
	// maxReorgDepth(2) — the dangerous "truncate at an unverified point" branch.
	for _, fb := range []uint64{9, 8} {
		mock.ExpectQuery(regexp.QuoteMeta("SELECT block_hash FROM indexed_blocks WHERE chain_id = $1 AND block_number = $2")).
			WithArgs(idx.network.ChainID, fb).
			WillReturnRows(sqlmock.NewRows([]string{"block_hash"}).AddRow("0xstalehashnevermatchingthechain"))
	}
	forkBlock := fromBlock - 1 - uint64(idx.maxReorgDepth) // 7
	mock.ExpectBegin()
	// NULL MAX (no indexed rows) must not shrink the invalidated range below
	// the triggering block, which was never inserted.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT MAX(block_number) FROM indexed_blocks WHERE chain_id = $1")).
		WithArgs(idx.network.ChainID).
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(nil))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE chain_id = $1 AND block_number >= $2")).
		WithArgs(idx.network.ChainID, int64(forkBlock+1)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM block_metrics WHERE chain_id = $1 AND block_number >= $2")).
		WithArgs(idx.network.ChainID, int64(forkBlock+1)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM indexed_blocks WHERE chain_id = $1 AND block_number >= $2")).
		WithArgs(idx.network.ChainID, forkBlock+1).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT key, value").
		WithArgs(idx.network.ChainID, models.MetadataReorgRewindFrom, models.MetadataReorgInvalidatedThrough).
		WillReturnRows(sqlmock.NewRows([]string{"key", "value"}))
	mock.ExpectExec("INSERT INTO indexer_metadata").
		WithArgs(idx.network.ChainID, models.MetadataReorgRewindFrom, "8", models.MetadataReorgInvalidatedThrough, "10").
		WillReturnResult(sqlmock.NewResult(1, 2))
	mock.ExpectExec("INSERT INTO indexer_metadata").
		WithArgs(idx.network.ChainID, models.MetadataLastIndexedBlock, strconv.FormatUint(forkBlock, 10)).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	err := idx.handleReorg(fromBlock)
	if err == nil || !errors.Is(err, errReorgDetected) {
		t.Fatalf("expected errReorgDetected, got %v", err)
	}
	if got := idx.GetLastIndexedBlock(); got != forkBlock {
		t.Fatalf("expected lastIndexedBlock=%d after depth-cap truncation, got %d", forkBlock, got)
	}
	if from, through := idx.consumeReorgReset(); from != forkBlock+1 || through != fromBlock {
		t.Fatalf("expected invalidated range [%d %d], got [%d %d]", forkBlock+1, fromBlock, from, through)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestHandleReorg_StoredHashDBErrorAborts(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB
	idx.ethClient, _ = newMockEthClient(t, 10)

	// A transient DB error reading the stored hash must abort the reorg, NOT be
	// treated as "past indexed range" and trigger a delete/rewind.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT block_hash FROM indexed_blocks WHERE chain_id = $1 AND block_number = $2")).
		WithArgs(idx.network.ChainID, uint64(9)).
		WillReturnError(errors.New("connection reset"))

	err := idx.handleReorg(10)
	if err == nil || errors.Is(err, errReorgDetected) {
		t.Fatalf("expected a hard DB error (not errReorgDetected), got %v", err)
	}
	if !strings.Contains(err.Error(), "stored hash") {
		t.Fatalf("expected a stored-hash read error, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected/unmet sqlmock expectations (no delete should occur): %v", err)
	}
}

// expectHandleReorgThroughDeletes registers the sqlmock expectations for
// handleReorg(5) up to and including the three range DELETEs: fork walk
// confirming block 4 as the fork point, then MAX(block_number)=7 bounding the
// invalidated range at [5, 7].
func expectHandleReorgThroughDeletes(t *testing.T, idx *Indexer, mock sqlmock.Sqlmock) {
	t.Helper()

	forkBlock := uint64(4)
	block, err := idx.ethClient.GetBlockByNumber(context.Background(), forkBlock)
	if err != nil {
		t.Fatalf("failed to get fork block: %v", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT block_hash FROM indexed_blocks WHERE chain_id = $1 AND block_number = $2")).
		WithArgs(idx.network.ChainID, forkBlock).
		WillReturnRows(sqlmock.NewRows([]string{"block_hash"}).AddRow(block.Hash().Hex()))
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT MAX(block_number) FROM indexed_blocks WHERE chain_id = $1")).
		WithArgs(idx.network.ChainID).
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(int64(7)))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE chain_id = $1 AND block_number >= $2")).
		WithArgs(idx.network.ChainID, int64(forkBlock+1)).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM block_metrics WHERE chain_id = $1 AND block_number >= $2")).
		WithArgs(idx.network.ChainID, int64(forkBlock+1)).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM indexed_blocks WHERE chain_id = $1 AND block_number >= $2")).
		WithArgs(idx.network.ChainID, forkBlock+1).
		WillReturnResult(sqlmock.NewResult(0, 2))
}

// A reorg that lands while a prior invalidated range is still unrecovered
// (crash loop, back-to-back reorgs) must widen the persisted marker, never
// narrow it — narrowing would let the completion check clear the marker while
// part of the older range is still missing. The live signal must cover the
// merged range whenever the prior marker may never have been queued in this
// process, and only the fresh range once it provably was.
func TestHandleReorg_MergesPersistedRecoveryMarker(t *testing.T) {
	tests := []struct {
		name string
		// alreadySignaled marks the prior marker's range as queued in this
		// process, which narrows the live signal to the fresh range.
		alreadySignaled   bool
		priorRows         [][2]string
		wantFrom          string
		wantThrough       string
		wantSignalFrom    uint64
		wantSignalThrough uint64
	}{
		{
			name: "prior wider range widens the merged marker and the live signal",
			priorRows: [][2]string{
				{models.MetadataReorgRewindFrom, "2"},
				{models.MetadataReorgInvalidatedThrough, "9"},
			},
			wantFrom:          "2",
			wantThrough:       "9",
			wantSignalFrom:    2,
			wantSignalThrough: 9,
		},
		{
			name:            "already-signaled prior range keeps the live signal fresh",
			alreadySignaled: true,
			priorRows: [][2]string{
				{models.MetadataReorgRewindFrom, "2"},
				{models.MetadataReorgInvalidatedThrough, "9"},
			},
			wantFrom:          "2",
			wantThrough:       "9",
			wantSignalFrom:    5,
			wantSignalThrough: 7,
		},
		{
			name: "prior narrower range is absorbed",
			priorRows: [][2]string{
				{models.MetadataReorgRewindFrom, "6"},
				{models.MetadataReorgInvalidatedThrough, "6"},
			},
			wantFrom:          "5",
			wantThrough:       "7",
			wantSignalFrom:    5,
			wantSignalThrough: 7,
		},
		{
			name: "corrupt prior values are overwritten",
			priorRows: [][2]string{
				{models.MetadataReorgRewindFrom, "not-a-number"},
				{models.MetadataReorgInvalidatedThrough, "also-bad"},
			},
			wantFrom:          "5",
			wantThrough:       "7",
			wantSignalFrom:    5,
			wantSignalThrough: 7,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idx := newTestIndexer()
			idxDB, mock := newMockIndexerDB(t)
			idx.db = idxDB
			idx.ethClient, _ = newMockEthClient(t, 10)
			if tt.alreadySignaled {
				atomic.StoreUint32(&idx.reorgRecoverySignaled, 1)
			}

			expectHandleReorgThroughDeletes(t, idx, mock)
			priorMarker := sqlmock.NewRows([]string{"key", "value"})
			for _, row := range tt.priorRows {
				priorMarker.AddRow(row[0], row[1])
			}
			mock.ExpectQuery("SELECT key, value").
				WithArgs(idx.network.ChainID, models.MetadataReorgRewindFrom, models.MetadataReorgInvalidatedThrough).
				WillReturnRows(priorMarker)
			mock.ExpectExec("INSERT INTO indexer_metadata").
				WithArgs(idx.network.ChainID, models.MetadataReorgRewindFrom, tt.wantFrom, models.MetadataReorgInvalidatedThrough, tt.wantThrough).
				WillReturnResult(sqlmock.NewResult(1, 2))
			mock.ExpectExec("INSERT INTO indexer_metadata").
				WithArgs(idx.network.ChainID, models.MetadataLastIndexedBlock, "4").
				WillReturnResult(sqlmock.NewResult(1, 1))
			mock.ExpectCommit()

			if err := idx.handleReorg(5); !errors.Is(err, errReorgDetected) {
				t.Fatalf("expected errReorgDetected, got %v", err)
			}
			if from, through := idx.consumeReorgReset(); from != tt.wantSignalFrom || through != tt.wantSignalThrough {
				t.Fatalf("expected live signal [%d %d], got [%d %d]", tt.wantSignalFrom, tt.wantSignalThrough, from, through)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("unmet sqlmock expectations: %v", err)
			}
		})
	}
}

// Committing the deletions without the marker would recreate the crash hole
// the marker exists to close, so a marker persist failure must roll the whole
// cleanup back and leave every indexed row and the watermark untouched.
func TestHandleReorg_MarkerPersistFailureAbortsCleanup(t *testing.T) {
	t.Run("marker read error", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB
		idx.ethClient, _ = newMockEthClient(t, 10)
		atomic.StoreUint64(&idx.lastIndexedBlock, 8)

		expectHandleReorgThroughDeletes(t, idx, mock)
		mock.ExpectQuery("SELECT key, value").
			WithArgs(idx.network.ChainID, models.MetadataReorgRewindFrom, models.MetadataReorgInvalidatedThrough).
			WillReturnError(errors.New("marker read failed"))
		mock.ExpectRollback()

		err := idx.handleReorg(5)
		if err == nil || !strings.Contains(err.Error(), "failed to read reorg recovery marker") {
			t.Fatalf("expected marker read error, got %v", err)
		}
		if errors.Is(err, errReorgDetected) {
			t.Fatal("aborted cleanup must not report the reorg as handled")
		}
		if atomic.LoadUint32(&idx.reorgDetected) != 0 {
			t.Fatal("expected no reorg signal after aborted cleanup")
		}
		if got := atomic.LoadUint64(&idx.reorgEpoch); got != 0 {
			t.Fatalf("expected reorgEpoch unchanged after aborted cleanup, got %d", got)
		}
		if got := idx.GetLastIndexedBlock(); got != 8 {
			t.Fatalf("expected watermark unchanged at 8 after aborted cleanup, got %d", got)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet sqlmock expectations: %v", err)
		}
	})

	t.Run("marker write error", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB
		idx.ethClient, _ = newMockEthClient(t, 10)
		atomic.StoreUint64(&idx.lastIndexedBlock, 8)

		expectHandleReorgThroughDeletes(t, idx, mock)
		mock.ExpectQuery("SELECT key, value").
			WithArgs(idx.network.ChainID, models.MetadataReorgRewindFrom, models.MetadataReorgInvalidatedThrough).
			WillReturnRows(sqlmock.NewRows([]string{"key", "value"}))
		mock.ExpectExec("INSERT INTO indexer_metadata").
			WithArgs(idx.network.ChainID, models.MetadataReorgRewindFrom, "5", models.MetadataReorgInvalidatedThrough, "7").
			WillReturnError(errors.New("marker write failed"))
		mock.ExpectRollback()

		err := idx.handleReorg(5)
		if err == nil || !strings.Contains(err.Error(), "failed to persist reorg recovery marker") {
			t.Fatalf("expected marker write error, got %v", err)
		}
		if errors.Is(err, errReorgDetected) {
			t.Fatal("aborted cleanup must not report the reorg as handled")
		}
		if got := idx.GetLastIndexedBlock(); got != 8 {
			t.Fatalf("expected watermark unchanged at 8 after aborted cleanup, got %d", got)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet sqlmock expectations: %v", err)
		}
	})
}

func TestSeedReorgRecoveryFromMarker(t *testing.T) {
	t.Run("marker raises the reorg signal", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectQuery("SELECT key, value").
			WithArgs(idx.network.ChainID, models.MetadataReorgRewindFrom, models.MetadataReorgInvalidatedThrough).
			WillReturnRows(sqlmock.NewRows([]string{"key", "value"}).
				AddRow(models.MetadataReorgRewindFrom, "451").
				AddRow(models.MetadataReorgInvalidatedThrough, "520"))

		idx.seedReorgRecoveryFromMarker()

		if atomic.LoadUint32(&idx.reorgDetected) != 1 {
			t.Fatal("expected persisted marker to raise the reorg signal")
		}
		if from, through := idx.consumeReorgReset(); from != 451 || through != 520 {
			t.Fatalf("expected recovered range [451 520], got [%d %d]", from, through)
		}
	})

	t.Run("no marker is a no-op", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectQuery("SELECT key, value").
			WithArgs(idx.network.ChainID, models.MetadataReorgRewindFrom, models.MetadataReorgInvalidatedThrough).
			WillReturnRows(sqlmock.NewRows([]string{"key", "value"}))

		idx.seedReorgRecoveryFromMarker()

		if atomic.LoadUint32(&idx.reorgDetected) != 0 {
			t.Fatal("expected no reorg signal without a marker")
		}
	})

	t.Run("nil database is a no-op", func(t *testing.T) {
		idx := newTestIndexer()
		idx.db = nil

		idx.seedReorgRecoveryFromMarker()

		if atomic.LoadUint32(&idx.reorgDetected) != 0 {
			t.Fatal("expected no reorg signal without a database")
		}
	})

	t.Run("read error is non-fatal", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectQuery("SELECT key, value").
			WithArgs(idx.network.ChainID, models.MetadataReorgRewindFrom, models.MetadataReorgInvalidatedThrough).
			WillReturnError(errors.New("marker read failed"))

		idx.seedReorgRecoveryFromMarker()

		if atomic.LoadUint32(&idx.reorgDetected) != 0 {
			t.Fatal("expected no reorg signal on marker read error")
		}
	})

	t.Run("broken markers are surfaced, not acted on", func(t *testing.T) {
		brokenMarkers := map[string][][2]string{
			"half-written": {
				{models.MetadataReorgRewindFrom, "451"},
			},
			"unparseable": {
				{models.MetadataReorgRewindFrom, "not-a-number"},
				{models.MetadataReorgInvalidatedThrough, "520"},
			},
			"inverted range": {
				{models.MetadataReorgRewindFrom, "520"},
				{models.MetadataReorgInvalidatedThrough, "451"},
			},
		}
		for name, rows := range brokenMarkers {
			t.Run(name, func(t *testing.T) {
				idx := newTestIndexer()
				idxDB, mock := newMockIndexerDB(t)
				idx.db = idxDB

				markerRows := sqlmock.NewRows([]string{"key", "value"})
				for _, row := range rows {
					markerRows.AddRow(row[0], row[1])
				}
				mock.ExpectQuery("SELECT key, value").
					WithArgs(idx.network.ChainID, models.MetadataReorgRewindFrom, models.MetadataReorgInvalidatedThrough).
					WillReturnRows(markerRows)

				idx.seedReorgRecoveryFromMarker()

				if atomic.LoadUint32(&idx.reorgDetected) != 0 {
					t.Fatal("expected no reorg signal from a broken marker")
				}
			})
		}
	})
}

func TestCompleteReorgRecovery(t *testing.T) {
	expectMarker := func(idx *Indexer, mock sqlmock.Sqlmock) {
		mock.ExpectQuery("SELECT key, value").
			WithArgs(idx.network.ChainID, models.MetadataReorgRewindFrom, models.MetadataReorgInvalidatedThrough).
			WillReturnRows(sqlmock.NewRows([]string{"key", "value"}).
				AddRow(models.MetadataReorgRewindFrom, "5").
				AddRow(models.MetadataReorgInvalidatedThrough, "9"))
	}
	// expectForbiddenDelete registers the marker DELETE as the final
	// expectation so requireForbiddenDelete can prove it never ran.
	expectForbiddenDelete := func(idx *Indexer, mock sqlmock.Sqlmock) {
		mock.ExpectExec("DELETE FROM indexer_metadata").
			WithArgs(idx.network.ChainID, models.MetadataReorgRewindFrom, models.MetadataReorgInvalidatedThrough).
			WillReturnResult(sqlmock.NewResult(0, 2))
	}
	// requireForbiddenDelete asserts the DELETE registered as the final
	// expectation was NOT consumed while every earlier expectation was.
	// sqlmock cannot forbid a statement outright and the code under test
	// swallows unexpected-call errors, so the guard is inverted: if the
	// forbidden delete ran, all expectations are met and the test fails.
	requireForbiddenDelete := func(t *testing.T, mock sqlmock.Sqlmock) {
		t.Helper()
		err := mock.ExpectationsWereMet()
		if err == nil {
			t.Fatal("forbidden DELETE FROM indexer_metadata was executed")
		}
		if !strings.Contains(err.Error(), "DELETE FROM indexer_metadata") {
			t.Fatalf("expected the forbidden delete to be the unmet expectation, got: %v", err)
		}
		if strings.Contains(err.Error(), "SELECT key, value") || strings.Contains(err.Error(), "WITH indexed") {
			t.Fatalf("an expectation before the forbidden delete was not consumed: %v", err)
		}
	}

	t.Run("clears marker once range is covered", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		expectMarker(idx, mock)
		mock.ExpectQuery("WITH indexed AS").
			WithArgs(idx.network.ChainID, uint64(5), uint64(9)).
			WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(uint64(10)))
		mock.ExpectExec("DELETE FROM indexer_metadata").
			WithArgs(idx.network.ChainID, models.MetadataReorgRewindFrom, models.MetadataReorgInvalidatedThrough).
			WillReturnResult(sqlmock.NewResult(0, 2))

		idx.maybeCompleteReorgRecovery()

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet sqlmock expectations: %v", err)
		}
	})

	t.Run("keeps marker and re-raises recovery once while a gap remains", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		// First tick: no recovery signal was ever raised in this process
		// (startup seeding lost it), so the scanner must re-raise the range —
		// without this, the marker would stay inert until the next restart.
		expectMarker(idx, mock)
		mock.ExpectQuery("WITH indexed AS").
			WithArgs(idx.network.ChainID, uint64(5), uint64(9)).
			WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(uint64(7)))
		expectForbiddenDelete(idx, mock)

		idx.maybeCompleteReorgRecovery()

		if atomic.LoadUint32(&idx.reorgDetected) != 1 {
			t.Fatal("expected the gap scanner to re-raise recovery for a never-signaled marker")
		}
		if from, through := idx.consumeReorgReset(); from != 5 || through != 9 {
			t.Fatalf("expected re-raised range [5 9], got [%d %d]", from, through)
		}
		requireForbiddenDelete(t, mock)

		// Second tick: the range is queued now — still incomplete, so the
		// marker stays, but the scanner must not re-queue the range again.
		idxDB2, mock2 := newMockIndexerDB(t)
		idx.db = idxDB2
		expectMarker(idx, mock2)
		mock2.ExpectQuery("WITH indexed AS").
			WithArgs(idx.network.ChainID, uint64(5), uint64(9)).
			WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(uint64(7)))
		expectForbiddenDelete(idx, mock2)

		idx.maybeCompleteReorgRecovery()

		if atomic.LoadUint32(&idx.reorgDetected) != 0 {
			t.Fatal("expected no second re-raise for an already-signaled marker")
		}
		requireForbiddenDelete(t, mock2)
	})

	t.Run("epoch change between scan and delete aborts the clear", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB
		// A reorg/reindex cleanup committed after the caller sampled epoch 0.
		atomic.StoreUint64(&idx.reorgEpoch, 1)

		expectMarker(idx, mock)
		mock.ExpectQuery("WITH indexed AS").
			WithArgs(idx.network.ChainID, uint64(5), uint64(9)).
			WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(uint64(10)))
		expectForbiddenDelete(idx, mock)

		idx.completeReorgRecoveryIfCovered(0)

		requireForbiddenDelete(t, mock)
	})

	t.Run("no marker is a no-op", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectQuery("SELECT key, value").
			WithArgs(idx.network.ChainID, models.MetadataReorgRewindFrom, models.MetadataReorgInvalidatedThrough).
			WillReturnRows(sqlmock.NewRows([]string{"key", "value"}))
		expectForbiddenDelete(idx, mock)

		idx.maybeCompleteReorgRecovery()

		requireForbiddenDelete(t, mock)
	})

	t.Run("nil database is a no-op", func(t *testing.T) {
		idx := newTestIndexer()
		idx.db = nil

		idx.maybeCompleteReorgRecovery()
	})

	t.Run("marker read error keeps marker", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectQuery("SELECT key, value").
			WithArgs(idx.network.ChainID, models.MetadataReorgRewindFrom, models.MetadataReorgInvalidatedThrough).
			WillReturnError(errors.New("marker read failed"))
		expectForbiddenDelete(idx, mock)

		idx.maybeCompleteReorgRecovery()

		requireForbiddenDelete(t, mock)
	})

	t.Run("coverage scan error keeps marker", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		expectMarker(idx, mock)
		mock.ExpectQuery("WITH indexed AS").
			WithArgs(idx.network.ChainID, uint64(5), uint64(9)).
			WillReturnError(errors.New("coverage scan failed"))
		expectForbiddenDelete(idx, mock)

		idx.maybeCompleteReorgRecovery()

		requireForbiddenDelete(t, mock)
	})

	t.Run("delete error is non-fatal", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		expectMarker(idx, mock)
		mock.ExpectQuery("WITH indexed AS").
			WithArgs(idx.network.ChainID, uint64(5), uint64(9)).
			WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(uint64(10)))
		mock.ExpectExec("DELETE FROM indexer_metadata").
			WithArgs(idx.network.ChainID, models.MetadataReorgRewindFrom, models.MetadataReorgInvalidatedThrough).
			WillReturnError(errors.New("delete failed"))

		idx.maybeCompleteReorgRecovery()

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet sqlmock expectations: %v", err)
		}
	})
}

// The gap scanner tick is what eventually retires a recovered marker.
func TestRunGapScanner_CompletesReorgRecovery(t *testing.T) {
	idx := newTestIndexer()
	idx.gapScanInterval = 5 * time.Millisecond
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB

	mock.ExpectQuery("SELECT key, value").
		WithArgs(idx.network.ChainID, models.MetadataReorgRewindFrom, models.MetadataReorgInvalidatedThrough).
		WillReturnRows(sqlmock.NewRows([]string{"key", "value"}).
			AddRow(models.MetadataReorgRewindFrom, "5").
			AddRow(models.MetadataReorgInvalidatedThrough, "9"))
	mock.ExpectQuery("WITH indexed AS").
		WithArgs(idx.network.ChainID, uint64(5), uint64(9)).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(uint64(10)))
	mock.ExpectExec("DELETE FROM indexer_metadata").
		WithArgs(idx.network.ChainID, models.MetadataReorgRewindFrom, models.MetadataReorgInvalidatedThrough).
		WillReturnResult(sqlmock.NewResult(0, 2))

	done := make(chan struct{})
	go func() {
		idx.runGapScanner()
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for mock.ExpectationsWereMet() != nil {
		select {
		case <-deadline:
			t.Fatalf("gap scanner never cleared the recovered marker: %v", mock.ExpectationsWereMet())
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	idx.cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("gap scanner did not stop after cancel")
	}
}

// Regression test for the crash hole this marker closes: a LATEST-start
// network resumes at the current tip after a crash, and the startup gap scan
// only looks below the watermark — which the reorg rewound to the fork point.
// The deleted range sits entirely above it, so without the persisted marker
// it would never be re-indexed.
func TestStart_LatestStartRecoversPersistedReorgMarker(t *testing.T) {
	idx := newTestIndexer()
	// Long intervals keep the walker's first tick far beyond the test's
	// lifetime: runBlockIndexer consumes the reorg signal on its tick, and the
	// assertions below must read it first.
	idx.pollingInterval = 10 * time.Minute
	idx.mempoolPollingInterval = 10 * time.Minute
	idx.network.StartBlock = "LATEST"
	idx.startupGapScanBlocks = 1000

	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB
	idx.ethClient, _ = newMockEthClient(t, 1000)
	idx.attribution = attribution.NewService(idxDB)
	idx.attribution.SetChainID(idx.network.ChainID)

	// A reorg rewound the watermark to fork point 500, deleted [451, 520], and
	// the process crashed before any of the range was re-indexed.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT value FROM indexer_metadata WHERE chain_id = $1 AND key = $2")).
		WithArgs(idx.network.ChainID, models.MetadataLastIndexedBlock).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("500"))
	// resolveConfiguredStartBlock records the chain head it resolved LATEST to.
	mock.ExpectExec("INSERT INTO indexer_metadata").
		WithArgs(idx.network.ChainID, models.MetadataCurrentChainHead, "1000", models.MetadataChainHeadUpdatedAt, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 2))
	// Backfill inactive: determineStartBlock jumps ahead to the tip (1000).
	mock.ExpectQuery("SELECT key, value").
		WithArgs(idx.network.ChainID, models.MetadataBackfillActive, models.MetadataBackfillCurrentBlock, models.MetadataBackfillTargetBlock).
		WillReturnRows(sqlmock.NewRows([]string{"key", "value"}).AddRow(models.MetadataBackfillActive, "false"))
	// The startup gap scan window ends at the watermark and sees nothing.
	mock.ExpectQuery("WITH bounds AS").
		WithArgs(idx.network.ChainID, uint64(0), uint64(500), 1000).
		WillReturnRows(sqlmock.NewRows([]string{"block_number"}))
	// The persisted marker is the only remaining trace of the deleted range.
	mock.ExpectQuery("SELECT key, value").
		WithArgs(idx.network.ChainID, models.MetadataReorgRewindFrom, models.MetadataReorgInvalidatedThrough).
		WillReturnRows(sqlmock.NewRows([]string{"key", "value"}).
			AddRow(models.MetadataReorgRewindFrom, "451").
			AddRow(models.MetadataReorgInvalidatedThrough, "520"))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE chain_id = $1 AND block_number < 0")).
		WithArgs(idx.network.ChainID).
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := idx.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer idx.Stop()

	if atomic.LoadUint32(&idx.reorgDetected) != 1 {
		t.Fatal("expected the persisted reorg marker to raise the reorg signal at startup")
	}
	from, through := idx.consumeReorgReset()
	if from != 451 || through != 520 {
		t.Fatalf("expected recovered range [451 520], got [%d %d]", from, through)
	}
}

func TestBlockProcessingWorker_ProcessesTask(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB
	idx.ethClient, _ = newMockEthClient(t, 10)
	idx.blockTaskCh = make(chan BlockTask, 2)
	idx.failedBlocks[1] = 1

	mock.ExpectQuery(regexp.QuoteMeta("SELECT block_hash FROM indexed_blocks WHERE chain_id = $1 AND block_number = $2")).
		WithArgs(idx.network.ChainID, uint64(0)).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE chain_id = $1 AND block_number = $2 AND blob_index >= $3")).
		WithArgs(idx.network.ChainID, int64(1), 0).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO block_metrics").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO indexed_blocks").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	mock.ExpectExec("INSERT INTO indexer_metadata").
		WithArgs(idx.network.ChainID, models.MetadataLastIndexedBlock, "1", models.MetadataLastIndexedAt, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 2))

	done := make(chan struct{})
	go func() {
		idx.blockProcessingWorker(1)
		close(done)
	}()

	idx.blockTaskCh <- BlockTask{BlockNumber: 1}
	deadline := time.After(400 * time.Millisecond)
	for idx.GetLastIndexedBlock() != 1 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for processed block")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	idx.cancel()

	select {
	case <-done:
	case <-time.After(400 * time.Millisecond):
		t.Fatal("blockProcessingWorker did not exit")
	}

	if got := idx.GetLastIndexedBlock(); got != 1 {
		t.Fatalf("expected last indexed block 1, got %d", got)
	}
	idx.failedBlocksMu.Lock()
	_, exists := idx.failedBlocks[1]
	idx.failedBlocksMu.Unlock()
	if exists {
		t.Fatal("expected failed block entry to be cleared after successful processing")
	}
}

func TestBlockProcessingWorker_TracksFailedBlock(t *testing.T) {
	idx := newTestIndexer()
	ethClient, rpcSvc := newMockEthClient(t, 10)
	rpcSvc.failBlock = true
	idx.ethClient = ethClient
	idx.blockTaskCh = make(chan BlockTask, 1)
	idx.maxBlockRetries = 0

	done := make(chan struct{})
	go func() {
		idx.blockProcessingWorker(1)
		close(done)
	}()

	idx.blockTaskCh <- BlockTask{BlockNumber: 2}
	deadline := time.After(400 * time.Millisecond)
	for {
		idx.failedBlocksMu.Lock()
		failures := idx.failedBlocks[2]
		idx.failedBlocksMu.Unlock()
		if failures == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for failed block tracking")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	idx.cancel()

	select {
	case <-done:
	case <-time.After(400 * time.Millisecond):
		t.Fatal("blockProcessingWorker did not exit")
	}

	idx.failedBlocksMu.Lock()
	failures := idx.failedBlocks[2]
	idx.failedBlocksMu.Unlock()
	if failures != 1 {
		t.Fatalf("expected failed block retry count of 1, got %d", failures)
	}
}

// Regression: a worker whose block triggers reorg handling must not advance
// the watermark — the block was never inserted, and persisting it as
// last_indexed_block leaves a hole on crash. The invalidated range signaled to
// the walker must also include the triggering block itself, which can sit
// above every indexed row.
func TestBlockProcessingWorker_ReorgDoesNotAdvanceWatermark(t *testing.T) {
	idx := newTestIndexer()
	idxDB, mock := newMockIndexerDB(t)
	idx.db = idxDB
	idx.ethClient, _ = newMockEthClient(t, 10)
	idx.blockTaskCh = make(chan BlockTask, 1)
	atomic.StoreUint64(&idx.lastIndexedBlock, 8)

	forkBlock := uint64(4)
	canonical, err := idx.ethClient.GetBlockByNumber(context.Background(), forkBlock)
	if err != nil {
		t.Fatalf("failed to get canonical fork block: %v", err)
	}

	// processBlock(5): stored parent hash mismatches — reorg handling starts.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT block_hash FROM indexed_blocks WHERE chain_id = $1 AND block_number = $2")).
		WithArgs(idx.network.ChainID, forkBlock).
		WillReturnRows(sqlmock.NewRows([]string{"block_hash"}).AddRow("0xdeadbeef"))
	// handleReorg fork walk: block 4 still matches the chain — fork point.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT block_hash FROM indexed_blocks WHERE chain_id = $1 AND block_number = $2")).
		WithArgs(idx.network.ChainID, forkBlock).
		WillReturnRows(sqlmock.NewRows([]string{"block_hash"}).AddRow(canonical.Hash().Hex()))
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT MAX(block_number) FROM indexed_blocks WHERE chain_id = $1")).
		WithArgs(idx.network.ChainID).
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(nil))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE chain_id = $1 AND block_number >= $2")).
		WithArgs(idx.network.ChainID, int64(forkBlock+1)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM block_metrics WHERE chain_id = $1 AND block_number >= $2")).
		WithArgs(idx.network.ChainID, int64(forkBlock+1)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM indexed_blocks WHERE chain_id = $1 AND block_number >= $2")).
		WithArgs(idx.network.ChainID, forkBlock+1).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT key, value").
		WithArgs(idx.network.ChainID, models.MetadataReorgRewindFrom, models.MetadataReorgInvalidatedThrough).
		WillReturnRows(sqlmock.NewRows([]string{"key", "value"}))
	mock.ExpectExec("INSERT INTO indexer_metadata").
		WithArgs(idx.network.ChainID, models.MetadataReorgRewindFrom, "5", models.MetadataReorgInvalidatedThrough, "5").
		WillReturnResult(sqlmock.NewResult(1, 2))
	mock.ExpectExec("INSERT INTO indexer_metadata").
		WithArgs(idx.network.ChainID, models.MetadataLastIndexedBlock, strconv.FormatUint(forkBlock, 10)).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	done := make(chan struct{})
	go func() {
		idx.blockProcessingWorker(1)
		close(done)
	}()

	idx.blockTaskCh <- BlockTask{BlockNumber: 5}
	deadline := time.After(400 * time.Millisecond)
	for atomic.LoadUint32(&idx.reorgDetected) != 1 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for reorg signal")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	// Give a buggy worker time to advance the watermark before asserting.
	time.Sleep(50 * time.Millisecond)
	idx.cancel()

	select {
	case <-done:
	case <-time.After(400 * time.Millisecond):
		t.Fatal("blockProcessingWorker did not exit")
	}

	if got := idx.GetLastIndexedBlock(); got != forkBlock {
		t.Fatalf("expected watermark to stay at fork point %d, got %d", forkBlock, got)
	}
	if from, through := idx.consumeReorgReset(); from != 5 || through != 5 {
		t.Fatalf("expected invalidated range [5 5] including the triggering block, got [%d %d]", from, through)
	}
}

func TestBackfillRangeFullyIndexed(t *testing.T) {
	t.Run("no database", func(t *testing.T) {
		idx := newTestIndexer()
		idx.db = nil
		if !idx.backfillRangeFullyIndexed(1, 10) {
			t.Fatal("expected nil-db indexer to treat range as covered")
		}
	})

	t.Run("fully covered", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectQuery("WITH indexed AS").
			WithArgs(idx.network.ChainID, uint64(100), uint64(200)).
			WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(uint64(201)))

		if !idx.backfillRangeFullyIndexed(100, 200) {
			t.Fatal("expected fully covered range to report complete")
		}
	})

	t.Run("gap defers completion", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectQuery("WITH indexed AS").
			WithArgs(idx.network.ChainID, uint64(100), uint64(200)).
			WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(uint64(150)))

		if idx.backfillRangeFullyIndexed(100, 200) {
			t.Fatal("expected range with a gap to defer completion")
		}
	})

	t.Run("query error defers completion", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectQuery("WITH indexed AS").
			WithArgs(idx.network.ChainID, uint64(100), uint64(200)).
			WillReturnError(errors.New("coverage scan failed"))

		if idx.backfillRangeFullyIndexed(100, 200) {
			t.Fatal("expected coverage scan error to defer completion")
		}
	})
}

func TestMempoolProcessingAndLoop(t *testing.T) {
	t.Run("processPendingTransactions success with empty txpool", func(t *testing.T) {
		idx := newTestIndexer()
		idx.ethClient, _ = newMockEthClient(t, 10)

		if err := idx.processPendingTransactions(); err != nil {
			t.Fatalf("processPendingTransactions() error = %v", err)
		}
	})

	t.Run("processPendingTransactions pending tx fetch error", func(t *testing.T) {
		idx := newTestIndexer()
		ethClient, rpcSvc := newMockEthClient(t, 10)
		rpcSvc.failBlock = true
		idx.ethClient = ethClient

		err := idx.processPendingTransactions()
		if err == nil || !strings.Contains(err.Error(), "failed to get pending transactions") {
			t.Fatalf("expected pending tx fetch error, got %v", err)
		}
	})

	t.Run("runMempoolIndexer stops on cancel", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, _ := newMockIndexerDB(t)
		idx.db = idxDB
		idx.ethClient, _ = newMockEthClient(t, 10)
		idx.mempoolPollingInterval = 5 * time.Millisecond

		done := make(chan struct{})
		go func() {
			idx.runMempoolIndexer()
			close(done)
		}()

		time.Sleep(20 * time.Millisecond)
		idx.cancel()

		select {
		case <-done:
		case <-time.After(200 * time.Millisecond):
			t.Fatal("runMempoolIndexer did not stop")
		}
	})

	t.Run("runMempoolReconciler stops on cancel", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, _ := newMockIndexerDB(t)
		idx.db = idxDB
		idx.ethClient, _ = newMockEthClient(t, 10)
		idx.mempoolReconcileInterval = 5 * time.Millisecond

		done := make(chan struct{})
		go func() {
			idx.runMempoolReconciler()
			close(done)
		}()

		time.Sleep(20 * time.Millisecond)
		idx.cancel()

		select {
		case <-done:
		case <-time.After(200 * time.Millisecond):
			t.Fatal("runMempoolReconciler did not stop")
		}
	})

	t.Run("runMempoolReconciler logs poll errors and keeps ticking", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, _ := newMockIndexerDB(t)
		idx.db = idxDB
		ethClient, rpcSvc := newMockEthClient(t, 10)
		rpcSvc.failBlock = true
		idx.ethClient = ethClient
		idx.mempoolReconcileInterval = 5 * time.Millisecond

		done := make(chan struct{})
		go func() {
			idx.runMempoolReconciler()
			close(done)
		}()

		// Let several failing ticks fire: the loop must survive them.
		time.Sleep(25 * time.Millisecond)
		idx.cancel()

		select {
		case <-done:
		case <-time.After(200 * time.Millisecond):
			t.Fatal("runMempoolReconciler did not stop after poll errors")
		}
	})

	t.Run("refreshPendingBlobLiveness bumps last_seen for still-pending txs", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB
		ethClient, ethSvc := newMockEthClient(t, 10)
		idx.ethClient = ethClient
		ethSvc.txByHash = newSignedBlobTx(t, int64(idx.network.ChainID), 3)
		ethSvc.txPending = true

		mock.ExpectQuery("SELECT DISTINCT tx_hash FROM mempool_blobs").
			WithArgs(idx.network.ChainID).
			WillReturnRows(sqlmock.NewRows([]string{"tx_hash"}).AddRow("0xtracked"))
		mock.ExpectExec("UPDATE mempool_blobs SET last_seen").
			WithArgs(idx.network.ChainID, utcTimeArg{}, sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))

		idx.refreshPendingBlobLiveness()

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("liveness refresh expectations not met: %v", err)
		}
	})

	t.Run("refreshPendingBlobLiveness leaves mined txs to the TTL sweep", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB
		ethClient, ethSvc := newMockEthClient(t, 10)
		idx.ethClient = ethClient
		ethSvc.txByHash = newSignedBlobTx(t, int64(idx.network.ChainID), 4)
		ethSvc.txPending = false

		// No UPDATE expectation: a tx the node reports as mined (or does not
		// know at all) must not have its liveness watermark bumped.
		mock.ExpectQuery("SELECT DISTINCT tx_hash FROM mempool_blobs").
			WithArgs(idx.network.ChainID).
			WillReturnRows(sqlmock.NewRows([]string{"tx_hash"}).AddRow("0xtracked"))

		idx.refreshPendingBlobLiveness()

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("liveness refresh expectations not met: %v", err)
		}
	})

	t.Run("refreshPendingBlobLiveness tolerates listing errors", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, _ := newMockIndexerDB(t)
		idx.db = idxDB
		idx.ethClient, _ = newMockEthClient(t, 10)

		// No expectations installed: the listing query fails and the refresh
		// must log-and-return without touching the eth client.
		idx.refreshPendingBlobLiveness()
	})

	t.Run("processPendingTransaction lookup error", func(t *testing.T) {
		idx := newTestIndexer()
		idx.ethClient, _ = newMockEthClient(t, 10)

		idx.processPendingTransaction(common.HexToHash("0x1"))
	})

	t.Run("processPendingTransaction skips non-pending tx", func(t *testing.T) {
		idx := newTestIndexer()
		client, ethSvc := newMockEthClient(t, 10)
		idx.ethClient = client
		ethSvc.txByHash = newSignedBlobTx(t, int64(idx.network.ChainID), 1)
		ethSvc.txPending = false

		idx.processPendingTransaction(ethSvc.txByHash.Hash())
	})

	t.Run("processPendingTransaction skips non-blob tx", func(t *testing.T) {
		idx := newTestIndexer()
		client, ethSvc := newMockEthClient(t, 10)
		idx.ethClient = client
		ethSvc.txByHash = newSignedDynamicTx(t, int64(idx.network.ChainID), 2)
		ethSvc.txPending = true

		idx.processPendingTransaction(ethSvc.txByHash.Hash())
	})

	t.Run("processPendingTransaction latest block error", func(t *testing.T) {
		idx := newTestIndexer()
		client, ethSvc := newMockEthClient(t, 10)
		idx.ethClient = client
		ethSvc.txByHash = newSignedBlobTx(t, int64(idx.network.ChainID), 3)
		ethSvc.txPending = true
		ethSvc.failBlock = true // causes GetBlockByNumber to fail

		idx.processPendingTransaction(ethSvc.txByHash.Hash())
	})

	t.Run("processPendingTransaction sender error", func(t *testing.T) {
		idx := newTestIndexer()
		client, ethSvc := newMockEthClient(t, 10)
		idx.ethClient = client
		ethSvc.txByHash = types.NewTx(&types.BlobTx{
			ChainID:    uint256.NewInt(uint64(idx.network.ChainID)),
			Nonce:      4,
			GasTipCap:  uint256.NewInt(1),
			GasFeeCap:  uint256.NewInt(2),
			Gas:        21_000,
			To:         common.Address{},
			Value:      uint256.NewInt(0),
			BlobFeeCap: uint256.NewInt(3),
			BlobHashes: []common.Hash{{4}},
		})
		ethSvc.txPending = true

		idx.processPendingTransaction(ethSvc.txByHash.Hash())
	})

	t.Run("processPendingTransaction inserts pending blob", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB
		client, ethSvc := newMockEthClient(t, 10)
		idx.ethClient = client

		keyTx := func() *types.Transaction {
			key, err := crypto.GenerateKey()
			if err != nil {
				t.Fatalf("failed to generate key: %v", err)
			}
			signer := types.LatestSignerForChainID(big.NewInt(int64(idx.network.ChainID)))
			tx := types.MustSignNewTx(key, signer, &types.BlobTx{
				ChainID:    uint256.NewInt(uint64(idx.network.ChainID)),
				Nonce:      1,
				GasTipCap:  uint256.NewInt(1),
				GasFeeCap:  uint256.NewInt(2),
				Gas:        21_000,
				To:         common.Address{},
				Value:      uint256.NewInt(0),
				BlobFeeCap: uint256.NewInt(3),
				BlobHashes: []common.Hash{{1}},
			})
			return tx
		}
		ethSvc.txByHash = keyTx()
		ethSvc.txPending = true

		txHash := ethSvc.txByHash.Hash().Hex()
		mock.ExpectBegin()
		mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS")).
			WithArgs(idx.network.ChainID, txHash).
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM mempool_blobs")).
			WithArgs(idx.network.ChainID, sqlmock.AnyArg(), int64(1), txHash, sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM mempool_blobs")).
			WithArgs(idx.network.ChainID, txHash, 1).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("INSERT INTO mempool_blobs").
			WithArgs(idx.network.ChainID, txHash, 0, sqlmock.AnyArg(), "",
				sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
				sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
				ethSvc.txByHash.BlobHashes()[0].Hex(), int64(1), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit()

		idx.processPendingTransaction(ethSvc.txByHash.Hash())

		// processPendingTransaction logs-and-drops DB errors, so the
		// expectations must be asserted explicitly for this to verify the
		// mempool write path (including versioned_hash) end to end.
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("pending insert expectations not met: %v", err)
		}
	})

	t.Run("processPendingTransactions processes blob tx list", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB
		client, ethSvc := newMockEthClient(t, 10)
		idx.ethClient = client

		key, err := crypto.GenerateKey()
		if err != nil {
			t.Fatalf("failed to generate key: %v", err)
		}
		signer := types.LatestSignerForChainID(big.NewInt(int64(idx.network.ChainID)))
		blobTx := types.MustSignNewTx(key, signer, &types.BlobTx{
			ChainID:    uint256.NewInt(uint64(idx.network.ChainID)),
			Nonce:      2,
			GasTipCap:  uint256.NewInt(1),
			GasFeeCap:  uint256.NewInt(2),
			Gas:        21_000,
			To:         common.Address{},
			Value:      uint256.NewInt(0),
			BlobFeeCap: uint256.NewInt(3),
			BlobHashes: []common.Hash{{2}},
		})
		// GetPendingTransactions reads the pending *block*
		// (eth_getBlockByNumber("pending", true)), which the mock serves from
		// blockTxs.
		ethSvc.blockTxs = []*types.Transaction{blobTx}

		txHash := blobTx.Hash().Hex()
		mock.ExpectBegin()
		mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS")).
			WithArgs(idx.network.ChainID, txHash).
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM mempool_blobs")).
			WithArgs(idx.network.ChainID, sqlmock.AnyArg(), int64(2), txHash, sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM mempool_blobs")).
			WithArgs(idx.network.ChainID, txHash, 1).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("INSERT INTO mempool_blobs").
			WithArgs(idx.network.ChainID, txHash, 0, sqlmock.AnyArg(), "",
				sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
				sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
				blobTx.BlobHashes()[0].Hex(), int64(2), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit()

		if err := idx.processPendingTransactions(); err != nil {
			t.Fatalf("processPendingTransactions() error = %v", err)
		}

		// processPendingTransactions logs-and-continues on insert errors, so a
		// nil return alone proves nothing about the write; assert the
		// expectations to verify the mempool write path end to end.
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("pending insert expectations not met: %v", err)
		}
	})

	t.Run("processPendingTransactions skips tx with sender error", func(t *testing.T) {
		idx := newTestIndexer()
		client, ethSvc := newMockEthClient(t, 10)
		idx.ethClient = client

		unsignedBlobTx := types.NewTx(&types.BlobTx{
			ChainID:    uint256.NewInt(uint64(idx.network.ChainID)),
			Nonce:      5,
			GasTipCap:  uint256.NewInt(1),
			GasFeeCap:  uint256.NewInt(2),
			Gas:        21_000,
			To:         common.Address{},
			Value:      uint256.NewInt(0),
			BlobFeeCap: uint256.NewInt(3),
			BlobHashes: []common.Hash{{5}},
		})
		// Served via the pending block (blockTxs), like the subtest above. No
		// DB mock is installed, so reaching insertPendingBlobs would panic —
		// the unsigned tx must be skipped at the sender step.
		ethSvc.blockTxs = []*types.Transaction{unsignedBlobTx}

		if err := idx.processPendingTransactions(); err != nil {
			t.Fatalf("processPendingTransactions() error = %v", err)
		}
	})
}

func TestRunMempoolCleanup(t *testing.T) {
	t.Run("stops on cancel", func(t *testing.T) {
		idx := newTestIndexer()
		idx.mempoolTTL = 30 * time.Minute
		// Use a longer interval so only one tick fires during the sleep window
		idx.mempoolCleanupInterval = 50 * time.Millisecond

		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		// One tick sweeps stale pending blobs then stale replacements. The
		// cutoffs must be UTC: they are compared server-side against
		// timezone-less TIMESTAMP columns, which discard the offset lib/pq
		// encodes — a local-zone cutoff would sweep shifted by the UTC offset
		// on non-UTC hosts.
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM mempool_blobs WHERE chain_id = $1 AND COALESCE(last_seen, timestamp) < $2")).
			WithArgs(idx.network.ChainID, utcTimeArg{}).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blob_replacements WHERE chain_id = $1 AND replaced_at < $2")).
			WithArgs(idx.network.ChainID, utcTimeArg{}).
			WillReturnResult(sqlmock.NewResult(0, 0))

		done := make(chan struct{})
		go func() {
			idx.runMempoolCleanup()
			close(done)
		}()

		// Wait for one tick to fire, then cancel
		time.Sleep(80 * time.Millisecond)
		idx.cancel()

		select {
		case <-done:
		case <-time.After(200 * time.Millisecond):
			t.Fatal("runMempoolCleanup did not stop")
		}

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("cleanup tick expectations not met: %v", err)
		}
	})

	t.Run("handles DB error gracefully", func(t *testing.T) {
		idx := newTestIndexer()
		idx.mempoolTTL = 30 * time.Minute
		idx.mempoolCleanupInterval = 50 * time.Millisecond

		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM mempool_blobs WHERE chain_id = $1 AND COALESCE(last_seen, timestamp) < $2")).
			WithArgs(idx.network.ChainID, utcTimeArg{}).
			WillReturnError(errors.New("db connection lost"))

		done := make(chan struct{})
		go func() {
			idx.runMempoolCleanup()
			close(done)
		}()

		time.Sleep(80 * time.Millisecond)
		idx.cancel()

		select {
		case <-done:
		case <-time.After(200 * time.Millisecond):
			t.Fatal("runMempoolCleanup did not stop after DB error")
		}
	})
}

type mockSubscription struct {
	errCh chan error
}

func (m *mockSubscription) Err() <-chan error {
	return m.errCh
}

func (m *mockSubscription) Unsubscribe() {}

func TestSubscriptionHandlers(t *testing.T) {
	t.Run("handleNewBlockSubscription queues block", func(t *testing.T) {
		idx := newTestIndexer()
		idx.blockTaskCh = make(chan BlockTask, 1)
		idx.blockSub = &ethereum.BlockSubscription{
			Subscription: &mockSubscription{errCh: make(chan error, 1)},
			Headers:      make(chan *types.Header, 1),
		}
		atomic.StoreUint64(&idx.lastIndexedBlock, 5)

		done := make(chan struct{})
		go func() {
			idx.handleNewBlockSubscription()
			close(done)
		}()

		idx.blockSub.Headers <- &types.Header{Number: big.NewInt(6)}

		select {
		case task := <-idx.blockTaskCh:
			if task.BlockNumber != 6 {
				t.Fatalf("expected queued block 6, got %d", task.BlockNumber)
			}
		case <-time.After(200 * time.Millisecond):
			t.Fatal("timed out waiting for queued block from subscription")
		}

		idx.cancel()
		<-done
	})

	t.Run("handleNewBlockSubscription resubscribe failure exits", func(t *testing.T) {
		idx := newTestIndexer()
		idx.ethClient = &ethereum.Client{} // non-websocket: resubscribe returns error
		errCh := make(chan error, 1)
		idx.blockSub = &ethereum.BlockSubscription{
			Subscription: &mockSubscription{errCh: errCh},
			Headers:      make(chan *types.Header, 1),
		}

		done := make(chan struct{})
		go func() {
			idx.handleNewBlockSubscription()
			close(done)
		}()

		errCh <- errors.New("subscription dropped")
		select {
		case <-done:
		case <-time.After(200 * time.Millisecond):
			t.Fatal("expected handler to exit after resubscribe failure")
		}
	})

	t.Run("handlePendingTransactionSubscription handles hash and starts polling on resubscribe failure", func(t *testing.T) {
		idx := newTestIndexer()
		idx.ethClient, _ = newMockEthClient(t, 10)
		errCh := make(chan error, 1)
		hashCh := make(chan common.Hash, 1)
		idx.pendingTxSub = &ethereum.PendingTxSubscription{
			Subscription: &mockSubscription{errCh: errCh},
			Hashes:       hashCh,
		}

		done := make(chan struct{})
		go func() {
			idx.handlePendingTransactionSubscription()
			close(done)
		}()

		hashCh <- common.HexToHash("0x1")
		time.Sleep(10 * time.Millisecond)

		errCh <- errors.New("subscription dropped")

		select {
		case <-done:
		case <-time.After(200 * time.Millisecond):
			t.Fatal("expected pending tx handler to exit after resubscribe failure")
		}

		if atomic.LoadUint32(&idx.mempoolPollingStarted) != 1 {
			t.Fatal("expected mempool polling fallback to start after resubscribe failure")
		}
		idx.cancel()
		idx.wg.Wait()
	})

	t.Run("handlePendingTransactionSubscription starts polling when error channel closes", func(t *testing.T) {
		idx := newTestIndexer()
		errCh := make(chan error)
		close(errCh)
		idx.pendingTxSub = &ethereum.PendingTxSubscription{
			Subscription: &mockSubscription{errCh: errCh},
			Hashes:       make(chan common.Hash),
		}

		done := make(chan struct{})
		go func() {
			idx.handlePendingTransactionSubscription()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(200 * time.Millisecond):
			t.Fatal("expected handler to exit after closed error channel")
		}

		if atomic.LoadUint32(&idx.mempoolPollingStarted) != 1 {
			t.Fatal("expected mempool polling fallback to start")
		}
		idx.cancel()
		idx.wg.Wait()
	})

	t.Run("handlePendingTransactionSubscription starts polling when hash channel closes", func(t *testing.T) {
		idx := newTestIndexer()
		hashCh := make(chan common.Hash)
		close(hashCh)
		idx.pendingTxSub = &ethereum.PendingTxSubscription{
			Subscription: &mockSubscription{errCh: make(chan error)},
			Hashes:       hashCh,
		}

		done := make(chan struct{})
		go func() {
			idx.handlePendingTransactionSubscription()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(200 * time.Millisecond):
			t.Fatal("expected handler to exit after closed hash channel")
		}

		if atomic.LoadUint32(&idx.mempoolPollingStarted) != 1 {
			t.Fatal("expected mempool polling fallback to start")
		}
		idx.cancel()
		idx.wg.Wait()
	})
}
