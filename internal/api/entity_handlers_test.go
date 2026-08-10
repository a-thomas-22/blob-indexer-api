package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/lib/pq"

	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
)

func entityRequest(t *testing.T, target, key string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, http.NoBody)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("key", key)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func sampleEntityRows() []entityDetailRow {
	first := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	second := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	return []entityDetailRow{
		{
			Address:             "0xAaAa000000000000000000000000000000000001",
			DisplayName:         "Fancy Rollup",
			Category:            "rollup",
			BlobCount:           3,
			TotalCostWei:        "300000000000000000000",
			LastTimestamp:       sql.NullTime{Time: first, Valid: true},
			InRegistry:          true,
			EntityName:          "Fancy Rollup",
			EntityCategory:      "rollup",
			EntityBlobCount:     4,
			EntityTotalCostWei:  "400000000000000000001",
			EntityLastTimestamp: sql.NullTime{Time: first, Valid: true},
			BlobSharePercent:    50,
			SpendSharePercent:   25.5,
		},
		{
			Address:             "0xBbBb000000000000000000000000000000000002",
			DisplayName:         "Fancy Rollup",
			Category:            "rollup",
			BlobCount:           1,
			TotalCostWei:        "100000000000000000001",
			LastTimestamp:       sql.NullTime{Time: second, Valid: true},
			InRegistry:          true,
			EntityName:          "Fancy Rollup",
			EntityCategory:      "rollup",
			EntityBlobCount:     4,
			EntityTotalCostWei:  "400000000000000000001",
			EntityLastTimestamp: sql.NullTime{Time: first, Valid: true},
			BlobSharePercent:    50,
			SpendSharePercent:   25.5,
		},
		{
			Address:             "0xcccc000000000000000000000000000000000003",
			DisplayName:         "Fancy Rollup",
			Category:            "rollup",
			BlobCount:           0,
			TotalCostWei:        "0",
			InRegistry:          true,
			EntityName:          "Fancy Rollup",
			EntityCategory:      "rollup",
			EntityBlobCount:     4,
			EntityTotalCostWei:  "400000000000000000001",
			EntityLastTimestamp: sql.NullTime{Time: first, Valid: true},
			BlobSharePercent:    50,
			SpendSharePercent:   25.5,
		},
	}
}

