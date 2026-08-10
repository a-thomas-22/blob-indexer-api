//go:build integration

package api

// End-to-end checks of GET /entities/{key} and the entity-filtered blob
// listings against a real Postgres (TEST_DB_URL). The unit tests drive the
// handlers through a mock DB, which never parses SQL, so this is the only
// check that the entity query constants are valid SQL, that multi-address
// aggregation is exact (counts, big-integer wei sums, max timestamps), that
// range filtering follows the /users window semantics, and that the entity
// keys served here agree with the /charts/attribution-usage summary shares.

import (
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db"
	"github.com/a-thomas-22/blob-indexer-api/internal/testdb"
)

// Stored-form sender addresses: the indexer writes go-ethereum's EIP-55
// checksummed rendering, which the from= filter also normalizes input to.
var (
	entAddrA     = common.HexToAddress("0xaaaa000000000000000000000000000000000001").Hex() // Fancy Rollup, registry + activity
	entAddrB     = common.HexToAddress("0xbbbb000000000000000000000000000000000002").Hex() // Fancy Rollup, registry + activity
	entAddrCReg  = common.HexToAddress("0xcccc000000000000000000000000000000000003").Hex() // Fancy Rollup, registry only (no activity)
	entAddrGhost = common.HexToAddress("0xdddd000000000000000000000000000000000004").Hex() // Fancy Rollup in indexed history, not in registry
	entAddrOther = common.HexToAddress("0xeeee000000000000000000000000000000000005").Hex() // Other Rollup
	entAddrNone  = common.HexToAddress("0xffff000000000000000000000000000000000006").Hex() // unattributed
)

type entitySeedBlob struct {
	addr        string
	attribution string
	age         time.Duration
	cost        string
}

func seedEntityFixtures(t *testing.T, sqlxDB *sqlx.DB, base time.Time) []entitySeedBlob {
	t.Helper()

	blobs := []entitySeedBlob{
		{entAddrA, "Fancy Rollup", 30 * time.Minute, "111111111111111111111"},
		{entAddrA, "Fancy Rollup", 3 * time.Hour, "222222222222222222222"},
		{entAddrA, "Fancy Rollup", 10 * 24 * time.Hour, "3"},
		{entAddrB, "Fancy Rollup", 2 * time.Hour, "400000000000000000004"},
		{entAddrGhost, "Fancy Rollup", 5 * time.Hour, "9"},
		{entAddrOther, "Other Rollup", 50 * time.Minute, "5"},
		{entAddrNone, "", 40 * time.Minute, "7"},
	}
	for i, b := range blobs {
		if _, err := sqlxDB.Exec(`
			INSERT INTO blobs (
				chain_id, block_number, blob_index, tx_hash, from_address, user_attribution,
				blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
				timestamp, max_fee_per_blob_gas, blob_gas_used
			) VALUES (1, $1, 0, $2, $3, $4, 131072, 1000, 5, $5, $6, 2000, 131072)
		`, 1000+i, "0xtx-entity-"+strconv.Itoa(i), b.addr, b.attribution, b.cost, base.Add(-b.age)); err != nil {
			t.Fatalf("seed blob %d: %v", i, err)
		}
	}

	// Registry rows use the lowercase form the attribution service stores.
	for addr, name := range map[string]string{
		entAddrA:     "Fancy Rollup",
		entAddrB:     "Fancy Rollup",
		entAddrCReg:  "Fancy Rollup",
		entAddrOther: "Other Rollup",
	} {
		if _, err := sqlxDB.Exec(`
			INSERT INTO blob_users (chain_id, address, name, description, category, first_seen, last_seen)
			VALUES (1, LOWER($1), $2, '', 'rollup', $3, $3)
		`, addr, name, base); err != nil {
			t.Fatalf("seed blob_users %s: %v", addr, err)
		}
	}

	for i, pending := range []struct {
		addr string
		cost string
	}{
		{entAddrB, "11"},
		{entAddrNone, "13"},
	} {
		if _, err := sqlxDB.Exec(`
			INSERT INTO mempool_blobs (
				chain_id, tx_hash, blob_index, from_address, user_attribution,
				blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_wei,
				timestamp, max_fee_per_blob_gas, blob_gas_used
			) VALUES (1, $1, 0, $2, '', 131072, 50, 10, $3, $4, 60, 131072)
		`, "0xtx-entity-pending-"+strconv.Itoa(i), pending.addr, pending.cost, base.Add(-time.Minute)); err != nil {
			t.Fatalf("seed mempool blob %d: %v", i, err)
		}
	}

	return blobs
}

