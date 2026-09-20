//go:build integration

package api

// End-to-end checks of /blob/{txHash}/inclusion against a real Postgres
// (TEST_DB_URL): that the summary and skipped queries are valid SQL whose
// columns unify with their row structs, that the window and counts match
// what the seeded rows imply, that the including block bounds the history,
// that a freshly seen pending transaction gets an empty window rather than
// none, and that both queries reach the per-transaction index instead of
// walking the candidate, builder or metrics tables.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

// seedInclusionFixtures lays out a 12s-slot chain of six blocks and three
// transactions:
//
//	block  builder      snapshot  metrics  candidate rows
//	2000   titan        yes       yes      0xwait too_recent (block predates first seen)
//	2001   titan        yes       yes      0xwait eligible
//	2002   (none)       -         yes      0xwait no_room
//	2003   titan        no        yes      (none: indexed from history)  ← includes 0xnever
//	2004   beaverbuild  yes       no       0xwait priced_out_blob_fee
//	2005   titan        yes       yes      0xpend nonce_gap              ← includes 0xwait
//	2006   (none)       -         yes      0xwait eligible (reorg leftover past inclusion)
//
// 0xwait was first seen 2s after block 2000's timestamp and landed in 2005;
// 0xpend is still in the mempool, first seen 2s before block 2005; 0xnever
// was indexed from history with no first-seen time; 0xfresh is pending, first
// seen 3s after block 2005, and no block has recorded it yet — block 2006,
// the only block since, has no builder row, which the window boundary must
// see through.
// The handler validates the path hash as a 32-byte hex string, so the seeded
// hashes must be real ones.
var (
	inclusionTxWait   = "0x" + strings.Repeat("a1", 32)
	inclusionTxPend   = "0x" + strings.Repeat("b2", 32)
	inclusionTxNever  = "0x" + strings.Repeat("c3", 32)
	inclusionTxFresh  = "0x" + strings.Repeat("d4", 32)
	inclusionTxNewest = "0x" + strings.Repeat("e5", 32)
)

