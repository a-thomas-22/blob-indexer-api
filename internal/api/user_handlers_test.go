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

	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	_ "github.com/a-thomas-22/blob-indexer-api/internal/testutil"
)

func TestGetTopBlobUsers_Success(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			users := dest.(*[]models.BlobUserStats)
			*users = []models.BlobUserStats{
				{Address: "0xabc", Name: "Alice", BlobCount: 10, TotalCostETH: "1.5", LastTimestamp: time.Now()},
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
					TotalCostETH:      "47185487462400",
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
	if resp.Data[0].TotalCostWei != "47185487462400" || resp.Data[0].TotalCostETH != "47185487462400" {
		t.Fatalf("unexpected user cost fields: %+v", resp.Data[0])
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
					TotalCostETH:      "2.5",
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

func TestTopUnattributedBlobUsersQueryUsesKnownUserRowExistence(t *testing.T) {
	if !strings.Contains(queryTopUnattributedBlobUsersWithOptions, "bu.id AS known_user_id") {
		t.Fatal("expected unattributed query to select a known user row marker")
	}
	if !strings.Contains(queryTopUnattributedBlobUsersWithOptions, "MAX(known_user_id) IS NULL") {
		t.Fatal("expected unattributed query to filter by known user row existence")
	}
	if strings.Contains(queryTopUnattributedBlobUsersWithOptions, "known_name") {
		t.Fatal("unattributed query should not use known user name content as the row-existence check")
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
	req := httptest.NewRequest(http.MethodGet, "/?window=30d", http.NoBody)
	w := httptest.NewRecorder()
	a.GetTopBlobUsers(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
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
					TotalCostETH:      "4.2",
					BlobSharePercent:  80,
					SpendSharePercent: 91.5,
				},
				{
					Category:          "unknown",
					BlobCount:         4,
					TotalCostETH:      "0.39",
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

func TestGetUserBreakdown_InvalidWindow(t *testing.T) {
	a := newTestAPI()
	req := httptest.NewRequest(http.MethodGet, "/?window=30d", http.NoBody)
	w := httptest.NewRecorder()
	a.GetUserBreakdown(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
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
				TotalCostETH:      "1.5",
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