func sumEntityCosts(t *testing.T, costs ...string) string {
	t.Helper()
	total := new(big.Int)
	for _, c := range costs {
		v, ok := new(big.Int).SetString(c, 10)
		if !ok {
			t.Fatalf("bad cost %q", c)
		}
		total.Add(total, v)
	}
	return total.String()
}

// expectedSharePercent mirrors the SQL ROUND((num/den)*100, 6)::float8.
func expectedSharePercent(t *testing.T, num, den string) float64 {
	t.Helper()
	n, ok := new(big.Rat).SetString(num)
	if !ok {
		t.Fatalf("bad numerator %q", num)
	}
	d, ok := new(big.Rat).SetString(den)
	if !ok || d.Sign() == 0 {
		t.Fatalf("bad denominator %q", den)
	}
	share := new(big.Rat).Quo(n, d)
	share.Mul(share, big.NewRat(100, 1))
	rounded, err := strconv.ParseFloat(share.FloatString(6), 64)
	if err != nil {
		t.Fatalf("parse share: %v", err)
	}
	return rounded
}

func getEntity(t *testing.T, a *API, key, query string) (int, EntityResponse) {
	t.Helper()
	w := httptest.NewRecorder()
	a.GetEntityByKey(w, entityRequest(t, "/"+query, key))
	var resp struct {
		Success bool           `json:"success"`
		Data    EntityResponse `json:"data"`
	}
	if w.Code == http.StatusOK {
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode entity response: %v", err)
		}
	}
	return w.Code, resp.Data
}

func listBlobs(t *testing.T, a *API, handler func(http.ResponseWriter, *http.Request), query string) (int, []BlobResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/"+query, http.NoBody)
	w := httptest.NewRecorder()
	handler(w, req)
	var resp struct {
		Success bool           `json:"success"`
		Data    []BlobResponse `json:"data"`
	}
	if w.Code == http.StatusOK {
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode blob list response: %v", err)
		}
	}
	return w.Code, resp.Data
}