func TestSlugifyEntityKey(t *testing.T) {
	cases := map[string]string{
		"Scroll":        "scroll",
		"scroll":        "scroll",
		"Arbitrum One":  "arbitrum_one",
		"arbitrum_one":  "arbitrum_one",
		" Fancy-Rollup": "fancy_rollup",
		"Base (old)":    "base_old",
		"0x!!":          "0x",
		"__x__":         "x",
		"!!!":           "",
		"":              "",
	}
	for input, want := range cases {
		if got := slugifyEntityKey(input); got != want {
			t.Errorf("slugifyEntityKey(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestGetEntityByKey_Success(t *testing.T) {
	var gotQuery string
	var gotArgs []interface{}
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			gotQuery = query
			gotArgs = args
			setSliceResult(dest, sampleEntityRows())
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := entityRequest(t, "/", "fancy_rollup")
	w := httptest.NewRecorder()
	a.GetEntityByKey(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotQuery != queryEntityDetailAll {
		t.Errorf("expected the all-history query without a range param, got %q", gotQuery)
	}
	wantArgs := []interface{}{42, "fancy_rollup", "all"}
	if len(gotArgs) != len(wantArgs) || gotArgs[0] != wantArgs[0] || gotArgs[1] != wantArgs[1] || gotArgs[2] != wantArgs[2] {
		t.Errorf("query args = %v, want %v", gotArgs, wantArgs)
	}

	var resp struct {
		Success bool           `json:"success"`
		Data    EntityResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	data := resp.Data
	if data.Key != "fancy_rollup" || data.Name != "Fancy Rollup" || data.Category != "rollup" {
		t.Errorf("unexpected entity identity: %+v", data)
	}
	if data.ChainID != 42 || data.NetworkName != "testnet" || data.Range != "all" {
		t.Errorf("unexpected envelope fields: %+v", data)
	}
	if data.BlobCount != 4 || data.TotalCostWei != "400000000000000000001" {
		t.Errorf("unexpected entity aggregates: %+v", data)
	}
	if data.TotalCostEth != "400.000000000000000001" {
		t.Errorf("total_cost_eth = %q, want exact 18-decimal rendering", data.TotalCostEth)
	}
	if data.LastTimestamp == nil || !data.LastTimestamp.Equal(time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("unexpected entity last_timestamp: %v", data.LastTimestamp)
	}
	if data.BlobSharePercent != 50 || data.SpendSharePercent != 25.5 {
		t.Errorf("unexpected share percents: %+v", data)
	}
	if len(data.Addresses) != 3 {
		t.Fatalf("expected 3 addresses, got %+v", data.Addresses)
	}
	if data.Addresses[0].Address != "0xAaAa000000000000000000000000000000000001" ||
		data.Addresses[1].Address != "0xBbBb000000000000000000000000000000000002" {
		t.Errorf("expected busiest-first address order, got %+v", data.Addresses)
	}
	if data.Addresses[1].TotalCostEth != "100.000000000000000001" {
		t.Errorf("address total_cost_eth = %q", data.Addresses[1].TotalCostEth)
	}
	zero := data.Addresses[2]
	if zero.BlobCount != 0 || zero.TotalCostWei != "0" || zero.LastTimestamp != nil || !zero.InRegistry {
		t.Errorf("unexpected zero-activity registry address row: %+v", zero)
	}
	wantCache := fmt.Sprintf("public, max-age=%d, s-maxage=%d",
		int(aggregateCacheTTL.Seconds()), int(aggregateEdgeTTL.Seconds()))
	if got := w.Header().Get("Cache-Control"); got != wantCache {
		t.Errorf("Cache-Control = %q, want %q", got, wantCache)
	}
}

func TestGetEntityByKey_WindowedQueryAndNameFallback(t *testing.T) {
	var gotQuery string
	var gotArgs []interface{}
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			gotQuery = query
			gotArgs = args
			setSliceResult(dest, sampleEntityRows())
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	// A URL-escaped display name resolves to the canonical key.
	req := entityRequest(t, "/?range=24h", "Fancy%20Rollup")
	w := httptest.NewRecorder()
	a.GetEntityByKey(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotQuery != queryEntityDetailWindowed {
		t.Errorf("expected the windowed query for range=24h")
	}
	if len(gotArgs) != 3 || gotArgs[1] != "fancy_rollup" || gotArgs[2] != "24h" {
		t.Errorf("query args = %v, want canonical key and 24h window", gotArgs)
	}
	var resp struct {
		Data EntityResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Data.Key != "fancy_rollup" || resp.Data.Range != "24h" {
		t.Errorf("unexpected key/range echo: %+v", resp.Data)
	}
}

func TestGetEntityByKey_WindowAliasParam(t *testing.T) {
	var gotArgs []interface{}
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			gotArgs = args
			setSliceResult(dest, sampleEntityRows())
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := entityRequest(t, "/?window=7d", "fancy_rollup")
	w := httptest.NewRecorder()
	a.GetEntityByKey(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if len(gotArgs) != 3 || gotArgs[2] != "7d" {
		t.Errorf("query args = %v, want the 7d window via the legacy alias", gotArgs)
	}
}

func TestGetEntityByKey_NotFound(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return nil // no rows
		},
	}
	a := newTestAPIWithDB(db)
	req := entityRequest(t, "/", "no_such_entity")
	w := httptest.NewRecorder()
	a.GetEntityByKey(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetEntityByKey_ReservedKeys(t *testing.T) {
	for _, key := range []string{"unknown", "other", "Unknown", "OTHER"} {
		a := newTestAPIWithDB(&mockDB{
			selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
				t.Fatalf("reserved key %q must not reach the database", key)
				return nil
			},
		})
		w := httptest.NewRecorder()
		a.GetEntityByKey(w, entityRequest(t, "/", key))
		if w.Code != http.StatusNotFound {
			t.Errorf("key %q: expected 404, got %d", key, w.Code)
		}
	}
}

func TestGetEntityByKey_InvalidKey(t *testing.T) {
	a := newTestAPI()
	w := httptest.NewRecorder()
	a.GetEntityByKey(w, entityRequest(t, "/", "%21%21"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a key with no alphanumerics, got %d", w.Code)
	}
}

func TestGetEntityByKey_InvalidRange(t *testing.T) {
	a := newTestAPI()
	w := httptest.NewRecorder()
	a.GetEntityByKey(w, entityRequest(t, "/?range=2h", "fancy_rollup"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetEntityByKey_ConflictingRangeAndWindow(t *testing.T) {
	a := newTestAPI()
	w := httptest.NewRecorder()
	a.GetEntityByKey(w, entityRequest(t, "/?range=24h&window=7d", "fancy_rollup"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetEntityByKey_BadNetwork(t *testing.T) {
	a := newTestAPI()
	w := httptest.NewRecorder()
	a.GetEntityByKey(w, entityRequest(t, "/?network=unknown", "fancy_rollup"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetEntityByKey_DBError(t *testing.T) {
	a := newTestAPIWithDB(&mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return fmt.Errorf("db error")
		},
	})
	w := httptest.NewRecorder()
	a.GetEntityByKey(w, entityRequest(t, "/", "fancy_rollup"))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestGetEntityByKey_DBTimeout(t *testing.T) {
	a := newTestAPIWithDB(&mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return context.DeadlineExceeded
		},
	})
	w := httptest.NewRecorder()
	a.GetEntityByKey(w, entityRequest(t, "/", "fancy_rollup"))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

func TestGetEntityByKey_CacheHit(t *testing.T) {
	queries := 0
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			queries++
			setSliceResult(dest, sampleEntityRows())
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		a.GetEntityByKey(w, entityRequest(t, "/", "fancy_rollup"))
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i, w.Code)
		}
	}
	if queries != 1 {
		t.Errorf("expected the second request to be served from cache, got %d queries", queries)
	}
	// A different range is a different cache entry.
	w := httptest.NewRecorder()
	a.GetEntityByKey(w, entityRequest(t, "/?range=1h", "fancy_rollup"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if queries != 2 {
		t.Errorf("expected a distinct cache entry per range, got %d queries", queries)
	}
}

func TestGetEntityByKey_NotFoundNotCached(t *testing.T) {
	queries := 0
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			queries++
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		a.GetEntityByKey(w, entityRequest(t, "/", "no_such_entity"))
		if w.Code != http.StatusNotFound {
			t.Fatalf("request %d: expected 404, got %d", i, w.Code)
		}
	}
	if queries != 2 {
		t.Errorf("negative results must not be cached, got %d queries", queries)
	}
}

// entityFilterDB serves the two-step entity listing flow: the address
// resolution query, then the by-addresses listing query.
func entityFilterDB(t *testing.T, addresses []string, listQueries *[]string, listArgs *[][]interface{}) *mockDB {
	t.Helper()
	return &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			switch d := dest.(type) {
			case *[]string:
				if query != queryEntityAddresses {
					t.Errorf("address resolution used query %q", query)
				}
				*d = addresses
				return nil
			case *[]models.Blob:
				*listQueries = append(*listQueries, query)
				*listArgs = append(*listArgs, args)
				*d = []models.Blob{{
					ChainID:           42,
					BlockNumber:       100,
					TxHash:            validTestTxHash,
					FromAddress:       addresses[0],
					BaseFeePerBlobGas: "1000",
					TipPerBlobGas:     "100",
					TotalCostWei:      "131072000",
					Timestamp:         time.Now(),
					Confirmed:         true,
				}}
				return nil
			default:
				t.Errorf("unexpected select dest %T", dest)
				return nil
			}
		},
	}
}

func TestGetLatestBlobs_EntityFilter(t *testing.T) {
	addresses := []string{
		"0xAaAa000000000000000000000000000000000001",
		"0xBbBb000000000000000000000000000000000002",
	}
	var listQueries []string
	var listArgs [][]interface{}
	a := newTestAPIWithDB(entityFilterDB(t, addresses, &listQueries, &listArgs))

	// A display-name entity value canonicalizes to the same key.
	req := httptest.NewRequest(http.MethodGet, "/?entity=Fancy+Rollup&limit=5&offset=3", http.NoBody)
	w := httptest.NewRecorder()
	a.GetLatestBlobs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(listQueries) != 1 || listQueries[0] != queryLatestBlobsByAddresses {
		t.Fatalf("expected one by-addresses listing query, got %v", listQueries)
	}
	args := listArgs[0]
	if len(args) != 4 || args[0] != 42 || args[2] != 5 || args[3] != 3 {
		t.Errorf("listing args = %v, want chain 42, limit 5, offset 3", args)
	}
	arr, ok := args[1].(*pq.StringArray)
	if !ok || len(*arr) != 2 || (*arr)[0] != addresses[0] || (*arr)[1] != addresses[1] {
		t.Errorf("expected the resolved address array, got %#v", args[1])
	}

	var resp struct {
		Success bool           `json:"success"`
		Data    []BlobResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Success || len(resp.Data) != 1 {
		t.Errorf("unexpected response: %+v", resp)
	}
}

func TestGetMempoolBlobs_EntityFilter(t *testing.T) {
	addresses := []string{"0xAaAa000000000000000000000000000000000001"}
	var listQueries []string
	var listArgs [][]interface{}
	a := newTestAPIWithDB(entityFilterDB(t, addresses, &listQueries, &listArgs))

	req := httptest.NewRequest(http.MethodGet, "/?entity=fancy_rollup", http.NoBody)
	w := httptest.NewRecorder()
	a.GetMempoolBlobs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(listQueries) != 1 || listQueries[0] != queryMempoolBlobsByAddresses {
		t.Fatalf("expected the mempool by-addresses query, got %v", listQueries)
	}
}

func TestGetLatestBlobs_EntityAndFromConflict(t *testing.T) {
	a := newTestAPIWithDB(&mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			t.Fatal("conflicting filters must not reach the database")
			return nil
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/?entity=fancy_rollup&from="+validTestAddress, http.NoBody)
	w := httptest.NewRecorder()
	a.GetLatestBlobs(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetLatestBlobs_EntityUnknown(t *testing.T) {
	a := newTestAPIWithDB(&mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return nil // resolution finds no addresses
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/?entity=no_such_entity", http.NoBody)
	w := httptest.NewRecorder()
	a.GetLatestBlobs(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetLatestBlobs_EntityInvalid(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?entity=%21%21", http.NoBody)
	w := httptest.NewRecorder()
	a.GetLatestBlobs(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetMempoolBlobs_EntityReserved(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?entity=unknown", http.NoBody)
	w := httptest.NewRecorder()
	a.GetMempoolBlobs(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestResolveEntityAddresses_CachesNonEmpty(t *testing.T) {
	queries := 0
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			switch d := dest.(type) {
			case *[]string:
				queries++
				*d = []string{"0xAaAa000000000000000000000000000000000001"}
			case *[]models.Blob:
				*d = nil
			}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/?entity=fancy_rollup", http.NoBody)
		w := httptest.NewRecorder()
		a.GetLatestBlobs(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i, w.Code)
		}
	}
	if queries != 1 {
		t.Errorf("expected address resolution to be cached, got %d resolutions", queries)
	}
}

func TestResolveEntityAddresses_CachesNegative(t *testing.T) {
	queries := 0
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if _, ok := dest.(*[]string); ok {
				queries++
			}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/?entity=no_such_entity", http.NoBody)
		w := httptest.NewRecorder()
		a.GetLatestBlobs(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("request %d: expected 404, got %d", i, w.Code)
		}
	}
	// The listings take only the standard rate limit, so empty resolutions
	// must be cached too — repeated unknown keys cannot re-run the scan.
	if queries != 1 {
		t.Errorf("expected the empty resolution to be cached, got %d resolutions", queries)
	}
}

func TestEntityQueriesShareKeyDerivation(t *testing.T) {
	// The entity queries and the attribution chart must derive keys with the
	// byte-identical SQL expression, or the identifiers stop joining across
	// endpoints.
	for name, tc := range map[string]struct {
		query string
		expr  string
	}{
		"detail_all":      {queryEntityDetailAll, entityKeySQL("display_name")},
		"detail_windowed": {queryEntityDetailWindowed, entityKeySQL("a.display_name")},
		"addresses":       {queryEntityAddresses, entityKeySQL("display_name")},
	} {
		if !strings.Contains(tc.query, tc.expr) {
			t.Errorf("%s query does not embed the shared entity key expression", name)
		}
	}
	if !strings.Contains(queryAttributionUsageTimeChart, entityKeySQL("src.raw_name")) {
		t.Error("attribution chart no longer embeds the shared entity key expression")
	}
}
