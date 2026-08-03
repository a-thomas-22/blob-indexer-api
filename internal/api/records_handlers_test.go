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

	_ "github.com/a-thomas-22/blob-indexer-api/internal/testutil"
)

// recordsListCount is how many limit-sized top lists one /records response
// contains: four streak leaderboards plus base fee peaks, expensive blocks,
// busiest hours, busiest days, utilization days, and top spenders.
const recordsListCount = 10

// recordsMockDB answers the /records reads from canned rows, recording the
// limit each one was issued with.
type recordsMockDB struct {
	mockDB
	topStreaks      map[string][]recordStreakRow
	currentRuns     map[string]*recordStreakRow
	peaks           []recordBaseFeePeakRow
	expensiveBlocks []recordExpensiveBlockRow
	hours           []recordBucketRow
	days            []recordBucketRow
	utilizationDays []recordUtilizationDayRow
	spenders        []recordTopSpenderRow
	selectLimits    []int
	selectErr       error
	getErr          error
}

func newRecordsMockDB() *recordsMockDB {
	m := &recordsMockDB{
		topStreaks:  map[string][]recordStreakRow{},
		currentRuns: map[string]*recordStreakRow{},
	}
	m.selectFn = m.handleSelect
	m.getFn = m.handleGet
	return m
}

func (m *recordsMockDB) handleSelect(_ context.Context, dest interface{}, query string, args ...interface{}) error {
	if m.selectErr != nil {
		return m.selectErr
	}
	switch typed := dest.(type) {
	case *[]recordStreakRow:
		kind, _ := args[1].(string)
		m.selectLimits = append(m.selectLimits, args[2].(int))
		*typed = m.topStreaks[kind]
	case *[]recordBaseFeePeakRow:
		m.selectLimits = append(m.selectLimits, args[1].(int))
		*typed = m.peaks
	case *[]recordExpensiveBlockRow:
		m.selectLimits = append(m.selectLimits, args[1].(int))
		*typed = m.expensiveBlocks
	case *[]recordBucketRow:
		// The hourly and daily rankings share a row type, so they are told
		// apart by the bucket size baked into the query text.
		m.selectLimits = append(m.selectLimits, args[1].(int))
		if strings.Contains(query, "86400") {
			*typed = m.days
		} else {
			*typed = m.hours
		}
	case *[]recordUtilizationDayRow:
		m.selectLimits = append(m.selectLimits, args[1].(int))
		*typed = m.utilizationDays
	case *[]recordTopSpenderRow:
		m.selectLimits = append(m.selectLimits, args[1].(int))
		*typed = m.spenders
	default:
		return errors.New("unexpected select destination")
	}
	return nil
}

func (m *recordsMockDB) handleGet(_ context.Context, dest interface{}, _ string, args ...interface{}) error {
	if m.getErr != nil {
		return m.getErr
	}
	row, ok := dest.(*recordStreakRow)
	if !ok {
		return errors.New("unexpected get destination")
	}
	kind, _ := args[1].(string)
	current, ok := m.currentRuns[kind]
	if !ok || current == nil {
		return sql.ErrNoRows
	}
	*row = *current
	return nil
}

