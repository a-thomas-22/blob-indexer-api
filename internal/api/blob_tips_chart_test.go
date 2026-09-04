package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func blobTipsTestRows(bucket1, bucket2 time.Time) []blobTipsChartRow {
	rangeEnd := bucket2.Add(5 * time.Minute)
	summary := func(row blobTipsChartRow) blobTipsChartRow {
		row.RangeStart = bucket1
		row.RangeEnd = rangeEnd
		row.SummaryTotalBlobs = 6
		row.SummaryPricedBlobs = 5
		row.SummaryAveragePriorityFeeWei = "2200000000"
		row.SummaryMedianPriorityFeeWei = "1000000000"
		row.SummaryP95PriorityFeeWei = "5000000000"
		row.SummaryMaxPriorityFeeWei = "5000000000"
		return row
	}
	return []blobTipsChartRow{
		summary(blobTipsChartRow{
			Timestamp:                   bucket1,
			BucketBlobCount:             4,
			BucketAveragePriorityFeeWei: "2500000000",
			BucketMedianPriorityFeeWei:  "1000000000",
			BucketP95PriorityFeeWei:     "5000000000",
			BucketMaxPriorityFeeWei:     "5000000000",
			Key:                         sql.NullString{String: "optimism", Valid: true},
			Name:                        sql.NullString{String: "Optimism", Valid: true},
			Category:                    sql.NullString{String: "rollup", Valid: true},
			Address:                     sql.NullString{String: "0xop", Valid: true},
			SeriesBlobCount:             2,
			SeriesAveragePriorityFeeWei: "4500000000",
			SeriesMaxPriorityFeeWei:     "5000000000",
		}),
		summary(blobTipsChartRow{
			Timestamp:                   bucket1,
			BucketBlobCount:             4,
			BucketAveragePriorityFeeWei: "2500000000",
			BucketMedianPriorityFeeWei:  "1000000000",
			BucketP95PriorityFeeWei:     "5000000000",
			BucketMaxPriorityFeeWei:     "5000000000",
			Key:                         sql.NullString{String: "arbitrum", Valid: true},
			Name:                        sql.NullString{String: "Arbitrum", Valid: true},
			Category:                    sql.NullString{String: "rollup", Valid: true},
			SeriesBlobCount:             2,
			SeriesAveragePriorityFeeWei: "500000000",
			SeriesMaxPriorityFeeWei:     "1000000000",
		}),
		summary(blobTipsChartRow{
			Timestamp:                   bucket2,
			BucketBlobCount:             1,
			BucketAveragePriorityFeeWei: "1000000000",
			BucketMedianPriorityFeeWei:  "1000000000",
			BucketP95PriorityFeeWei:     "1000000000",
			BucketMaxPriorityFeeWei:     "1000000000",
			Key:                         sql.NullString{String: "arbitrum", Valid: true},
			Name:                        sql.NullString{String: "Arbitrum", Valid: true},
			Category:                    sql.NullString{String: "rollup", Valid: true},
			SeriesBlobCount:             1,
			SeriesAveragePriorityFeeWei: "1000000000",
			SeriesMaxPriorityFeeWei:     "1000000000",
		}),
		// A bucket with blobs but none priced projects zero stats and no series.
		summary(blobTipsChartRow{
			Timestamp:                   bucket2.Add(5 * time.Minute),
			BucketAveragePriorityFeeWei: "0",
			BucketMedianPriorityFeeWei:  "0",
			BucketP95PriorityFeeWei:     "0",
			BucketMaxPriorityFeeWei:     "0",
			SeriesAveragePriorityFeeWei: "0",
			SeriesMaxPriorityFeeWei:     "0",
		}),
	}
}

