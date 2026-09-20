//go:build integration

package api

// End-to-end checks of the block-builder endpoints against a real Postgres
// (TEST_DB_URL). The unit tests drive the handlers through a mock DB that
// never parses SQL, so this is the only check that the builder query
// constants are valid SQL, that their columns unify with the row structs,
// and that the arithmetic the handlers present — share percentages, the
// inclusion index, tip and wait percentiles, long-tail grouping — matches
// what the seeded rows imply. It also asserts the 24h plans stay on the
// range indexes instead of scanning blobs.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db"
	"github.com/a-thomas-22/blob-indexer-api/internal/testdb"
)

// Seeded sender addresses. The indexer stores EIP-55 checksummed addresses,
// but the builder queries never re-derive them, so plain literals are fine
// as long as the blob_users join is matched case-insensitively.
const (
	buildAddrFancy = "0xFancyRollupSender"
	buildAddrSolo  = "0xSoloSender"
	buildAddrGeth  = "0xGethBlockSender"
	buildAddrSkip  = "0xSkippedSender"
)

func resetBuilderSchema(t *testing.T, suffix string) (*sqlx.DB, string) {
	t.Helper()
	url := testdb.URL(t, suffix)
	sqlxDB, err := sqlx.Connect("postgres", url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sqlxDB.Close() })

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
	if _, err := sqlxDB.Exec(`
		INSERT INTO networks (chain_id, name, start_block) VALUES (1, 'mainnet', '0')
		ON CONFLICT (chain_id) DO NOTHING
	`); err != nil {
		t.Fatalf("seed network: %v", err)
	}
	return sqlxDB, url
}