func decodeRecords(t *testing.T, w *httptest.ResponseRecorder) RecordsResponse {
	t.Helper()
	var resp struct {
		Success bool            `json:"success"`
		Data    RecordsResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Success {
		t.Fatal("expected success response")
	}
	return resp.Data
}

func seedRecordsMock(m *recordsMockDB) {
	start := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	m.topStreaks[streakKindFull] = []recordStreakRow{
		{StartBlock: 22811332, EndBlock: 22811345, Length: 14, StartTimestamp: start, EndTimestamp: start.Add(168 * time.Second)},
		{StartBlock: 22000000, EndBlock: 22000008, Length: 9, StartTimestamp: start, EndTimestamp: start.Add(96 * time.Second)},
	}
	m.topStreaks[streakKindAboveTarget] = []recordStreakRow{
		{StartBlock: 21000000, EndBlock: 21000099, Length: 100, StartTimestamp: start, EndTimestamp: start.Add(1200 * time.Second)},
	}
	m.topStreaks[streakKindDrought] = []recordStreakRow{
		{StartBlock: 20000000, EndBlock: 20000031, Length: 32, StartTimestamp: start, EndTimestamp: start.Add(384 * time.Second)},
	}
	m.topStreaks[streakKindBelowTarget] = []recordStreakRow{
		{StartBlock: 20500000, EndBlock: 20500199, Length: 200, StartTimestamp: start, EndTimestamp: start.Add(2400 * time.Second)},
	}
	m.currentRuns[streakKindAboveTarget] = &recordStreakRow{
		StartBlock: 22900000, EndBlock: 22900003, Length: 4,
		StartTimestamp: start, EndTimestamp: start.Add(36 * time.Second),
	}
	m.peaks = []recordBaseFeePeakRow{
		{BlockNumber: 19426587, BlockTimestamp: start, BlobBaseFee: "496587109376", BlobCount: 6},
	}
	m.expensiveBlocks = []recordExpensiveBlockRow{
		{BlockNumber: 19426580, BlockTimestamp: start, BlobCount: 6, BlobBaseFee: "400000000000", TotalCostWei: "314572800000000000"},
	}
	m.hours = []recordBucketRow{
		{BucketStart: start.Truncate(time.Hour), BlobCount: 4211, TotalCostWei: "18446744073709551616"},
	}
	m.days = []recordBucketRow{
		{BucketStart: start.Truncate(24 * time.Hour), BlobCount: 98431, TotalCostWei: "28446744073709551616"},
	}
	m.utilizationDays = []recordUtilizationDayRow{
		{DayStart: start.Truncate(24 * time.Hour), BlockCount: 7150, BlobCount: 39204,
			AverageUtilizationPercent: 87.42, BlocksAtMax: 1204, BlocksAboveTarget: 5310},
	}
	m.spenders = []recordTopSpenderRow{
		{FromAddress: validTestAddress, UserAttribution: "Arbitrum", BlobCount: 1284102, TotalCostWei: "18446744073709551616"},
	}
}

func TestGetBlobRecords_Success(t *testing.T) {
	db := newRecordsMockDB()
	seedRecordsMock(db)
	a := newTestAPIWithDB(db)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/records", http.NoBody)
	w := httptest.NewRecorder()
	a.GetBlobRecords(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	data := decodeRecords(t, w)

	if data.NetworkID != 42 || data.NetworkName != "testnet" {
		t.Fatalf("unexpected network identity: %+v", data)
	}
	if data.GeneratedAt.IsZero() {
		t.Fatal("expected generated_at to be set")
	}
	if len(data.FullBlockStreaks.Top) != 2 {
		t.Fatalf("expected 2 full streaks, got %d", len(data.FullBlockStreaks.Top))
	}
	top := data.FullBlockStreaks.Top[0]
	if top.Length != 14 || top.StartBlock != 22811332 || top.EndBlock != 22811345 {
		t.Fatalf("unexpected top full streak: %+v", top)
	}
	// The tip did not qualify for the full predicate, so current is null while
	// the above-target streak is still running.
	if data.FullBlockStreaks.Current != nil {
		t.Fatalf("expected a null current full streak, got %+v", data.FullBlockStreaks.Current)
	}
	if data.AboveTargetStreaks.Current == nil || data.AboveTargetStreaks.Current.Length != 4 {
		t.Fatalf("unexpected current above-target streak: %+v", data.AboveTargetStreaks.Current)
	}
	if len(data.BaseFeePeaks) != 1 || data.BaseFeePeaks[0].BlobBaseFee != "496587109376" {
		t.Fatalf("unexpected base fee peaks: %+v", data.BaseFeePeaks)
	}
	if data.BaseFeePeaks[0].BlobBaseFeeGwei != "496.587109376" {
		t.Fatalf("unexpected gwei rendering: %q", data.BaseFeePeaks[0].BlobBaseFeeGwei)
	}
	if len(data.BusiestHours) != 1 || data.BusiestHours[0].BlobCount != 4211 {
		t.Fatalf("unexpected busiest hours: %+v", data.BusiestHours)
	}
	if data.BusiestHours[0].TotalCostWei != "18446744073709551616" {
		t.Fatalf("wei total must survive as a string: %q", data.BusiestHours[0].TotalCostWei)
	}
}

// The blob-flow records page reads these exact keys; a rename silently blanks
// its historical cards, so the wire shape is asserted directly.
func TestGetBlobRecords_WireFieldNames(t *testing.T) {
	db := newRecordsMockDB()
	seedRecordsMock(db)
	a := newTestAPIWithDB(db)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/records", http.NoBody)
	w := httptest.NewRecorder()
	a.GetBlobRecords(w, req)

	var envelope struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&envelope); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	for _, key := range []string{
		"network_id", "network_name", "generated_at",
		"full_block_streaks", "above_target_streaks", "base_fee_peaks", "busiest_hours",
	} {
		if _, ok := envelope.Data[key]; !ok {
			t.Fatalf("missing top-level field %q in %v", key, envelope.Data)
		}
	}

	var streaks map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Data["full_block_streaks"], &streaks); err != nil {
		t.Fatalf("failed to decode full_block_streaks: %v", err)
	}
	for _, key := range []string{"current", "top"} {
		if _, ok := streaks[key]; !ok {
			t.Fatalf("missing streaks field %q", key)
		}
	}
	// A missing current run must serialize as null, not be omitted: the
	// frontend distinguishes "no run in progress" from "field absent".
	if string(streaks["current"]) != "null" {
		t.Fatalf("expected current to be null, got %s", streaks["current"])
	}

	var runs []map[string]json.RawMessage
	if err := json.Unmarshal(streaks["top"], &runs); err != nil {
		t.Fatalf("failed to decode top runs: %v", err)
	}
	for _, key := range []string{"length", "start_block", "end_block", "start_timestamp", "end_timestamp"} {
		if _, ok := runs[0][key]; !ok {
			t.Fatalf("missing run field %q in %v", key, runs[0])
		}
	}

	var peaks []map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Data["base_fee_peaks"], &peaks); err != nil {
		t.Fatalf("failed to decode base_fee_peaks: %v", err)
	}
	for _, key := range []string{"block_number", "timestamp", "blob_base_fee", "blob_base_fee_gwei", "blob_count"} {
		if _, ok := peaks[0][key]; !ok {
			t.Fatalf("missing peak field %q in %v", key, peaks[0])
		}
	}

	var hours []map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Data["busiest_hours"], &hours); err != nil {
		t.Fatalf("failed to decode busiest_hours: %v", err)
	}
	for _, key := range []string{"hour_start", "blob_count", "total_cost_wei"} {
		if _, ok := hours[0][key]; !ok {
			t.Fatalf("missing hour field %q in %v", key, hours[0])
		}
	}
}

