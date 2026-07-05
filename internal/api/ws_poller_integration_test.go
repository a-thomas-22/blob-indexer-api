//go:build integration

package api

// End-to-end check of the notification pipeline against a real Postgres: the
// block_metrics trigger (migration 6) fires pg_notify on commit, the poller's
// pq.Listener receives it, and a new_block WebSocket event reaches a client —
// without waiting for a poll tick.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db"
	"github.com/a-thomas-22/blob-indexer-api/internal/testdb"
)

func TestPollerListenNotifyEndToEnd(t *testing.T) {
	url := testdb.URL(t, "api")
	sqlxDB, err := sqlx.Connect("postgres", url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sqlxDB.Close()

	for _, stmt := range []string{
		"DROP SCHEMA IF EXISTS public CASCADE",
		"CREATE SCHEMA public",
		"GRANT ALL ON SCHEMA public TO PUBLIC",
	} {
		if _, err := sqlxDB.Exec(stmt); err != nil {
			t.Fatalf("reset schema (%s): %v", stmt, err)
		}
	}
	if err := db.RunMigrations(url); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if _, err := sqlxDB.Exec(`
		INSERT INTO block_metrics (chain_id, block_number, block_timestamp, blob_count)
		VALUES (1, 100, $1, 0)
	`, now); err != nil {
		t.Fatalf("seed baseline block: %v", err)
	}

	hub := NewHub()
	go hub.Run()
	defer hub.Stop()

	client := &Client{
		hub:         hub,
		send:        make(chan []byte, 64),
		networkName: "mainnet",
	}
	hub.register <- client
	for hub.ClientCount() != 1 {
		time.Sleep(time.Millisecond)
	}

	networks := map[int]config.NetworkConfig{1: {Name: "mainnet", ChainID: 1, Enabled: true}}
	// A long poll interval proves detection comes from LISTEN/NOTIFY, not the
	// catch-up scan — except the baseline, which needs one fast first tick.
	poller := NewPoller(sqlxDB, hub, networks, 100*time.Millisecond, time.Hour)
	poller.listenerFactory = pqListenerFactory(url)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go poller.Run(ctx)

	// Wait for the baseline tick, then commit a new block.
	time.Sleep(500 * time.Millisecond)
	if _, err := sqlxDB.Exec(`
		INSERT INTO block_metrics (chain_id, block_number, block_timestamp, blob_count)
		VALUES (1, 101, $1, 0)
	`, now.Add(12*time.Second)); err != nil {
		t.Fatalf("insert new block: %v", err)
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case msg := <-client.send:
			var e WSEvent
			if err := json.Unmarshal(msg, &e); err != nil {
				continue
			}
			if e.Type != EventNewBlock {
				continue
			}
			raw, _ := json.Marshal(e.Data)
			var data NewBlockData
			if err := json.Unmarshal(raw, &data); err != nil {
				t.Fatalf("unmarshal new_block: %v", err)
			}
			if data.BlockNumber != 101 {
				continue // baseline-era block, keep waiting
			}
			if data.Pricing == nil {
				t.Fatal("new_block missing pricing")
			}
			return // success
		case <-deadline:
			t.Fatal("no new_block event within 10s of the committed block")
		}
	}
}
