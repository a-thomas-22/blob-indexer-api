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

	_ "github.com/a-thomas-22/blob-indexer-api/internal/testutil"
)

func builderShareRequest(query string) *http.Request {
	return httptest.NewRequest(http.MethodGet, "/charts/builder-share"+query, http.NoBody)
}

func decodeBuilderShare(t *testing.T, w *httptest.ResponseRecorder) BuilderShareChartResponse {
	t.Helper()
	var resp struct {
		Success bool                      `json:"success"`
		Data    BuilderShareChartResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode builder share chart: %v", err)
	}
	if !resp.Success {
		t.Fatal("expected success=true")
	}
	return resp.Data
}

// builderShareRow builds one (bucket, series) row of the chart query result.
func builderShareRow(bucket time.Time, key, name string, blocks, blobs, bucketBlocks, bucketBlobs, totalBlocks, totalBlobs int64) builderShareChartRow {
	row := builderShareChartRow{
		Timestamp:          bucket,
		RangeStart:         bucket.Add(-time.Hour),
		RangeEnd:           bucket.Add(time.Hour),
		BucketBlocks:       bucketBlocks,
		BucketBlobs:        bucketBlobs,
		SeriesBlocks:       blocks,
		SeriesBlobs:        blobs,
		SummaryTotalBlocks: totalBlocks,
		SummaryTotalBlobs:  totalBlobs,
	}
	if key != "" {
		row.SeriesKey = sql.NullString{String: key, Valid: true}
		row.SeriesName = sql.NullString{String: name, Valid: true}
	}
	return row
}

func TestGetBuilderShareChart_Success(t *testing.T) {
	bucket1 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	bucket2 := bucket1.Add(5 * time.Minute)
	rows := []builderShareChartRow{
		builderShareRow(bucket1, "titan", "Titan", 3, 12, 5, 20, 10, 40),
		builderShareRow(bucket1, "other", "Other", 2, 8, 5, 20, 10, 40),
		// Second bucket carries only Titan, so the 'other' series must be
		// zero-filled there rather than absent.
		builderShareRow(bucket2, "titan", "Titan", 5, 20, 5, 20, 10, 40),
	}
	var gotArgs []interface{}
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if !strings.Contains(query, "block_builders") {
				t.Fatalf("unexpected query: %s", query)
			}
			gotArgs = args
			setSliceResult(dest, rows)
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	w := httptest.NewRecorder()
	a.GetBuilderShareChart(w, builderShareRequest("?range=24h"))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	data := decodeBuilderShare(t, w)
	if data.Range != "24h" || data.ChainID != 42 {
		t.Fatalf("unexpected identity: %+v", data)
	}
	if data.Summary.TotalBlocks != 10 || data.Summary.TotalBlobs != 40 {
		t.Errorf("summary totals = %+v", data.Summary)
	}
	if len(data.Points) != 2 {
		t.Fatalf("expected 2 points, got %d", len(data.Points))
	}
	// Every series carries a value in every bucket.
	for i, point := range data.Points {
		if len(point.Values) != 2 {
			t.Errorf("point %d values = %+v, want both series", i, point.Values)
		}
	}
	if got := data.Points[1].Values["other"]; got != (BuilderShareValue{}) {
		t.Errorf("expected 'other' zero-filled in the second bucket, got %+v", got)
	}
	if got := data.Points[0].Values["titan"]; got.Blocks != 3 || got.Blobs != 12 {
		t.Errorf("titan bucket value = %+v", got)
	}

	// The long-tail bucket always sorts last, even when it outweighs a named
	// builder, so clients can render it as the tail it represents.
	if len(data.Series) != 2 || data.Series[0].Key != "titan" || data.Series[1].Key != "other" {
		t.Fatalf("series order = %+v", data.Series)
	}
	if !data.Series[0].Known {
		t.Error("titan is a registry match and must report known=true")
	}
	if data.Series[1].Known {
		t.Error("the aggregate 'other' series must never report known=true")
	}
	if len(data.Summary.Shares) != 2 {
		t.Fatalf("shares = %+v", data.Summary.Shares)
	}
	titan := data.Summary.Shares[0]
	if titan.Blocks != 8 || titan.Blobs != 32 || titan.BlockSharePercent != 80 || titan.BlobSharePercent != 80 {
		t.Errorf("titan share = %+v", titan)
	}

	// Time granularity passes (chain, start, end, bucket seconds, limit).
	if len(gotArgs) != 5 || gotArgs[0] != 42 || gotArgs[4] != defaultBuilderSeriesLimit {
		t.Errorf("query args = %v", gotArgs)
	}
	wantCache := fmt.Sprintf("public, max-age=%d, s-maxage=%d",
		int(aggregateCacheTTL.Seconds()), int(aggregateEdgeTTL.Seconds()))
	if got := w.Header().Get("Cache-Control"); got != wantCache {
		t.Errorf("Cache-Control = %q, want %q", got, wantCache)
	}
}