func TestGetBlobRecords_TimestampsAreUTC(t *testing.T) {
	db := newRecordsMockDB()
	seedRecordsMock(db)
	// lib/pq hands back TIMESTAMP columns in the session zone; the response
	// contract is ISO-8601 UTC regardless.
	tokyo := time.FixedZone("JST", 9*3600)
	shifted := time.Date(2026, 6, 1, 21, 0, 0, 0, tokyo)
	db.topStreaks[streakKindFull][0].StartTimestamp = shifted
	db.peaks[0].BlockTimestamp = shifted
	db.hours[0].BucketStart = shifted

	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/records", http.NoBody)
	w := httptest.NewRecorder()
	a.GetBlobRecords(w, req)

	body := w.Body.String()
	if strings.Contains(body, "+09:00") {
		t.Fatalf("expected UTC timestamps, found a zone offset in %s", body)
	}
	data := decodeRecords(t, w)
	if got := data.BaseFeePeaks[0].Timestamp; got.UTC() != shifted.UTC() {
		t.Fatalf("timestamp changed instant: got %s, want %s", got, shifted.UTC())
	}
}

func TestGetBlobRecords_LimitHandling(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  int
	}{
		{name: "default", query: "", want: DefaultRecordsLimit},
		{name: "explicit", query: "?limit=25", want: 25},
		{name: "clamped up", query: "?limit=0", want: 1},
		{name: "clamped up from negative", query: "?limit=-5", want: 1},
		{name: "clamped down", query: "?limit=5000", want: MaxQueryLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newRecordsMockDB()
			a := newTestAPIWithDB(db)
			req := httptest.NewRequest(http.MethodGet, "/api/v1/records"+tc.query, http.NoBody)
			w := httptest.NewRecorder()
			a.GetBlobRecords(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
			}
			// Every top list is sized by the same limit.
			if len(db.selectLimits) != recordsListCount {
				t.Fatalf("expected %d limited reads, got %d", recordsListCount, len(db.selectLimits))
			}
			for _, got := range db.selectLimits {
				if got != tc.want {
					t.Fatalf("expected limit %d on every list, got %v", tc.want, db.selectLimits)
				}
			}
		})
	}
}