// seedBuilderFixtures lays out five blocks across three builders inside the
// last hour, so every bounded range covers them:
//
//	1000  titan        3 blobs   proposer payment 0.01 ETH, live snapshot
//	1001  titan        6 blobs   full block, payment 0.03 ETH, live snapshot
//	1002  titan        0 blobs   no payment, indexed from history
//	1003  beaverbuild  2 blobs
//	1004  extra:geth   1 blob    legacy blob row with no recorded tip
func seedBuilderFixtures(t *testing.T, sqlxDB *sqlx.DB, base time.Time) {
	t.Helper()

	blockTime := func(minutesAgo int) time.Time {
		return base.Add(-time.Duration(minutesAgo) * time.Minute)
	}

	if _, err := sqlxDB.Exec(`
		INSERT INTO blob_users (chain_id, address, name, category, first_seen, last_seen) VALUES
			(1, LOWER($1), 'Fancy Rollup', 'rollup', $2, $2)
	`, buildAddrFancy, base); err != nil {
		t.Fatalf("seed blob_users: %v", err)
	}

	if _, err := sqlxDB.Exec(`
		INSERT INTO block_metrics (chain_id, block_number, block_timestamp, blob_count, blob_params_max) VALUES
			(1, 1000, $1, 3, 6),
			(1, 1001, $2, 6, 6),
			(1, 1002, $3, 0, 6),
			(1, 1003, $4, 2, 6),
			(1, 1004, $5, 1, 6)
	`, blockTime(30), blockTime(25), blockTime(20), blockTime(15), blockTime(10)); err != nil {
		t.Fatalf("seed block_metrics: %v", err)
	}

	// extra_data is 0x-hex: "Titan (titanbuilder.xyz)", "beaverbuild.org",
	// and "geth" respectively.
	if _, err := sqlxDB.Exec(`
		INSERT INTO block_builders (
			chain_id, block_number, block_timestamp, fee_recipient, extra_data,
			builder_key, builder_name, tx_count, proposer_payment_wei, proposer_payment_to,
			candidate_snapshot, pending_candidate_txs, eligible_skipped_txs,
			eligible_skipped_blobs, eligible_skipped_max_tip
		) VALUES
			(1, 1000, $1, '0xTitanA', '0x546974616e2028746974616e6275696c6465722e78797a29', 'titan', 'Titan', 180, 10000000000000000, '0xProposer', TRUE, 4, 2, 3, 7000000000),
			(1, 1001, $2, '0xTitanA', '0x546974616e2028746974616e6275696c6465722e78797a29', 'titan', 'Titan', 150, 30000000000000000, '0xProposer', TRUE, 2, 0, 0, NULL),
			(1, 1002, $3, '0xTitanB', '0x546974616e2028746974616e6275696c6465722e78797a29', 'titan', 'Titan', 90, NULL, NULL, FALSE, NULL, NULL, NULL, NULL),
			(1, 1003, $4, '0xBeaver', '0x6265617665726275696c642e6f7267', 'beaverbuild', 'beaverbuild', 120, NULL, NULL, FALSE, NULL, NULL, NULL, NULL),
			(1, 1004, $5, '0xLocal', '0x67657468', 'extra:geth', 'geth', 60, NULL, NULL, FALSE, NULL, NULL, NULL, NULL)
	`, blockTime(30), blockTime(25), blockTime(20), blockTime(15), blockTime(10)); err != nil {
		t.Fatalf("seed block_builders: %v", err)
	}

	// Blob rows, one per blob, grouped into transactions. first_seen_at is
	// deliberately AFTER the block timestamp on two of them so the negative
	// time-to-inclusion path is exercised end to end.
	type seedBlob struct {
		block      int64
		index      int
		txHash     string
		from       string
		attributed string
		tip        interface{}
		firstSeen  interface{}
		txIndex    interface{}
		at         time.Time
	}
	b1000, b1001, b1003, b1004 := blockTime(30), blockTime(25), blockTime(15), blockTime(10)
	rows := []seedBlob{
		// titan 1000: one 2-blob tx seen 4.2s early, one 1-blob tx seen 1.5s LATE.
		{1000, 0, "0xtitan-a", buildAddrFancy, "Fancy Rollup", "1000000000", b1000.Add(-4200 * time.Millisecond), 4, b1000},
		{1000, 1, "0xtitan-a", buildAddrFancy, "Fancy Rollup", "1000000000", b1000.Add(-4200 * time.Millisecond), 4, b1000},
		{1000, 2, "0xtitan-b", buildAddrSolo, "", "5000000000", b1000.Add(1500 * time.Millisecond), 9, b1000},
		// titan 1001: a 6-blob tx never seen pending (arrived with its block).
		{1001, 0, "0xtitan-c", buildAddrFancy, "Fancy Rollup", "3000000000", nil, 1, b1001},
		{1001, 1, "0xtitan-c", buildAddrFancy, "Fancy Rollup", "3000000000", nil, 1, b1001},
		{1001, 2, "0xtitan-c", buildAddrFancy, "Fancy Rollup", "3000000000", nil, 1, b1001},
		{1001, 3, "0xtitan-c", buildAddrFancy, "Fancy Rollup", "3000000000", nil, 1, b1001},
		{1001, 4, "0xtitan-c", buildAddrFancy, "Fancy Rollup", "3000000000", nil, 1, b1001},
		{1001, 5, "0xtitan-c", buildAddrFancy, "Fancy Rollup", "3000000000", nil, 1, b1001},
		// beaverbuild 1003: a 2-blob tx seen 2s after slot start.
		{1003, 0, "0xbeaver-a", buildAddrFancy, "Fancy Rollup", "2000000000", b1003.Add(2 * time.Second), 3, b1003},
		{1003, 1, "0xbeaver-a", buildAddrFancy, "Fancy Rollup", "2000000000", b1003.Add(2 * time.Second), 3, b1003},
		// extra:geth 1004: a legacy row with neither tip nor first-seen time.
		{1004, 0, "0xgeth-a", buildAddrGeth, "", nil, nil, nil, b1004},
	}
	for _, row := range rows {
		if _, err := sqlxDB.Exec(`
			INSERT INTO blobs (
				chain_id, block_number, blob_index, tx_hash, from_address, user_attribution,
				blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
				timestamp, max_fee_per_blob_gas, blob_gas_used,
				max_priority_fee_per_gas, max_fee_per_gas, priority_fee_per_gas,
				first_seen_at, tx_index
			) VALUES (1, $1, $2, $3, $4, $5, 131072, 10, 2, 1310720, $6, 12, 131072, $7, 60000000000, $7, $8, $9)
		`, row.block, row.index, row.txHash, row.from, row.attributed, row.at, row.tip, row.firstSeen, row.txIndex); err != nil {
			t.Fatalf("seed blob %s/%d: %v", row.txHash, row.index, err)
		}
	}

	// Candidate detail on titan's first block: two eligible transactions plus
	// one the classifier disqualified, which must never reach /builders/{key}.
	if _, err := sqlxDB.Exec(`
		INSERT INTO blob_inclusion_candidates (
			chain_id, block_number, block_timestamp, tx_hash, from_address, user_attribution,
			nonce, blob_count, max_priority_fee_per_gas, max_fee_per_gas, max_fee_per_blob_gas,
			first_seen_at, reason
		) VALUES
			(1, 1000, $1, '0xskip-1', $2, 'Fancy Rollup', 7, 2, 7000000000, 50000000000, 20, $1, 'eligible'),
			(1, 1000, $1, '0xskip-2', $3, NULL, 3, 1, 3000000000, 50000000000, 20, $1, 'eligible'),
			(1, 1000, $1, '0xskip-3', $3, NULL, 4, 5, 9000000000, 50000000000, 20, $1, 'too_recent')
	`, blockTime(30), buildAddrFancy, buildAddrSkip); err != nil {
		t.Fatalf("seed blob_inclusion_candidates: %v", err)
	}

	// A fee-bump event carrying the 000017 fee context, plus a legacy row
	// that predates it.
	if _, err := sqlxDB.Exec(`
		INSERT INTO blob_replacements (
			chain_id, replaced_tx_hash, replacement_tx_hash, from_address, nonce, replaced_at,
			replaced_max_priority_fee_per_gas, replaced_max_fee_per_blob_gas, replaced_first_seen_at,
			replacement_max_priority_fee_per_gas, replacement_max_fee_per_blob_gas
		) VALUES
			(1, '0xold', '0xnew', $1, 7, $2, 1000000000, 2000000000, $3, 3000000000, 4000000000),
			(1, '0xancient', '0xolder', $1, 6, $2, NULL, NULL, NULL, NULL, NULL)
	`, buildAddrFancy, base, base.Add(-time.Minute)); err != nil {
		t.Fatalf("seed blob_replacements: %v", err)
	}
}

func newBuilderTestAPI(sqlxDB *sqlx.DB) *API {
	a := newTestAPIWithDB(&db.DB{DB: sqlxDB})
	a.networks = map[int]config.NetworkConfig{
		1: {Name: "mainnet", ChainID: 1, Enabled: true},
	}
	return a
}

func nearly(t *testing.T, label string, got, want, tolerance float64) {
	t.Helper()
	if math.Abs(got-want) > tolerance {
		t.Errorf("%s = %v, want %v (±%v)", label, got, want, tolerance)
	}
}

