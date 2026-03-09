package ethereum

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

type testRPCSub struct {
	errCh        chan error
	unsubscribed bool
}

func (s *testRPCSub) Err() <-chan error {
	return s.errCh
}

func (s *testRPCSub) Unsubscribe() {
	s.unsubscribed = true
}

type rpcEthService struct {
	latest      uint64
	failBlock   bool
	blobBaseFee string
	blobFeeErr  error
	txByHash    *types.Transaction
	txByHashErr error
	txByHashNil bool
}

func (e *rpcEthService) headerPayload(number uint64) (map[string]interface{}, error) {
	header := &types.Header{
		ParentHash:  common.BigToHash(big.NewInt(int64(number))),
		UncleHash:   types.EmptyUncleHash,
		Root:        common.BigToHash(big.NewInt(2)),
		TxHash:      types.EmptyTxsHash,
		ReceiptHash: types.EmptyReceiptsHash,
		Difficulty:  big.NewInt(1),
		Number:      big.NewInt(int64(number)),
		GasLimit:    30_000_000,
		GasUsed:     0,
		Time:        uint64(time.Now().Unix()),
		Extra:       []byte{},
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
	payload["transactions"] = []interface{}{}
	payload["uncles"] = []interface{}{}
	return payload, nil
}

func (e *rpcEthService) GetBlockByNumber(_ context.Context, blockNum string, _ bool) (interface{}, error) {
	if e.failBlock {
		return nil, errors.New("block lookup failed")
	}

	number := e.latest
	if blockNum != "latest" {
		parsed, err := strconv.ParseUint(strings.TrimPrefix(blockNum, "0x"), 16, 64)
		if err == nil {
			number = parsed
		}
	}

	return e.headerPayload(number)
}

func (e *rpcEthService) BlobBaseFee(_ context.Context) (string, error) {
	if e.blobFeeErr != nil {
		return "", e.blobFeeErr
	}
	if e.blobBaseFee == "" {
		return "0x3b9aca00", nil
	}
	return e.blobBaseFee, nil
}

func (e *rpcEthService) txPayload(tx *types.Transaction, pending bool) (map[string]interface{}, error) {
	data, err := tx.MarshalJSON()
	if err != nil {
		return nil, err
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	if !pending {
		payload["blockNumber"] = "0x1"
		payload["blockHash"] = common.BigToHash(big.NewInt(1)).Hex()
	}
	return payload, nil
}

func (e *rpcEthService) GetTransactionByHash(_ context.Context, _ common.Hash) (interface{}, error) {
	if e.txByHashErr != nil {
		return nil, e.txByHashErr
	}
	if e.txByHashNil || e.txByHash == nil {
		return nil, nil
	}
	return e.txPayload(e.txByHash, false)
}

func (e *rpcEthService) NewHeads(ctx context.Context) (*rpc.Subscription, error) {
	notifier, ok := rpc.NotifierFromContext(ctx)
	if !ok {
		return nil, rpc.ErrNotificationsUnsupported
	}
	sub := notifier.CreateSubscription()
	go func() {
		payload, _ := e.headerPayload(e.latest)
		_ = notifier.Notify(sub.ID, payload)
	}()
	return sub, nil
}

func (e *rpcEthService) NewPendingTransactions(ctx context.Context) (*rpc.Subscription, error) {
	notifier, ok := rpc.NotifierFromContext(ctx)
	if !ok {
		return nil, rpc.ErrNotificationsUnsupported
	}
	sub := notifier.CreateSubscription()
	go func() {
		_ = notifier.Notify(sub.ID, common.BigToHash(big.NewInt(1)).Hex())
	}()
	return sub, nil
}

type rpcTxpoolService struct {
	result map[string]*types.Transaction
	err    error
}

func (t *rpcTxpoolService) Content(_ context.Context) (map[string]*types.Transaction, error) {
	if t.err != nil {
		return nil, t.err
	}
	if t.result == nil {
		return map[string]*types.Transaction{}, nil
	}
	return t.result, nil
}

func newRPCClient(t *testing.T, ethSvc *rpcEthService, txpoolSvc *rpcTxpoolService, ws bool) *Client {
	t.Helper()

	srv := rpc.NewServer()
	if err := srv.RegisterName("eth", ethSvc); err != nil {
		t.Fatalf("failed to register eth service: %v", err)
	}
	if txpoolSvc != nil {
		if err := srv.RegisterName("txpool", txpoolSvc); err != nil {
			t.Fatalf("failed to register txpool service: %v", err)
		}
	}

	rpcClient := rpc.DialInProc(srv)
	return &Client{
		ethClient:     ethclient.NewClient(rpcClient),
		rpcClient:     rpcClient,
		isWebsocket:   ws,
		blockSubs:     make(map[string]*BlockSubscription),
		pendingTxSubs: make(map[string]*PendingTxSubscription),
	}
}

func signedTx(t *testing.T) *types.Transaction {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	signer := types.LatestSignerForChainID(big.NewInt(1))
	return types.MustSignNewTx(key, signer, &types.DynamicFeeTx{
		ChainID:   big.NewInt(1),
		Nonce:     1,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(2),
		Gas:       21_000,
		To:        &common.Address{},
		Value:     big.NewInt(1),
	})
}

func TestNewClient_SuccessAndError(t *testing.T) {
	srv := rpc.NewServer()
	ethSvc := &rpcEthService{latest: 10}
	if err := srv.RegisterName("eth", ethSvc); err != nil {
		t.Fatalf("register eth service: %v", err)
	}
	httpSrv := httptest.NewServer(srv)
	defer httpSrv.Close()

	client, err := NewClient(httpSrv.URL)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if client.IsWebsocket() {
		t.Fatal("expected HTTP client to not be websocket")
	}
	if client.blockSubs == nil || client.pendingTxSubs == nil {
		t.Fatal("expected subscription maps to be initialized")
	}
	client.Close()

	_, err = NewClient("://bad-url")
	if err == nil {
		t.Fatal("expected NewClient to fail for unreachable endpoint")
	}
}

func TestNewClient_WithRateLimit(t *testing.T) {
	srv := rpc.NewServer()
	ethSvc := &rpcEthService{latest: 10}
	if err := srv.RegisterName("eth", ethSvc); err != nil {
		t.Fatalf("register eth service: %v", err)
	}
	httpSrv := httptest.NewServer(srv)
	defer httpSrv.Close()

	client, err := NewClient(httpSrv.URL, WithRateLimit(RateLimitConfig{
		RequestsPerSecond: 50,
		MaxRetries:        2,
		InitialBackoff:    time.Second,
	}))
	if err != nil {
		t.Fatalf("NewClient() with rate limit error = %v", err)
	}
	defer client.Close()

	if client.IsWebsocket() {
		t.Fatal("expected HTTP client")
	}

	// Verify the client works by making an RPC call.
	ctx := context.Background()
	_, err = client.GetLatestBlockNumber(ctx)
	if err != nil {
		t.Fatalf("GetLatestBlockNumber() error = %v", err)
	}
}

func TestNewClient_WithRateLimit_SubOneRate(t *testing.T) {
	srv := rpc.NewServer()
	ethSvc := &rpcEthService{latest: 5}
	if err := srv.RegisterName("eth", ethSvc); err != nil {
		t.Fatalf("register eth service: %v", err)
	}
	httpSrv := httptest.NewServer(srv)
	defer httpSrv.Close()

	// RequestsPerSecond < 1 should still result in burst=1.
	client, err := NewClient(httpSrv.URL, WithRateLimit(RateLimitConfig{
		RequestsPerSecond: 0.5,
		MaxRetries:        1,
		InitialBackoff:    time.Second,
	}))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer client.Close()

	ctx := context.Background()
	_, err = client.GetLatestBlockNumber(ctx)
	if err != nil {
		t.Fatalf("GetLatestBlockNumber() error = %v", err)
	}
}

func TestSubscriptions_SuccessPaths(t *testing.T) {
	ethSvc := &rpcEthService{latest: 12}
	c := newRPCClient(t, ethSvc, &rpcTxpoolService{}, true)
	defer c.Close()

	ctx := context.Background()
	blockSub, err := c.SubscribeToNewHeads(ctx, "heads")
	if err != nil {
		t.Fatalf("SubscribeToNewHeads() error = %v", err)
	}
	select {
	case <-blockSub.Headers:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for head notification")
	}

	blockSub2, err := c.SubscribeToNewHeads(ctx, "heads")
	if err != nil {
		t.Fatalf("SubscribeToNewHeads(existing) error = %v", err)
	}
	if blockSub2 != blockSub {
		t.Fatal("expected existing block subscription to be reused")
	}

	pendingSub, err := c.SubscribeToPendingTransactions(ctx, "pending")
	if err != nil {
		t.Fatalf("SubscribeToPendingTransactions() error = %v", err)
	}
	select {
	case <-pendingSub.Hashes:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for pending tx notification")
	}

	pendingSub2, err := c.SubscribeToPendingTransactions(ctx, "pending")
	if err != nil {
		t.Fatalf("SubscribeToPendingTransactions(existing) error = %v", err)
	}
	if pendingSub2 != pendingSub {
		t.Fatal("expected existing pending subscription to be reused")
	}

	c.UnsubscribeFromNewHeads("heads")
	c.UnsubscribeFromPendingTransactions("pending")
	if len(c.blockSubs) != 0 || len(c.pendingTxSubs) != 0 {
		t.Fatal("expected all subscriptions to be removed")
	}
}

func TestGetLatestBlockAndGetBlockByNumber(t *testing.T) {
	ethSvc := &rpcEthService{latest: 25}
	c := newRPCClient(t, ethSvc, &rpcTxpoolService{}, false)
	defer c.Close()

	latest, err := c.GetLatestBlockNumber(context.Background())
	if err != nil {
		t.Fatalf("GetLatestBlockNumber() error = %v", err)
	}
	if latest != 25 {
		t.Fatalf("expected latest block 25, got %d", latest)
	}

	block, err := c.GetBlockByNumber(context.Background(), 7)
	if err != nil {
		t.Fatalf("GetBlockByNumber() error = %v", err)
	}
	if block.NumberU64() != 7 {
		t.Fatalf("expected block number 7, got %d", block.NumberU64())
	}

	ethSvc.failBlock = true
	if _, err := c.GetLatestBlockNumber(context.Background()); err == nil {
		t.Fatal("expected GetLatestBlockNumber to fail")
	}
	if _, err := c.GetBlockByNumber(context.Background(), 7); err == nil {
		t.Fatal("expected GetBlockByNumber to fail")
	}
}

func TestGetPendingTransactions_TxpoolAndFallback(t *testing.T) {
	tx := signedTx(t)

	t.Run("txpool success", func(t *testing.T) {
		ethSvc := &rpcEthService{latest: 10}
		c := newRPCClient(t, ethSvc, &rpcTxpoolService{result: map[string]*types.Transaction{tx.Hash().Hex(): tx}}, false)
		defer c.Close()

		pending, err := c.GetPendingTransactions(context.Background())
		if err != nil {
			t.Fatalf("GetPendingTransactions() error = %v", err)
		}
		if len(pending) != 1 {
			t.Fatalf("expected 1 tx, got %d", len(pending))
		}
	})

	t.Run("fallback to pending block", func(t *testing.T) {
		ethSvc := &rpcEthService{latest: 10}
		c := newRPCClient(t, ethSvc, nil, false) // no txpool service => fallback branch
		defer c.Close()

		pending, err := c.GetPendingTransactions(context.Background())
		if err != nil {
			t.Fatalf("GetPendingTransactions() fallback error = %v", err)
		}
		if len(pending) != 0 {
			t.Fatalf("expected empty pending map, got %d entries", len(pending))
		}
	})

	t.Run("fallback error", func(t *testing.T) {
		ethSvc := &rpcEthService{latest: 10, failBlock: true}
		c := newRPCClient(t, ethSvc, nil, false)
		defer c.Close()

		if _, err := c.GetPendingTransactions(context.Background()); err == nil {
			t.Fatal("expected GetPendingTransactions to fail")
		}
	})
}

func TestGetBlobBaseFee_RPCBranches(t *testing.T) {
	t.Run("valid value", func(t *testing.T) {
		ethSvc := &rpcEthService{latest: 10, blobBaseFee: "0x2a"}
		c := newRPCClient(t, ethSvc, &rpcTxpoolService{}, false)
		defer c.Close()

		fee, err := c.GetBlobBaseFee(context.Background(), 1)
		if err != nil {
			t.Fatalf("GetBlobBaseFee() error = %v", err)
		}
		if fee.Int64() != 42 {
			t.Fatalf("expected fee 42, got %d", fee.Int64())
		}
	})

	t.Run("empty value falls back to default", func(t *testing.T) {
		ethSvc := &rpcEthService{latest: 10, blobBaseFee: "0x"}
		c := newRPCClient(t, ethSvc, &rpcTxpoolService{}, false)
		defer c.Close()

		fee, err := c.GetBlobBaseFee(context.Background(), 1)
		if err != nil {
			t.Fatalf("GetBlobBaseFee() error = %v", err)
		}
		if fee.Int64() != 1_000_000_000 {
			t.Fatalf("expected default fee, got %d", fee.Int64())
		}
	})

	t.Run("invalid value falls back to default", func(t *testing.T) {
		ethSvc := &rpcEthService{latest: 10, blobBaseFee: "0xzz"}
		c := newRPCClient(t, ethSvc, &rpcTxpoolService{}, false)
		defer c.Close()

		fee, err := c.GetBlobBaseFee(context.Background(), 1)
		if err != nil {
			t.Fatalf("GetBlobBaseFee() error = %v", err)
		}
		if fee.Int64() != 1_000_000_000 {
			t.Fatalf("expected default fee, got %d", fee.Int64())
		}
	})

	t.Run("rpc error", func(t *testing.T) {
		ethSvc := &rpcEthService{latest: 10, blobFeeErr: errors.New("blob fee failed")}
		c := newRPCClient(t, ethSvc, &rpcTxpoolService{}, false)
		defer c.Close()

		if _, err := c.GetBlobBaseFee(context.Background(), 1); err == nil {
			t.Fatal("expected GetBlobBaseFee to fail")
		}
	})
}

func TestGetTransactionByHash(t *testing.T) {
	tx := signedTx(t)

	t.Run("success", func(t *testing.T) {
		ethSvc := &rpcEthService{latest: 10, txByHash: tx}
		c := newRPCClient(t, ethSvc, &rpcTxpoolService{}, false)
		defer c.Close()

		got, pending, err := c.GetTransactionByHash(context.Background(), tx.Hash())
		if err != nil {
			t.Fatalf("GetTransactionByHash() error = %v", err)
		}
		if pending {
			t.Fatal("expected non-pending transaction")
		}
		if got.Hash() != tx.Hash() {
			t.Fatalf("unexpected tx hash: got %s want %s", got.Hash(), tx.Hash())
		}
	})

	t.Run("not found", func(t *testing.T) {
		ethSvc := &rpcEthService{latest: 10, txByHashNil: true}
		c := newRPCClient(t, ethSvc, &rpcTxpoolService{}, false)
		defer c.Close()

		if _, _, err := c.GetTransactionByHash(context.Background(), common.Hash{}); err == nil {
			t.Fatal("expected not-found error")
		}
	})

	t.Run("rpc error", func(t *testing.T) {
		ethSvc := &rpcEthService{latest: 10, txByHashErr: errors.New("lookup failed")}
		c := newRPCClient(t, ethSvc, &rpcTxpoolService{}, false)
		defer c.Close()

		if _, _, err := c.GetTransactionByHash(context.Background(), common.Hash{}); err == nil {
			t.Fatal("expected lookup error")
		}
	})
}

func TestClose_UnsubscribesAndClearsMaps(t *testing.T) {
	ethSvc := &rpcEthService{latest: 10}
	c := newRPCClient(t, ethSvc, &rpcTxpoolService{}, false)

	blockSub := &testRPCSub{errCh: make(chan error)}
	pendingSub := &testRPCSub{errCh: make(chan error)}

	c.blockSubs["a"] = &BlockSubscription{Subscription: blockSub, Headers: make(chan *types.Header)}
	c.pendingTxSubs["b"] = &PendingTxSubscription{Subscription: pendingSub, Hashes: make(chan common.Hash)}

	c.Close()

	if !blockSub.unsubscribed || !pendingSub.unsubscribed {
		t.Fatal("expected all subscriptions to be unsubscribed")
	}
	if len(c.blockSubs) != 0 || len(c.pendingTxSubs) != 0 {
		t.Fatal("expected subscription maps to be cleared")
	}
}
