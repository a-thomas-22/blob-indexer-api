package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

func TestParseChartRequest_AutoGranularity(t *testing.T) {
	now := time.Date(2026, 5, 24, 12, 3, 4, 0, time.UTC)
	req := httptest.NewRequest(http.MethodGet, "/?range=24h", http.NoBody)

	chart, err := parseChartRequest(req, now, true)
	if err != nil {
		t.Fatalf("parseChartRequest returned error: %v", err)
	}

	if chart.Range != chartRange24h {
		t.Fatalf("Range = %q, want %q", chart.Range, chartRange24h)
	}
	if chart.Granularity != chartGranularityMinute {
		t.Fatalf("Granularity = %q, want %q", chart.Granularity, chartGranularityMinute)
	}
	if chart.BucketSeconds != 300 {
		t.Fatalf("BucketSeconds = %d, want 300", chart.BucketSeconds)
	}
	wantEnd := time.Date(2026, 5, 24, 12, 5, 0, 0, time.UTC)
	if !chart.EndTime.Equal(wantEnd) {
		t.Fatalf("EndTime = %s, want %s", chart.EndTime, wantEnd)
	}
	if !chart.StartTime.Equal(wantEnd.Add(-24 * time.Hour)) {
		t.Fatalf("StartTime = %s, want %s", chart.StartTime, wantEnd.Add(-24*time.Hour))
	}
}