func TestBuilderEndpointsAgainstRealPostgres(t *testing.T) {
	sqlxDB, _ := resetBuilderSchema(t, "api_builders")
	base := time.Now().UTC().Truncate(time.Second)
	seedBuilderFixtures(t, sqlxDB, base)
	a := newBuilderTestAPI(sqlxDB)

	t.Run("BuildersLeaderboard", func(t *testing.T) {
		w := httptest.NewRecorder()
		a.GetBuilders(w, httptest.NewRequest(http.MethodGet, "/builders?range=24h", http.NoBody))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		data := decodeBuilders(t, w)

		if data.Totals != (BuilderTotals{Blocks: 5, BlobBlocks: 4, Blobs: 12}) {
			t.Fatalf("totals = %+v, want 5/4/12", data.Totals)
		}
		if len(data.Builders) != 3 {
			t.Fatalf("expected 3 builders, got %+v", data.Builders)
		}
		// Ordered by blocks desc: titan (3), then beaverbuild and extra:geth
		// (1 each) broken by key.
		wantOrder := []string{"titan", "beaverbuild", "extra:geth"}
		for i, key := range wantOrder {
			if data.Builders[i].Key != key {
				t.Fatalf("builder[%d] = %s, want %s", i, data.Builders[i].Key, key)
			}
		}

		titan := data.Builders[0]
		if titan.Name != "Titan" || !titan.Known {
			t.Errorf("titan identity = %+v", titan)
		}
		if titan.Blocks != 3 || titan.BlobBlocks != 2 || titan.Blobs != 9 || titan.FullBlocks != 1 {
			t.Errorf("titan volume = %+v", titan)
		}
		if titan.BlockSharePercent != 60 || titan.BlobSharePercent != 75 {
			t.Errorf("titan shares = %v / %v, want 60 / 75", titan.BlockSharePercent, titan.BlobSharePercent)
		}
		if titan.AvgBlobsPerBlobBlock != 4.5 {
			t.Errorf("titan avg_blobs_per_blob_block = %v, want 4.5", titan.AvgBlobsPerBlobBlock)
		}
		// Only two of titan's three fee recipients appear, busiest first.
		if len(titan.FeeRecipients) != 2 || titan.FeeRecipients[0] != "0xTitanA" {
			t.Errorf("titan fee_recipients = %v", titan.FeeRecipients)
		}
		if titan.MEVBoostBlocks != 2 {
			t.Errorf("titan mev_boost_blocks = %d, want 2", titan.MEVBoostBlocks)
		}
		// percentile_disc over {0.01, 0.03} ETH takes the lower observed
		// payment; the total is the exact big-integer sum.
		if titan.ProposerPayment == nil ||
			titan.ProposerPayment.MedianWei != "10000000000000000" ||
			titan.ProposerPayment.TotalWei != "40000000000000000" ||
			titan.ProposerPayment.TotalEth != "0.04" {
			t.Errorf("titan proposer_payment = %+v", titan.ProposerPayment)
		}
		// Per-transaction tips: 1, 5, and 3 gwei. percentile_cont
		// interpolates: p10 = 1.4, p50 = 3, p90 = 4.6 gwei.
		if titan.Tip == nil {
			t.Fatal("expected titan tip stats")
		}
		if titan.Tip.TxCount != 3 || titan.Tip.MinGwei != "1" || titan.Tip.P10Gwei != "1.4" ||
			titan.Tip.P50Gwei != "3" || titan.Tip.P90Gwei != "4.6" {
			t.Errorf("titan tip = %+v", titan.Tip)
		}
		// Waits: +4200ms and -1500ms; the 6-blob tx has no first-seen time.
		if titan.TimeToInclusionMs == nil || titan.TimeToInclusionMs.SampleCount != 2 {
			t.Fatalf("titan time_to_inclusion_ms = %+v", titan.TimeToInclusionMs)
		}
		nearly(t, "titan wait p50", titan.TimeToInclusionMs.P50, 1350, 1)
		nearly(t, "titan wait p90", titan.TimeToInclusionMs.P90, 3630, 1)
		// Two of titan's three blocks were snapshotted; one of those had
		// eligible transactions left out.
		if titan.Candidates == nil {
			t.Fatal("expected titan candidate stats")
		}
		if titan.Candidates.SnapshotBlocks != 2 || titan.Candidates.BlocksWithEligibleSkipped != 1 ||
			titan.Candidates.EligibleSkippedTxs != 2 || titan.Candidates.EligibleSkippedBlobs != 3 ||
			titan.Candidates.EligibleSkippedMaxTipGwei != "7" {
			t.Errorf("titan candidates = %+v", titan.Candidates)
		}

		beaver := data.Builders[1]
		if beaver.ProposerPayment != nil || beaver.Candidates != nil {
			t.Errorf("beaverbuild should have no payment or snapshot stats: %+v", beaver)
		}
		// Its only sample was seen after slot start, so the median wait is
		// negative and must survive to the wire.
		if beaver.TimeToInclusionMs == nil || beaver.TimeToInclusionMs.SampleCount != 1 {
			t.Fatalf("beaverbuild time_to_inclusion_ms = %+v", beaver.TimeToInclusionMs)
		}
		nearly(t, "beaverbuild wait p50", beaver.TimeToInclusionMs.P50, -2000, 1)

		geth := data.Builders[2]
		if geth.Known {
			t.Error("extra:geth is a registry fallback and must report known=false")
		}
		// Its only blob row predates the priority-fee column.
		if geth.Tip != nil || geth.TimeToInclusionMs != nil {
			t.Errorf("extra:geth should have no tip or wait stats: %+v", geth)
		}
	})

	t.Run("BuilderDetail", func(t *testing.T) {
		w := httptest.NewRecorder()
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("key", "titan")
		req := httptest.NewRequest(http.MethodGet, "/builders/titan?range=24h", http.NoBody)
		a.GetBuilderByKey(w, req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx)))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		data := decodeBuilderDetail(t, w)

		// The aggregate half matches /builders, and the denominators still
		// cover every builder rather than just this one.
		if data.Builder.Key != "titan" || data.Builder.Blobs != 9 {
			t.Fatalf("builder = %+v", data.Builder)
		}
		if data.Totals.Blobs != 12 {
			t.Errorf("totals = %+v, want the window-wide 12 blobs", data.Totals)
		}

		if len(data.Users) != 2 {
			t.Fatalf("expected 2 user rows, got %+v", data.Users)
		}
		fancy := data.Users[0]
		if fancy.Key != "fancy_rollup" || fancy.Name != "Fancy Rollup" || !fancy.IsEntity {
			t.Errorf("fancy row identity = %+v", fancy)
		}
		if fancy.Blobs != 8 || fancy.TxCount != 2 {
			t.Errorf("fancy volume = %+v", fancy)
		}
		// 8 of titan's 9 blobs, against 10 of the window's 12.
		nearly(t, "fancy within share", fancy.ShareWithinBuilderPercent, 8.0/9*100, 1e-4)
		nearly(t, "fancy overall share", fancy.ShareOverallPercent, 10.0/12*100, 1e-4)
		if fancy.InclusionIndex == nil {
			t.Fatal("expected an inclusion index for fancy_rollup")
		}
		nearly(t, "fancy inclusion_index", *fancy.InclusionIndex, (8.0/9)/(10.0/12), 1e-4)

		solo := data.Users[1]
		// Unattributed senders stay keyed by address, matching /users.
		if solo.Key != buildAddrSolo || solo.IsEntity {
			t.Errorf("solo row identity = %+v", solo)
		}
		nearly(t, "solo inclusion_index", *solo.InclusionIndex, (1.0/9)/(1.0/12), 1e-4)
		if solo.TipP50Gwei != "5" {
			t.Errorf("solo tip_p50_gwei = %q, want 5", solo.TipP50Gwei)
		}
		// The solo transaction was seen after slot start.
		if solo.TimeToInclusionMs == nil {
			t.Fatal("expected a wait for the solo sender")
		}
		nearly(t, "solo wait p50", solo.TimeToInclusionMs.P50, -1500, 1)

		// Only reason='eligible' rows are surfaced; the too_recent row and
		// its 5 blobs must not appear anywhere.
		if len(data.Skipped) != 2 {
			t.Fatalf("expected 2 skipped rows, got %+v", data.Skipped)
		}
		if data.Skipped[0].Key != "fancy_rollup" || data.Skipped[0].Txs != 1 || data.Skipped[0].Blobs != 2 ||
			data.Skipped[0].MaxTipGwei != "7" || data.Skipped[0].P50TipGwei != "7" {
			t.Errorf("skipped[0] = %+v", data.Skipped[0])
		}
		if data.Skipped[1].Key != buildAddrSkip || data.Skipped[1].Blobs != 1 || data.Skipped[1].MaxTipGwei != "3" {
			t.Errorf("skipped[1] = %+v", data.Skipped[1])
		}
		// The rows above are rebuilt from candidate detail that is pruned on
		// a retention window, while builder.candidates sums permanent
		// aggregates; skipped_detail_from says where the detail starts. Every
		// seeded candidate sits on block 1000.
		if data.SkippedDetailFrom == nil {
			t.Fatal("expected skipped_detail_from alongside a skipped breakdown")
		}
		if !data.SkippedDetailFrom.Equal(base.Add(-30 * time.Minute)) {
			t.Errorf("skipped_detail_from = %v, want %v", data.SkippedDetailFrom, base.Add(-30*time.Minute))
		}

		if len(data.RecentBlocks) != 3 {
			t.Fatalf("expected 3 recent blocks, got %+v", data.RecentBlocks)
		}
		// Newest first.
		if data.RecentBlocks[0].BlockNumber != 1002 || data.RecentBlocks[2].BlockNumber != 1000 {
			t.Errorf("recent block order = %+v", data.RecentBlocks)
		}
		if data.RecentBlocks[0].CandidateSnapshot || data.RecentBlocks[0].ProposerPaymentWei != nil {
			t.Errorf("block 1002 was indexed from history: %+v", data.RecentBlocks[0])
		}
		newest := data.RecentBlocks[2]
		if newest.BlobCount != 3 || newest.BlobParamsMax == nil || *newest.BlobParamsMax != 6 ||
			newest.ProposerPaymentWei == nil || *newest.ProposerPaymentWei != "10000000000000000" ||
			newest.EligibleSkippedTxs == nil || *newest.EligibleSkippedTxs != 2 ||
			newest.EligibleSkippedMaxTipWei == nil || *newest.EligibleSkippedMaxTipWei != "7000000000" {
			t.Errorf("block 1000 = %+v", newest)
		}
	})

	t.Run("BuilderDetailUnknownKey", func(t *testing.T) {
		w := httptest.NewRecorder()
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("key", "nosuchbuilder")
		req := httptest.NewRequest(http.MethodGet, "/builders/nosuchbuilder", http.NoBody)
		a.GetBuilderByKey(w, req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx)))
		if w.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("BuilderShareChartLongTail", func(t *testing.T) {
		// limit=1 keeps only the top builder by blobs, so beaverbuild and
		// extra:geth must collapse into a single 'other' series.
		w := httptest.NewRecorder()
		a.GetBuilderShareChart(w, httptest.NewRequest(http.MethodGet, "/charts/builder-share?range=24h&granularity=hour&limit=1", http.NoBody))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		data := decodeBuilderShare(t, w)

		if data.Summary.TotalBlocks != 5 || data.Summary.TotalBlobs != 12 {
			t.Fatalf("summary = %+v, want 5 blocks / 12 blobs", data.Summary)
		}
		if len(data.Series) != 2 {
			t.Fatalf("expected titan + other, got %+v", data.Series)
		}
		if data.Series[0].Key != "titan" || data.Series[1].Key != "other" {
			t.Fatalf("series = %+v", data.Series)
		}
		if data.Series[1].Name != "Other" || data.Series[1].Known {
			t.Errorf("the aggregate series = %+v", data.Series[1])
		}
		shares := map[string]BuilderShareChartShare{}
		for _, share := range data.Summary.Shares {
			shares[share.Key] = share
		}
		if got := shares["titan"]; got.Blocks != 3 || got.Blobs != 9 || got.BlobSharePercent != 75 {
			t.Errorf("titan share = %+v", got)
		}
		// beaverbuild (1 block, 2 blobs) + extra:geth (1 block, 1 blob).
		if got := shares["other"]; got.Blocks != 2 || got.Blobs != 3 || got.BlockSharePercent != 40 {
			t.Errorf("other share = %+v", got)
		}
		// Every bucket carries a value for every series, zero-filled.
		for i, point := range data.Points {
			if len(point.Values) != 2 {
				t.Fatalf("point %d values = %+v, want both series", i, point.Values)
			}
		}
	})

	t.Run("BuilderShareChartBlockGranularity", func(t *testing.T) {
		w := httptest.NewRecorder()
		a.GetBuilderShareChart(w, httptest.NewRequest(http.MethodGet, "/charts/builder-share?range=1h&granularity=block", http.NoBody))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		data := decodeBuilderShare(t, w)
		if len(data.Points) != 5 {
			t.Fatalf("expected one point per block, got %d", len(data.Points))
		}
		if data.Points[0].StartBlock == nil || *data.Points[0].StartBlock != 1000 {
			t.Errorf("first point = %+v", data.Points[0])
		}
	})

	t.Run("BlockDetailCarriesBuilderAndCandidates", func(t *testing.T) {
		w := httptest.NewRecorder()
		a.GetBlockByNumber(w, newBlockRequestForChain("1000"))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		data := decodeBlockDetail(t, w)
		if data.Builder == nil || data.Builder.Key != "titan" || data.Builder.Name != "Titan" {
			t.Fatalf("builder = %+v", data.Builder)
		}
		if data.Builder.ExtraDataText != "Titan (titanbuilder.xyz)" {
			t.Errorf("extra_data_text = %q", data.Builder.ExtraDataText)
		}
		if data.Builder.ProposerPaymentEth != "0.01" || data.Builder.PendingCandidateTxs == nil {
			t.Errorf("builder payment/snapshot = %+v", data.Builder)
		}
		// Every classified candidate appears here, including the ones the
		// builder endpoints exclude.
		if len(data.Candidates) != 3 {
			t.Fatalf("expected 3 candidates, got %+v", data.Candidates)
		}
		// Ordered by bid, highest first.
		if data.Candidates[0].TxHash != "0xskip-3" || data.Candidates[0].Reason != "too_recent" {
			t.Errorf("candidate order = %+v", data.Candidates)
		}
		if data.Candidates[1].MaxPriorityFeePerGasGwei != "7" || data.Candidates[1].Nonce == nil {
			t.Errorf("candidate[1] = %+v", data.Candidates[1])
		}
		// user_attribution is NULL on one row and must project as absent.
		if data.Candidates[2].UserAttribution != "" {
			t.Errorf("candidate[2] attribution = %q, want empty", data.Candidates[2].UserAttribution)
		}

		// The blob rows carry the two new columns and the derived wait.
		if len(data.Blobs) != 3 {
			t.Fatalf("expected 3 blobs, got %d", len(data.Blobs))
		}
		var late *BlobResponse
		for i := range data.Blobs {
			if data.Blobs[i].TxHash == "0xtitan-b" {
				late = &data.Blobs[i]
			}
		}
		if late == nil {
			t.Fatal("expected the late-seen blob in the block payload")
		}
		if late.TxIndex == nil || *late.TxIndex != 9 || late.FirstSeenAt == nil {
			t.Errorf("late blob columns = %+v", late)
		}
		if late.TimeToInclusionMs == nil || *late.TimeToInclusionMs != -1500 {
			t.Errorf("late blob time_to_inclusion_ms = %v, want -1500", late.TimeToInclusionMs)
		}
	})

	t.Run("ReplacementsCarryFeeContext", func(t *testing.T) {
		w := httptest.NewRecorder()
		a.GetBlobReplacements(w, httptest.NewRequest(http.MethodGet, "/blob/replacements", http.NoBody))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var resp struct {
			Data []BlobReplacementResponse `json:"data"`
		}
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Data) != 2 {
			t.Fatalf("expected 2 events, got %d", len(resp.Data))
		}
		var bumped, legacy *BlobReplacementResponse
		for i := range resp.Data {
			if resp.Data[i].ReplacedTxHash == "0xold" {
				bumped = &resp.Data[i]
			} else {
				legacy = &resp.Data[i]
			}
		}
		if bumped == nil || legacy == nil {
			t.Fatalf("unexpected events: %+v", resp.Data)
		}
		if bumped.ReplacedMaxPriorityFeePerGasGwei != "1" || bumped.ReplacementMaxPriorityFeePerGasGwei != "3" ||
			bumped.ReplacedMaxFeePerBlobGasGwei != "2" || bumped.ReplacementMaxFeePerBlobGasGwei != "4" ||
			bumped.ReplacedFirstSeenAt == nil {
			t.Errorf("fee-bump event = %+v", bumped)
		}
		if legacy.ReplacedMaxPriorityFeePerGas != nil || legacy.ReplacedFirstSeenAt != nil {
			t.Errorf("pre-migration event should carry no fee context: %+v", legacy)
		}
	})

	t.Run("MempoolProjectionCarriesFirstSeen", func(t *testing.T) {
		// Pending rows project their own timestamp as first_seen_at and have
		// no tx_index; this is the only check that the projection's columns
		// still unify with the confirmed one across their UNION.
		if _, err := sqlxDB.Exec(`
			INSERT INTO mempool_blobs (
				chain_id, tx_hash, blob_index, from_address, user_attribution,
				blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
				timestamp, max_fee_per_blob_gas, blob_gas_used
			) VALUES (1, '0xpending', 0, $1, '', 131072, 10, 2, 1310720, $2, 12, 131072)
		`, buildAddrFancy, base.Add(-time.Minute)); err != nil {
			t.Fatalf("seed mempool blob: %v", err)
		}
		t.Cleanup(func() {
			_, _ = sqlxDB.Exec(`DELETE FROM mempool_blobs WHERE chain_id = 1 AND tx_hash = '0xpending'`)
		})

		w := httptest.NewRecorder()
		a.GetMempoolBlobs(w, httptest.NewRequest(http.MethodGet, "/blob/mempool", http.NoBody))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var resp struct {
			Data []BlobResponse `json:"data"`
		}
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Data) != 1 {
			t.Fatalf("expected 1 pending blob, got %d", len(resp.Data))
		}
		pending := resp.Data[0]
		if pending.FirstSeenAt == nil || !pending.FirstSeenAt.Equal(base.Add(-time.Minute)) {
			t.Errorf("pending first_seen_at = %v", pending.FirstSeenAt)
		}
		if pending.TxIndex != nil || pending.TimeToInclusionMs != nil {
			t.Errorf("pending rows have no position or wait: %+v", pending)
		}
	})
}