func decodeBlobTipsResponse(t *testing.T, w *httptest.ResponseRecorder) BlobTipsChartResponse {
	t.Helper()
	var resp struct {
		Success bool                  `json:"success"`
		Data    BlobTipsChartResponse `json:"data"`
		Error   string                `json:"error,omitempty"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected success, got %q", resp.Error)
	}
	return resp.Data
}

func TestGetBlobTipsChart_SuccessAndZeroFill(t *testing.T) {
	bucket1 := time.Date(2026, 9, 4, 11, 0, 0, 0, time.UTC)
	bucket2 := bucket1.Add(5 * time.Minute)
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if !strings.Contains(query, "bucketed_series") || !strings.Contains(query, "LIMIT $5") {
				t.Fatalf("unexpected blob tips query: %s", query)
			}
			if !strings.Contains(query, "AND bl.priority_fee_per_gas IS NOT NULL") || !strings.Contains(query, "FROM range_blocks rb") {
				t.Fatalf("expected the blob scan to cover only priced rows and the total to come from block_metrics: %s", query)
			}
			if !strings.Contains(query, "generate_series") || strings.Contains(query, "FROM block_metrics") {
				t.Fatalf("expected the time-bucketed query: %s", query)
			}
			if len(args) != 5 {
				t.Fatalf("expected 5 args, got %d", len(args))
			}
			if args[0] != 42 || args[3] != int64(300) || args[4] != 3 {
				t.Fatalf("unexpected args: %#v", args)
			}
			setSliceResult(dest, blobTipsTestRows(bucket1, bucket2))
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?range=24h&limit=3", http.NoBody)
	w := httptest.NewRecorder()

	a.GetBlobTipsChart(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	data := decodeBlobTipsResponse(t, w)

	if !data.StartTime.Equal(bucket1) || !data.EndTime.Equal(bucket2.Add(5*time.Minute)) {
		t.Fatalf("unexpected range: %s .. %s", data.StartTime, data.EndTime)
	}
	if len(data.Points) != 3 {
		t.Fatalf("expected 3 points, got %d", len(data.Points))
	}

	first := data.Points[0]
	if first.BlobCount != 4 || first.AveragePriorityFeeGwei != "2.5" || first.MedianPriorityFeeGwei != "1" || first.P95PriorityFeeGwei != "5" || first.MaxPriorityFeeGwei != "5" {
		t.Fatalf("unexpected first bucket stats: %+v", first)
	}
	if first.StartBlock != nil || first.EndBlock != nil {
		t.Fatalf("time buckets must not carry block bounds: %+v", first)
	}
	if got := first.Values["optimism"]; got.BlobCount != 2 || got.AveragePriorityFeeGwei != "4.5" || got.MaxPriorityFeeGwei != "5" {
		t.Fatalf("unexpected optimism value: %+v", got)
	}

	// Series present in the range but absent from a bucket are zero-filled,
	// and a bucket with no priced blobs still appears with zero stats.
	if got := data.Points[1].Values["optimism"]; got.BlobCount != 0 || got.AveragePriorityFeeGwei != "0" || got.MaxPriorityFeeGwei != "0" {
		t.Fatalf("expected zero-filled optimism value, got %+v", got)
	}
	empty := data.Points[2]
	if empty.BlobCount != 0 || empty.MaxPriorityFeeGwei != "0" || len(empty.Values) != 2 {
		t.Fatalf("unexpected empty bucket: %+v", empty)
	}

	// Arbitrum posted more blobs, so it leads even though Optimism outbid it.
	if len(data.Series) != 2 || data.Series[0].Key != "arbitrum" || data.Series[1].Key != "optimism" {
		t.Fatalf("unexpected series order: %+v", data.Series)
	}
	if data.Series[1].Address != "0xop" || data.Series[0].Address != "" {
		t.Fatalf("unexpected series addresses: %+v", data.Series)
	}

	if data.Summary.TotalBlobs != 6 || data.Summary.PricedBlobs != 5 {
		t.Fatalf("unexpected summary counts: %+v", data.Summary)
	}
	if data.Summary.AveragePriorityFeeGwei != "2.2" || data.Summary.MedianPriorityFeeGwei != "1" || data.Summary.P95PriorityFeeGwei != "5" || data.Summary.MaxPriorityFeeGwei != "5" {
		t.Fatalf("unexpected summary fees: %+v", data.Summary)
	}
	if len(data.Summary.Shares) != 2 {
		t.Fatalf("expected 2 shares, got %+v", data.Summary.Shares)
	}
	// Arbitrum: (0.5 gwei * 2 + 1 gwei * 1) / 3 blobs, blob-weighted rather
	// than a mean of bucket means.
	arb := data.Summary.Shares[0]
	if arb.Key != "arbitrum" || arb.BlobCount != 3 || arb.BlobSharePercent != 60 || arb.AveragePriorityFeeGwei != "0.666666666666666667" || arb.MaxPriorityFeeGwei != "1" {
		t.Fatalf("unexpected arbitrum share: %+v", arb)
	}
	op := data.Summary.Shares[1]
	if op.Key != "optimism" || op.BlobCount != 2 || op.BlobSharePercent != 40 || op.AveragePriorityFeeGwei != "4.5" || op.MaxPriorityFeeGwei != "5" {
		t.Fatalf("unexpected optimism share: %+v", op)
	}
}

func TestGetBlobTipsChart_BlockGranularity(t *testing.T) {
	blockTime := time.Date(2026, 9, 4, 11, 0, 0, 0, time.UTC)
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if !strings.Contains(query, "FROM block_metrics bm") || !strings.Contains(query, "LIMIT $4") || strings.Contains(query, "generate_series") {
				t.Fatalf("expected the block-bucketed query: %s", query)
			}
			if len(args) != 4 || args[0] != 42 || args[3] != defaultAttributionSeriesLimit {
				t.Fatalf("unexpected args: %#v", args)
			}
			rows := blobTipsTestRows(blockTime, blockTime.Add(12*time.Second))[:1]
			rows[0].BlockNumber = sql.NullInt64{Int64: 100, Valid: true}
			setSliceResult(dest, rows)
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/?range=1h&granularity=block", http.NoBody)
	w := httptest.NewRecorder()

	a.GetBlobTipsChart(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	data := decodeBlobTipsResponse(t, w)
	if len(data.Points) != 1 {
		t.Fatalf("expected 1 point, got %d", len(data.Points))
	}
	point := data.Points[0]
	if point.StartBlock == nil || *point.StartBlock != 100 || point.EndBlock == nil || *point.EndBlock != 100 {
		t.Fatalf("expected block bounds 100..100, got %+v", point)
	}
}

func TestGetBlobTipsChart_RejectsAllRange(t *testing.T) {
	a := newTestAPIWithDB(&mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			t.Fatal("range=all must be rejected before querying")
			return nil
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/?range=all", http.NoBody)
	w := httptest.NewRecorder()

	a.GetBlobTipsChart(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "range=all is not supported for blob-tips") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestGetBlobTipsChart_ErrorPaths(t *testing.T) {
	t.Run("invalid range", func(t *testing.T) {
		a := newTestAPI()
		req := httptest.NewRequest(http.MethodGet, "/?range=2h", http.NoBody)
		w := httptest.NewRecorder()
		a.GetBlobTipsChart(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", w.Code)
		}
	})

	t.Run("invalid limit", func(t *testing.T) {
		a := newTestAPI()
		req := httptest.NewRequest(http.MethodGet, "/?range=1h&limit=0", http.NoBody)
		w := httptest.NewRecorder()
		a.GetBlobTipsChart(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", w.Code)
		}
	})

	t.Run("unknown network", func(t *testing.T) {
		a := newTestAPI()
		req := httptest.NewRequest(http.MethodGet, "/?network=nope", http.NoBody)
		w := httptest.NewRecorder()
		a.GetBlobTipsChart(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", w.Code)
		}
	})

	t.Run("database error", func(t *testing.T) {
		a := newTestAPIWithDB(&mockDB{
			selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
				return errors.New("boom")
			},
		})
		req := httptest.NewRequest(http.MethodGet, "/?range=1h", http.NoBody)
		w := httptest.NewRecorder()
		a.GetBlobTipsChart(w, req)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500, got %d", w.Code)
		}
	})

	t.Run("database timeout", func(t *testing.T) {
		a := newTestAPIWithDB(&mockDB{
			selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
				return context.DeadlineExceeded
			},
		})
		req := httptest.NewRequest(http.MethodGet, "/?range=1h", http.NoBody)
		w := httptest.NewRecorder()
		a.GetBlobTipsChart(w, req)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d", w.Code)
		}
	})
}

func TestBuildBlobTipsChartResponse_NoRows(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	chart := chartRequest{Range: "1h", Granularity: "minute", BucketSeconds: 60, StartTime: now.Add(-time.Hour), EndTime: now, GeneratedAt: now}

	resp := buildBlobTipsChartResponse(42, "testnet", chart, nil)

	if len(resp.Points) != 0 || len(resp.Series) != 0 {
		t.Fatalf("expected empty points and series, got %+v", resp)
	}
	if resp.Summary.TotalBlobs != 0 || resp.Summary.AveragePriorityFeeGwei != "0" || resp.Summary.MaxPriorityFeeGwei != "0" || len(resp.Summary.Shares) != 0 {
		t.Fatalf("expected zero summary, got %+v", resp.Summary)
	}
	if !resp.StartTime.Equal(chart.StartTime) || !resp.EndTime.Equal(chart.EndTime) {
		t.Fatalf("expected request bounds, got %s .. %s", resp.StartTime, resp.EndTime)
	}
	encoded, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"series":[]`) || !strings.Contains(string(encoded), `"shares":[]`) {
		t.Fatalf("expected empty arrays rather than null on the wire: %s", encoded)
	}
}

func TestGweiOrZero(t *testing.T) {
	cases := map[string]string{
		"":              "0",
		"garbage":       "0",
		"0":             "0",
		"1500000000":    "1.5",
		"2500000000.25": "2.50000000025",
	}
	for wei, want := range cases {
		if got := gweiOrZero(wei); got != want {
			t.Errorf("gweiOrZero(%q) = %q, want %q", wei, got, want)
		}
	}
}