func TestGetBuilderShareChart_BlockGranularity(t *testing.T) {
	bucket := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	row := builderShareRow(bucket, "titan", "Titan", 1, 4, 1, 4, 1, 4)
	row.BlockNumber = sql.NullInt64{Int64: 900, Valid: true}

	var gotArgs []interface{}
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			gotArgs = args
			setSliceResult(dest, []builderShareChartRow{row})
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	w := httptest.NewRecorder()
	a.GetBuilderShareChart(w, builderShareRequest("?range=1h&granularity=block"))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	data := decodeBuilderShare(t, w)
	if len(data.Points) != 1 || data.Points[0].StartBlock == nil || *data.Points[0].StartBlock != 900 {
		t.Fatalf("expected one block-keyed point, got %+v", data.Points)
	}
	// Block granularity drops the bucket-seconds argument.
	if len(gotArgs) != 4 {
		t.Errorf("query args = %v, want 4 for block granularity", gotArgs)
	}
}

func TestGetBuilderShareChart_RejectsRangeAll(t *testing.T) {
	a := newTestAPIWithDB(&mockDB{})
	w := httptest.NewRecorder()
	a.GetBuilderShareChart(w, builderShareRequest("?range=all"))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	var resp Response
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, want := range []string{"1h", "24h", "7d", "30d"} {
		if !strings.Contains(resp.Error, want) {
			t.Errorf("error %q does not name %q", resp.Error, want)
		}
	}
}

func TestGetBuilderShareChart_BadParams(t *testing.T) {
	testCases := []struct {
		name  string
		query string
	}{
		{name: "granularity", query: "?granularity=fortnight"},
		{name: "limit", query: "?limit=0"},
		{name: "limit-non-numeric", query: "?limit=lots"},
		{name: "network", query: "?network=999"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestAPIWithDB(&mockDB{})
			if tc.name == "network" {
				a.networks = nil
			}
			w := httptest.NewRecorder()
			a.GetBuilderShareChart(w, builderShareRequest(tc.query))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d", w.Code)
			}
		})
	}
}

func TestGetBuilderShareChart_DBError(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return fmt.Errorf("db error")
		},
	}
	a := newTestAPIWithDB(db)
	w := httptest.NewRecorder()
	a.GetBuilderShareChart(w, builderShareRequest(""))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestGetBuilderShareChart_Empty(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	w := httptest.NewRecorder()
	a.GetBuilderShareChart(w, builderShareRequest("?range=1h"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{`"series":[]`, `"points":[]`, `"shares":[]`} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %s in an empty chart, body: %s", want, body)
		}
	}
}

// A bucket row with no series (an empty bucket in the generate_series spine)
// still produces a point, and a series row whose block count is zero is not
// promoted into the series list.
func TestBuildBuilderShareChartResponse_EmptyBuckets(t *testing.T) {
	bucket := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	chart := chartRequest{Range: "1h", Granularity: "minute", BucketSeconds: 60}
	rows := []builderShareChartRow{
		builderShareRow(bucket, "", "", 0, 0, 0, 0, 3, 9),
		builderShareRow(bucket.Add(time.Minute), "titan", "Titan", 3, 9, 3, 9, 3, 9),
		// Defensive: a zero-block series row is ignored rather than creating
		// a phantom series.
		builderShareRow(bucket.Add(time.Minute), "ghost", "Ghost", 0, 0, 3, 9, 3, 9),
	}
	response := buildBuilderShareChartResponse(42, "testnet", chart, rows)
	if len(response.Points) != 2 {
		t.Fatalf("expected 2 points, got %d", len(response.Points))
	}
	if len(response.Series) != 1 || response.Series[0].Key != "titan" {
		t.Fatalf("series = %+v", response.Series)
	}
	if got := response.Points[0].Values["titan"]; got != (BuilderShareValue{}) {
		t.Errorf("empty bucket should zero-fill titan, got %+v", got)
	}
}

func TestParseBuilderSeriesLimit(t *testing.T) {
	for raw, want := range map[string]int{
		"":   defaultBuilderSeriesLimit,
		"  ": defaultBuilderSeriesLimit,
		"3":  3,
		"99": maxBuilderSeriesLimit,
	} {
		got, err := parseBuilderSeriesLimit(raw)
		if err != nil {
			t.Fatalf("parseBuilderSeriesLimit(%q): %v", raw, err)
		}
		if got != want {
			t.Errorf("parseBuilderSeriesLimit(%q) = %d, want %d", raw, got, want)
		}
	}
	for _, raw := range []string{"0", "-1", "many"} {
		if _, err := parseBuilderSeriesLimit(raw); err == nil {
			t.Errorf("expected parseBuilderSeriesLimit(%q) to fail", raw)
		}
	}
}
