package ethereum

import (
	"context"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// ClientOption configures the Ethereum client.
type ClientOption func(*clientOptions)

type clientOptions struct {
	rateLimitConfig *RateLimitConfig
}

// WithRateLimit configures RPC rate limiting and 429 handling.
func WithRateLimit(cfg RateLimitConfig) ClientOption {
	return func(o *clientOptions) {
		o.rateLimitConfig = &cfg
	}
}

// BlockSubscription represents a subscription to new blocks
type BlockSubscription struct {
	Subscription ethereum.Subscription
	Headers      chan *types.Header
}

// PendingTxSubscription represents a subscription to pending transactions
type PendingTxSubscription struct {
	Subscription ethereum.Subscription
	Hashes       chan common.Hash
}

// Client is a wrapper around the Ethereum client
type Client struct {
	ethClient        *ethclient.Client
	rpcClient        *rpc.Client
	isWebsocket      bool
	blockSubs        map[string]*BlockSubscription
	pendingTxSubs    map[string]*PendingTxSubscription
	blobBaseFeeCache *big.Int
	blobBaseFeeTime  time.Time
	mu               sync.RWMutex
}

// NewClient creates a new Ethereum client. Options are applied via functional
// options (e.g., WithRateLimit). The variadic signature is backward-compatible.
func NewClient(rpcURL string, opts ...ClientOption) (*Client, error) {
	// Determine if this is a WebSocket URL
	isWebsocket := strings.HasPrefix(rpcURL, "ws://") || strings.HasPrefix(rpcURL, "wss://")

	// Apply options.
	var options clientOptions
	for _, opt := range opts {
		opt(&options)
	}

	var rpcClient *rpc.Client
	var err error

	if !isWebsocket && options.rateLimitConfig != nil {
		transport := newRateLimitedTransport(http.DefaultTransport, *options.rateLimitConfig)
		httpClient := &http.Client{Transport: transport}
		rpcClient, err = rpc.DialOptions(context.Background(), rpcURL, rpc.WithHTTPClient(httpClient))
	} else {
		rpcClient, err = rpc.Dial(rpcURL)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Ethereum node: %w", err)
	}

	ethClient := ethclient.NewClient(rpcClient)
	return &Client{
		ethClient:     ethClient,
		rpcClient:     rpcClient,
		isWebsocket:   isWebsocket,
		blockSubs:     make(map[string]*BlockSubscription),
		pendingTxSubs: make(map[string]*PendingTxSubscription),
	}, nil
}

// IsWebsocket returns true if the client is connected via WebSocket
func (c *Client) IsWebsocket() bool {
	return c.isWebsocket
}

// SubscribeToNewHeads subscribes to new block headers
func (c *Client) SubscribeToNewHeads(ctx context.Context, id string) (*BlockSubscription, error) {
	if !c.isWebsocket {
		return nil, fmt.Errorf("websocket connection required for subscriptions")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Check if we already have a subscription with this ID
	if sub, exists := c.blockSubs[id]; exists {
		return sub, nil
	}

	// Create a new subscription
	headers := make(chan *types.Header)
	sub, err := c.ethClient.SubscribeNewHead(ctx, headers)
	if err != nil {
		return nil, fmt.Errorf("failed to subscribe to new heads: %w", err)
	}

	blockSub := &BlockSubscription{
		Subscription: sub,
		Headers:      headers,
	}

	// Store the subscription
	c.blockSubs[id] = blockSub
	return blockSub, nil
}

// SubscribeToPendingTransactions subscribes to pending transactions
func (c *Client) SubscribeToPendingTransactions(ctx context.Context, id string) (*PendingTxSubscription, error) {
	if !c.isWebsocket {
		return nil, fmt.Errorf("websocket connection required for subscriptions")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Check if we already have a subscription with this ID
	if sub, exists := c.pendingTxSubs[id]; exists {
		return sub, nil
	}

	// Create a new subscription
	hashes := make(chan common.Hash)
	sub, err := c.rpcClient.EthSubscribe(ctx, hashes, "newPendingTransactions")
	if err != nil {
		return nil, fmt.Errorf("failed to subscribe to pending transactions: %w", err)
	}

	pendingTxSub := &PendingTxSubscription{
		Subscription: sub,
		Hashes:       hashes,
	}

	// Store the subscription
	c.pendingTxSubs[id] = pendingTxSub
	return pendingTxSub, nil
}

// UnsubscribeFromNewHeads unsubscribes from new block headers
func (c *Client) UnsubscribeFromNewHeads(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if sub, exists := c.blockSubs[id]; exists {
		sub.Subscription.Unsubscribe()
		delete(c.blockSubs, id)
	}
}

// UnsubscribeFromPendingTransactions unsubscribes from pending transactions
func (c *Client) UnsubscribeFromPendingTransactions(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if sub, exists := c.pendingTxSubs[id]; exists {
		sub.Subscription.Unsubscribe()
		delete(c.pendingTxSubs, id)
	}
}

// GetLatestBlockNumber gets the latest block number
func (c *Client) GetLatestBlockNumber(ctx context.Context) (uint64, error) {
	header, err := c.ethClient.HeaderByNumber(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to get latest block header: %w", err)
	}
	if header == nil || header.Number == nil {
		return 0, fmt.Errorf("latest block header is missing number")
	}
	return header.Number.Uint64(), nil
}

// GetBlockByNumber gets a block by its number
func (c *Client) GetBlockByNumber(ctx context.Context, number uint64) (*types.Block, error) {
	block, err := c.ethClient.BlockByNumber(ctx, big.NewInt(int64(number)))
	if err != nil {
		return nil, fmt.Errorf("failed to get block %d: %w", number, err)
	}
	return block, nil
}

// GetPendingTransactions gets pending transactions from the mempool
func (c *Client) GetPendingTransactions(ctx context.Context) (map[string]*types.Transaction, error) {
	// Try to get pending transactions using txpool_content
	var txpoolResult map[string]*types.Transaction
	err := c.rpcClient.CallContext(ctx, &txpoolResult, "txpool_content")
	if err == nil && txpoolResult != nil {
		return txpoolResult, nil
	}

	// If txpool_content is not available, try to get the pending block
	pendingBlock, err := c.ethClient.BlockByNumber(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get pending block: %w", err)
	}

	// Create a map of transactions from the pending block
	result := make(map[string]*types.Transaction)
	for _, tx := range pendingBlock.Transactions() {
		result[tx.Hash().Hex()] = tx
	}

	return result, nil
}

// IsBlobTransaction checks if a transaction is a blob transaction
func (c *Client) IsBlobTransaction(tx *types.Transaction) bool {
	// Check if the transaction has blob data (EIP-4844)
	// This is a simplified check and may need to be updated based on the final EIP-4844 implementation
	return tx.Type() == types.BlobTxType
}

// GetBlobBaseFee gets the base fee per blob gas for a block with caching
func (c *Client) GetBlobBaseFee(ctx context.Context, blockNumber uint64) (*big.Int, error) {
	c.mu.RLock()
	// Check if we have a cached value that's less than 30 seconds old
	if c.blobBaseFeeCache != nil && time.Since(c.blobBaseFeeTime) < 30*time.Second {
		cachedValue := new(big.Int).Set(c.blobBaseFeeCache)
		c.mu.RUnlock()
		return cachedValue, nil
	}
	c.mu.RUnlock()

	var result string
	// The eth_blobBaseFee method doesn't take any arguments, it returns the current blob base fee
	// We ignore the blockNumber parameter and just get the current blob base fee
	err := c.rpcClient.CallContext(ctx, &result, "eth_blobBaseFee")
	if err != nil {
		return nil, fmt.Errorf("failed to get blob base fee for block %d: %w", blockNumber, err)
	}

	// If the result is empty or "0x", return a default value
	if result == "" || result == "0x" {
		return big.NewInt(1000000000), nil // Default value of 1 Gwei
	}

	baseFee, success := big.NewInt(0).SetString(result[2:], 16)
	if !success {
		// If we can't parse the result, return a default value
		return big.NewInt(1000000000), nil // Default value of 1 Gwei
	}

	// Cache the result
	c.mu.Lock()
	c.blobBaseFeeCache = new(big.Int).Set(baseFee)
	c.blobBaseFeeTime = time.Now()
	c.mu.Unlock()

	return baseFee, nil
}

// GetBlockTimestamp gets the timestamp of a block
func (c *Client) GetBlockTimestamp(block *types.Block) time.Time {
	return time.Unix(int64(block.Time()), 0)
}

// GetTransactionByHash gets a transaction by its hash
func (c *Client) GetTransactionByHash(ctx context.Context, hash common.Hash) (*types.Transaction, bool, error) {
	return c.ethClient.TransactionByHash(ctx, hash)
}

// Close closes the Ethereum client and all subscriptions
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Unsubscribe from all block subscriptions
	for id, sub := range c.blockSubs {
		sub.Subscription.Unsubscribe()
		delete(c.blockSubs, id)
	}

	// Unsubscribe from all pending transaction subscriptions
	for id, sub := range c.pendingTxSubs {
		sub.Subscription.Unsubscribe()
		delete(c.pendingTxSubs, id)
	}

	c.ethClient.Close()
	c.rpcClient.Close()
}