func seedInclusionFixtures(t *testing.T, sqlxDB *sqlx.DB, base time.Time) {
	t.Helper()
	at := func(block int64) time.Time { return base.Add(time.Duration(block-2000) * 12 * time.Second) }

	if _, err := sqlxDB.Exec(`
		INSERT INTO block_metrics (chain_id, block_number, block_timestamp, blob_count, blob_params_max, blob_base_fee) VALUES
			(1, 2000, $1, 2, 6, 100),
			(1, 2001, $2, 3, 6, 1500000000),
			(1, 2002, $3, 6, 6, 200),
			(1, 2003, $4, 1, 6, 300),
			(1, 2005, $5, 4, 6, 400),
			(1, 2006, $6, 0, 6, 500)
	`, at(2000), at(2001), at(2002), at(2003), at(2005), at(2006)); err != nil {
		t.Fatalf("seed block_metrics: %v", err)
	}
	if _, err := sqlxDB.Exec(`
		INSERT INTO block_builders (
			chain_id, block_number, block_timestamp, fee_recipient, extra_data,
			builder_key, builder_name, tx_count, proposer_payment_wei, proposer_payment_to,
			candidate_snapshot, pending_candidate_txs, eligible_skipped_txs,
			eligible_skipped_blobs, eligible_skipped_max_tip
		) VALUES
			(1, 2000, $1, '0xTitanA', '0x546974616e', 'titan', 'Titan', 100, NULL, NULL, TRUE, 1, 0, 0, NULL),
			(1, 2001, $2, '0xTitanA', '0x546974616e', 'titan', 'Titan', 110, 10000000000000000, '0xProposer', TRUE, 3, 1, 2, 7000000000),
			(1, 2003, $3, '0xTitanB', '0x546974616e', 'titan', 'Titan', 90, NULL, NULL, FALSE, NULL, NULL, NULL, NULL),
			(1, 2004, $4, '0xBeaver', '0x6265617665726275696c642e6f7267', 'beaverbuild', 'beaverbuild', 120, NULL, NULL, TRUE, 2, 0, 0, NULL),
			(1, 2005, $5, '0xTitanA', '0x546974616e', 'titan', 'Titan', 130, NULL, NULL, TRUE, 1, 0, 0, NULL)
	`, at(2000), at(2001), at(2003), at(2004), at(2005)); err != nil {
		t.Fatalf("seed block_builders: %v", err)
	}

	waitSeen := at(2000).Add(2 * time.Second)
	for _, row := range []struct {
		block     int64
		txHash    string
		firstSeen interface{}
		txIndex   interface{}
	}{
		{2005, inclusionTxWait, waitSeen, 5},
		{2003, inclusionTxNever, nil, nil},
	} {
		if _, err := sqlxDB.Exec(`
			INSERT INTO blobs (
				chain_id, block_number, blob_index, tx_hash, from_address, user_attribution,
				blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
				timestamp, max_fee_per_blob_gas, blob_gas_used,
				max_priority_fee_per_gas, max_fee_per_gas, priority_fee_per_gas,
				first_seen_at, tx_index
			) VALUES (1, $1, 0, $2, $3, 'Fancy Rollup', 131072, 10, 2, 1310720, $4, 12, 131072, 1000000000, 60000000000, 1000000000, $5, $6)
		`, row.block, row.txHash, buildAddrFancy, at(row.block), row.firstSeen, row.txIndex); err != nil {
			t.Fatalf("seed blob %s: %v", row.txHash, err)
		}
	}

	pendSeen := at(2005).Add(-2 * time.Second)
	freshSeen := at(2005).Add(3 * time.Second)
	if _, err := sqlxDB.Exec(`
		INSERT INTO mempool_blobs (
			chain_id, tx_hash, blob_index, from_address, user_attribution,
			blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
			timestamp, max_fee_per_blob_gas, blob_gas_used
		) VALUES
			(1, $3, 0, $1, '', 131072, 10, 2, 1310720, $2, 12, 131072),
			(1, $5, 0, $1, '', 131072, 10, 2, 1310720, $4, 12, 131072)
	`, buildAddrSolo, pendSeen, inclusionTxPend, freshSeen, inclusionTxFresh); err != nil {
		t.Fatalf("seed mempool blobs: %v", err)
	}

	if _, err := sqlxDB.Exec(`
		INSERT INTO blob_inclusion_candidates (
			chain_id, block_number, block_timestamp, tx_hash, from_address, user_attribution,
			nonce, blob_count, max_priority_fee_per_gas, max_fee_per_gas, max_fee_per_blob_gas,
			first_seen_at, reason
		) VALUES
			(1, 2000, $1, $10, $6, 'Fancy Rollup', 7, 1, 1000000000, 60000000000, 12, $8, 'too_recent'),
			(1, 2001, $2, $10, $6, 'Fancy Rollup', 7, 1, 1000000000, 60000000000, 12, $8, 'eligible'),
			(1, 2002, $3, $10, $6, 'Fancy Rollup', 7, 1, 1000000000, 60000000000, 12, $8, 'no_room'),
			(1, 2004, $4, $10, $6, 'Fancy Rollup', 7, 1, 1000000000, 60000000000, 12, $8, 'priced_out_blob_fee'),
			(1, 2006, $12, $10, $6, 'Fancy Rollup', 7, 1, 1000000000, 60000000000, 12, $8, 'eligible'),
			(1, 2005, $5, $11, $7, NULL, 9, 1, 1000000000, 60000000000, 12, $9, 'nonce_gap')
	`, at(2000), at(2001), at(2002), at(2004), at(2005), buildAddrFancy, buildAddrSolo, waitSeen, pendSeen, inclusionTxWait, inclusionTxPend, at(2006)); err != nil {
		t.Fatalf("seed blob_inclusion_candidates: %v", err)
	}
}