func TestEntityEndpointsAgainstRealPostgres(t *testing.T) {
	// This test resets its schema, so it runs on this package's dedicated
	// database rather than TEST_DB_URL itself.
	url := testdb.URL(t, "api_entities")
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

	base := time.Now().UTC().Truncate(time.Second)
	seeded := seedEntityFixtures(t, sqlxDB, base)

	a := newTestAPIWithDB(&db.DB{DB: sqlxDB})
	a.networks = map[int]config.NetworkConfig{
		1: {Name: "mainnet", ChainID: 1, Enabled: true},
	}

	fancyAllCost := sumEntityCosts(t, seeded[0].cost, seeded[1].cost, seeded[2].cost, seeded[3].cost, seeded[4].cost)
	confirmedTotalCost := sumEntityCosts(t,
		seeded[0].cost, seeded[1].cost, seeded[2].cost, seeded[3].cost, seeded[4].cost, seeded[5].cost, seeded[6].cost)
	allSpendDenominator := sumEntityCosts(t, confirmedTotalCost, "11", "13")

	t.Run("AllHistoryAggregation", func(t *testing.T) {
		code, data := getEntity(t, a, "fancy_rollup", "")
		if code != http.StatusOK {
			t.Fatalf("expected 200, got %d", code)
		}
		if data.Key != "fancy_rollup" || data.Name != "Fancy Rollup" || data.Category != "rollup" || data.Range != "all" {
			t.Fatalf("unexpected identity: %+v", data)
		}
		if data.BlobCount != 5 {
			t.Errorf("blob_count = %d, want 5", data.BlobCount)
		}
		// Exact big-integer sum: any float64 detour would corrupt the low digits.
		if data.TotalCostWei != fancyAllCost {
			t.Errorf("total_cost_wei = %s, want %s", data.TotalCostWei, fancyAllCost)
		}
		if data.LastTimestamp == nil || !data.LastTimestamp.Equal(base.Add(-30*time.Minute)) {
			t.Errorf("last_timestamp = %v, want the newest blob at %v", data.LastTimestamp, base.Add(-30*time.Minute))
		}
		// 5 entity blobs of 7 confirmed + 2 pending, matching /users' share denominators.
		if want := expectedSharePercent(t, "5", "9"); data.BlobSharePercent != want {
			t.Errorf("blob_share_percent = %v, want %v", data.BlobSharePercent, want)
		}
		if want := expectedSharePercent(t, fancyAllCost, allSpendDenominator); data.SpendSharePercent != want {
			t.Errorf("spend_share_percent = %v, want %v", data.SpendSharePercent, want)
		}

		if len(data.Addresses) != 4 {
			t.Fatalf("expected 4 addresses, got %+v", data.Addresses)
		}
		// Busiest first: addrA (3 blobs), then addrB (1 blob, big spend), then
		// addrGhost (1 blob, tiny spend), then the zero-activity registry row.
		wantOrder := []string{entAddrA, entAddrB, entAddrGhost, strings.ToLower(entAddrCReg)}
		for i, want := range wantOrder {
			if data.Addresses[i].Address != want {
				t.Fatalf("address order[%d] = %s, want %s (full: %+v)", i, data.Addresses[i].Address, want, data.Addresses)
			}
		}
		addrA := data.Addresses[0]
		wantACost := sumEntityCosts(t, seeded[0].cost, seeded[1].cost, seeded[2].cost)
		if addrA.BlobCount != 3 || addrA.TotalCostWei != wantACost || !addrA.InRegistry {
			t.Errorf("unexpected addrA row: %+v", addrA)
		}
		if addrA.LastTimestamp == nil || !addrA.LastTimestamp.Equal(base.Add(-30*time.Minute)) {
			t.Errorf("addrA last_timestamp = %v", addrA.LastTimestamp)
		}
		if addrA.TotalCostEth == "" {
			t.Error("expected a non-empty total_cost_eth")
		}
		if ghost := data.Addresses[2]; ghost.InRegistry {
			t.Errorf("addrGhost should be flagged as not in the registry: %+v", ghost)
		}
		if reg := data.Addresses[3]; reg.BlobCount != 0 || reg.TotalCostWei != "0" || reg.LastTimestamp != nil || !reg.InRegistry {
			t.Errorf("unexpected zero-activity registry row: %+v", reg)
		}
	})

	t.Run("RangeFiltering", func(t *testing.T) {
		code, day := getEntity(t, a, "fancy_rollup", "?range=24h")
		if code != http.StatusOK {
			t.Fatalf("expected 200, got %d", code)
		}
		if day.Range != "24h" || day.BlobCount != 4 {
			t.Fatalf("24h window: got range %s count %d, want 4 blobs (the 10d-old one excluded): %+v", day.Range, day.BlobCount, day)
		}
		want24hCost := sumEntityCosts(t, seeded[0].cost, seeded[1].cost, seeded[3].cost, seeded[4].cost)
		if day.TotalCostWei != want24hCost {
			t.Errorf("24h total_cost_wei = %s, want %s", day.TotalCostWei, want24hCost)
		}
		if len(day.Addresses) != 4 {
			t.Fatalf("24h window should still list every address: %+v", day.Addresses)
		}
		if day.Addresses[0].Address != entAddrA || day.Addresses[0].BlobCount != 2 {
			t.Errorf("24h busiest address = %+v, want addrA with 2 blobs", day.Addresses[0])
		}

		code, hour := getEntity(t, a, "fancy_rollup", "?range=1h")
		if code != http.StatusOK {
			t.Fatalf("expected 200, got %d", code)
		}
		if hour.BlobCount != 1 || hour.TotalCostWei != seeded[0].cost {
			t.Fatalf("1h window: got count %d cost %s, want only the 30m-old blob", hour.BlobCount, hour.TotalCostWei)
		}
		// Windowed rows keep the exact all-history last-seen, mirroring /users.
		var addrB *EntityAddressResponse
		for i := range hour.Addresses {
			if strings.EqualFold(hour.Addresses[i].Address, entAddrB) {
				addrB = &hour.Addresses[i]
			}
		}
		if addrB == nil || addrB.BlobCount != 0 {
			t.Fatalf("addrB should appear with no in-window activity: %+v", hour.Addresses)
		}
		if addrB.LastTimestamp == nil || !addrB.LastTimestamp.Equal(base.Add(-2*time.Hour)) {
			t.Errorf("addrB last_timestamp = %v, want its all-history last-seen", addrB.LastTimestamp)
		}
	})

	t.Run("NameResolution", func(t *testing.T) {
		for _, key := range []string{"Fancy%20Rollup", "FANCY_ROLLUP", "fancy-rollup"} {
			code, data := getEntity(t, a, key, "")
			if code != http.StatusOK || data.Key != "fancy_rollup" {
				t.Errorf("key %q: got %d / %q, want 200 with canonical key", key, code, data.Key)
			}
		}
	})

	t.Run("UnknownKey404", func(t *testing.T) {
		for _, key := range []string{"no_such_entity", "unknown", "other"} {
			if code, _ := getEntity(t, a, key, ""); code != http.StatusNotFound {
				t.Errorf("key %q: expected 404, got %d", key, code)
			}
		}
	})

	t.Run("EntityFilteredLatest", func(t *testing.T) {
		code, blobs := listBlobs(t, a, a.GetLatestBlobs, "?entity=fancy_rollup&limit=10")
		if code != http.StatusOK {
			t.Fatalf("expected 200, got %d", code)
		}
		// The union across the entity's addresses, newest first.
		wantFrom := []string{entAddrA, entAddrB, entAddrA, entAddrGhost, entAddrA}
		if len(blobs) != len(wantFrom) {
			t.Fatalf("expected %d blobs, got %+v", len(wantFrom), blobs)
		}
		for i, want := range wantFrom {
			if blobs[i].FromAddress != want {
				t.Errorf("blob[%d].from = %s, want %s", i, blobs[i].FromAddress, want)
			}
		}
		for i := 1; i < len(blobs); i++ {
			if blobs[i].Timestamp.After(blobs[i-1].Timestamp) {
				t.Errorf("blobs not in timestamp-descending order at %d", i)
			}
		}

		// Same limit/offset semantics as from=.
		code, page := listBlobs(t, a, a.GetLatestBlobs, "?entity=fancy_rollup&limit=2&offset=1")
		if code != http.StatusOK || len(page) != 2 {
			t.Fatalf("pagination: got %d with %d blobs", code, len(page))
		}
		if page[0].TxHash != blobs[1].TxHash || page[1].TxHash != blobs[2].TxHash {
			t.Errorf("offset pagination mismatch: %+v vs full list %+v", page, blobs[:3])
		}

		if code, _ := listBlobs(t, a, a.GetLatestBlobs, "?entity=no_such_entity"); code != http.StatusNotFound {
			t.Errorf("unknown entity: expected 404, got %d", code)
		}

		// from= keeps working unchanged alongside the new filter.
		code, fromBlobs := listBlobs(t, a, a.GetLatestBlobs, "?from="+entAddrA+"&limit=10")
		if code != http.StatusOK || len(fromBlobs) != 3 {
			t.Fatalf("from= filter: got %d with %d blobs, want 3", code, len(fromBlobs))
		}
	})

	t.Run("EntityFilteredMempool", func(t *testing.T) {
		code, pending := listBlobs(t, a, a.GetMempoolBlobs, "?entity=fancy_rollup&limit=10")
		if code != http.StatusOK {
			t.Fatalf("expected 200, got %d", code)
		}
		if len(pending) != 1 || pending[0].FromAddress != entAddrB || pending[0].Confirmed {
			t.Fatalf("expected exactly addrB's pending blob, got %+v", pending)
		}
	})

	t.Run("ChartKeyAgreement", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/?range=24h", http.NoBody)
		w := httptest.NewRecorder()
		a.GetAttributionUsageChart(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("chart: expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var resp struct {
			Data AttributionUsageChartResponse `json:"data"`
		}
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode chart response: %v", err)
		}
		shares := map[string]AttributionUsageShare{}
		for _, share := range resp.Data.Summary.Shares {
			shares[share.Key] = share
		}
		fancy, ok := shares["fancy_rollup"]
		if !ok {
			t.Fatalf("chart summary shares missing fancy_rollup: %+v", shares)
		}
		if _, ok := shares["other_rollup"]; !ok {
			t.Fatalf("chart summary shares missing other_rollup: %+v", shares)
		}

		// The chart's 24h share and /entities' 24h aggregates describe the same
		// blobs, so counts and exact wei sums must agree key-for-key.
		_, day := getEntity(t, a, "fancy_rollup", "?range=24h")
		if int64(fancy.BlobCount) != day.BlobCount {
			t.Errorf("chart blob_count %d != entity blob_count %d", fancy.BlobCount, day.BlobCount)
		}
		if fancy.TotalCostWei != day.TotalCostWei {
			t.Errorf("chart total_cost_wei %s != entity total_cost_wei %s", fancy.TotalCostWei, day.TotalCostWei)
		}
		if fancy.Name != day.Name || fancy.Category != day.Category {
			t.Errorf("chart identity (%s, %s) != entity identity (%s, %s)", fancy.Name, fancy.Category, day.Name, day.Category)
		}
	})
}
