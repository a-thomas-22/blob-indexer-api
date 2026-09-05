//go:build integration

package db

import (
	"context"
	"testing"
	"time"
)

func TestPriorityFeeBackfillQueriesAgainstRealPostgres(t *testing.T) {
	sqlxDB := newRecordsTestDB(t)
	database := &DB{DB: sqlxDB}
	ctx := context.Background()
	ts := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)

	// Block 100: two rows of one unpriced tx plus an unpriced tx from another
	// sender. Block 101: already priced. Block 102: unpriced. Block 103 is
	// outside the window the test walks.
	if _, err := sqlxDB.Exec(`
		INSERT INTO blobs (
			chain_id, block_number, blob_index, tx_hash, from_address, user_attribution,
			blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei, timestamp,
			max_priority_fee_per_gas, max_fee_per_gas, priority_fee_per_gas
		) VALUES
			(1, 100, 0, '0xa', '0xop', 'Optimism', 131072, 10, 2, 1310720, $1, NULL, NULL, NULL),
			(1, 100, 1, '0xa', '0xop', 'Optimism', 131072, 10, 2, 1310720, $1, NULL, NULL, NULL),
			(1, 100, 2, '0xb', '0xarb', 'Arbitrum', 131072, 10, 2, 1310720, $1, NULL, NULL, NULL),
			(1, 101, 0, '0xc', '0xarb', 'Arbitrum', 131072, 10, 2, 1310720, $1, 7, 70, 7),
			(1, 102, 0, '0xd', '0xop', 'Optimism', 131072, 10, 2, 1310720, $1, NULL, NULL, NULL),
			(1, 103, 0, '0xe', '0xop', 'Optimism', 131072, 10, 2, 1310720, $1, NULL, NULL, NULL)
	`, ts); err != nil {
		t.Fatalf("seed blobs: %v", err)
	}

	blocks, err := database.BlocksMissingPriorityFees(ctx, recordsTestChainID, 100, 102)
	if err != nil {
		t.Fatalf("BlocksMissingPriorityFees: %v", err)
	}
	if len(blocks) != 2 || blocks[0] != 100 || blocks[1] != 102 {
		t.Fatalf("missing blocks = %v, want [100 102]", blocks)
	}
	if _, err := database.BlocksMissingPriorityFees(ctx, recordsTestChainID, 5, 4); err == nil {
		t.Fatal("expected inverted bounds to be rejected")
	}

	// The update covers tx 0xa (two rows), tx 0xd, and, stale, tx 0xc, whose
	// rows are already priced and must keep their fees.
	updated, err := database.UpdateBlobPriorityFees(ctx, recordsTestChainID, []BlobPriorityFeeUpdate{
		{BlockNumber: 100, TxHash: "0xa", MaxPriorityFeePerGas: "2000000000", MaxFeePerGas: "30000000000", PriorityFeePerGas: "1500000000"},
		{BlockNumber: 102, TxHash: "0xd", MaxPriorityFeePerGas: "500000000", MaxFeePerGas: "20000000000", PriorityFeePerGas: "500000000"},
		{BlockNumber: 101, TxHash: "0xc", MaxPriorityFeePerGas: "1", MaxFeePerGas: "1", PriorityFeePerGas: "1"},
	})
	if err != nil {
		t.Fatalf("UpdateBlobPriorityFees: %v", err)
	}
	if updated != 3 {
		t.Fatalf("updated = %d rows, want 3", updated)
	}
	if n, err := database.UpdateBlobPriorityFees(ctx, recordsTestChainID, nil); err != nil || n != 0 {
		t.Fatalf("empty update = %d, %v; want 0, nil", n, err)
	}

	type row struct {
		Block  int64   `db:"block_number"`
		Index  int     `db:"blob_index"`
		TipCap *string `db:"max_priority_fee_per_gas"`
		FeeCap *string `db:"max_fee_per_gas"`
		Paid   *string `db:"priority_fee_per_gas"`
	}
	var rows []row
	if err := sqlxDB.SelectContext(ctx, &rows, `
		SELECT block_number, blob_index, max_priority_fee_per_gas::text, max_fee_per_gas::text, priority_fee_per_gas::text
		FROM blobs WHERE chain_id = $1 ORDER BY block_number, blob_index`, recordsTestChainID); err != nil {
		t.Fatalf("read rows: %v", err)
	}
	want := map[[2]int64]string{
		{100, 0}: "1500000000",
		{100, 1}: "1500000000",
		{101, 0}: "7",
		{102, 0}: "500000000",
	}
	for _, r := range rows {
		key := [2]int64{r.Block, int64(r.Index)}
		expected, priced := want[key]
		if !priced {
			if r.Paid != nil {
				t.Fatalf("row %v unexpectedly priced: %+v", key, r)
			}
			continue
		}
		if r.Paid == nil || *r.Paid != expected {
			t.Fatalf("row %v priority fee = %v, want %s", key, r.Paid, expected)
		}
		if r.TipCap == nil || r.FeeCap == nil {
			t.Fatalf("row %v lost its fee caps: %+v", key, r)
		}
	}
	if len(rows) != 6 {
		t.Fatalf("expected 6 rows, got %d", len(rows))
	}

	// Block 103 stays unpriced and is the only block left in a wider walk.
	remaining, err := database.BlocksMissingPriorityFees(ctx, recordsTestChainID, 0, 1000)
	if err != nil {
		t.Fatalf("BlocksMissingPriorityFees after update: %v", err)
	}
	if len(remaining) != 2 || remaining[0] != 100 || remaining[1] != 103 {
		t.Fatalf("remaining = %v, want [100 103] (0xb in block 100 was not in the update)", remaining)
	}
}