func getInclusion(t *testing.T, a *API, txHash string) (*httptest.ResponseRecorder, BlobInclusionResponse) {
	t.Helper()
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("txHash", txHash)
	req := httptest.NewRequest(http.MethodGet, "/blob/"+txHash+"/inclusion", http.NoBody)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	a.GetBlobInclusion(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("%s: expected 200, got %d: %s", txHash, w.Code, w.Body.String())
	}
	var resp struct {
		Data BlobInclusionResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return w, resp.Data
}

func TestBlobInclusionAgainstRealPostgres(t *testing.T) {
	sqlxDB, _ := resetBuilderSchema(t, "api_blob_inclusion")
	base := time.Now().UTC().Truncate(time.Second).Add(-10 * time.Minute)
	seedInclusionFixtures(t, sqlxDB, base)
	a := newBuilderTestAPI(sqlxDB)

	t.Run("ConfirmedTimeline", func(t *testing.T) {
		w, got := getInclusion(t, a, inclusionTxWait)
		if !got.Confirmed || got.Included == nil {
			t.Fatalf("expected a confirmed, included transaction: %+v", got)
		}
		if got.TimeToInclusionMs == nil || *got.TimeToInclusionMs != 58000 {
			t.Errorf("time_to_inclusion_ms = %v, want 58000", got.TimeToInclusionMs)
		}
		inc := got.Included
		if inc.BlockNumber != 2005 || inc.TxIndex == nil || *inc.TxIndex != 5 {
			t.Errorf("included = %+v, want block 2005 tx_index 5", inc)
		}
		if inc.Builder == nil || inc.Builder.Key != "titan" || !inc.Builder.CandidateSnapshot {
			t.Errorf("included.builder = %+v, want titan with a snapshot", inc.Builder)
		}
		if inc.BlobCount == nil || *inc.BlobCount != 4 || inc.MaxBlobs == nil || *inc.MaxBlobs != 6 {
			t.Errorf("included occupancy = %v/%v, want 4/6", inc.BlobCount, inc.MaxBlobs)
		}
		if inc.BlobBaseFee == nil || *inc.BlobBaseFee != "400" {
			t.Errorf("included.blob_base_fee = %v, want 400", inc.BlobBaseFee)
		}

		// Window: from LEAST(first block at/after first seen = 2001, first
		// candidate = 2000) = 2000 to the block before inclusion. Blocks are
		// counted from block_metrics — 2004 has no metrics row, so four of
		// the five heights count — while snapshots come from block_builders:
		// 2000, 2001 and 2004 took one (2002 has no builder row, 2003 was
		// indexed from history).
		if got.Window == nil {
			t.Fatal("expected a window")
		}
		if got.Window.FromBlock != 2000 || got.Window.ToBlock != 2004 || got.Window.Blocks != 4 || got.Window.SnapshotBlocks != 3 {
			t.Errorf("window = %+v, want 2000..2004, 4 blocks, 3 snapshots", got.Window)
		}
		// The leftover row at 2006 sits past inclusion and must not count.
		if got.SkippedBlocks != 4 || got.EligibleSkippedBlocks != 1 || got.SkippedTruncated {
			t.Errorf("skipped/eligible = %d/%d truncated=%v, want 4/1 false", got.SkippedBlocks, got.EligibleSkippedBlocks, got.SkippedTruncated)
		}
		if len(got.Skipped) != 4 {
			t.Fatalf("skipped = %+v, want 4 entries", got.Skipped)
		}
		wantOrder := []int64{2000, 2001, 2002, 2004}
		wantReason := []string{"too_recent", "eligible", "no_room", "priced_out_blob_fee"}
		wantWaited := []int64{-2000, 10000, 22000, 46000}
		for i, entry := range got.Skipped {
			if entry.BlockNumber != wantOrder[i] || entry.Reason != wantReason[i] {
				t.Errorf("skipped[%d] = block %d %s, want %d %s", i, entry.BlockNumber, entry.Reason, wantOrder[i], wantReason[i])
			}
			if entry.WaitedMs == nil || *entry.WaitedMs != wantWaited[i] {
				t.Errorf("skipped[%d].waited_ms = %v, want %d", i, entry.WaitedMs, wantWaited[i])
			}
		}
		// 2001: builder and metrics both present, with the fee in gwei.
		if b := got.Skipped[1]; b.Builder == nil || b.Builder.Key != "titan" || b.Builder.ProposerPaymentEth != "0.01" ||
			b.BlobCount == nil || *b.BlobCount != 3 || b.BlobBaseFeeGwei != "1.5" {
			t.Errorf("skipped[1] = %+v, want titan with 3/6 blobs at 1.5 gwei", b)
		}
		// 2002: no builder row yet, metrics still describe the full block.
		if b := got.Skipped[2]; b.Builder != nil || b.BlobCount == nil || *b.BlobCount != 6 {
			t.Errorf("skipped[2] = %+v, want no builder and 6 blobs", b)
		}
		// 2004: builder without metrics.
		if b := got.Skipped[3]; b.Builder == nil || b.Builder.Key != "beaverbuild" || b.BlobCount != nil || b.BlobBaseFee != nil {
			t.Errorf("skipped[3] = %+v, want beaverbuild with no occupancy", b)
		}
		if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age=") {
			t.Errorf("confirmed timeline should be cacheable, got Cache-Control %q", cc)
		}
	})

	t.Run("PendingTimeline", func(t *testing.T) {
		w, got := getInclusion(t, a, inclusionTxPend)
		if got.Confirmed || got.Included != nil || got.TimeToInclusionMs != nil {
			t.Fatalf("expected a pending transaction: %+v", got)
		}
		if got.FirstSeenAt == nil {
			t.Fatal("pending rows project their own first-seen time")
		}
		// From LEAST(first block at/after first seen = 2005, first candidate
		// = 2005) to the newest indexed block, 2006, which has no builder
		// row and so counts as waited through but not as a snapshot.
		if got.Window == nil || got.Window.FromBlock != 2005 || got.Window.ToBlock != 2006 || got.Window.Blocks != 2 || got.Window.SnapshotBlocks != 1 {
			t.Errorf("window = %+v, want 2005..2006 with one snapshot block", got.Window)
		}
		if got.SkippedBlocks != 1 || got.EligibleSkippedBlocks != 0 || len(got.Skipped) != 1 || got.Skipped[0].Reason != "nonce_gap" {
			t.Errorf("skipped = %d/%d %+v, want one nonce_gap entry", got.SkippedBlocks, got.EligibleSkippedBlocks, got.Skipped)
		}
		if waited := got.Skipped[0].WaitedMs; waited == nil || *waited != 2000 {
			t.Errorf("skipped[0].waited_ms = %v, want 2000", waited)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "" {
			t.Errorf("pending timeline must not be cached, got %q", cc)
		}
	})

	t.Run("FreshPendingTransactionSeesBuilderlessBlock", func(t *testing.T) {
		_, got := getInclusion(t, a, inclusionTxFresh)
		if got.Confirmed || got.Included != nil || got.FirstSeenAt == nil {
			t.Fatalf("expected a pending transaction with a first-seen time: %+v", got)
		}
		// The only block since first seen, 2006, has a metrics row but no
		// builder row: the boundary must still find it (a builder-only probe
		// would see no block at all and report the empty span 2007..2006),
		// and it counts as waited through without a snapshot.
		if got.Window == nil || got.Window.FromBlock != 2006 || got.Window.ToBlock != 2006 || got.Window.Blocks != 1 || got.Window.SnapshotBlocks != 0 {
			t.Errorf("window = %+v, want 2006..2006 with no snapshot block", got.Window)
		}
		if got.SkippedBlocks != 0 || len(got.Skipped) != 0 || got.SkippedTruncated {
			t.Errorf("expected no skipped blocks, got %d %+v", got.SkippedBlocks, got.Skipped)
		}
	})

	t.Run("PendingPastEveryIndexedBlockHasEmptySpan", func(t *testing.T) {
		// First seen after the newest indexed block: first_seen_at bounds
		// the wait but no block has been produced in it, so the window is
		// the empty span past the newest block, not null.
		if _, err := sqlxDB.Exec(`
			INSERT INTO mempool_blobs (
				chain_id, tx_hash, blob_index, from_address, user_attribution,
				blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
				timestamp, max_fee_per_blob_gas, blob_gas_used
			) VALUES (1, $1, 0, $2, '', 131072, 10, 2, 1310720, $3, 12, 131072)
		`, inclusionTxNewest, buildAddrSolo, base.Add(6*12*time.Second+5*time.Second)); err != nil {
			t.Fatalf("seed mempool blob: %v", err)
		}
		_, got := getInclusion(t, a, inclusionTxNewest)
		if got.Window == nil || got.Window.FromBlock != 2007 || got.Window.ToBlock != 2006 || got.Window.Blocks != 0 || got.Window.SnapshotBlocks != 0 {
			t.Errorf("window = %+v, want the empty span 2007..2006", got.Window)
		}
	})

	t.Run("HistoryOnlyTransaction", func(t *testing.T) {
		_, got := getInclusion(t, a, inclusionTxNever)
		if !got.Confirmed || got.Included == nil || got.Included.BlockNumber != 2003 {
			t.Fatalf("expected inclusion at 2003: %+v", got)
		}
		if got.FirstSeenAt != nil || got.TimeToInclusionMs != nil {
			t.Errorf("no first-seen time should yield no wait: %+v", got)
		}
		if got.Window != nil {
			t.Errorf("nothing bounds the window, got %+v", got.Window)
		}
		if got.SkippedBlocks != 0 || len(got.Skipped) != 0 {
			t.Errorf("expected no skipped blocks, got %d %+v", got.SkippedBlocks, got.Skipped)
		}
		if got.Included.Builder == nil || got.Included.Builder.CandidateSnapshot {
			t.Errorf("included.builder = %+v, want titan without a snapshot", got.Included.Builder)
		}
	})
}

// TestBlobInclusionQueryPlansUseTxIndex seeds a week of candidate rows and
// checks that neither inclusion query walks the candidate, builder or
// metrics tables: the per-transaction probe must land on
// idx_blob_inclusion_candidates_chain_tx_block.
func TestBlobInclusionQueryPlansUseTxIndex(t *testing.T) {
	sqlxDB, _ := resetBuilderSchema(t, "api_blob_inclusion_explain")

	const (
		totalBlocks    = 7 * 7200
		poolPerBlock   = 4
		secondsPerStep = 12
	)
	now := time.Now().UTC().Truncate(time.Hour)
	oldest := now.Add(-totalBlocks * secondsPerStep * time.Second)

	if _, err := sqlxDB.Exec(`
		INSERT INTO block_builders (
			chain_id, block_number, block_timestamp, fee_recipient, extra_data,
			builder_key, builder_name, tx_count, candidate_snapshot
		)
		SELECT 1, g, $1::timestamp + (g * $2 * INTERVAL '1 second'), '0xBuilder' || (g % 5),
			'0x67657468', 'builder-' || (g % 5), 'Builder ' || (g % 5), 100, TRUE
		FROM generate_series(1, $3) AS g
	`, oldest, secondsPerStep, totalBlocks); err != nil {
		t.Fatalf("seed block_builders: %v", err)
	}
	if _, err := sqlxDB.Exec(`
		INSERT INTO block_metrics (chain_id, block_number, block_timestamp, blob_count, blob_params_max)
		SELECT 1, g, $1::timestamp + (g * $2 * INTERVAL '1 second'), 3, 6
		FROM generate_series(1, $3) AS g
	`, oldest, secondsPerStep, totalBlocks); err != nil {
		t.Fatalf("seed block_metrics: %v", err)
	}
	// Every block leaves a pool of poolPerBlock transactions behind, each of
	// which waits ten blocks: enough distinct hashes that a tx_hash probe is
	// far cheaper than a scan.
	if _, err := sqlxDB.Exec(`
		INSERT INTO blob_inclusion_candidates (
			chain_id, block_number, block_timestamp, tx_hash, from_address, user_attribution,
			nonce, blob_count, max_priority_fee_per_gas, max_fee_per_gas, max_fee_per_blob_gas,
			first_seen_at, reason
		)
		SELECT 1, g, $1::timestamp + (g * $2 * INTERVAL '1 second'),
			'0xcand' || ((g / 10) * $4 + p), '0xsender' || p, NULL, g, 1, 1000000000, 60000000000, 20,
			$1::timestamp + ((g / 10) * 10 * $2 * INTERVAL '1 second'), 'eligible'
		FROM generate_series(1, $3) AS g, generate_series(0, $4 - 1) AS p
	`, oldest, secondsPerStep, totalBlocks, poolPerBlock); err != nil {
		t.Fatalf("seed blob_inclusion_candidates: %v", err)
	}
	if _, err := sqlxDB.Exec("ANALYZE"); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	explain := func(name, query string, args ...interface{}) string {
		t.Helper()
		rows, err := sqlxDB.Query("EXPLAIN "+query, args...)
		if err != nil {
			t.Fatalf("%s: explain: %v", name, err)
		}
		defer rows.Close()
		var plan []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("%s: scan: %v", name, err)
			}
			plan = append(plan, line)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: rows: %v", name, err)
		}
		text := strings.Join(plan, "\n")
		t.Logf("===== %s =====\n%s", name, text)
		return text
	}
	assertPlan := func(name, plan string) {
		t.Helper()
		for _, table := range []string{"blob_inclusion_candidates", "block_builders", "block_metrics"} {
			if strings.Contains(plan, "Seq Scan on "+table) {
				t.Errorf("%s: plan sequentially scans %s:\n%s", name, table, plan)
			}
		}
		if !strings.Contains(plan, "idx_blob_inclusion_candidates_chain_tx_block") {
			t.Errorf("%s: plan does not use the per-transaction index:\n%s", name, plan)
		}
	}

	const txHash = "0xcand40002"
	firstSeen := oldest.Add(10000 * 10 * secondsPerStep * time.Second)
	for _, tc := range []struct {
		name     string
		included interface{}
	}{
		{"confirmed", int64(100010)},
		{"pending", nil},
	} {
		assertPlan("summary "+tc.name, explain("summary "+tc.name, queryBlobInclusionSummary, 1, txHash, firstSeen, tc.included))
		assertPlan("blocks "+tc.name, explain("blocks "+tc.name, queryBlobInclusionBlocks, 1, txHash, tc.included, blobInclusionSkippedLimit))
	}
}
