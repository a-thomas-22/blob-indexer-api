//go:build integration

package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jmoiron/sqlx"

	"github.com/a-thomas-22/blob-indexer-api/internal/blobparams"
)

// TestBlobScheduleRoundTrip exercises the network_blob_schedule migration and
// the upsert/select methods that back the eth_config learning path.
func TestBlobScheduleRoundTrip(t *testing.T) {
	sqlxDB, err := sqlx.Connect("postgres", integrationDBURL(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sqlxDB.Close()

	resetSchema(t, sqlxDB)
	m := migrator(t, sqlxDB)
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up: %v", err)
	}

	if _, err := sqlxDB.Exec(`
		INSERT INTO networks (chain_id, name, start_block, is_enabled)
		VALUES (560048, 'hoodi-test', '0', true)
		ON CONFLICT (chain_id) DO NOTHING
	`); err != nil {
		t.Fatalf("seed network: %v", err)
	}

	database := &DB{DB: sqlxDB}
	ctx := context.Background()

	// Empty schedule reads back as no entries.
	got, err := database.GetBlobSchedule(ctx, 560048)
	if err != nil {
		t.Fatalf("GetBlobSchedule (empty): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want 0 entries initially, got %d", len(got))
	}

	// Insert two boundaries out of order; GetBlobSchedule must return them
	// ascending by activation time.
	entries := []blobparams.ScheduleEntry{
		{ActivationTime: 1762955544, Target: 14, Max: 21, UpdateFraction: 11684671},
		{ActivationTime: 1762365720, Target: 10, Max: 15, UpdateFraction: 8346193},
	}
	if err := database.UpsertBlobScheduleEntries(ctx, 560048, entries, "eth_config", time.Unix(1762955600, 0)); err != nil {
		t.Fatalf("UpsertBlobScheduleEntries: %v", err)
	}

	got, err = database.GetBlobSchedule(ctx, 560048)
	if err != nil {
		t.Fatalf("GetBlobSchedule: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d", len(got))
	}
	if got[0].ActivationTime != 1762365720 || got[1].ActivationTime != 1762955544 {
		t.Errorf("entries not ascending by activation time: %+v", got)
	}
	if got[0].Target != 10 || got[0].Max != 15 || got[0].UpdateFraction != 8346193 {
		t.Errorf("first entry = %+v, want 10/15/8346193", got[0])
	}

	// Re-observing a boundary upserts the latest advertised params.
	if err := database.UpsertBlobScheduleEntries(ctx, 560048,
		[]blobparams.ScheduleEntry{{ActivationTime: 1762955544, Target: 21, Max: 32, UpdateFraction: 20609697}},
		"eth_config", time.Unix(1763000000, 0)); err != nil {
		t.Fatalf("UpsertBlobScheduleEntries (update): %v", err)
	}
	got, err = database.GetBlobSchedule(ctx, 560048)
	if err != nil {
		t.Fatalf("GetBlobSchedule (after update): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 entries after upsert, got %d", len(got))
	}
	if got[1].Target != 21 || got[1].Max != 32 {
		t.Errorf("upserted boundary = %+v, want 21/32", got[1])
	}
}

// TestReconcileFutureBlobSchedule verifies that a rescheduled/cancelled future
// fork is pruned while past/current boundaries are preserved.
func TestReconcileFutureBlobSchedule(t *testing.T) {
	sqlxDB, err := sqlx.Connect("postgres", integrationDBURL(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sqlxDB.Close()

	resetSchema(t, sqlxDB)
	m := migrator(t, sqlxDB)
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up: %v", err)
	}
	if _, err := sqlxDB.Exec(`
		INSERT INTO networks (chain_id, name, start_block, is_enabled)
		VALUES (560048, 'hoodi-test', '0', true) ON CONFLICT (chain_id) DO NOTHING
	`); err != nil {
		t.Fatalf("seed network: %v", err)
	}

	database := &DB{DB: sqlxDB}
	ctx := context.Background()
	now := time.Unix(2000, 0)

	// Seed a past boundary (t=1000) and a future ghost the node later moves (t=5000).
	if err := database.UpsertBlobScheduleEntries(ctx, 560048, []blobparams.ScheduleEntry{
		{ActivationTime: 1000, Target: 6, Max: 9, UpdateFraction: 5007716},
		{ActivationTime: 5000, Target: 10, Max: 15, UpdateFraction: 8346193},
	}, "eth_config", now); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Node now advertises the same past boundary plus the fork rescheduled to t=6000.
	if err := database.ReconcileFutureBlobSchedule(ctx, 560048, []blobparams.ScheduleEntry{
		{ActivationTime: 1000, Target: 6, Max: 9, UpdateFraction: 5007716},
		{ActivationTime: 6000, Target: 10, Max: 15, UpdateFraction: 8346193},
	}, "eth_config", now); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got, err := database.GetBlobSchedule(ctx, 560048)
	if err != nil {
		t.Fatalf("GetBlobSchedule: %v", err)
	}
	times := make([]uint64, len(got))
	for i, e := range got {
		times[i] = e.ActivationTime
	}
	// t=1000 preserved (past), t=5000 pruned (stale future ghost), t=6000 added.
	if len(got) != 2 || times[0] != 1000 || times[1] != 6000 {
		t.Errorf("after reconcile want [1000 6000], got %v", times)
	}
}

func TestUpsertBlobScheduleEntries_DuplicateActivationTimeRejected(t *testing.T) {
	sqlxDB, err := sqlx.Connect("postgres", integrationDBURL(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sqlxDB.Close()

	resetSchema(t, sqlxDB)
	m := migrator(t, sqlxDB)
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up: %v", err)
	}

	database := &DB{DB: sqlxDB}
	err = database.UpsertBlobScheduleEntries(context.Background(), 560048, []blobparams.ScheduleEntry{
		{ActivationTime: 100, Target: 3, Max: 6, UpdateFraction: 1},
		{ActivationTime: 100, Target: 6, Max: 9, UpdateFraction: 2},
	}, "eth_config", time.Unix(1, 0))
	if err == nil {
		t.Fatal("expected error for duplicate activation time within a batch")
	}
}

// Empty batch is a no-op (nil error, no rows), and needs no network FK row.
func TestUpsertBlobScheduleEntries_EmptyBatch(t *testing.T) {
	sqlxDB, err := sqlx.Connect("postgres", integrationDBURL(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sqlxDB.Close()

	resetSchema(t, sqlxDB)
	m := migrator(t, sqlxDB)
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up: %v", err)
	}

	database := &DB{DB: sqlxDB}
	if err := database.UpsertBlobScheduleEntries(context.Background(), 560048, nil, "eth_config", time.Unix(1, 0)); err != nil {
		t.Fatalf("empty batch should be a no-op, got %v", err)
	}
}
