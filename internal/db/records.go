package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// recompute rebuilds every cataloged streak predicate over a closed block
// range. The function widens the range to whole runs itself, so overlapping or
// repeated calls are idempotent.
const recomputeBlobBlockStreaksRange = `SELECT blob_block_streaks_recompute_all($1, $2, $3)`

// indexedBlockBounds reads the lowest and highest block that has a
// block_metrics row for one network. Both aggregates collapse to ordered
// index probes on the (chain_id, block_number) primary key, so this stays two
// lookups rather than a scan; a COUNT here would force one.
const indexedBlockBounds = `
	SELECT MIN(block_number) AS min_block, MAX(block_number) AS max_block
	FROM block_metrics
	WHERE chain_id = $1
`

// BlockRange is a closed [Min, Max] range of indexed block numbers.
// HasBlocks is false when the network has no indexed blocks at all, in which
// case the bounds carry no meaning.
type BlockRange struct {
	Min       int64
	Max       int64
	HasBlocks bool
}

// IndexedBlockBounds returns the lowest and highest block_metrics block for a
// network.
func (db *DB) IndexedBlockBounds(ctx context.Context, networkID int) (BlockRange, error) {
	var row struct {
		Min sql.NullInt64 `db:"min_block"`
		Max sql.NullInt64 `db:"max_block"`
	}
	if err := db.GetContext(ctx, &row, indexedBlockBounds, networkID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return BlockRange{}, nil
		}
		return BlockRange{}, fmt.Errorf("failed to read indexed block bounds for network %d: %w", networkID, err)
	}
	if !row.Min.Valid || !row.Max.Valid {
		return BlockRange{}, nil
	}
	return BlockRange{Min: row.Min.Int64, Max: row.Max.Int64, HasBlocks: true}, nil
}

// BackfillBlobBlockStreaksChunk rebuilds the streak leaderboard rows for the
// closed block range [fromBlock, toBlock]. Unlike the fine chart rollup
// backfill, chunk bounds need no alignment: the recompute widens the range to
// cover whole runs, so a run straddling a chunk boundary is rebuilt in full by
// whichever chunk reaches it first and rebuilt identically by the next one.
// Ascending chunks therefore leave no seam.
func (db *DB) BackfillBlobBlockStreaksChunk(ctx context.Context, networkID int, fromBlock, toBlock int64) error {
	if toBlock < fromBlock {
		return fmt.Errorf("streak backfill chunk for network %d has inverted bounds [%d, %d]", networkID, fromBlock, toBlock)
	}
	if _, err := db.ExecContext(ctx, recomputeBlobBlockStreaksRange, networkID, fromBlock, toBlock); err != nil {
		return fmt.Errorf("failed to backfill blob block streaks for network %d [%d, %d]: %w",
			networkID, fromBlock, toBlock, err)
	}
	return nil
}