// A fee-only update must not recompute any aggregate: migration 000016
// guards the blobs UPDATE trigger functions to return before any work when
// no aggregate-relevant column changed. The test drives the observable
// effect (network_blob_stats and blob_user_stats unchanged) and then proves
// the triggers still fire for a cost change, so a dropped trigger cannot
// pass as a guarded one.
func TestFeeOnlyBlobUpdatesSkipAggregateTriggers(t *testing.T) {
	sqlxDB := newRecordsTestDB(t)
	database := &DB{DB: sqlxDB}
	ctx := context.Background()
	ts := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)

	if _, err := sqlxDB.Exec(`
		INSERT INTO blobs (
			chain_id, block_number, blob_index, tx_hash, from_address, user_attribution,
			blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei, timestamp
		) VALUES (1, 100, 0, '0xa', '0xop', 'Optimism', 131072, 10, 2, 1000, $1)
	`, ts); err != nil {
		t.Fatalf("seed blob: %v", err)
	}

	type snapshot struct {
		NetworkCost string `db:"network_cost"`
		UserCost    string `db:"user_cost"`
		Rollups     int    `db:"rollups"`
	}
	read := func() snapshot {
		t.Helper()
		var s snapshot
		if err := sqlxDB.GetContext(ctx, &s, `
			SELECT
				(SELECT sum_total_cost::text FROM network_blob_stats WHERE chain_id = $1) AS network_cost,
				(SELECT total_cost_wei::text FROM blob_user_stats WHERE chain_id = $1 AND from_address = '0xop') AS user_cost,
				(SELECT COUNT(*) FROM blob_chart_rollups WHERE chain_id = $1) AS rollups
		`, recordsTestChainID); err != nil {
			t.Fatalf("read aggregates: %v", err)
		}
		return s
	}
	before := read()
	if before.NetworkCost != "1000" || before.UserCost != "1000" || before.Rollups == 0 {
		t.Fatalf("unexpected seeded aggregates: %+v", before)
	}

	// A fee-only update, with an empty paid fee stored as NULL.
	if _, err := database.UpdateBlobPriorityFees(ctx, recordsTestChainID, []BlobPriorityFeeUpdate{
		{BlockNumber: 100, TxHash: "0xa", MaxPriorityFeePerGas: "3", MaxFeePerGas: "30", PriorityFeePerGas: ""},
	}); err != nil {
		t.Fatalf("UpdateBlobPriorityFees: %v", err)
	}
	var paid *string
	var feeCap *string
	if err := sqlxDB.QueryRowContext(ctx,
		"SELECT priority_fee_per_gas::text, max_fee_per_gas::text FROM blobs WHERE chain_id = $1 AND block_number = 100", recordsTestChainID).
		Scan(&paid, &feeCap); err != nil {
		t.Fatalf("read fee: %v", err)
	}
	if paid != nil || feeCap == nil || *feeCap != "30" {
		t.Fatalf("expected NULL paid fee with caps recorded, got paid=%v fee_cap=%v", paid, feeCap)
	}
	if after := read(); after != before {
		t.Fatalf("fee-only update changed aggregates: before %+v, after %+v", before, after)
	}

	// The same triggers still fire for a column they read.
	if _, err := sqlxDB.ExecContext(ctx,
		"UPDATE blobs SET total_cost_wei = 5000 WHERE chain_id = $1 AND block_number = 100", recordsTestChainID); err != nil {
		t.Fatalf("cost update: %v", err)
	}
	if after := read(); after.NetworkCost != "5000" || after.UserCost != "5000" {
		t.Fatalf("cost update did not reach the aggregates: %+v", after)
	}
}
