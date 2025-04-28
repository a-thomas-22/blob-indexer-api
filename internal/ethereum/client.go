package ethereum

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// Client is a wrapper around the Ethereum client
type Client struct {
	ethClient *ethclient.Client
	rpcClient *rpc.Client
}

// NewClient creates a new Ethereum client
func NewClient(rpcURL string) (*Client, error) {
	rpcClient, err := rpc.Dial(rpcURL)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Ethereum node: %w", err)
	}

	ethClient := ethclient.NewClient(rpcClient)
	return &Client{
		ethClient: ethClient,
		rpcClient: rpcClient,
	}, nil
}

// GetLatestBlockNumber gets the latest block number
func (c *Client) GetLatestBlockNumber(ctx context.Context) (uint64, error) {
	header, err := c.ethClient.HeaderByNumber(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to get latest block header: %w", err)
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
	var result map[string]*types.Transaction
	err := c.rpcClient.CallContext(ctx, &result, "txpool_content")
	if err != nil {
		return nil, fmt.Errorf("failed to get pending transactions: %w", err)
	}
	return result, nil
}

// IsBlobTransaction checks if a transaction is a blob transaction
func (c *Client) IsBlobTransaction(tx *types.Transaction) bool {
	// Check if the transaction has blob data (EIP-4844)
	// This is a simplified check and may need to be updated based on the final EIP-4844 implementation
	return tx.Type() == types.BlobTxType
}

// GetBlobBaseFee gets the base fee per blob gas for a block
func (c *Client) GetBlobBaseFee(ctx context.Context, blockNumber uint64) (*big.Int, error) {
	var result string
	err := c.rpcClient.CallContext(ctx, &result, "eth_blobBaseFee", fmt.Sprintf("0x%x", blockNumber))
	if err != nil {
		return nil, fmt.Errorf("failed to get blob base fee for block %d: %w", blockNumber, err)
	}

	baseFee, success := big.NewInt(0).SetString(result[2:], 16)
	if !success {
		return nil, fmt.Errorf("failed to parse blob base fee: %s", result)
	}

	return baseFee, nil
}

// GetBlockTimestamp gets the timestamp of a block
func (c *Client) GetBlockTimestamp(block *types.Block) time.Time {
	return time.Unix(int64(block.Time()), 0)
}

// Close closes the Ethereum client
func (c *Client) Close() {
	c.ethClient.Close()
	c.rpcClient.Close()
}
