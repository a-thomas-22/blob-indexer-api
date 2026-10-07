package api

import (
	"context"
	"database/sql"

	"github.com/jmoiron/sqlx"
)

// windowQuerier is the read surface the guarded builder queries run
// through: the pool itself or the guard's transaction.
type windowQuerier interface {
	SelectContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error
	GetContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error
}

// txBeginner is satisfied by the production *db.DB through its embedded
// *sqlx.DB. The unit-test doubles do not implement it and run the queries
// unguarded, which is fine: they never reach a planner.
type txBeginner interface {
	BeginTxx(ctx context.Context, opts *sql.TxOptions) (*sqlx.Tx, error)
}

// withoutNestedLoops runs fn in a read-only transaction with nested-loop
// joins disabled, for the builder window aggregates.
//
// Those queries join window-sized sets to each other by block number —
// block_builders rows to block_metrics, and the window's transactions to its
// blocks — and every one of them is planned from the window's row estimate.
// block_builders, block_metrics and blobs have no index led by their
// timestamp, only (chain_id, timestamp), so the planner cannot look up the
// column's live maximum and estimates any window newer than the last
// ANALYZE at a single row. With a one-row outer side it picks a nested loop
// whose inner side is the whole window again, so the work grows with the
// square of the window: in October 2026 a stale histogram (last analyzed
// six days earlier) took a sepolia 24h leaderboard to 54s, discarding 352M
// joined rows, and every range above 1h returned 503 on every network.
//
// A nested loop is never the right shape for these joins: both sides are
// the window, so a hash or merge join is linear whatever the estimate. With
// fresh statistics the planner already picks hash joins and the guard costs
// nothing (measured on production: same timings either way). The
// LIMIT-driven recent-blocks read is deliberately left out — a nested loop
// of primary-key lookups is its correct plan.
//
// SET LOCAL scopes the setting to this transaction, so the pooled
// connection goes back to the pool with the server default.
func (a *API) withoutNestedLoops(ctx context.Context, fn func(q windowQuerier) error) error {
	beginner, ok := a.db.(txBeginner)
	if !ok {
		return fn(a.db)
	}
	tx, err := beginner.BeginTxx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "SET LOCAL enable_nestloop = off"); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
