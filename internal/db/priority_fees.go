package db

import (
	"context"
	"fmt"

	"github.com/lib/pq"
)

// BlobPriorityFeeUpdate carries the execution-layer fees of one blob
// transaction for UpdateBlobPriorityFees. Every blob row of the transaction
// in the block receives the same values.
type BlobPriorityFeeUpdate struct {
	BlockNumber          int64
	TxHash               string
	MaxPriorityFeePerGas string
	MaxFeePerGas         string
	PriorityFeePerGas    string
}

// BlocksMissingPriorityFees lists, in ascending order, the blocks in the
// closed range [fromBlock, toBlock] that still hold a blob row without a
// recorded priority fee. The walk is keyed on the (chain_id, block_number,
// blob_index) unique index, so the cost is bounded by the range width rather
// than the table.
func (db *DB) BlocksMissingPriorityFees(ctx context.Context, networkID int, fromBlock, toBlock int64) ([]int64, error) {
	if toBlock < fromBlock {
		return nil, fmt.Errorf("priority fee backfill window for network %d has inverted bounds [%d, %d]", networkID, fromBlock, toBlock)
	}
	var blocks []int64
	if err := db.SelectContext(ctx, &blocks, blocksMissingPriorityFees, networkID, fromBlock, toBlock); err != nil {
		return nil, fmt.Errorf("failed to list blocks missing priority fees for network %d [%d, %d]: %w",
			networkID, fromBlock, toBlock, err)
	}
	return blocks, nil
}

// UpdateBlobPriorityFees writes execution-layer fees onto existing blob rows
// in one statement, keyed by (block, tx hash). Only rows still lacking a fee
// are touched: a reorg replay or operator reindex that lands between the
// backfill's block fetch and this write already carries the canonical
// block's fees, and those must win over values derived from a fetch that
// may predate the replacement. An empty PriorityFeePerGas stores NULL (the
// caps are still recorded), matching what the live path writes for a block
// without an execution base fee. The statement sets only the three fee
// columns, which the guarded UPDATE trigger functions from migration 000016
// recognize as a no-op for every aggregate, so nothing is recomputed.
// Returns the number of rows updated.
func (db *DB) UpdateBlobPriorityFees(ctx context.Context, networkID int, updates []BlobPriorityFeeUpdate) (int64, error) {
	if len(updates) == 0 {
		return 0, nil
	}
	blocks := make([]int64, 0, len(updates))
	hashes := make([]string, 0, len(updates))
	tipCaps := make([]string, 0, len(updates))
	feeCaps := make([]string, 0, len(updates))
	paid := make([]string, 0, len(updates))
	for _, update := range updates {
		blocks = append(blocks, update.BlockNumber)
		hashes = append(hashes, update.TxHash)
		tipCaps = append(tipCaps, update.MaxPriorityFeePerGas)
		feeCaps = append(feeCaps, update.MaxFeePerGas)
		paid = append(paid, update.PriorityFeePerGas)
	}

	result, err := db.ExecContext(ctx, updateBlobPriorityFees, networkID,
		pq.Array(blocks), pq.Array(hashes), pq.Array(tipCaps), pq.Array(feeCaps), pq.Array(paid))
	if err != nil {
		return 0, fmt.Errorf("failed to update blob priority fees for network %d (%d transactions): %w",
			networkID, len(updates), err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to read blob priority fee update count for network %d: %w", networkID, err)
	}
	return updated, nil
}

const blocksMissingPriorityFees = `
	SELECT DISTINCT block_number
	FROM blobs
	WHERE chain_id = $1
		AND block_number >= $2
		AND block_number <= $3
		AND priority_fee_per_gas IS NULL
	ORDER BY block_number ASC
`

const updateBlobPriorityFees = `
	UPDATE blobs b
	SET
		max_priority_fee_per_gas = u.tip_cap::numeric,
		max_fee_per_gas = u.fee_cap::numeric,
		priority_fee_per_gas = NULLIF(u.paid, '')::numeric
	FROM unnest($2::bigint[], $3::text[], $4::text[], $5::text[], $6::text[])
		AS u(block_number, tx_hash, tip_cap, fee_cap, paid)
	WHERE b.chain_id = $1
		AND b.block_number = u.block_number
		AND b.tx_hash = u.tx_hash
		AND b.priority_fee_per_gas IS NULL
`