// A sender whose stored attribution changed inside the window must still
// collapse to one entity row, the way /users?group=entity does: the address
// is aggregated first and one attribution chosen for it, rather than each
// transaction carrying the name it happened to be stored with. Otherwise the
// builder's user rows, their shares and their entity links disagree with the
// leaderboard the frontend shows beside them.
func TestBuilderDetailGroupsEntitiesLikeUsers(t *testing.T) {
	sqlxDB, _ := resetBuilderSchema(t, "api_builders_grouping")
	base := time.Now().UTC().Truncate(time.Second)
	const renamed = "0xRenamedSender"

	if _, err := sqlxDB.Exec(`
		INSERT INTO block_metrics (chain_id, block_number, block_timestamp, blob_count, blob_params_max) VALUES
			(1, 2000, $1, 1, 6),
			(1, 2001, $2, 1, 6)
	`, base.Add(-20*time.Minute), base.Add(-10*time.Minute)); err != nil {
		t.Fatalf("seed block_metrics: %v", err)
	}
	if _, err := sqlxDB.Exec(`
		INSERT INTO block_builders (
			chain_id, block_number, block_timestamp, fee_recipient, extra_data,
			builder_key, builder_name, tx_count, candidate_snapshot,
			pending_candidate_txs, eligible_skipped_txs, eligible_skipped_blobs
		) VALUES
			(1, 2000, $1, '0xTitanA', '0x67657468', 'titan', 'Titan', 10, TRUE, 1, 1, 1),
			(1, 2001, $2, '0xTitanA', '0x67657468', 'titan', 'Titan', 10, TRUE, 1, 1, 1)
	`, base.Add(-20*time.Minute), base.Add(-10*time.Minute)); err != nil {
		t.Fatalf("seed block_builders: %v", err)
	}

	// One address, two blocks, two different stored attribution names — the
	// registry renamed the rollup mid-window.
	for _, row := range []struct {
		block       int64
		txHash      string
		attribution string
		at          time.Time
	}{
		{2000, "0xrenamed-a", "Old Rollup", base.Add(-20 * time.Minute)},
		{2001, "0xrenamed-b", "New Rollup", base.Add(-10 * time.Minute)},
	} {
		if _, err := sqlxDB.Exec(`
			INSERT INTO blobs (
				chain_id, block_number, blob_index, tx_hash, from_address, user_attribution,
				blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
				timestamp, max_fee_per_blob_gas, blob_gas_used,
				max_priority_fee_per_gas, max_fee_per_gas, priority_fee_per_gas, first_seen_at, tx_index
			) VALUES (1, $1, 0, $2, $3, $4, 131072, 10, 2, 1310720, $5, 12, 131072,
				1000000000, 60000000000, 1000000000, $5, 1)
		`, row.block, row.txHash, renamed, row.attribution, row.at); err != nil {
			t.Fatalf("seed blob %s: %v", row.txHash, err)
		}
	}

	// The same rename in the candidate detail behind `skipped`.
	if _, err := sqlxDB.Exec(`
		INSERT INTO blob_inclusion_candidates (
			chain_id, block_number, block_timestamp, tx_hash, from_address, user_attribution,
			nonce, blob_count, max_priority_fee_per_gas, max_fee_per_gas, max_fee_per_blob_gas,
			first_seen_at, reason
		) VALUES
			(1, 2000, $1, '0xskip-old', $3, 'Old Rollup', 1, 1, 2000000000, 50000000000, 20, $1, 'eligible'),
			(1, 2001, $2, '0xskip-new', $3, 'New Rollup', 2, 1, 4000000000, 50000000000, 20, $2, 'eligible')
	`, base.Add(-20*time.Minute), base.Add(-10*time.Minute), renamed); err != nil {
		t.Fatalf("seed blob_inclusion_candidates: %v", err)
	}

	a := newBuilderTestAPI(sqlxDB)
	w := httptest.NewRecorder()
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("key", "titan")
	req := httptest.NewRequest(http.MethodGet, "/builders/titan?range=24h", http.NoBody)
	a.GetBuilderByKey(w, req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx)))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	data := decodeBuilderDetail(t, w)

	if len(data.Users) != 1 {
		t.Fatalf("the address must collapse into one entity row, got %+v", data.Users)
	}
	user := data.Users[0]
	// MAX() over the window's names picks 'Old Rollup', exactly as
	// /users?group=entity's per-address aggregation does.
	if user.Key != "old_rollup" || !user.IsEntity || user.Name != "Old Rollup" {
		t.Errorf("user row identity = %+v", user)
	}
	if user.Blobs != 2 || user.TxCount != 2 {
		t.Errorf("user volume = %+v, want both transactions on one row", user)
	}
	if user.ShareWithinBuilderPercent != 100 {
		t.Errorf("share_within_builder_percent = %v, want 100", user.ShareWithinBuilderPercent)
	}

	if len(data.Skipped) != 1 {
		t.Fatalf("the skipped breakdown must group by the same rule, got %+v", data.Skipped)
	}
	if data.Skipped[0].Key != "old_rollup" || data.Skipped[0].Txs != 2 {
		t.Errorf("skipped row = %+v", data.Skipped[0])
	}
}

