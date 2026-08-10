package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/go-chi/chi/v5"
	"github.com/lib/pq"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	_ "github.com/a-thomas-22/blob-indexer-api/internal/testutil"
)

func TestGetTopBlobUsers_Success(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			users := dest.(*[]models.BlobUserStats)
			*users = []models.BlobUserStats{
				{Address: "0xabc", Name: "Alice", BlobCount: 10, TotalCostWei: "1.5", LastTimestamp: time.Now()},
			}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?limit=5", http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetTopBlobUsers_SortSpendWindow(t *testing.T) {
	var gotQuery string
	var gotArgs []interface{}
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			gotQuery = query
			gotArgs = append([]interface{}{}, args...)
			users := dest.(*[]models.BlobUserStats)
			*users = []models.BlobUserStats{
				{
					Address:           "0xabc",
					Name:              "Alice",
					Category:          "rollup",
					BlobCount:         10,
					TotalCostWei:      "47185487462400",
					LastTimestamp:     time.Now(),
					BlobSharePercent:  62.5,
					SpendSharePercent: 75,
				},
			}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?network=42&limit=5&offset=2&sort=spend&window=24h", http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if gotQuery != queryTopBlobUsersWithOptions {
		t.Fatal("expected options query to be used")
	}
	wantArgs := []interface{}{42, 5, 2, apiWindow24h, "spend"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("expected args %v, got %v", wantArgs, gotArgs)
	}

	var resp struct {
		Success bool           `json:"success"`
		Data    []UserResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Success || len(resp.Data) != 1 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.Data[0].Category != "rollup" || resp.Data[0].BlobSharePercent != 62.5 || resp.Data[0].SpendSharePercent != 75 {
		t.Fatalf("unexpected user share fields: %+v", resp.Data[0])
	}
	if resp.Data[0].TotalCostWei != "47185487462400" {
		t.Fatalf("unexpected user cost fields: %+v", resp.Data[0])
	}
}

