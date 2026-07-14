package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/a-thomas-22/blob-indexer-api/internal/blobparams"
)

// blobScheduleRow is the sqlx scan target for a network_blob_schedule row.
type blobScheduleRow struct {
	ActivationTime uint64 `db:"activation_time"`
	Target         int    `db:"target"`
	Max            int    `db:"max"`
	UpdateFraction uint64 `db:"update_fraction"`
}

// GetBlobSchedule returns the learned blob-parameter schedule for a network,
// ordered by activation time ascending. An empty slice means nothing has been
// learned yet, and callers fall back to the compiled chain config.
func (db *DB) GetBlobSchedule(ctx context.Context, chainID int) ([]blobparams.ScheduleEntry, error) {
	const query = `
		SELECT activation_time, target, max, update_fraction
		FROM network_blob_schedule
		WHERE chain_id = $1
		ORDER BY activation_time ASC
	`
	var rows []blobScheduleRow
	if err := db.SelectContext(ctx, &rows, query, chainID); err != nil {
		return nil, fmt.Errorf("failed to load blob schedule for network %d: %w", chainID, err)
	}
	entries := make([]blobparams.ScheduleEntry, len(rows))
	for i, r := range rows {
		entries[i] = blobparams.ScheduleEntry{
			ActivationTime: r.ActivationTime,
			Target:         r.Target,
			Max:            r.Max,
			UpdateFraction: r.UpdateFraction,
		}
	}
	return entries, nil
}

// UpsertBlobScheduleEntries records learned fork boundaries for a network. Re-
// observing a boundary (same chain_id, activation_time) overwrites it with the
// latest advertised parameters. Duplicate activation times within one batch are
// rejected so a caller bug cannot silently drop a boundary.
func (db *DB) UpsertBlobScheduleEntries(ctx context.Context, chainID int, entries []blobparams.ScheduleEntry, source string, observedAt time.Time) error {
	if len(entries) == 0 {
		return nil
	}
	query, args, err := buildBlobScheduleUpsert(chainID, entries, source, observedAt)
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("failed to upsert %d blob schedule entries for network %d: %w", len(entries), chainID, err)
	}
	return nil
}

// ReconcileFutureBlobSchedule upserts the freshly-learned entries and, in the
// same transaction, deletes any previously-persisted boundary that is still in
// the future (activation_time > observedAt) but absent from this batch. That
// removes stale ghosts from forks the node has rescheduled to a new time or
// canceled outright, which would otherwise keep activating at their old
// timestamp. Past and current boundaries are never touched — they already
// happened and the node no longer advertises them.
func (db *DB) ReconcileFutureBlobSchedule(ctx context.Context, chainID int, entries []blobparams.ScheduleEntry, source string, observedAt time.Time) error {
	if len(entries) == 0 {
		return nil
	}
	upsertQuery, upsertArgs, err := buildBlobScheduleUpsert(chainID, entries, source, observedAt)
	if err != nil {
		return err
	}

	keep := make([]int64, len(entries))
	for i, e := range entries {
		keep[i] = int64(e.ActivationTime)
	}

	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin blob schedule reconcile for network %d: %w", chainID, err)
	}
	defer func() { _ = tx.Rollback() }()

	const deleteStale = `
		DELETE FROM network_blob_schedule
		WHERE chain_id = $1
		  AND activation_time > $2
		  AND activation_time <> ALL($3)
	`
	if _, err := tx.ExecContext(ctx, deleteStale, chainID, observedAt.Unix(), pq.Array(keep)); err != nil {
		return fmt.Errorf("failed to prune stale future blob schedule for network %d: %w", chainID, err)
	}
	if _, err := tx.ExecContext(ctx, upsertQuery, upsertArgs...); err != nil {
		return fmt.Errorf("failed to upsert %d blob schedule entries for network %d: %w", len(entries), chainID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit blob schedule reconcile for network %d: %w", chainID, err)
	}
	return nil
}

// buildBlobScheduleUpsert constructs the multi-row INSERT ... ON CONFLICT for a
// batch of schedule entries. Shared arg $1/$2/$3 are chain_id/observed_at/source;
// per-entry args start at $4. Rejects duplicate activation times within the batch.
func buildBlobScheduleUpsert(chainID int, entries []blobparams.ScheduleEntry, source string, observedAt time.Time) (query string, args []interface{}, err error) {
	seen := make(map[uint64]struct{}, len(entries))
	var values strings.Builder
	args = make([]interface{}, 0, 3+len(entries)*4)
	args = append(args, chainID, observedAt, source)
	for i, e := range entries {
		if _, dup := seen[e.ActivationTime]; dup {
			return "", nil, fmt.Errorf("duplicate activation_time %d in blob schedule batch for network %d", e.ActivationTime, chainID)
		}
		seen[e.ActivationTime] = struct{}{}
		if i > 0 {
			values.WriteString(", ")
		}
		// (chain_id, activation_time, target, max, update_fraction, source, observed_at)
		fmt.Fprintf(&values, "($1, $%d, $%d, $%d, $%d, $3, $2)",
			len(args)+1, len(args)+2, len(args)+3, len(args)+4)
		args = append(args, e.ActivationTime, e.Target, e.Max, e.UpdateFraction)
	}

	query = `
		INSERT INTO network_blob_schedule
			(chain_id, activation_time, target, max, update_fraction, source, observed_at)
		VALUES ` + values.String() + `
		ON CONFLICT (chain_id, activation_time) DO UPDATE SET
			target = EXCLUDED.target,
			max = EXCLUDED.max,
			update_fraction = EXCLUDED.update_fraction,
			source = EXCLUDED.source,
			observed_at = EXCLUDED.observed_at
	`
	return query, args, nil
}
