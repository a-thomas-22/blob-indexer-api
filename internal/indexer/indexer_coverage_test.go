package indexer

import (
	"context"
	"database/sql"
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

type testTxPoolRPC struct {
	txs map[string]*types.Transaction
}

func (p *testTxPoolRPC) Content(_ context.Context) (map[string]*types.Transaction, error) {
	if p.txs != nil {
		return p.txs, nil
	}
	return map[string]*types.Transaction{}, nil
}

func newMockEthClient(t *testing.T, latest uint64) (*ethereum.Client, *testEthRPC) {
	client, ethSvc, _ := newMockEthClientWithTxPool(t, latest)
	return client, ethSvc
}

func newMockEthClientWithTxPool(t *testing.T, latest uint64) (*ethereum.Client, *testEthRPC, *testTxPoolRPC) {
	t.Helper()

	rpcServer := rpc.NewServer()
	ethSvc := &testEthRPC{latest: latest}
	if err := rpcServer.RegisterName("eth", ethSvc); err != nil {
		t.Fatalf("failed to register eth rpc service: %v", err)
	}
	txPoolSvc := &testTxPoolRPC{}
	if err := rpcServer.RegisterName("txpool", txPoolSvc); err != nil {
		t.Fatalf("failed to register txpool rpc service: %v", err)
	}

	httpServer := httptest.NewServer(rpcServer)
	t.Cleanup(httpServer.Close)

	client, err := ethereum.NewClient(httpServer.URL)
	if err != nil {
		t.Fatalf("failed to create ethereum client: %v", err)
	}
	t.Cleanup(client.Close)

	return client, ethSvc, txPoolSvc
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
	}
}

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

	start, err := idx.determineStartBlock()
	if err != nil {
		t.Fatalf("determineStartBlock() error = %v", err)
	}
	if start != 251 {
		t.Fatalf("expected backfill resume block 251, got %d", start)
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
			WithArgs(idx.network.ChainID, models.MetadataCurrentChainHead, "120").
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectExec("INSERT INTO indexer_metadata").
			WithArgs(idx.network.ChainID, models.MetadataChainHeadUpdatedAt, sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(1, 1))
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
	client, _ := newMockEthClient(t, 10)
	idx := newTestIndexer()
	idx.ethClient = client
	idx.pollingInterval = 5 * time.Millisecond
	idx.batchSize = 2
	idx.blockTaskCh = make(chan BlockTask, 10)
	atomic.StoreUint64(&idx.lastIndexedBlock, 8)
	atomic.StoreUint32(&idx.reorgDetected, 1)

	done := make(chan struct{})
	go func() {
		idx.runBlockIndexer(1)
		close(done)
	}()

	got := make([]uint64, 0, 2)
	timeout := time.After(300 * time.Millisecond)
	for len(got) < 2 {
		select {
		case task := <-idx.blockTaskCh:
			got = append(got, task.BlockNumber)
		case <-timeout:
			t.Fatalf("timed out waiting for reorg-queued blocks; got %v", got)
		}
	}

	idx.cancel()
	<-done

	if got[0] != 9 || got[1] != 10 {
		t.Fatalf("expected blocks [9 10], got %v", got)
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
		WithArgs(idx.network.ChainID, models.MetadataLastIndexedBlock, "12").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO indexer_metadata").
		WithArgs(idx.network.ChainID, models.MetadataLastIndexedAt, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

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
		WithArgs(idx.network.ChainID, models.MetadataCurrentChainHead, "99").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO indexer_metadata").
		WithArgs(idx.network.ChainID, models.MetadataChainHeadUpdatedAt, models.FormatMetadataTimestamp(observedAt)).
		WillReturnResult(sqlmock.NewResult(1, 1))

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
		WithArgs(idx.network.ChainID, models.MetadataBackfillActive, "true").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO indexer_metadata").
		WithArgs(idx.network.ChainID, models.MetadataBackfillStartBlock, "10").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO indexer_metadata").
		WithArgs(idx.network.ChainID, models.MetadataBackfillCurrentBlock, "15").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO indexer_metadata").
		WithArgs(idx.network.ChainID, models.MetadataBackfillTargetBlock, "20").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO indexer_metadata").
		WithArgs(idx.network.ChainID, models.MetadataBackfillUpdatedAt, models.FormatMetadataTimestamp(observedAt)).
		WillReturnResult(sqlmock.NewResult(1, 1))

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
		mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS (SELECT 1 FROM blobs WHERE chain_id = $1 AND tx_hash = $2)")).
			WithArgs(blob.ChainID, blob.TxHash).
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM mempool_blobs WHERE chain_id = $1 AND tx_hash = $2 AND blob_index >= $3")).
			WithArgs(blob.ChainID, blob.TxHash, 1).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectPrepare("INSERT INTO mempool_blobs")
		mock.ExpectExec("INSERT INTO mempool_blobs").
			WithArgs(blob.ChainID, blob.TxHash, 0, blob.FromAddress, blob.UserAttribution,
				blob.BlobSizeBytes, blob.BaseFeePerBlobGas, blob.TipPerBlobGas, blob.TotalCostWei,
				blob.Timestamp, blob.MaxFeePerBlobGas, blob.BlobGasUsed).
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit()

		if err := idx.insertPendingBlobs([]models.Blob{blob}); err != nil {
			t.Fatalf("insertPendingBlobs() error = %v", err)
		}
	})

	t.Run("upserts multiple blobs at per-tx ordinals and trims surplus", func(t *testing.T) {
		// blob_index is the per-transaction ordinal (0..N-1): a re-poll upserts
		// the same rows in place, and the leading DELETE trims rows past the
		// current blob count if the tx shrank. Nothing here can grow unbounded,
		// unlike the old pool-wide index counter in blobs.
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB
		blob := newBlobFixture()
		blobs := []models.Blob{blob, blob, blob}

		mock.ExpectBegin()
		mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS")).
			WithArgs(blob.ChainID, blob.TxHash).
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM mempool_blobs")).
			WithArgs(blob.ChainID, blob.TxHash, 3).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectPrepare("INSERT INTO mempool_blobs")
		for offset := 0; offset < len(blobs); offset++ {
			mock.ExpectExec("INSERT INTO mempool_blobs").
				WithArgs(blob.ChainID, blob.TxHash, offset, blob.FromAddress, blob.UserAttribution,
					blob.BlobSizeBytes, blob.BaseFeePerBlobGas, blob.TipPerBlobGas, blob.TotalCostWei,
					blob.Timestamp, blob.MaxFeePerBlobGas, blob.BlobGasUsed).
				WillReturnResult(sqlmock.NewResult(int64(offset+1), 1))
		}
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
			WithArgs(blob.ChainID, blob.TxHash, 1).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectPrepare("INSERT INTO mempool_blobs")
		mock.ExpectExec("INSERT INTO mempool_blobs").
			WillReturnError(errors.New("insert failed"))
		mock.ExpectRollback()

		err := idx.insertPendingBlobs([]models.Blob{blob})
		if err == nil || !strings.Contains(err.Error(), "failed to insert pending blob") {
			t.Fatalf("expected wrapped insert error, got %v", err)
		}
	})
}

