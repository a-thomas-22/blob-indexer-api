package api

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
)

func TestParseRollingStatsWindows_Defaults(t *testing.T) {
	windows, err := parseRollingStatsWindows("")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	wantLabels := []string{"5m", "1h", apiWindow24h, "7d"}
	if len(windows) != len(wantLabels) {
		t.Fatalf("expected %d windows, got %d", len(wantLabels), len(windows))
	}
	for i, want := range wantLabels {
		if windows[i].Label != want {
			t.Errorf("window %d label = %q, want %q", i, windows[i].Label, want)
		}
	}
}

func TestParseRollingStatsWindows_Custom(t *testing.T) {
	windows, err := parseRollingStatsWindows(" 15M,2h,3d ")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	want := []statsWindowSpec{
		{Label: "15m", Duration: 15 * time.Minute},
		{Label: "2h", Duration: 2 * time.Hour},
		{Label: "3d", Duration: 3 * 24 * time.Hour},
	}
	if len(windows) != len(want) {
		t.Fatalf("expected %d windows, got %d", len(want), len(windows))
	}
	for i := range want {
		if windows[i] != want[i] {
			t.Errorf("window %d = %+v, want %+v", i, windows[i], want[i])
		}
	}
}

func TestParseRollingStatsWindows_Invalid(t *testing.T) {
	tests := []string{
		"0m",
		"5x",
		"1.5h",
		"31d",
		"5m,5m",
		"1m,2m,3m,4m,5m,6m,7m,8m,9m",
	}

	for _, tt := range tests {
		t.Run(tt, func(t *testing.T) {
			if _, err := parseRollingStatsWindows(tt); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestGetRollingStatsWindows_Success(t *testing.T) {
	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			for _, want := range []string{
				"WITH requested_windows AS",
				"FROM blobs b",
				"b.confirmed = true",
				"FROM block_metrics bm",
				"ORDER BY wb.ord",
			} {
				if !strings.Contains(query, want) {
					t.Fatalf("expected query to contain %q: %s", want, query)
				}
			}
			if strings.Contains(query, "LATERAL") {
				t.Fatalf("rolling stats query should not use repeated lateral scans: %s", query)
			}
			if len(args) != 4 {
				t.Fatalf("expected 4 args, got %d", len(args))
			}
			if args[0] != 42 {
				t.Fatalf("expected network arg 42, got %v", args[0])
			}
			requireDriverValue(t, args[1], `{"1h"}`)
			requireDriverValue(t, args[2], `{3600}`)
			if generatedAt, ok := args[3].(time.Time); !ok || generatedAt.IsZero() {
				t.Fatalf("expected generated_at time arg, got %#v", args[3])
			}
			setSliceResult(dest, []rollingStatsWindowRow{
				{
					Window:             "1h",
					DurationSeconds:    3600,
					StartTime:          now.Add(-time.Hour),
					EndTime:            now,
					AverageBlobBaseFee: "100",
					MedianBlobBaseFee:  "90",
					P95BlobBaseFee:     "150",
					TotalBlobs:         12,
					TotalBlobGasUsed:   1572864,
					AverageUtilization: "0.750000",
					TotalCostETH:       "26494271031506069",
					UniqueSenders:      3,
				},
			})
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?network=testnet&windows=1h", http.NoBody)
	w := httptest.NewRecorder()

	a.GetRollingStatsWindows(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp struct {
		Success bool                 `json:"success"`
		Data    RollingStatsResponse `json:"data"`
		Error   string               `json:"error,omitempty"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected success response, got error %q", resp.Error)
	}
	if resp.Data.NetworkID != 42 || resp.Data.NetworkName != "testnet" {
		t.Fatalf("unexpected network in response: %+v", resp.Data)
	}
	if len(resp.Data.Windows) != 1 {
		t.Fatalf("expected 1 window, got %d", len(resp.Data.Windows))
	}
	window := resp.Data.Windows[0]
	if window.Window != "1h" || window.TotalBlobs != 12 || window.TotalBlobGasUsed != 1572864 {
		t.Fatalf("unexpected window response: %+v", window)
	}
	if window.AverageBlobBaseFee != "100" || window.MedianBlobBaseFee != "90" || window.P95BlobBaseFee != "150" {
		t.Fatalf("unexpected base fee stats: %+v", window)
	}
	if window.AverageBlobBaseFeeWei != "100" || window.MedianBlobBaseFeeWei != "90" || window.P95BlobBaseFeeWei != "150" {
		t.Fatalf("unexpected explicit base fee stats: %+v", window)
	}
	if window.AverageUtilization != "0.750000" || window.TotalCostETH != "26494271031506069" || window.TotalCostWei != "26494271031506069" || window.UniqueSenders != 3 {
		t.Fatalf("unexpected market stats: %+v", window)
	}
}

func TestGetRollingStatsWindows_SplitsRawAndRollupWindows(t *testing.T) {
	var queries []string
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			queries = append(queries, query)
			switch {
			case strings.Contains(query, "FROM blobs b"):
				requireDriverValue(t, args[1], `{"1h","24h"}`)
				requireDriverValue(t, args[2], `{3600,86400}`)
				setSliceResult(dest, []rollingStatsWindowRow{
					{Window: "1h", DurationSeconds: 3600, TotalBlobs: 1},
					{Window: apiWindow24h, DurationSeconds: 86400, TotalBlobs: 24},
				})
			case strings.Contains(query, "blob_chart_rollups"):
				if !strings.Contains(query, "block_metrics_rollups") {
					t.Fatalf("expected rollup query to read block_metrics_rollups: %s", query)
				}
				if strings.Contains(query, "FROM blobs b") || strings.Contains(query, "FROM block_metrics bm") {
					t.Fatalf("rollup query must not scan raw tables: %s", query)
				}
				requireDriverValue(t, args[1], `{"7d","30d"}`)
				requireDriverValue(t, args[2], `{604800,2592000}`)
				setSliceResult(dest, []rollingStatsWindowRow{
					{Window: "7d", DurationSeconds: 604800, TotalBlobs: 168},
					{Window: "30d", DurationSeconds: 2592000, TotalBlobs: 720},
				})
			default:
				t.Fatalf("unexpected query: %s", query)
			}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?network=testnet&windows=1h,7d,24h,30d", http.NoBody)
	w := httptest.NewRecorder()

	a.GetRollingStatsWindows(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if len(queries) != 2 {
		t.Fatalf("expected one raw and one rollup query, got %d queries", len(queries))
	}

	var resp struct {
		Success bool                 `json:"success"`
		Data    RollingStatsResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	wantOrder := []struct {
		label string
		blobs int
	}{{"1h", 1}, {"7d", 168}, {apiWindow24h, 24}, {"30d", 720}}
	if len(resp.Data.Windows) != len(wantOrder) {
		t.Fatalf("expected %d windows, got %d", len(wantOrder), len(resp.Data.Windows))
	}
	for i, want := range wantOrder {
		got := resp.Data.Windows[i]
		if got.Window != want.label || got.TotalBlobs != want.blobs {
			t.Fatalf("window %d = %q/%d, want %q/%d", i, got.Window, got.TotalBlobs, want.label, want.blobs)
		}
	}
}

func TestSplitRollingStatsWindows_CutoffBoundary(t *testing.T) {
	raw, rollup := splitRollingStatsWindows([]statsWindowSpec{
		{Label: "5m", Duration: 5 * time.Minute},
		{Label: apiWindow24h, Duration: 24 * time.Hour},
		{Label: "25h", Duration: 25 * time.Hour},
		{Label: "7d", Duration: 7 * 24 * time.Hour},
	})
	if len(raw) != 2 || raw[0].Label != "5m" || raw[1].Label != apiWindow24h {
		t.Fatalf("unexpected raw windows: %+v", raw)
	}
	if len(rollup) != 2 || rollup[0].Label != "25h" || rollup[1].Label != "7d" {
		t.Fatalf("unexpected rollup windows: %+v", rollup)
	}
}

func requireDriverValue(t *testing.T, arg interface{}, want string) {
	t.Helper()

	valuer, ok := arg.(driver.Valuer)
	if !ok {
		t.Fatalf("expected driver.Valuer arg, got %T", arg)
	}
	got, err := valuer.Value()
	if err != nil {
		t.Fatalf("failed to read driver value: %v", err)
	}
	if got != want {
		t.Fatalf("expected driver value %q, got %q", want, got)
	}
}

func TestGetRollingStatsWindows_InvalidWindows(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			t.Fatal("database should not be queried for invalid windows")
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?windows=0m", http.NoBody)
	w := httptest.NewRecorder()

	a.GetRollingStatsWindows(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetRollingStatsWindows_CacheHit(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			t.Fatal("DB should not be called on cache hit")
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	cacheKey := "rolling:42:5m,1h,24h,7d"
	a.rollingCache[cacheKey] = rollingStatsCacheEntry{
		response: RollingStatsResponse{
			NetworkID: 42,
			Windows: []RollingWindowStats{
				{Window: "5m", TotalBlobs: 7},
			},
		},
		expiresAt: time.Now().Add(time.Minute),
	}
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetRollingStatsWindows(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		Success bool                 `json:"success"`
		Data    RollingStatsResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Success || len(resp.Data.Windows) != 1 || resp.Data.Windows[0].TotalBlobs != 7 {
		t.Fatalf("unexpected cached response: %+v", resp)
	}
}

func TestGetRollingStatsWindows_BadNetwork(t *testing.T) {
	a := newTestAPI()
	a.networks = map[int]config.NetworkConfig{}
	req := httptest.NewRequest(http.MethodGet, "/?network=999", http.NoBody)
	w := httptest.NewRecorder()

	a.GetRollingStatsWindows(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetRollingStatsWindows_DBError(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return fmt.Errorf("db error")
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()

	a.GetRollingStatsWindows(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}
