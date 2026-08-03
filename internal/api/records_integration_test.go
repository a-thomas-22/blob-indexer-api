//go:build integration

package api

// End-to-end check of GET /records against a real Postgres (TEST_DB_URL). The
// unit tests drive the handler through sqlmock, which never parses SQL, so
// this is the only check that the four query constants are valid SQL, that
// their column names unify with the row structs, and that the values they
// produce agree with what the /blob/pricing predicates would say about the
// same blocks.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db"
	"github.com/a-thomas-22/blob-indexer-api/internal/testdb"
)

func TestGetBlobRecordsAgainstRealPostgres(t *testing.T) {
	// This test resets its schema, so it runs on this package's dedicated
	// database rather than TEST_DB_URL itself.
	url := testdb.URL(t, "api_records")
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

	// Blocks 100-109 under a 3-target / 6-max schedule. Full: 102-104 and the
	// tip run 108-109. Above target: 101-104, 106, and 108-109. Base fees rise
	// with block number so the peaks order is known.
	blobCounts := map[int64]int{
		100: 1, 101: 4, 102: 6, 103: 6, 104: 6,
		105: 2, 106: 5, 107: 3, 108: 6, 109: 6,
	}
	base := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	for block := int64(100); block <= 109; block++ {
		blobs := blobCounts[block]
		ts := base.Add(time.Duration(block-100) * 12 * time.Second)
		if _, err := sqlxDB.Exec(`
			INSERT INTO block_metrics (
				chain_id, block_number, block_timestamp, blob_count,
				blob_gas_used, blob_gas_target, blob_gas_limit,
				excess_blob_gas, blob_base_fee, base_fee_wei, utilization_ratio,
				blob_params_target, blob_params_max, update_fraction
			) VALUES (1, $1, $2, $3, $4, 393216, 786432, 0, $5, 500, 0.5, 3, 6, 3338477)
		`, block, ts, blobs, int64(blobs)*131072, 1000000+block); err != nil {
			t.Fatalf("seed block %d: %v", block, err)
		}
		for i := 0; i < blobs; i++ {
			if _, err := sqlxDB.Exec(`
				INSERT INTO blobs (
					chain_id, block_number, blob_index, tx_hash, from_address, user_attribution,
					blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
					timestamp, max_fee_per_blob_gas, blob_gas_used
				) VALUES (1, $1, $2, $3, '0xfrom', 'Rollup', 131072, 1000, 5, 131072000, $4, 2000, 131072)
			`, block, i, "0xtx"+time.Duration(block).String()+"-"+time.Duration(i).String(), ts); err != nil {
				t.Fatalf("seed blob %d/%d: %v", block, i, err)
			}
		}
	}

	a := newTestAPIWithDB(&db.DB{DB: sqlxDB})
	a.networks = map[int]config.NetworkConfig{
		1: {Name: "mainnet", ChainID: 1, Enabled: true},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/records?limit=5", http.NoBody)
	w := httptest.NewRecorder()
	a.GetBlobRecords(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Success bool            `json:"success"`
		Data    RecordsResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	data := resp.Data

	if data.NetworkID != 1 || data.NetworkName != "mainnet" {
		t.Fatalf("unexpected network identity: %+v", data)
	}

	// Longest first, ties broken by the most recent run.
	if len(data.FullBlockStreaks.Top) != 2 {
		t.Fatalf("expected 2 full runs, got %+v", data.FullBlockStreaks.Top)
	}
	if got := data.FullBlockStreaks.Top[0]; got.StartBlock != 102 || got.EndBlock != 104 || got.Length != 3 {
		t.Fatalf("unexpected longest full run: %+v", got)
	}
	if got := data.FullBlockStreaks.Top[1]; got.StartBlock != 108 || got.EndBlock != 109 || got.Length != 2 {
		t.Fatalf("unexpected second full run: %+v", got)
	}
	// Timestamps come from the run's boundary blocks.
	if !data.FullBlockStreaks.Top[0].StartTimestamp.Equal(base.Add(24 * time.Second)) {
		t.Fatalf("unexpected run start timestamp: %s", data.FullBlockStreaks.Top[0].StartTimestamp)
	}

	// The tip block is full, so the run ending there is also the current one.
	if data.FullBlockStreaks.Current == nil {
		t.Fatal("expected a current full run at the tip")
	}
	if got := *data.FullBlockStreaks.Current; got.StartBlock != 108 || got.EndBlock != 109 {
		t.Fatalf("unexpected current full run: %+v", got)
	}

	if len(data.AboveTargetStreaks.Top) != 3 {
		t.Fatalf("expected 3 above-target runs, got %+v", data.AboveTargetStreaks.Top)
	}
	if got := data.AboveTargetStreaks.Top[0]; got.StartBlock != 101 || got.EndBlock != 104 || got.Length != 4 {
		t.Fatalf("unexpected longest above-target run: %+v", got)
	}

	// Peaks: fee ascends with block number, so the newest blocks lead.
	if len(data.BaseFeePeaks) != 5 {
		t.Fatalf("expected 5 peaks at limit=5, got %d", len(data.BaseFeePeaks))
	}
	if got := data.BaseFeePeaks[0]; got.BlockNumber != 109 || got.BlobBaseFee != "1000109" || got.BlobCount != 6 {
		t.Fatalf("unexpected top peak: %+v", got)
	}
	if got := data.BaseFeePeaks[0].BlobBaseFeeGwei; got != "0.001000109" {
		t.Fatalf("unexpected gwei rendering: %q", got)
	}
	for i := 1; i < len(data.BaseFeePeaks); i++ {
		if data.BaseFeePeaks[i].BlockNumber >= data.BaseFeePeaks[i-1].BlockNumber {
			t.Fatalf("peaks are not in descending fee order: %+v", data.BaseFeePeaks)
		}
	}

	// All ten blocks fall in one UTC hour; 45 blobs at 131072000 wei each.
	if len(data.BusiestHours) != 1 {
		t.Fatalf("expected 1 busy hour, got %+v", data.BusiestHours)
	}
	hour := data.BusiestHours[0]
	if !hour.HourStart.Equal(base.Truncate(time.Hour)) {
		t.Fatalf("unexpected hour bucket start: %s", hour.HourStart)
	}
	if hour.BlobCount != 45 {
		t.Fatalf("expected 45 blobs in the hour, got %d", hour.BlobCount)
	}
	if hour.TotalCostWei != "5898240000" {
		t.Fatalf("unexpected hourly cost: %q", hour.TotalCostWei)
	}

	// Droughts: blocks 100..109 all carry blobs, so there is no drought run.
	if len(data.DroughtStreaks.Top) != 0 {
		t.Fatalf("expected no drought runs, got %+v", data.DroughtStreaks.Top)
	}
	// Below target is strictly under the 3-blob target, so blocks 100 (1 blob)
	// and 105 (2) qualify while block 107, which sits exactly at target, does
	// not. That strictness is the whole point of the predicate.
	if len(data.BelowTargetStreaks.Top) != 2 {
		t.Fatalf("expected 2 below-target runs, got %+v", data.BelowTargetStreaks.Top)
	}
	for _, run := range data.BelowTargetStreaks.Top {
		if run.StartBlock == 107 || run.EndBlock == 107 {
			t.Fatalf("a block exactly at target must not count as below it: %+v", run)
		}
	}

	// Most expensive block: fee times blob count. Block 109 has the highest fee
	// and a full 6 blobs, so it also spends the most; block 108 follows. This
	// ordering differs from base_fee_peaks, where 107 (fee 1000107, 3 blobs)
	// outranks 106 (fee 1000106, 5 blobs).
	if len(data.MostExpensiveBlocks) != 5 {
		t.Fatalf("expected 5 expensive blocks, got %d", len(data.MostExpensiveBlocks))
	}
	if got := data.MostExpensiveBlocks[0]; got.BlockNumber != 109 || got.BlobCount != 6 {
		t.Fatalf("unexpected top spender block: %+v", got)
	}
	// 1000109 wei/gas * 6 blobs * 131072 gas per blob.
	if got := data.MostExpensiveBlocks[0].TotalCostWei; got != "786517721088" {
		t.Fatalf("unexpected block spend: %q", got)
	}
	// The two block leaderboards must actually rank differently, or this one
	// is just base_fee_peaks under another name. Fees rise with block number
	// here, so the fee ranking is 109, 108, 107, ... while the spend ranking
	// promotes the 6-blob blocks over the higher-fee but emptier 107 (3 blobs)
	// and 106 (5): volume beats price.
	if got := data.BaseFeePeaks[2].BlockNumber; got != 107 {
		t.Fatalf("expected block 107 third by fee, got %d", got)
	}
	if got := data.MostExpensiveBlocks[2].BlockNumber; got != 104 {
		t.Fatalf("expected block 104 third by spend, got %d", got)
	}
	for _, b := range data.MostExpensiveBlocks {
		if b.BlockNumber == 106 || b.BlockNumber == 107 {
			t.Fatalf("a partly filled block outranked a full one by spend: %+v", data.MostExpensiveBlocks)
		}
	}

	// One UTC day bucket holds every seeded block.
	if len(data.BusiestDays) != 1 || data.BusiestDays[0].BlobCount != 45 {
		t.Fatalf("unexpected busiest days: %+v", data.BusiestDays)
	}
	if !data.BusiestDays[0].DayStart.Equal(base.Truncate(24 * time.Hour)) {
		t.Fatalf("unexpected day bucket start: %s", data.BusiestDays[0].DayStart)
	}
	if len(data.HighestUtilizationDays) != 1 {
		t.Fatalf("unexpected utilization days: %+v", data.HighestUtilizationDays)
	}
	if got := data.HighestUtilizationDays[0]; got.BlockCount != 10 || got.BlobCount != 45 {
		t.Fatalf("unexpected utilization day counts: %+v", got)
	}

	// One sender carries every blob, at 131072000 wei each.
	if len(data.TopSpenders) != 1 {
		t.Fatalf("unexpected top spenders: %+v", data.TopSpenders)
	}
	if got := data.TopSpenders[0]; got.Address != "0xfrom" || got.BlobCount != 45 {
		t.Fatalf("unexpected top spender: %+v", got)
	}
	if got := data.TopSpenders[0].TotalCostWei; got != "5898240000" {
		t.Fatalf("unexpected spender total: %q", got)
	}

	// A block that stops qualifying at the tip clears the current run without
	// touching the historical leaderboard.
	if _, err := sqlxDB.Exec(`
		UPDATE block_metrics SET blob_count = 1, blob_gas_used = 131072
		WHERE chain_id = 1 AND block_number = 109
	`); err != nil {
		t.Fatalf("demote tip block: %v", err)
	}
	a.invalidateBlockCaches(1)

	w = httptest.NewRecorder()
	a.GetBlobRecords(w, httptest.NewRequest(http.MethodGet, "/api/v1/records?limit=5", http.NoBody))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Data.FullBlockStreaks.Current != nil {
		t.Fatalf("expected no current full run after the tip stopped qualifying, got %+v",
			resp.Data.FullBlockStreaks.Current)
	}
	if got := resp.Data.FullBlockStreaks.Top[0]; got.StartBlock != 102 || got.Length != 3 {
		t.Fatalf("historical leaderboard changed unexpectedly: %+v", got)
	}
}