func TestInsertBlockData(t *testing.T) {
	indexedBlock := models.IndexedBlock{ChainID: 42, BlockNumber: 10, BlockHash: "0xhash", ParentHash: "0xparent"}
	blob := newBlobFixture()

	t.Run("success", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectBegin()
		// Expect pending blob promotion cleanup before confirmed insert
		mock.ExpectExec("DELETE FROM mempool_blobs WHERE").
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectPrepare("INSERT INTO blobs")
		mock.ExpectExec("INSERT INTO blobs").
			WithArgs(blob.ChainID, blob.BlockNumber, blob.BlobIndex, blob.TxHash, blob.FromAddress, blob.UserAttribution,
				blob.BlobSizeBytes, blob.BaseFeePerBlobGas, blob.TipPerBlobGas, blob.TotalCostWei,
				blob.Timestamp, blob.Confirmed, blob.MaxFeePerBlobGas, blob.BlobGasUsed).
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectExec("INSERT INTO indexed_blocks").
			WithArgs(indexedBlock.ChainID, indexedBlock.BlockNumber, indexedBlock.BlockHash, indexedBlock.ParentHash).
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit()

		if err := idx.insertBlockData([]models.Blob{blob}, indexedBlock, nil); err != nil {
			t.Fatalf("insertBlockData() error = %v", err)
		}
	})

	t.Run("prepare error", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectBegin()
		mock.ExpectExec("DELETE FROM mempool_blobs WHERE").
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectPrepare("INSERT INTO blobs").WillReturnError(errors.New("prepare failed"))
		mock.ExpectRollback()

		err := idx.insertBlockData([]models.Blob{blob}, indexedBlock, nil)
		if err == nil || !strings.Contains(err.Error(), "failed to prepare blob statement") {
			t.Fatalf("expected prepare error, got %v", err)
		}
	})

	t.Run("blob insert error", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectBegin()
		mock.ExpectExec("DELETE FROM mempool_blobs WHERE").
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectPrepare("INSERT INTO blobs")
		mock.ExpectExec("INSERT INTO blobs").
			WillReturnError(errors.New("insert failed"))
		mock.ExpectRollback()

		err := idx.insertBlockData([]models.Blob{blob}, indexedBlock, nil)
		if err == nil || !strings.Contains(err.Error(), "failed to insert blob") {
			t.Fatalf("expected blob insert error, got %v", err)
		}
	})

	t.Run("indexed block insert error", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectBegin()
		mock.ExpectExec("INSERT INTO indexed_blocks").
			WithArgs(indexedBlock.ChainID, indexedBlock.BlockNumber, indexedBlock.BlockHash, indexedBlock.ParentHash).
			WillReturnError(errors.New("indexed insert failed"))
		mock.ExpectRollback()

		err := idx.insertBlockData(nil, indexedBlock, nil)
		if err == nil || !strings.Contains(err.Error(), "failed to record indexed block") {
			t.Fatalf("expected indexed block error, got %v", err)
		}
	})

	t.Run("commit error", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectBegin()
		mock.ExpectExec("INSERT INTO indexed_blocks").
			WithArgs(indexedBlock.ChainID, indexedBlock.BlockNumber, indexedBlock.BlockHash, indexedBlock.ParentHash).
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit().WillReturnError(errors.New("commit failed"))
		mock.ExpectRollback()

		err := idx.insertBlockData(nil, indexedBlock, nil)
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
		mock.ExpectExec("INSERT INTO block_metrics").
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectExec("INSERT INTO indexed_blocks").
			WithArgs(indexedBlock.ChainID, indexedBlock.BlockNumber, indexedBlock.BlockHash, indexedBlock.ParentHash).
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit()

		if err := idx.insertBlockData(nil, indexedBlock, metrics); err != nil {
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
		mock.ExpectExec("INSERT INTO block_metrics").
			WillReturnError(errors.New("metrics insert failed"))
		mock.ExpectRollback()

		err := idx.insertBlockData(nil, indexedBlock, metrics)
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
	// Expect pending blob promotion cleanup
	mock.ExpectExec("DELETE FROM mempool_blobs WHERE").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectPrepare("INSERT INTO blobs")
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
			true,
			sqlmock.AnyArg(), // max_fee_per_blob_gas
			sqlmock.AnyArg(), // blob_gas_used
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
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE chain_id = $1 AND block_number >= $2")).
		WithArgs(idx.network.ChainID, int64(5)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM block_metrics WHERE chain_id = $1 AND block_number >= $2")).
		WithArgs(idx.network.ChainID, int64(5)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM indexed_blocks WHERE chain_id = $1 AND block_number >= $2")).
		WithArgs(idx.network.ChainID, uint64(5)).
		WillReturnResult(sqlmock.NewResult(0, 1))
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
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE chain_id = $1 AND block_number >= $2")).
			WithArgs(idx.network.ChainID, int64(forkBlock+1)).
			WillReturnResult(sqlmock.NewResult(0, 2))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM block_metrics WHERE chain_id = $1 AND block_number >= $2")).
			WithArgs(idx.network.ChainID, int64(forkBlock+1)).
			WillReturnResult(sqlmock.NewResult(0, 2))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM indexed_blocks WHERE chain_id = $1 AND block_number >= $2")).
			WithArgs(idx.network.ChainID, forkBlock+1).
			WillReturnResult(sqlmock.NewResult(0, 2))
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
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE chain_id = $1 AND block_number >= $2")).
		WithArgs(idx.network.ChainID, int64(forkBlock+1)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM block_metrics WHERE chain_id = $1 AND block_number >= $2")).
		WithArgs(idx.network.ChainID, int64(forkBlock+1)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM indexed_blocks WHERE chain_id = $1 AND block_number >= $2")).
		WithArgs(idx.network.ChainID, forkBlock+1).
		WillReturnResult(sqlmock.NewResult(0, 0))
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
	mock.ExpectExec("INSERT INTO block_metrics").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO indexed_blocks").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	mock.ExpectExec("INSERT INTO indexer_metadata").
		WithArgs(idx.network.ChainID, models.MetadataLastIndexedBlock, "1").
		WillReturnResult(sqlmock.NewResult(1, 1))

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
		mock.ExpectQuery(regexp.QuoteMeta("SELECT blob_index FROM blobs")).
			WithArgs(idx.network.ChainID, txHash).
			WillReturnRows(sqlmock.NewRows([]string{"blob_index"}))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs")).
			WithArgs(idx.network.ChainID, txHash).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectQuery(regexp.QuoteMeta("SELECT MAX(blob_index)")).
			WithArgs(idx.network.ChainID, int64(-1)).
			WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(nil))
		mock.ExpectPrepare("INSERT INTO blobs")
		mock.ExpectExec("INSERT INTO blobs").
			WithArgs(idx.network.ChainID, int64(-1), 0, txHash, sqlmock.AnyArg(), "", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), false, sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit()

		idx.processPendingTransaction(ethSvc.txByHash.Hash())
	})

	t.Run("processPendingTransactions processes blob tx list", func(t *testing.T) {
		idx := newTestIndexer()
		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB
		client, _, txpool := newMockEthClientWithTxPool(t, 10)
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
		txpool.txs = map[string]*types.Transaction{blobTx.Hash().Hex(): blobTx}

		txHash := blobTx.Hash().Hex()
		mock.ExpectBegin()
		mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS")).
			WithArgs(idx.network.ChainID, txHash).
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
		mock.ExpectQuery(regexp.QuoteMeta("SELECT blob_index FROM blobs")).
			WithArgs(idx.network.ChainID, txHash).
			WillReturnRows(sqlmock.NewRows([]string{"blob_index"}))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs")).
			WithArgs(idx.network.ChainID, txHash).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectQuery(regexp.QuoteMeta("SELECT MAX(blob_index)")).
			WithArgs(idx.network.ChainID, int64(-1)).
			WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(nil))
		mock.ExpectPrepare("INSERT INTO blobs")
		mock.ExpectExec("INSERT INTO blobs").
			WithArgs(idx.network.ChainID, int64(-1), 0, txHash, sqlmock.AnyArg(), "", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), false, sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit()

		if err := idx.processPendingTransactions(); err != nil {
			t.Fatalf("processPendingTransactions() error = %v", err)
		}
	})

	t.Run("processPendingTransactions skips tx with sender error", func(t *testing.T) {
		idx := newTestIndexer()
		client, _, txpool := newMockEthClientWithTxPool(t, 10)
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
		txpool.txs = map[string]*types.Transaction{unsignedBlobTx.Hash().Hex(): unsignedBlobTx}

		if err := idx.processPendingTransactions(); err != nil {
			t.Fatalf("processPendingTransactions() error = %v", err)
		}
	})

	t.Run("processPendingTransactions pending tx fetch error", func(t *testing.T) {
		idx := newTestIndexer()
		client, ethSvc, _ := newMockEthClientWithTxPool(t, 10)
		idx.ethClient = client
		ethSvc.failBlock = true // causes pending block lookup to fail

		err := idx.processPendingTransactions()
		if err == nil || !strings.Contains(err.Error(), "failed to get pending transactions") {
			t.Fatalf("expected pending tx fetch error, got %v", err)
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

		mock.ExpectExec("DELETE FROM blobs WHERE chain_id = .*").
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
	})

	t.Run("handles DB error gracefully", func(t *testing.T) {
		idx := newTestIndexer()
		idx.mempoolTTL = 30 * time.Minute
		idx.mempoolCleanupInterval = 50 * time.Millisecond

		idxDB, mock := newMockIndexerDB(t)
		idx.db = idxDB

		mock.ExpectExec("DELETE FROM blobs WHERE chain_id = .*").
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