func TestParseChartRequest_InvalidInputs(t *testing.T) {
	tests := []string{
		"/?range=2h",
		"/?granularity=second",
		"/?range=all&granularity=block",
		"/?range=all&granularity=hour",
		"/?range=24h&granularity=minute",
		"/?range=1h&granularity=day",
		"/?limit=0",
	}

	for _, url := range tests {
		t.Run(url, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, url, http.NoBody)
			if _, err := parseChartRequest(req, time.Now().UTC(), true); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestParseChartRequest_AttributionLimitDoesNotCapPoints(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/?range=24h&limit=2", http.NoBody)

	chart, err := parseChartRequest(req, time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC), false)
	if err != nil {
		t.Fatalf("parseChartRequest returned error: %v", err)
	}
	if chart.PointLimit != maxChartPointLimit {
		t.Fatalf("PointLimit = %d, want %d", chart.PointLimit, maxChartPointLimit)
	}

	seriesLimit, err := parseAttributionSeriesLimit(req.URL.Query().Get("limit"))
	if err != nil {
		t.Fatalf("parseAttributionSeriesLimit returned error: %v", err)
	}
	if seriesLimit != 2 {
		t.Fatalf("seriesLimit = %d, want 2", seriesLimit)
	}
}

func TestGetBlobMarketChart_Success(t *testing.T) {
	rangeStart := time.Date(2026, 5, 24, 11, 0, 0, 0, time.UTC)
	rangeEnd := rangeStart.Add(time.Hour)
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if !strings.Contains(query, "generate_series") || !strings.Contains(query, "selected_metrics AS MATERIALIZED") || !strings.Contains(query, "summary_current_base_fee_wei") {
				t.Fatalf("unexpected blob market query: %s", query)
			}
			if len(args) != 4 {
				t.Fatalf("expected 4 args, got %d", len(args))
			}
			if args[0] != 42 || args[3] != int64(60) {
				t.Fatalf("unexpected args: %#v", args)
			}
			setSliceResult(dest, []blobMarketChartRow{
				{
					Timestamp:                    rangeStart,
					RangeStart:                   rangeStart,
					RangeEnd:                     rangeEnd,
					StartBlock:                   sql.NullInt64{Int64: 100, Valid: true},
					EndBlock:                     sql.NullInt64{Int64: 104, Valid: true},
					AverageBlobBaseFeeWei:        "1000000000",
					MedianBlobBaseFeeWei:         "2000000000",
					P95BlobBaseFeeWei:            "3000000000",
					BlobCount:                    7,
					BlobGasUsed:                  917504,
					BlobGasTarget:                1966080,
					AverageUtilization:           "0.466667",
					TotalCostWei:                 "700",
					UniqueSenders:                2,
					SummaryCurrentBaseFeeWei:     "4000000000",
					SummaryAverageBlobBaseFeeWei: "1000000000",
					SummaryMedianBlobBaseFeeWei:  "2000000000",
					SummaryP95BlobBaseFeeWei:     "3000000000",
					SummaryTotalBlobs:            7,
					SummaryTotalBlobGasUsed:      917504,
					SummaryAverageUtilization:    "0.466667",
					SummaryTotalCostWei:          "700",
					SummaryUniqueSenders:         2,
				},
			})
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?range=1h", http.NoBody)
	w := httptest.NewRecorder()

	a.GetBlobMarketChart(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		Success bool                    `json:"success"`
		Data    BlobMarketChartResponse `json:"data"`
		Error   string                  `json:"error,omitempty"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected success, got %q", resp.Error)
	}
	if resp.Data.Summary.CurrentBaseFeeGwei != "4" || resp.Data.Points[0].AverageBlobBaseFeeGwei != "1" {
		t.Fatalf("unexpected gwei conversion: %+v", resp.Data)
	}
	if resp.Data.Points[0].StartBlock == nil || *resp.Data.Points[0].StartBlock != 100 {
		t.Fatalf("expected start block 100, got %+v", resp.Data.Points[0].StartBlock)
	}
}

func TestGetBlobMarketChart_RejectsAllRange(t *testing.T) {
	db := &mockDB{
		selectFn: func(context.Context, interface{}, string, ...interface{}) error {
			t.Fatal("database should not be queried for range=all")
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?range=all", http.NoBody)
	w := httptest.NewRecorder()

	a.GetBlobMarketChart(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetAttributionUsageChart_SuccessAndZeroFill(t *testing.T) {
	bucket1 := time.Date(2026, 5, 24, 11, 0, 0, 0, time.UTC)
	bucket2 := bucket1.Add(5 * time.Minute)
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if !strings.Contains(query, "LIMIT $7") || !strings.Contains(query, "bucketed_usage") {
				t.Fatalf("unexpected attribution query: %s", query)
			}
			if !strings.Contains(query, "FLOOR(EXTRACT(EPOCH FROM bl.timestamp)") || !strings.Contains(query, "FROM bounds b") {
				t.Fatalf("expected raw attribution query to bucket one bounded blob scan: %s", query)
			}
			if strings.Contains(query, "FROM buckets bu") {
				t.Fatalf("expected raw attribution query not to join blobs once per bucket: %s", query)
			}
			if len(args) != 7 {
				t.Fatalf("expected 7 args, got %d", len(args))
			}
			if args[6] != defaultAttributionSeriesLimit {
				t.Fatalf("expected default series limit %d, got %#v", defaultAttributionSeriesLimit, args[6])
			}
			setSliceResult(dest, []attributionUsageChartRow{
				{
					Timestamp:    bucket1,
					RangeStart:   bucket1,
					RangeEnd:     bucket2.Add(5 * time.Minute),
					Key:          sql.NullString{String: "base", Valid: true},
					Name:         sql.NullString{String: "Base", Valid: true},
					Category:     sql.NullString{String: "rollup", Valid: true},
					Address:      sql.NullString{String: "0x123", Valid: true},
					BlobCount:    3,
					TotalCostWei: "30",
					BlobGasUsed:  393216,
				},
				{
					Timestamp:    bucket1,
					RangeStart:   bucket1,
					RangeEnd:     bucket2.Add(5 * time.Minute),
					Key:          sql.NullString{String: "other", Valid: true},
					Name:         sql.NullString{String: "Other", Valid: true},
					Category:     sql.NullString{String: "other", Valid: true},
					BlobCount:    1,
					TotalCostWei: "10",
					BlobGasUsed:  131072,
				},
				{
					Timestamp:  bucket2,
					RangeStart: bucket1,
					RangeEnd:   bucket2.Add(5 * time.Minute),
				},
			})
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?range=24h&limit=5", http.NoBody)
	w := httptest.NewRecorder()

	a.GetAttributionUsageChart(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		Success bool                          `json:"success"`
		Data    AttributionUsageChartResponse `json:"data"`
		Error   string                        `json:"error,omitempty"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected success, got %q", resp.Error)
	}
	if len(resp.Data.Points) != 2 {
		t.Fatalf("expected 2 points, got %d", len(resp.Data.Points))
	}
	if got := resp.Data.Points[1].Values["base"].TotalCostWei; got != "0" {
		t.Fatalf("expected zero-filled base value in empty bucket, got %q", got)
	}
	if resp.Data.Summary.TotalBlobs != 4 || resp.Data.Summary.TotalCostWei != "40" {
		t.Fatalf("unexpected summary: %+v", resp.Data.Summary)
	}
	if resp.Data.Summary.Shares[0].BlobSharePercent != 75 {
		t.Fatalf("expected top share 75, got %+v", resp.Data.Summary.Shares)
	}
}

func TestGetCostComparisonChart_Success(t *testing.T) {
	bucket := time.Date(2026, 5, 24, 11, 0, 0, 0, time.UTC)
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if !strings.Contains(query, "calldata_equivalent_cost_wei") || !strings.Contains(query, "summary_savings_percent") {
				t.Fatalf("unexpected cost comparison query: %s", query)
			}
			if !strings.Contains(query, "blob_chart_rollups") {
				t.Fatalf("expected hour granularity to read rollups: %s", query)
			}
			if len(args) != 6 {
				t.Fatalf("expected 6 args, got %d", len(args))
			}
			if args[5] != calldataGasPerByte {
				t.Fatalf("expected calldata gas per byte arg %d, got %#v", calldataGasPerByte, args[5])
			}
			setSliceResult(dest, []costComparisonChartRow{
				{
					Timestamp:                        bucket,
					RangeStart:                       bucket,
					RangeEnd:                         bucket.Add(time.Hour),
					BlobCount:                        1,
					BlobBytes:                        131072,
					BlobCostWei:                      "100",
					CalldataEquivalentCostWei:        "1600",
					SavingsWei:                       "1500",
					SavingsPercent:                   93.75,
					SummaryBlobCostWei:               "100",
					SummaryCalldataEquivalentCostWei: "1600",
					SummarySavingsWei:                "1500",
					SummarySavingsPercent:            93.75,
				},
			})
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?range=7d&granularity=hour&limit=200", http.NoBody)
	w := httptest.NewRecorder()

	a.GetCostComparisonChart(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		Success bool                        `json:"success"`
		Data    CostComparisonChartResponse `json:"data"`
		Error   string                      `json:"error,omitempty"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected success, got %q", resp.Error)
	}
	if resp.Data.Model.CalldataGasPerByte != calldataGasPerByte || !strings.Contains(resp.Data.Model.Description, "Approximation") {
		t.Fatalf("unexpected model: %+v", resp.Data.Model)
	}
	if resp.Data.Model.BlobSizeBytes != blobSizeBytes {
		t.Fatalf("blob_size_bytes = %d, want %d", resp.Data.Model.BlobSizeBytes, blobSizeBytes)
	}
	if resp.Data.Points[0].AverageExecutionBaseFeeWei != nil {
		t.Fatalf("expected omitted execution base fee, got %v", *resp.Data.Points[0].AverageExecutionBaseFeeWei)
	}
	if resp.Data.Summary.SavingsPercent != 93.75 {
		t.Fatalf("unexpected summary: %+v", resp.Data.Summary)
	}
}

func TestGetCostComparisonChart_MinuteUsesSingleRangeScan(t *testing.T) {
	bucket := time.Date(2026, 5, 24, 11, 0, 0, 0, time.UTC)
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if !strings.Contains(query, "range_blobs AS MATERIALIZED") || !strings.Contains(query, "FLOOR(EXTRACT(EPOCH FROM bl.timestamp)") {
				t.Fatalf("expected raw cost query to bucket one bounded blob scan: %s", query)
			}
			if strings.Contains(query, "blob_chart_rollups") {
				t.Fatalf("expected minute granularity to read raw blobs, got rollup query: %s", query)
			}
			if strings.Contains(query, "bl.timestamp >= b.bucket_start") {
				t.Fatalf("expected raw cost query not to join blobs once per bucket: %s", query)
			}
			if len(args) != 7 {
				t.Fatalf("expected 7 args, got %d", len(args))
			}
			if args[4] != int64(300) || args[6] != calldataGasPerByte {
				t.Fatalf("unexpected args: %#v", args)
			}
			setSliceResult(dest, []costComparisonChartRow{
				{
					Timestamp:                        bucket,
					RangeStart:                       bucket,
					RangeEnd:                         bucket.Add(24 * time.Hour),
					BlobCostWei:                      "0",
					CalldataEquivalentCostWei:        "0",
					SavingsWei:                       "0",
					SummaryBlobCostWei:               "0",
					SummaryCalldataEquivalentCostWei: "0",
					SummarySavingsWei:                "0",
				},
			})
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?range=24h&granularity=auto", http.NoBody)
	w := httptest.NewRecorder()

	a.GetCostComparisonChart(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestChartHandlers_ErrorPaths(t *testing.T) {
	handlers := []struct {
		name    string
		handler func(*API, http.ResponseWriter, *http.Request)
	}{
		{
			name: "blob market",
			handler: func(a *API, w http.ResponseWriter, r *http.Request) {
				a.GetBlobMarketChart(w, r)
			},
		},
		{
			name: "attribution usage",
			handler: func(a *API, w http.ResponseWriter, r *http.Request) {
				a.GetAttributionUsageChart(w, r)
			},
		},
		{
			name: "cost comparison",
			handler: func(a *API, w http.ResponseWriter, r *http.Request) {
				a.GetCostComparisonChart(w, r)
			},
		},
	}

	for _, tc := range handlers {
		t.Run(tc.name+" bad network", func(t *testing.T) {
			a := newTestAPI()
			a.networks = nil
			req := httptest.NewRequest(http.MethodGet, "/?network=missing", http.NoBody)
			w := httptest.NewRecorder()

			tc.handler(a, w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d", w.Code)
			}
		})

		t.Run(tc.name+" invalid range", func(t *testing.T) {
			a := newTestAPIWithDB(&mockDB{
				selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
					t.Fatal("database should not be queried")
					return nil
				},
			})
			req := httptest.NewRequest(http.MethodGet, "/?range=2h", http.NoBody)
			w := httptest.NewRecorder()

			tc.handler(a, w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d", w.Code)
			}
		})

		t.Run(tc.name+" database error", func(t *testing.T) {
			a := newTestAPIWithDB(&mockDB{
				selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
					return errors.New("db failed")
				},
			})
			req := httptest.NewRequest(http.MethodGet, "/?range=1h", http.NoBody)
			w := httptest.NewRecorder()

			tc.handler(a, w, req)

			if w.Code != http.StatusInternalServerError {
				t.Fatalf("expected 500, got %d", w.Code)
			}
		})
	}

	t.Run("attribution invalid series limit", func(t *testing.T) {
		a := newTestAPIWithDB(&mockDB{})
		req := httptest.NewRequest(http.MethodGet, "/?limit=0", http.NoBody)
		w := httptest.NewRecorder()

		a.GetAttributionUsageChart(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", w.Code)
		}
	})
}

func TestChartHandlers_BlockGranularity(t *testing.T) {
	blockTime := time.Date(2026, 5, 24, 11, 0, 0, 0, time.UTC)

	t.Run("blob market", func(t *testing.T) {
		db := &mockDB{
			selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
				if !strings.Contains(query, "selected_blocks") {
					t.Fatalf("expected block chart query, got %s", query)
				}
				if len(args) != 3 {
					t.Fatalf("expected 3 args, got %d", len(args))
				}
				setSliceResult(dest, []blobMarketChartRow{
					{
						Timestamp:                    blockTime,
						RangeStart:                   blockTime.Add(-time.Hour),
						RangeEnd:                     blockTime.Add(time.Hour),
						StartBlock:                   sql.NullInt64{Int64: 123, Valid: true},
						EndBlock:                     sql.NullInt64{Int64: 123, Valid: true},
						AverageBlobBaseFeeWei:        "1",
						MedianBlobBaseFeeWei:         "1",
						P95BlobBaseFeeWei:            "1",
						AverageUtilization:           "0",
						TotalCostWei:                 "0",
						SummaryCurrentBaseFeeWei:     "1",
						SummaryAverageBlobBaseFeeWei: "1",
						SummaryMedianBlobBaseFeeWei:  "1",
						SummaryP95BlobBaseFeeWei:     "1",
						SummaryAverageUtilization:    "0",
						SummaryTotalCostWei:          "0",
					},
				})
				return nil
			},
		}
		a := newTestAPIWithDB(db)
		req := httptest.NewRequest(http.MethodGet, "/?range=1h&granularity=block&limit=400", http.NoBody)
		w := httptest.NewRecorder()

		a.GetBlobMarketChart(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
	})

	t.Run("attribution usage", func(t *testing.T) {
		db := &mockDB{
			selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
				if !strings.Contains(query, "bm.block_timestamp AS bucket_start") {
					t.Fatalf("expected block attribution query, got %s", query)
				}
				if len(args) != 4 || args[3] != defaultAttributionSeriesLimit {
					t.Fatalf("unexpected args: %#v", args)
				}
				setSliceResult(dest, []attributionUsageChartRow{
					{
						Timestamp:   blockTime,
						RangeStart:  blockTime.Add(-time.Hour),
						RangeEnd:    blockTime,
						BlockNumber: sql.NullInt64{Int64: 100, Valid: true},
						Key:         sql.NullString{String: "base", Valid: true},
						Name:        sql.NullString{String: "Base", Valid: true},
						Category:    sql.NullString{String: "rollup", Valid: true},
					},
					{
						Timestamp:   blockTime,
						RangeStart:  blockTime.Add(-time.Hour),
						RangeEnd:    blockTime,
						BlockNumber: sql.NullInt64{Int64: 101, Valid: true},
						Key:         sql.NullString{String: "base", Valid: true},
						Name:        sql.NullString{String: "Base", Valid: true},
						Category:    sql.NullString{String: "rollup", Valid: true},
					},
				})
				return nil
			},
		}
		a := newTestAPIWithDB(db)
		req := httptest.NewRequest(http.MethodGet, "/?range=1h&granularity=block&limit=5", http.NoBody)
		w := httptest.NewRecorder()

		a.GetAttributionUsageChart(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		var resp struct {
			Success bool                          `json:"success"`
			Data    AttributionUsageChartResponse `json:"data"`
		}
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if len(resp.Data.Points) != 2 {
			t.Fatalf("expected same-timestamp blocks to remain separate points, got %d", len(resp.Data.Points))
		}
		if resp.Data.Points[0].StartBlock == nil || resp.Data.Points[1].StartBlock == nil ||
			*resp.Data.Points[0].StartBlock != 100 || *resp.Data.Points[1].StartBlock != 101 {
			t.Fatalf("unexpected block anchors: %+v", resp.Data.Points)
		}
	})

	t.Run("cost comparison", func(t *testing.T) {
		db := &mockDB{
			selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
				if !strings.Contains(query, "selected_blocks") {
					t.Fatalf("expected block cost query, got %s", query)
				}
				if len(args) != 4 || args[3] != calldataGasPerByte {
					t.Fatalf("unexpected args: %#v", args)
				}
				setSliceResult(dest, []costComparisonChartRow{
					{
						Timestamp:                        blockTime,
						RangeStart:                       blockTime.Add(-time.Hour),
						RangeEnd:                         blockTime,
						BlobCostWei:                      "0",
						CalldataEquivalentCostWei:        "0",
						SavingsWei:                       "0",
						SummaryBlobCostWei:               "0",
						SummaryCalldataEquivalentCostWei: "0",
						SummarySavingsWei:                "0",
					},
				})
				return nil
			},
		}
		a := newTestAPIWithDB(db)
		req := httptest.NewRequest(http.MethodGet, "/?range=1h&granularity=block&limit=400", http.NoBody)
		w := httptest.NewRecorder()

		a.GetCostComparisonChart(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
	})
}

func TestChartHelpers_EdgeCases(t *testing.T) {
	for _, rangeLabel := range []string{chartRange1h, chartRange24h, chartRange7d, chartRange30d, chartRangeAll} {
		if _, _, _, err := autoChartGranularity(rangeLabel); err != nil {
			t.Fatalf("autoChartGranularity(%q) returned error: %v", rangeLabel, err)
		}
	}
	if _, _, _, err := autoChartGranularity("bad"); err == nil {
		t.Fatal("expected invalid auto granularity range to error")
	}
	if got, err := parseChartPointLimit("5000"); err != nil || got != maxChartPointLimit {
		t.Fatalf("parseChartPointLimit clamp = %d, %v", got, err)
	}
	if got, err := parseAttributionSeriesLimit("5000"); err != nil || got != maxAttributionSeriesLimit {
		t.Fatalf("parseAttributionSeriesLimit clamp = %d, %v", got, err)
	}
	if _, err := parseAttributionSeriesLimit("nope"); err == nil {
		t.Fatal("expected invalid attribution limit to error")
	}
	if got := alignChartEnd(time.Date(2026, 5, 24, 12, 3, 4, 0, time.UTC), 0); got.Second() != 4 {
		t.Fatalf("expected unaligned time for zero bucket, got %s", got)
	}
	if got := estimatedTimePoints(0, 60); got != 0 {
		t.Fatalf("estimatedTimePoints zero duration = %d, want 0", got)
	}
	if got := estimatedBlockPoints(-time.Second); got != 0 {
		t.Fatalf("estimatedBlockPoints negative duration = %d, want 0", got)
	}
	if got := estimatedBlockPoints(time.Minute); got != 5 {
		t.Fatalf("estimatedBlockPoints one minute = %d, want 5", got)
	}
	if nullInt64Ptr(sql.NullInt64{}) != nil {
		t.Fatal("expected nil int64 pointer")
	}
	if nullStringPtr(sql.NullString{}) != nil {
		t.Fatal("expected nil string pointer")
	}
	if got := nonEmptyDecimal(""); got != "0" {
		t.Fatalf("nonEmptyDecimal empty = %q, want 0", got)
	}
	if got := formatDecimalWeiAsGwei("not-a-number"); got != "" {
		t.Fatalf("formatDecimalWeiAsGwei invalid = %q, want empty", got)
	}
	if got := addDecimalStrings("bad", "2"); got != "2" {
		t.Fatalf("addDecimalStrings invalid left = %q, want 2", got)
	}
	if got := compareDecimalStrings("10", "2"); got <= 0 {
		t.Fatalf("compareDecimalStrings(10,2) = %d, want positive", got)
	}
	if got := decimalSharePercent("bad", "10"); got != 0 {
		t.Fatalf("decimalSharePercent invalid = %f, want 0", got)
	}
	if got := decimalSharePercent("1", "0"); got != 0 {
		t.Fatalf("decimalSharePercent zero total = %f, want 0", got)
	}
	if got := chartPercentage(1, 0); got != 0 {
		t.Fatalf("chartPercentage zero total = %f, want 0", got)
	}
	if got := formatRatDecimal(nil, 6); got != "0" {
		t.Fatalf("formatRatDecimal nil = %q, want 0", got)
	}
	if got := formatRatDecimal(new(big.Rat), 6); got != "0" {
		t.Fatalf("formatRatDecimal zero = %q, want 0", got)
	}
}

func TestChartRoutesMounted(t *testing.T) {
	a := newTestAPIWithDB(&mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			switch dest.(type) {
			case *[]blobMarketChartRow:
				setSliceResult(dest, []blobMarketChartRow{})
			case *[]attributionUsageChartRow:
				setSliceResult(dest, []attributionUsageChartRow{})
			case *[]costComparisonChartRow:
				setSliceResult(dest, []costComparisonChartRow{})
			default:
				t.Fatalf("unexpected dest type %T", dest)
			}
			return nil
		},
	})
	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		a.mountPublicRoutes(r, func(next http.Handler) http.Handler {
			return next
		})
	})

	for _, path := range []string{
		"/api/v1/charts/blob-market",
		"/api/v1/charts/attribution-usage",
		"/api/v1/charts/cost-comparison",
	} {
		req := httptest.NewRequest(http.MethodGet, path+"?range=1h", http.NoBody)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s returned %d", path, w.Code)
		}
	}
}

func TestChartServedByRollups(t *testing.T) {
	tests := []struct {
		granularity   string
		bucketSeconds int64
		want          bool
	}{
		{chartGranularityMinute, 60, false},
		{chartGranularityMinute, 300, false},
		{chartGranularityHour, 3600, true},
		{chartGranularityHour, 21600, true},
		{chartGranularityDay, 86400, true},
		{chartGranularityBlock, approxBlockSeconds, false},
	}
	for _, tc := range tests {
		chart := chartRequest{Granularity: tc.granularity, BucketSeconds: tc.bucketSeconds}
		if got := chartServedByRollups(chart); got != tc.want {
			t.Fatalf("chartServedByRollups(%s, %d) = %v, want %v", tc.granularity, tc.bucketSeconds, got, tc.want)
		}
	}
}

func TestGetBlobMarketChart_RollupRouting(t *testing.T) {
	queried := false
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			queried = true
			if !strings.Contains(query, "block_metrics_rollups") || !strings.Contains(query, "blob_chart_rollups") {
				t.Fatalf("expected rollup-backed query for range=7d, got: %s", query)
			}
			if len(args) != 4 {
				t.Fatalf("expected 4 args, got %d", len(args))
			}
			if args[0] != 42 || args[3] != int64(3600) {
				t.Fatalf("unexpected args: %#v", args)
			}
			setSliceResult(dest, []blobMarketChartRow{})
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?range=7d", http.NoBody)
	w := httptest.NewRecorder()

	a.GetBlobMarketChart(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !queried {
		t.Fatal("expected a database query")
	}
}

func TestGetAttributionUsageChart_AllRangeUsesRollups(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if !strings.Contains(query, "blob_chart_rollups") || !strings.Contains(query, "LIMIT $6") {
				t.Fatalf("expected rollup-backed attribution query for range=all, got: %s", query)
			}
			if len(args) != 6 {
				t.Fatalf("expected 6 args, got %d", len(args))
			}
			if args[1] != chartRangeAll || args[4] != int64(86400) {
				t.Fatalf("unexpected args: %#v", args)
			}
			setSliceResult(dest, []attributionUsageChartRow{})
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?range=all", http.NoBody)
	w := httptest.NewRecorder()

	a.GetAttributionUsageChart(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestChartHandlers_ResponseCacheHitsDBOnce(t *testing.T) {
	queries := 0
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			queries++
			setSliceResult(dest, []costComparisonChartRow{})
			return nil
		},
	}
	a := newTestAPIWithDB(db)

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/?range=30d", http.NoBody)
		w := httptest.NewRecorder()
		a.GetCostComparisonChart(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i, w.Code)
		}
		if got := w.Header().Get("Cache-Control"); got != "public, max-age=30, s-maxage=30" {
			t.Fatalf("request %d: Cache-Control = %q", i, got)
		}
	}

	if queries != 1 {
		t.Fatalf("expected exactly 1 database query across identical requests, got %d", queries)
	}
}

func TestChartHandlers_CacheErrorNotStored(t *testing.T) {
	queries := 0
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			queries++
			if queries == 1 {
				return errors.New("db failed")
			}
			setSliceResult(dest, []blobMarketChartRow{})
			return nil
		},
	}
	a := newTestAPIWithDB(db)

	req := httptest.NewRequest(http.MethodGet, "/?range=24h", http.NoBody)
	w := httptest.NewRecorder()
	a.GetBlobMarketChart(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/?range=24h", http.NoBody)
	w = httptest.NewRecorder()
	a.GetBlobMarketChart(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 after transient failure, got %d", w.Code)
	}
	if queries != 2 {
		t.Fatalf("expected 2 queries, got %d", queries)
	}
}

func TestChartHandlers_DBTimeoutReturns503(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return context.DeadlineExceeded
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?range=24h", http.NoBody)
	w := httptest.NewRecorder()

	a.GetBlobMarketChart(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "5" {
		t.Fatalf("Retry-After = %q, want \"5\"", got)
	}
}