func TestGetBlobRecords_InvalidLimit(t *testing.T) {
	a := newTestAPIWithDB(newRecordsMockDB())
	req := httptest.NewRequest(http.MethodGet, "/api/v1/records?limit=abc", http.NoBody)
	w := httptest.NewRecorder()
	a.GetBlobRecords(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetBlobRecords_UnknownNetwork(t *testing.T) {
	a := newTestAPIWithDB(newRecordsMockDB())
	req := httptest.NewRequest(http.MethodGet, "/api/v1/records?network=99999", http.NoBody)
	w := httptest.NewRecorder()
	a.GetBlobRecords(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetBlobRecords_EmptyHistory(t *testing.T) {
	a := newTestAPIWithDB(newRecordsMockDB())
	req := httptest.NewRequest(http.MethodGet, "/api/v1/records", http.NoBody)
	w := httptest.NewRecorder()
	a.GetBlobRecords(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// Empty lists must serialize as [] rather than null so the frontend can
	// render "no history yet" without a null guard on every list.
	for _, want := range []string{`"top":[]`, `"base_fee_peaks":[]`, `"busiest_hours":[]`, `"current":null`} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected %s in empty response %s", want, body)
		}
	}
}

func TestGetBlobRecords_QueryError(t *testing.T) {
	db := newRecordsMockDB()
	db.selectErr = errors.New("boom")
	a := newTestAPIWithDB(db)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/records", http.NoBody)
	w := httptest.NewRecorder()
	a.GetBlobRecords(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestGetBlobRecords_CurrentStreakLookupError(t *testing.T) {
	db := newRecordsMockDB()
	db.getErr = errors.New("boom")
	a := newTestAPIWithDB(db)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/records", http.NoBody)
	w := httptest.NewRecorder()
	a.GetBlobRecords(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestGetBlobRecords_DatabaseTimeoutIsRetryable(t *testing.T) {
	db := newRecordsMockDB()
	db.selectErr = context.DeadlineExceeded
	a := newTestAPIWithDB(db)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/records", http.NoBody)
	w := httptest.NewRecorder()
	a.GetBlobRecords(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("expected a Retry-After header on an overload response")
	}
}

func TestGetBlobRecords_CachesAndInvalidatesOnNewBlock(t *testing.T) {
	db := newRecordsMockDB()
	seedRecordsMock(db)
	a := newTestAPIWithDB(db)

	call := func() {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/records", http.NoBody)
		w := httptest.NewRecorder()
		a.GetBlobRecords(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
	}

	call()
	first := len(db.selectLimits)
	call()
	if len(db.selectLimits) != first {
		t.Fatalf("expected the second call to be served from cache, got %d reads", len(db.selectLimits))
	}

	// A new block can end or extend the streak in progress, so the poller's
	// invalidation must drop the entry.
	a.invalidateBlockCaches(42)
	call()
	if len(db.selectLimits) != first*2 {
		t.Fatalf("expected a re-read after invalidation, got %d reads", len(db.selectLimits))
	}
}

func TestGetBlobRecords_CacheIsPerLimit(t *testing.T) {
	db := newRecordsMockDB()
	seedRecordsMock(db)
	a := newTestAPIWithDB(db)

	for _, url := range []string{"/api/v1/records?limit=5", "/api/v1/records?limit=7"} {
		req := httptest.NewRequest(http.MethodGet, url, http.NoBody)
		w := httptest.NewRecorder()
		a.GetBlobRecords(w, req)
	}
	if len(db.selectLimits) != recordsListCount*2 {
		t.Fatalf("expected %d reads across two distinct limits, got %v", recordsListCount*2, db.selectLimits)
	}
}

func TestGetBlobRecords_SetsCacheControl(t *testing.T) {
	a := newTestAPIWithDB(newRecordsMockDB())
	req := httptest.NewRequest(http.MethodGet, "/api/v1/records", http.NoBody)
	w := httptest.NewRecorder()
	a.GetBlobRecords(w, req)

	if got := w.Header().Get("Cache-Control"); got != "public, max-age=15, s-maxage=15" {
		t.Fatalf("unexpected Cache-Control: %q", got)
	}
}