func TestGetTopBlobUsers_DefaultAllUsesRollup(t *testing.T) {
	var gotQuery string
	var gotArgs []interface{}
	db := &mockDB{
		selectFn: func(_ context.Context, dest interface{}, query string, args ...interface{}) error {
			gotQuery = query
			gotArgs = append([]interface{}{}, args...)
			users := dest.(*[]models.BlobUserStats)
			*users = []models.BlobUserStats{}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?network=42", http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if gotQuery != queryTopBlobUsersAllByCount {
		t.Fatal("expected all-window rollup query to be used")
	}
	wantArgs := []interface{}{42, 10, 0, "all"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("expected args %v, got %v", wantArgs, gotArgs)
	}
	// Requests that never named a range keep the historical response shape:
	// no meta echo.
	if strings.Contains(w.Body.String(), `"meta"`) {
		t.Fatalf("expected no meta on omitted range, got %s", w.Body.String())
	}
}

func TestGetTopUnattributedBlobUsers_SortSpendWindow(t *testing.T) {
	var gotQuery string
	var gotArgs []interface{}
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			gotQuery = query
			gotArgs = append([]interface{}{}, args...)
			users := dest.(*[]models.BlobUserStats)
			*users = []models.BlobUserStats{
				{
					Address:           "0xunknown",
					Category:          "unknown",
					BlobCount:         12,
					TotalCostWei:      "2.5",
					LastTimestamp:     time.Now(),
					BlobSharePercent:  55.5,
					SpendSharePercent: 66.6,
				},
			}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?network=42&limit=5&offset=2&sort=spend&window=24h", http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopUnattributedBlobUsers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if gotQuery != queryTopUnattributedBlobUsersWithOptions {
		t.Fatal("expected unattributed options query to be used")
	}
	wantArgs := []interface{}{42, 5, 2, apiWindow24h, "spend"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("expected args %v, got %v", wantArgs, gotArgs)
	}

	var resp struct {
		Success bool           `json:"success"`
		Data    []UserResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Success || len(resp.Data) != 1 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.Data[0].Address != "0xunknown" || resp.Data[0].Name != "" || resp.Data[0].BlobCount != 12 {
		t.Fatalf("unexpected unattributed user: %+v", resp.Data[0])
	}
}

func TestGetTopUnattributedBlobUsers_DefaultAllUsesRollup(t *testing.T) {
	var gotQuery string
	db := &mockDB{
		selectFn: func(_ context.Context, dest interface{}, query string, _ ...interface{}) error {
			gotQuery = query
			users := dest.(*[]models.BlobUserStats)
			*users = []models.BlobUserStats{}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?network=42", http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopUnattributedBlobUsers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if gotQuery != queryTopUnattributedBlobUsersAllByCount {
		t.Fatal("expected unattributed all-window rollup query to be used")
	}
}

func TestTopUnattributedBlobUsersQueryUsesKnownUserRowExistence(t *testing.T) {
	if !strings.Contains(queryTopUnattributedBlobUsersWithOptions, "MAX(bu.id) IS NULL") {
		t.Fatal("expected unattributed query to filter by known user row existence")
	}
	if strings.Contains(queryTopUnattributedBlobUsersWithOptions, "known_name") {
		t.Fatal("unattributed query should not use known user name content as the row-existence check")
	}
}

func TestTopUnattributedBlobUsersAllQueryUsesKnownUserRowExistence(t *testing.T) {
	if !strings.Contains(queryTopUnattributedBlobUsersAllByCount, "bu.id IS NULL") {
		t.Fatal("expected all-window unattributed query to filter by known user row existence")
	}
}

func TestGetTopBlobUsers_InvalidLimit(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?limit=-1", http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetTopBlobUsers_InvalidSort(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?sort=blocks", http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetTopBlobUsers_InvalidWindow(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?window=12h", http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "invalid window parameter") {
		t.Fatalf("expected window-specific error, got %s", w.Body.String())
	}
}

func TestGetTopBlobUsers_InvalidRange(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?range=100blocks", http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	var resp Response
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Success || resp.Error != "invalid range parameter" || resp.ErrorCode != errCodeInvalidRequest {
		t.Fatalf("unexpected error envelope: %+v", resp)
	}
}

func TestGetTopBlobUsers_ConflictingRangeAndWindow(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?range=24h&window=7d", http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetTopBlobUsers_RangeWindowedQueriesAndMetaEcho(t *testing.T) {
	for _, tc := range []struct {
		url       string
		wantRange string
	}{
		{"/?network=42&range=1h", "1h"},
		{"/?network=42&range=24h", "24h"},
		{"/?network=42&range=30d", "30d"},
		{"/?network=42&window=30d", "30d"},
		{"/?network=42&range=24h&window=24h", "24h"},
	} {
		t.Run(tc.url, func(t *testing.T) {
			var gotQuery string
			var gotArgs []interface{}
			db := &mockDB{
				selectFn: func(_ context.Context, dest interface{}, query string, args ...interface{}) error {
					gotQuery = query
					gotArgs = append([]interface{}{}, args...)
					users := dest.(*[]models.BlobUserStats)
					*users = []models.BlobUserStats{}
					return nil
				},
			}
			a := newTestAPIWithDB(db)
			req := httptest.NewRequest(http.MethodGet, tc.url, http.NoBody)
			w := httptest.NewRecorder()
			a.GetTopBlobUsers(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			if gotQuery != queryTopBlobUsersWithOptions {
				t.Fatal("expected windowed query to be used")
			}
			wantArgs := []interface{}{42, 10, 0, tc.wantRange, "count"}
			if !reflect.DeepEqual(gotArgs, wantArgs) {
				t.Fatalf("expected args %v, got %v", wantArgs, gotArgs)
			}

			// An empty window is a success with an empty list, not an error.
			body := w.Body.String()
			if !strings.Contains(body, `"data":[]`) {
				t.Fatalf("expected empty data array, got %s", body)
			}
			if !strings.Contains(body, fmt.Sprintf(`"meta":{"range":%q}`, tc.wantRange)) {
				t.Fatalf("expected meta range echo %q, got %s", tc.wantRange, body)
			}
		})
	}
}

func TestGetTopBlobUsers_RangeAllUsesRollupAndEchoesMeta(t *testing.T) {
	var gotQuery string
	db := &mockDB{
		selectFn: func(_ context.Context, dest interface{}, query string, _ ...interface{}) error {
			gotQuery = query
			users := dest.(*[]models.BlobUserStats)
			*users = []models.BlobUserStats{}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?network=42&range=all", http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if gotQuery != queryTopBlobUsersAllByCount {
		t.Fatal("expected all-window rollup query to be used")
	}
	if !strings.Contains(w.Body.String(), `"meta":{"range":"all"}`) {
		t.Fatalf("expected meta range echo, got %s", w.Body.String())
	}
}

func TestGetTopBlobUsers_GroupEntityDefaultAll(t *testing.T) {
	var gotQuery string
	var gotArgs []interface{}
	db := &mockDB{
		selectFn: func(_ context.Context, dest interface{}, query string, args ...interface{}) error {
			gotQuery = query
			gotArgs = append([]interface{}{}, args...)
			groups := dest.(*[]models.BlobUserGroupStats)
			*groups = []models.BlobUserGroupStats{
				{
					Key:               "scroll",
					Name:              "Scroll",
					Category:          "rollup",
					Addresses:         pq.StringArray{"0xbusy", "0xquiet"},
					BlobCount:         348007,
					TotalCostWei:      "4718548746240",
					LastTimestamp:     time.Now(),
					BlobSharePercent:  57.142857,
					SpendSharePercent: 40,
				},
				{
					Key:           "0xsolo",
					Category:      "unknown",
					Addresses:     pq.StringArray{"0xsolo"},
					BlobCount:     12,
					TotalCostWei:  "42",
					LastTimestamp: time.Now(),
				},
			}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?network=42&group=entity", http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if gotQuery != queryTopBlobUserGroupsAll {
		t.Fatal("expected all-window grouped query to be used")
	}
	// Grouped all-history queries always take the sort parameter, unlike the
	// per-address all-history variants with their static ORDER BY.
	wantArgs := []interface{}{42, 10, 0, "all", "count"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("expected args %v, got %v", wantArgs, gotArgs)
	}

	var resp struct {
		Success bool           `json:"success"`
		Data    []UserResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Success || len(resp.Data) != 2 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	entity := resp.Data[0]
	if entity.Key != "scroll" || entity.Name != "Scroll" || entity.Category != "rollup" {
		t.Fatalf("unexpected entity identity: %+v", entity)
	}
	if entity.Address != "0xbusy" || !reflect.DeepEqual(entity.Addresses, []string{"0xbusy", "0xquiet"}) {
		t.Fatalf("expected busiest-first member addresses with primary echo, got %+v", entity)
	}
	if entity.BlobCount != 348007 || entity.TotalCostWei != "4718548746240" || entity.BlobSharePercent != 57.142857 {
		t.Fatalf("unexpected entity aggregates: %+v", entity)
	}
	solo := resp.Data[1]
	if solo.Key != "0xsolo" || solo.Address != "0xsolo" || solo.Name != "" || len(solo.Addresses) != 1 {
		t.Fatalf("unexpected unattributed grouped row: %+v", solo)
	}
}

func TestGetTopBlobUsers_GroupEntityMetaEcho(t *testing.T) {
	for _, tc := range []struct {
		url      string
		wantMeta string
	}{
		{"/?network=42&group=entity", `"meta":{"group":"entity"}`},
		{"/?network=42&range=24h&group=entity", `"meta":{"range":"24h","group":"entity"}`},
		{"/?network=42&group=address", `"meta":{"group":"address"}`},
	} {
		t.Run(tc.url, func(t *testing.T) {
			a := newTestAPIWithDB(&mockDB{})
			req := httptest.NewRequest(http.MethodGet, tc.url, http.NoBody)
			w := httptest.NewRecorder()
			a.GetTopBlobUsers(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			if !strings.Contains(w.Body.String(), tc.wantMeta) {
				t.Fatalf("expected meta echo %s, got %s", tc.wantMeta, w.Body.String())
			}
		})
	}
}

func TestGetTopBlobUsers_GroupEntityWindowed(t *testing.T) {
	var gotQuery string
	var gotArgs []interface{}
	db := &mockDB{
		selectFn: func(_ context.Context, dest interface{}, query string, args ...interface{}) error {
			gotQuery = query
			gotArgs = append([]interface{}{}, args...)
			groups := dest.(*[]models.BlobUserGroupStats)
			*groups = []models.BlobUserGroupStats{}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?network=42&limit=5&offset=2&sort=spend&range=24h&group=entity", http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if gotQuery != queryTopBlobUserGroupsWithOptions {
		t.Fatal("expected windowed grouped query to be used")
	}
	wantArgs := []interface{}{42, 5, 2, apiWindow24h, "spend"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("expected args %v, got %v", wantArgs, gotArgs)
	}
}

func TestGetTopBlobUsers_GroupAddressKeepsPerAddressQuery(t *testing.T) {
	var gotQuery string
	db := &mockDB{
		selectFn: func(_ context.Context, dest interface{}, query string, _ ...interface{}) error {
			gotQuery = query
			users := dest.(*[]models.BlobUserStats)
			*users = []models.BlobUserStats{{Address: "0xabc", BlobCount: 1, TotalCostWei: "1"}}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?network=42&group=address", http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if gotQuery != queryTopBlobUsersAllByCount {
		t.Fatal("expected per-address rollup query for group=address")
	}
	// Per-address rows must not grow the grouped-only fields.
	body := w.Body.String()
	if strings.Contains(body, `"key"`) || strings.Contains(body, `"addresses"`) {
		t.Fatalf("expected no grouped fields on per-address rows, got %s", body)
	}
}

func TestGetTopBlobUsers_InvalidGroup(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?group=rollup", http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "invalid group parameter") {
		t.Fatalf("expected group-specific error, got %s", w.Body.String())
	}
}

func TestGetTopUnattributedBlobUsers_GroupEntityRejected(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?group=entity", http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopUnattributedBlobUsers(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "not supported for unattributed users") {
		t.Fatalf("expected unattributed rejection, got %s", w.Body.String())
	}
}

func TestGetTopBlobUsers_GroupEntityCacheHit(t *testing.T) {
	db := &mockDB{
		selectFn: func(_ context.Context, _ interface{}, _ string, _ ...interface{}) error {
			t.Fatal("DB should not be called on cache hit")
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	cacheKey := fmt.Sprintf("entity:%d:%d:%d:%s:%s", 42, 10, 0, userSortCount, userWindowAll)
	a.topUsersCache[cacheKey] = topUsersCacheEntry{
		response:  []UserResponse{{Address: "0xcached", Key: "cached", Addresses: []string{"0xcached"}}},
		expiresAt: time.Now().Add(time.Minute),
	}
	req := httptest.NewRequest(http.MethodGet, "/?group=entity", http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"key":"cached"`) {
		t.Fatalf("expected cached grouped row, got %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"meta":{"group":"entity"}`) {
		t.Fatalf("expected meta group echo on cache hit, got %s", w.Body.String())
	}
}

func TestGetTopBlobUsers_BadNetwork(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?network=missing", http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetTopBlobUsers_InvalidOffset(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?offset=-5", http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetTopBlobUsers_ExcessiveOffset(t *testing.T) {
	db := &mockDB{
		selectFn: func(_ context.Context, dest interface{}, _ string, _ ...interface{}) error {
			users := dest.(*[]models.BlobUserStats)
			*users = []models.BlobUserStats{}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/?offset=%d", MaxQueryOffset+1), http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetUserBreakdown_Success(t *testing.T) {
	var gotQuery string
	var gotArgs []interface{}
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			gotQuery = query
			gotArgs = append([]interface{}{}, args...)
			shares := dest.(*[]models.BlobUserCategoryShare)
			*shares = []models.BlobUserCategoryShare{
				{
					Category:          "rollup",
					BlobCount:         16,
					TotalCostWei:      "4.2",
					BlobSharePercent:  80,
					SpendSharePercent: 91.5,
				},
				{
					Category:          "unknown",
					BlobCount:         4,
					TotalCostWei:      "0.39",
					BlobSharePercent:  20,
					SpendSharePercent: 8.5,
				},
			}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?network=42&window=7d", http.NoBody)
	w := httptest.NewRecorder()
	a.GetUserBreakdown(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if gotQuery != queryBlobUserCategoryBreakdown {
		t.Fatal("expected category breakdown query to be used")
	}
	wantArgs := []interface{}{42, "7d"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("expected args %v, got %v", wantArgs, gotArgs)
	}

	var resp struct {
		Success bool                  `json:"success"`
		Data    UserBreakdownResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Success || resp.Data.Window != "7d" || len(resp.Data.CategoryShares) != 2 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.Data.CategoryShares[1].Category != "unknown" {
		t.Fatalf("expected unknown category fallback, got %+v", resp.Data.CategoryShares[1])
	}
}

func TestGetUserBreakdown_DefaultAllUsesRollup(t *testing.T) {
	var gotQuery string
	var gotArgs []interface{}
	db := &mockDB{
		selectFn: func(_ context.Context, dest interface{}, query string, args ...interface{}) error {
			gotQuery = query
			gotArgs = append([]interface{}{}, args...)
			shares := dest.(*[]models.BlobUserCategoryShare)
			*shares = []models.BlobUserCategoryShare{}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?network=42", http.NoBody)
	w := httptest.NewRecorder()
	a.GetUserBreakdown(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if gotQuery != queryBlobUserCategoryBreakdownAll {
		t.Fatal("expected all-window breakdown rollup query to be used")
	}
	wantArgs := []interface{}{42, "all"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("expected args %v, got %v", wantArgs, gotArgs)
	}
	if strings.Contains(w.Body.String(), `"meta"`) {
		t.Fatalf("expected no meta on omitted range, got %s", w.Body.String())
	}
}

func TestGetUserBreakdown_InvalidWindow(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?window=12h", http.NoBody)
	w := httptest.NewRecorder()
	a.GetUserBreakdown(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetUserBreakdown_RangeParam(t *testing.T) {
	var gotQuery string
	var gotArgs []interface{}
	db := &mockDB{
		selectFn: func(_ context.Context, dest interface{}, query string, args ...interface{}) error {
			gotQuery = query
			gotArgs = append([]interface{}{}, args...)
			shares := dest.(*[]models.BlobUserCategoryShare)
			*shares = []models.BlobUserCategoryShare{}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?network=42&range=30d", http.NoBody)
	w := httptest.NewRecorder()
	a.GetUserBreakdown(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if gotQuery != queryBlobUserCategoryBreakdown {
		t.Fatal("expected windowed breakdown query to be used")
	}
	wantArgs := []interface{}{42, "30d"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("expected args %v, got %v", wantArgs, gotArgs)
	}
	if !strings.Contains(w.Body.String(), `"meta":{"range":"30d"}`) {
		t.Fatalf("expected meta range echo, got %s", w.Body.String())
	}
}

func TestGetUserBreakdown_CacheHit(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			t.Fatal("DB should not be called on cache hit")
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	cacheKey := fmt.Sprintf("breakdown:%d:%s", 42, userWindowAll)
	a.breakdownCache[cacheKey] = userBreakdownCacheEntry{
		response: UserBreakdownResponse{
			ChainID: 42,
			Window:  string(userWindowAll),
			CategoryShares: []CategoryShareResponse{
				{Category: "rollup", BlobCount: 3},
			},
		},
		expiresAt: time.Now().Add(time.Minute),
	}
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetUserBreakdown(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		Success bool                  `json:"success"`
		Data    UserBreakdownResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Success || len(resp.Data.CategoryShares) != 1 || resp.Data.CategoryShares[0].Category != "rollup" {
		t.Fatalf("unexpected cached response: %+v", resp)
	}
}

func TestGetTopBlobUsers_DBError(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return fmt.Errorf("db error")
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?network=42", http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestGetTopBlobUsers_CustomLimit(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			users := dest.(*[]models.BlobUserStats)
			*users = []models.BlobUserStats{
				{Address: "0xabc", Name: "User1", BlobCount: 10},
			}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?network=42&limit=5", http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetTopBlobUsers_ExcessiveLimit(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			users := dest.(*[]models.BlobUserStats)
			*users = []models.BlobUserStats{}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?limit=5000", http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetTopBlobUsers_CacheHit(t *testing.T) {
	db := &mockDB{
		selectFn: func(_ context.Context, _ interface{}, _ string, _ ...interface{}) error {
			t.Fatal("DB should not be called on cache hit")
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	cacheKey := fmt.Sprintf("%d:%d:%d:%s:%s", 42, 10, 0, userSortCount, userWindowAll)
	a.topUsersCache[cacheKey] = topUsersCacheEntry{
		response:  []UserResponse{{Address: "0xcached"}},
		expiresAt: time.Now().Add(time.Minute),
	}
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetTopBlobUsers_CacheHitEchoesMeta(t *testing.T) {
	db := &mockDB{
		selectFn: func(_ context.Context, _ interface{}, _ string, _ ...interface{}) error {
			t.Fatal("DB should not be called on cache hit")
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	cacheKey := fmt.Sprintf("%d:%d:%d:%s:%s", 42, 10, 0, userSortCount, userWindow24h)
	a.topUsersCache[cacheKey] = topUsersCacheEntry{
		response:  []UserResponse{{Address: "0xcached"}},
		expiresAt: time.Now().Add(time.Minute),
	}
	req := httptest.NewRequest(http.MethodGet, "/?range=24h", http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"meta":{"range":"24h"}`) {
		t.Fatalf("expected meta range echo on cache hit, got %s", w.Body.String())
	}
}

func TestGetUserByAddress_Success(t *testing.T) {
	var gotQuery string
	var gotArgs []interface{}
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			gotQuery = query
			gotArgs = append([]interface{}{}, args...)
			user := dest.(*models.BlobUserStats)
			*user = models.BlobUserStats{
				Address:           validTestAddress,
				Name:              "TestRollup",
				Category:          "rollup",
				BlobCount:         42,
				TotalCostWei:      "1.5",
				LastTimestamp:     time.Now(),
				BlobSharePercent:  12.5,
				SpendSharePercent: 20,
			}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("address", validTestAddress)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	a.GetUserByAddress(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if gotQuery != queryUserByAddress {
		t.Fatal("expected enriched user-by-address query")
	}
	wantArgs := []interface{}{42, common.HexToAddress(validTestAddress).Hex()}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("expected args %v, got %v", wantArgs, gotArgs)
	}
	var resp struct {
		Success bool         `json:"success"`
		Data    UserResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Success {
		t.Error("expected Success=true")
	}
	if resp.Data.Category != "rollup" || resp.Data.BlobSharePercent != 12.5 || resp.Data.SpendSharePercent != 20 {
		t.Fatalf("unexpected enriched user fields: %+v", resp.Data)
	}
}

func TestGetUserByAddress_InvalidAddress(t *testing.T) {
	a := newTestAPI()
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("address", "notanaddress")
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	a.GetUserByAddress(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetUserByAddress_EmptyAddress(t *testing.T) {
	a := newTestAPI()
	// No chi route context → address will be empty
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetUserByAddress(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetUserByAddress_NotFound(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return sql.ErrNoRows
		},
	}
	a := newTestAPIWithDB(db)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("address", validTestAddress)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	a.GetUserByAddress(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetUserByAddress_DBError(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return fmt.Errorf("connection refused")
		},
	}
	a := newTestAPIWithDB(db)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("address", validTestAddress)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	a.GetUserByAddress(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestGetUserByAddress_BadNetwork(t *testing.T) {
	a := newTestAPI()
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("address", validTestAddress)
	req := httptest.NewRequest(http.MethodGet, "/?network=unknown", http.NoBody)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	a.GetUserByAddress(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetUserBreakdown_DBError(t *testing.T) {
	db := &mockDB{
		selectFn: func(_ context.Context, _ interface{}, _ string, _ ...interface{}) error {
			return fmt.Errorf("breakdown query failed")
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetUserBreakdown(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestGetUserBreakdown_BadNetwork(t *testing.T) {
	a := newTestAPI()
	a.networks = map[int]config.NetworkConfig{}
	req := httptest.NewRequest(http.MethodGet, "/?network=999", http.NoBody)
	w := httptest.NewRecorder()
	a.GetUserBreakdown(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}