// newBlockRequestForChain routes a /block/{number} request at chain 1, which
// is what the integration fixtures seed.
func newBlockRequestForChain(number string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("number", number)
	req := httptest.NewRequest(http.MethodGet, "/block/"+number+"?network=1", http.NoBody)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// Fixture shape for the plan test. blobsPerBlock is what makes the blobs
// table big enough to matter: the 60d history holds ~280k blob rows, past
// the point where a sequential scan is cheap, so a query that hides the
// window from the planner (a materialized `bounds` CTE, HL-38) really does
// plan as `Seq Scan on blobs` with the window demoted to a join filter. A
// one-blob-per-block fixture is small enough that the planner picks an
// index either way and the assertions below prove nothing.
const (
	planDays           = 60
	planBlocksPerDay   = 120
	planTotalBlocks    = planDays * planBlocksPerDay
	planSecondsPerStep = 86400 / planBlocksPerDay
	planBlobsPerBlock  = 40
)

// seedBuilderPlanFixture lays 60 days of blocks, builders, blobs and
// inclusion candidates down with one INSERT ... SELECT generate_series per
// table: the statement-level triggers on blobs make anything row-by-row far
// too slow at this size.
func seedBuilderPlanFixture(t *testing.T, sqlxDB *sqlx.DB, now time.Time) {
	t.Helper()
	oldest := now.Add(-planDays * 24 * time.Hour)

	if _, err := sqlxDB.Exec(`
		INSERT INTO block_metrics (chain_id, block_number, block_timestamp, blob_count, blob_params_max)
		SELECT 1, g, $1::timestamp + (g * $2 * INTERVAL '1 second'), 3, 6
		FROM generate_series(1, $3) AS g
	`, oldest, planSecondsPerStep, planTotalBlocks); err != nil {
		t.Fatalf("seed block_metrics: %v", err)
	}
	if _, err := sqlxDB.Exec(`
		INSERT INTO block_builders (
			chain_id, block_number, block_timestamp, fee_recipient, extra_data,
			builder_key, builder_name, tx_count, candidate_snapshot
		)
		SELECT 1, g, $1::timestamp + (g * $2 * INTERVAL '1 second'), '0xBuilder' || (g % 5),
			'0x67657468', 'builder-' || (g % 5), 'Builder ' || (g % 5), 100, FALSE
		FROM generate_series(1, $3) AS g
	`, oldest, planSecondsPerStep, planTotalBlocks); err != nil {
		t.Fatalf("seed block_builders: %v", err)
	}
	if _, err := sqlxDB.Exec(`
		INSERT INTO blobs (
			chain_id, block_number, blob_index, tx_hash, from_address, user_attribution,
			blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
			timestamp, max_fee_per_blob_gas, blob_gas_used,
			max_priority_fee_per_gas, max_fee_per_gas, priority_fee_per_gas, first_seen_at, tx_index
		)
		SELECT 1, g, i, '0xtx' || g || '_' || i, '0xsender' || (g % 7), '', 131072, 10, 2, 1310720,
			$1::timestamp + (g * $2 * INTERVAL '1 second'), 12, 131072,
			1000000000, 60000000000, 1000000000,
			$1::timestamp + (g * $2 * INTERVAL '1 second') - INTERVAL '3 seconds', i
		FROM generate_series(1, $3) AS g
		CROSS JOIN generate_series(0, $4 - 1) AS i
		-- Scramble the physical order so ANALYZE records a near-zero
		-- correlation between blobs.timestamp and the heap, which is what
		-- production looks like after in-place backfills have rewritten
		-- rows. With a perfectly correlated heap the planner can still
		-- reach a hidden window through a bitmap scan and the plan
		-- assertions below stop discriminating. md5 keeps it deterministic.
		ORDER BY md5((g * 1000 + i)::text)
	`, oldest, planSecondsPerStep, planTotalBlocks, planBlobsPerBlock); err != nil {
		t.Fatalf("seed blobs: %v", err)
	}
	// blob_inclusion_candidates needs rows too: an empty table is always
	// sequentially scanned, which would make the plan assertions vacuous
	// rather than a real check that the window bound reaches the
	// (chain_id, block_timestamp) index.
	if _, err := sqlxDB.Exec(`
		INSERT INTO blob_inclusion_candidates (
			chain_id, block_number, block_timestamp, tx_hash, from_address, user_attribution,
			nonce, blob_count, max_priority_fee_per_gas, max_fee_per_gas, max_fee_per_blob_gas,
			first_seen_at, reason
		)
		SELECT 1, g, $1::timestamp + (g * $2 * INTERVAL '1 second'), '0xcand' || g,
			'0xsender' || (g % 7), NULL, g, 1, 1000000000, 60000000000, 20,
			$1::timestamp + (g * $2 * INTERVAL '1 second') - INTERVAL '3 seconds', 'eligible'
		FROM generate_series(1, $3) AS g
	`, oldest, planSecondsPerStep, planTotalBlocks); err != nil {
		t.Fatalf("seed blob_inclusion_candidates: %v", err)
	}
	if _, err := sqlxDB.Exec("ANALYZE"); err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

// TestBuilderQueryPlansStayOnRangeIndexes seeds enough history that a 24h
// window is a small fraction of it, then asserts the planner reaches the
// builder rows and their blobs through the (chain_id, timestamp) indexes.
// A sequential scan of blobs here would mean every builder request reads the
// whole table, which is exactly what capping the range at 30d is meant to
// avoid.
func TestBuilderQueryPlansStayOnRangeIndexes(t *testing.T) {
	sqlxDB, _ := resetBuilderSchema(t, "api_builders_explain")
	now := time.Now().UTC().Truncate(time.Hour)
	seedBuilderPlanFixture(t, sqlxDB, now)

	end := now
	start := end.Add(-24 * time.Hour)

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

	// A sequential scan of any of these means the window bound is not being
	// pushed into an index, which is the whole reason range=all is rejected.
	// block_metrics is included because it has no timestamp index at all:
	// only the block-number bounds the builder queries derive keep it off a
	// full scan of the chain's history.
	assertNoSeqScan := func(name, plan string, tables ...string) {
		t.Helper()
		for _, table := range tables {
			if strings.Contains(plan, "Seq Scan on "+table) {
				t.Errorf("%s: plan sequentially scans %s:\n%s", name, table, plan)
			}
		}
	}

	// HL-38: a one-row `bounds` CTE holding the window is referenced from
	// more than one place in every builder query, which makes Postgres
	// materialize it. A materialized CTE is an optimization fence: the
	// timestamps never reach the predicates at plan time, the blobs range
	// is estimated at the default 1/3 * 1/3 of the table instead of the
	// handful of hours it really is, and production planned the whole
	// 86M-row table. The window is spliced in as the bare parameters now,
	// so no plan may contain a bounds CTE at all.
	assertNoBoundsCTE := func(name, plan string) {
		t.Helper()
		for _, node := range []string{"CTE bounds", "CTE Scan on bounds"} {
			if strings.Contains(plan, node) {
				t.Errorf("%s: plan still materializes the bounds CTE (%q):\n%s", name, node, plan)
			}
		}
	}

	assertPlan := func(name, plan string, tables ...string) {
		t.Helper()
		assertNoSeqScan(name, plan, tables...)
		assertNoBoundsCTE(name, plan)
	}

	plan := explain("builders 24h", queryBuilderAggregates, 1, start, end, "")
	assertPlan("builders 24h", plan, "blobs", "block_builders", "block_metrics")

	plan = explain("builder detail 24h", queryBuilderAggregates, 1, start, end, "builder-1")
	assertPlan("builder detail 24h", plan, "blobs", "block_builders", "block_metrics")

	plan = explain("builder users 24h", queryBuilderUsers, 1, start, end, "builder-1")
	assertPlan("builder users 24h", plan, "blobs", "block_builders")

	plan = explain("builder skipped 24h", queryBuilderSkipped, 1, start, end, "builder-1")
	assertPlan("builder skipped 24h", plan, "block_builders", "blob_inclusion_candidates")

	plan = explain("builder recent blocks", queryBuilderRecentBlocks, 1, start, end, "builder-1", builderRecentBlockLimit)
	assertPlan("builder recent blocks", plan, "block_builders", "block_metrics")

	plan = explain("builder-share chart 24h", queryBuilderShareTimeChart, 1, start, end, int64(3600), defaultBuilderSeriesLimit)
	assertPlan("builder-share chart 24h", plan, "block_builders", "block_metrics")

	plan = explain("builder-share chart by block", queryBuilderShareBlockChart, 1, start, end, defaultBuilderSeriesLimit)
	assertPlan("builder-share chart by block", plan, "block_builders", "block_metrics")

	plan = explain("builder skipped detail coverage", queryBuilderSkippedDetailFrom, 1, start, end)
	assertPlan("builder skipped detail coverage", plan, "blob_inclusion_candidates")

	// Sanity: the seeded window really is a small slice of the table, so the
	// plans above were a meaningful test of selectivity.
	var inWindow, total int
	if err := sqlxDB.Get(&inWindow, `SELECT COUNT(*) FROM block_builders WHERE chain_id = 1 AND block_timestamp >= $1 AND block_timestamp < $2`, start, end); err != nil {
		t.Fatalf("count window: %v", err)
	}
	if err := sqlxDB.Get(&total, `SELECT COUNT(*) FROM block_builders WHERE chain_id = 1`); err != nil {
		t.Fatalf("count total: %v", err)
	}
	if inWindow == 0 || total < inWindow*10 {
		t.Fatalf("seeded history is not selective enough: %d of %d rows in the window", inWindow, total)
	}
	// And that blobs is big enough for the access path to be a real choice:
	// below roughly a hundred thousand rows every path costs the same and
	// the assertions above stop discriminating between them.
	var totalBlobs int
	if err := sqlxDB.Get(&totalBlobs, `SELECT COUNT(*) FROM blobs WHERE chain_id = 1`); err != nil {
		t.Fatalf("count blobs: %v", err)
	}
	if totalBlobs < 100000 {
		t.Fatalf("seeded blobs table is too small to make the plan assertions meaningful: %d rows", totalBlobs)
	}
	fmt.Printf("builder EXPLAIN fixture: %d of %d block_builders rows in the 24h window, %d blob rows\n", inWindow, total, totalBlobs)
}
