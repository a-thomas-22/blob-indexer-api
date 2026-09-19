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

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	_ "github.com/a-thomas-22/blob-indexer-api/internal/testutil"
)

func builderRequest(query string) *http.Request {
	return httptest.NewRequest(http.MethodGet, "/builders"+query, http.NoBody)
}

// builderKeyRequest routes a request at GetBuilderByKey with the given raw
// {key} path param. chi hands the handler the still-escaped segment, so the
// param is set verbatim rather than through the request path.
func builderKeyRequest(key, query string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("key", key)
	req := httptest.NewRequest(http.MethodGet, "/builders/key"+query, http.NoBody)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// sampleBuilderAggregateRow is a fully populated row: every nullable block of
// the response has a value, so the builders below can blank individual
// fields to exercise the null paths.
func sampleBuilderAggregateRow() builderAggregateRow {
	return builderAggregateRow{
		BuilderKey:                "titan",
		BuilderName:               "Titan",
		FeeRecipients:             pq.StringArray{"0xaaa", "0xbbb"},
		Blocks:                    30,
		BlobBlocks:                20,
		Blobs:                     50,
		FullBlocks:                4,
		MEVBoostBlocks:            25,
		ProposerPaymentMedianWei:  sql.NullString{String: "42000000000000000", Valid: true},
		ProposerPaymentTotalWei:   sql.NullString{String: "1000000000000000000", Valid: true},
		SnapshotBlocks:            10,
		BlocksWithEligibleSkipped: 3,
		EligibleSkippedTxs:        7,
		EligibleSkippedBlobs:      9,
		EligibleSkippedMaxTipWei:  sql.NullString{String: "5000000000", Valid: true},
		TipTxCount:                40,
		TipMinWei:                 sql.NullString{String: "1000000000", Valid: true},
		TipP10Wei:                 sql.NullString{String: "1100000000", Valid: true},
		TipP50Wei:                 sql.NullString{String: "2000000000", Valid: true},
		TipP90Wei:                 sql.NullString{String: "9000000000", Valid: true},
		InclusionSampleCount:      35,
		InclusionP50Ms:            sql.NullFloat64{Float64: 4200, Valid: true},
		InclusionP90Ms:            sql.NullFloat64{Float64: 15000, Valid: true},
		TotalBlocks:               100,
		TotalBlobBlocks:           60,
		TotalBlobs:                200,
	}
}

func decodeBuilders(t *testing.T, w *httptest.ResponseRecorder) BuildersResponse {
	t.Helper()
	var resp struct {
		Success bool             `json:"success"`
		Data    BuildersResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode builders response: %v", err)
	}
	if !resp.Success {
		t.Fatal("expected success=true")
	}
	return resp.Data
}

func TestGetBuilders_Success(t *testing.T) {
	var gotArgs []interface{}
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if !strings.Contains(query, "block_builders") {
				t.Fatalf("unexpected query: %s", query)
			}
			gotArgs = args
			setSliceResult(dest, []builderAggregateRow{sampleBuilderAggregateRow()})
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	w := httptest.NewRecorder()
	a.GetBuilders(w, builderRequest("?range=24h"))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	data := decodeBuilders(t, w)
	if data.Range != "24h" || data.ChainID != 42 || data.NetworkName != "testnet" {
		t.Fatalf("unexpected identity: %+v", data)
	}
	if data.Totals != (BuilderTotals{Blocks: 100, BlobBlocks: 60, Blobs: 200}) {
		t.Errorf("totals = %+v", data.Totals)
	}
	// The window is minute-aligned and exactly one range wide.
	if got := data.Window.To.Sub(data.Window.From); got != 24*time.Hour {
		t.Errorf("window width = %v, want 24h", got)
	}
	if data.Window.To.Second() != 0 || data.Window.To.Nanosecond() != 0 {
		t.Errorf("window end %v is not minute-aligned", data.Window.To)
	}
	if len(data.Builders) != 1 {
		t.Fatalf("expected one builder, got %+v", data.Builders)
	}
	b := data.Builders[0]
	if b.Key != "titan" || b.Name != "Titan" || !b.Known {
		t.Errorf("unexpected identity: %+v", b)
	}
	if len(b.FeeRecipients) != 2 || b.FeeRecipients[0] != "0xaaa" {
		t.Errorf("fee_recipients = %v", b.FeeRecipients)
	}
	if b.BlockSharePercent != 30 || b.BlobSharePercent != 25 {
		t.Errorf("shares = %v / %v, want 30 / 25", b.BlockSharePercent, b.BlobSharePercent)
	}
	if b.AvgBlobsPerBlobBlock != 2.5 {
		t.Errorf("avg_blobs_per_blob_block = %v, want 2.5", b.AvgBlobsPerBlobBlock)
	}
	if b.ProposerPayment == nil || b.ProposerPayment.MedianEth != "0.042" || b.ProposerPayment.TotalEth != "1" {
		t.Errorf("proposer_payment = %+v", b.ProposerPayment)
	}
	if b.Tip == nil || b.Tip.TxCount != 40 || b.Tip.P50Gwei != "2" || b.Tip.MinGwei != "1" || b.Tip.P90Gwei != "9" {
		t.Errorf("tip = %+v", b.Tip)
	}
	if b.TimeToInclusionMs == nil || b.TimeToInclusionMs.P50 != 4200 || b.TimeToInclusionMs.SampleCount != 35 {
		t.Errorf("time_to_inclusion_ms = %+v", b.TimeToInclusionMs)
	}
	if b.Candidates == nil || b.Candidates.EligibleSkippedTxs != 7 || b.Candidates.EligibleSkippedMaxTipGwei != "5" {
		t.Errorf("candidates = %+v", b.Candidates)
	}

	if len(gotArgs) != 4 || gotArgs[0] != 42 || gotArgs[3] != "" {
		t.Errorf("query args = %v, want (42, start, end, \"\")", gotArgs)
	}
	wantCache := fmt.Sprintf("public, max-age=%d, s-maxage=%d",
		int(aggregateCacheTTL.Seconds()), int(aggregateEdgeTTL.Seconds()))
	if got := w.Header().Get("Cache-Control"); got != wantCache {
		t.Errorf("Cache-Control = %q, want %q", got, wantCache)
	}
}

// A builder with no sample behind a section must serialize that section as
// an explicit null, not omit it: the frontend distinguishes "no data" from
// "field missing from an older deployment".
func TestGetBuilders_NullSectionsSerializeAsNull(t *testing.T) {
	row := sampleBuilderAggregateRow()
	row.BuilderKey = "extra:geth"
	row.BuilderName = "geth"
	row.ProposerPaymentMedianWei = sql.NullString{}
	row.ProposerPaymentTotalWei = sql.NullString{}
	row.MEVBoostBlocks = 0
	row.TipTxCount = 0
	row.InclusionSampleCount = 0
	row.SnapshotBlocks = 0
	row.FeeRecipients = nil

	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			setSliceResult(dest, []builderAggregateRow{row})
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	w := httptest.NewRecorder()
	a.GetBuilders(w, builderRequest(""))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, field := range []string{"proposer_payment_wei", "tip", "time_to_inclusion_ms", "candidates"} {
		if !strings.Contains(body, `"`+field+`":null`) {
			t.Errorf("expected %q to serialize as null, body: %s", field, body)
		}
	}
	if !strings.Contains(body, `"fee_recipients":[]`) {
		t.Errorf("expected fee_recipients to serialize as [], body: %s", body)
	}
	data := decodeBuilders(t, w)
	if data.Builders[0].Known {
		t.Error("extra:-prefixed keys are registry fallbacks and must report known=false")
	}
	// Default range when none is given.
	if data.Range != "24h" {
		t.Errorf("range = %q, want the 24h default", data.Range)
	}
}

func TestGetBuilders_EmptyRange(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	w := httptest.NewRecorder()
	a.GetBuilders(w, builderRequest("?range=1h"))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"builders":[]`) {
		t.Errorf("expected builders to serialize as [], body: %s", body)
	}
	data := decodeBuilders(t, w)
	if data.Totals != (BuilderTotals{}) || len(data.Builders) != 0 {
		t.Errorf("expected a zeroed envelope, got %+v", data)
	}
}

func TestGetBuilders_BadRange(t *testing.T) {
	for _, value := range []string{"all", "90d", "1m", "1hour"} {
		t.Run(value, func(t *testing.T) {
			a := newTestAPIWithDB(&mockDB{})
			w := httptest.NewRecorder()
			a.GetBuilders(w, builderRequest("?range="+value))

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for range=%q, got %d", value, w.Code)
			}
			var resp Response
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			// The message must name every accepted value so a client that
			// sent `all` learns what to send instead.
			for _, want := range []string{"1h", "24h", "7d", "30d"} {
				if !strings.Contains(resp.Error, want) {
					t.Errorf("error %q does not name %q", resp.Error, want)
				}
			}
			if resp.ErrorCode != errCodeInvalidRequest {
				t.Errorf("error_code = %q", resp.ErrorCode)
			}
		})
	}
}

func TestGetBuilders_UnknownNetwork(t *testing.T) {
	a := newTestAPIWithDB(&mockDB{})
	a.networks = map[int]config.NetworkConfig{}
	w := httptest.NewRecorder()
	a.GetBuilders(w, builderRequest("?network=999"))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetBuilders_DBError(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return fmt.Errorf("db error")
		},
	}
	a := newTestAPIWithDB(db)
	w := httptest.NewRecorder()
	a.GetBuilders(w, builderRequest(""))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestGetBuilders_DBTimeoutIs503(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return context.DeadlineExceeded
		},
	}
	a := newTestAPIWithDB(db)
	w := httptest.NewRecorder()
	a.GetBuilders(w, builderRequest(""))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("expected Retry-After on an overload response")
	}
}

// builderDetailDB dispatches the four reads of /builders/{key} by destination
// type, so a test only has to supply the rows it cares about.
// detailFrom, when supplied, stands in for the oldest surviving candidate
// observation the skipped-detail-coverage read returns.
func builderDetailDB(aggregates []builderAggregateRow, users []builderUserRow, skipped []builderSkippedRow, blocks []builderRecentBlockRow, detailFrom ...time.Time) *mockDB {
	return &mockDB{
		getFn: func(_ context.Context, dest interface{}, query string, _ ...interface{}) error {
			target, ok := dest.(*sql.NullTime)
			if !ok {
				return fmt.Errorf("unexpected get destination %T", dest)
			}
			if query != queryBuilderSkippedDetailFrom {
				return fmt.Errorf("unexpected get query %q", query)
			}
			if len(detailFrom) > 0 {
				*target = sql.NullTime{Time: detailFrom[0], Valid: true}
			}
			return nil
		},
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			switch dest.(type) {
			case *[]builderAggregateRow:
				setSliceResult(dest, aggregates)
			case *[]builderUserRow:
				setSliceResult(dest, users)
			case *[]builderSkippedRow:
				setSliceResult(dest, skipped)
			case *[]builderRecentBlockRow:
				setSliceResult(dest, blocks)
			default:
				return fmt.Errorf("unexpected destination %T", dest)
			}
			return nil
		},
	}
}

func decodeBuilderDetail(t *testing.T, w *httptest.ResponseRecorder) BuilderDetailResponse {
	t.Helper()
	var resp struct {
		Success bool                  `json:"success"`
		Data    BuilderDetailResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode builder detail: %v", err)
	}
	if !resp.Success {
		t.Fatal("expected success=true")
	}
	return resp.Data
}

func TestGetBuilderByKey_Success(t *testing.T) {
	maxTip := "7000000000"
	payment := "42000000000000000"
	skippedTxs := 2
	blobParamsMax := 9
	db := builderDetailDB(
		[]builderAggregateRow{sampleBuilderAggregateRow()},
		[]builderUserRow{
			{
				Key:                       "fancy_rollup",
				Name:                      sql.NullString{String: "Fancy Rollup", Valid: true},
				IsEntity:                  true,
				Blobs:                     30,
				TxCount:                   12,
				TipP50Wei:                 sql.NullString{String: "2000000000", Valid: true},
				InclusionSampleCount:      11,
				InclusionP50Ms:            sql.NullFloat64{Float64: -1500, Valid: true},
				ShareWithinBuilderPercent: 60,
				ShareOverallPercent:       30,
			},
			{
				// No overall share: the sender posted nothing across the
				// window, leaving the inclusion index undefined.
				Key:                       validTestAddress,
				IsEntity:                  false,
				Blobs:                     1,
				TxCount:                   1,
				ShareWithinBuilderPercent: 2,
				ShareOverallPercent:       0,
			},
		},
		[]builderSkippedRow{{
			Key:       "other_rollup",
			Name:      sql.NullString{String: "Other Rollup", Valid: true},
			IsEntity:  true,
			Txs:       4,
			Blobs:     6,
			MaxTipWei: sql.NullString{String: maxTip, Valid: true},
			P50TipWei: sql.NullString{String: "3000000000", Valid: true},
		}},
		[]builderRecentBlockRow{{
			BlockNumber:              900,
			BlockTimestamp:           time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
			BlobCount:                6,
			BlobParamsMax:            &blobParamsMax,
			ProposerPaymentWei:       sql.NullString{String: payment, Valid: true},
			CandidateSnapshot:        true,
			EligibleSkippedTxs:       &skippedTxs,
			EligibleSkippedMaxTipWei: sql.NullString{String: maxTip, Valid: true},
		}},
	)
	a := newTestAPIWithDB(db)
	w := httptest.NewRecorder()
	a.GetBuilderByKey(w, builderKeyRequest("titan", "?range=7d"))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	data := decodeBuilderDetail(t, w)
	if data.Range != "7d" || data.Builder.Key != "titan" {
		t.Fatalf("unexpected identity: range=%q builder=%+v", data.Range, data.Builder)
	}
	if data.Totals.Blobs != 200 {
		t.Errorf("totals = %+v", data.Totals)
	}
	if len(data.Users) != 2 {
		t.Fatalf("expected 2 user rows, got %+v", data.Users)
	}
	// inclusion_index is within/overall: this builder carries twice the
	// entity's market-wide share.
	if data.Users[0].InclusionIndex == nil || *data.Users[0].InclusionIndex != 2 {
		t.Errorf("inclusion_index = %v, want 2", data.Users[0].InclusionIndex)
	}
	// A negative median wait survives: the builder had the transaction
	// before our node saw it.
	if data.Users[0].TimeToInclusionMs == nil || data.Users[0].TimeToInclusionMs.P50 != -1500 {
		t.Errorf("user time_to_inclusion_ms = %+v", data.Users[0].TimeToInclusionMs)
	}
	if data.Users[0].TipP50Gwei != "2" {
		t.Errorf("user tip_p50_gwei = %q", data.Users[0].TipP50Gwei)
	}
	if data.Users[1].InclusionIndex != nil {
		t.Errorf("expected a null inclusion index with zero overall share, got %v", *data.Users[1].InclusionIndex)
	}
	if data.Users[1].TimeToInclusionMs != nil {
		t.Errorf("expected no inclusion stats without samples, got %+v", data.Users[1].TimeToInclusionMs)
	}
	if len(data.Skipped) != 1 || data.Skipped[0].MaxTipGwei != "7" || data.Skipped[0].P50TipGwei != "3" {
		t.Errorf("skipped = %+v", data.Skipped)
	}
	if len(data.RecentBlocks) != 1 || data.RecentBlocks[0].BlockNumber != 900 ||
		data.RecentBlocks[0].ProposerPaymentWei == nil || *data.RecentBlocks[0].ProposerPaymentWei != payment {
		t.Errorf("recent_blocks = %+v", data.RecentBlocks)
	}
	if !data.RecentBlocks[0].CandidateSnapshot || data.RecentBlocks[0].EligibleSkippedTxs == nil {
		t.Errorf("recent block snapshot fields = %+v", data.RecentBlocks[0])
	}
	// No candidate rows survive in the window, so the skipped breakdown
	// covers none of it and the boundary is null rather than a made-up time.
	if data.SkippedDetailFrom != nil {
		t.Errorf("skipped_detail_from = %v, want null", data.SkippedDetailFrom)
	}
}

// candidate rows are pruned while the block_builders aggregates behind
// builder.candidates are permanent, so a long window mixes a full month of
// counts with a week of detail. skipped_detail_from is what tells a client
// where the detail actually starts.
func TestGetBuilderByKey_ReportsSkippedDetailCoverage(t *testing.T) {
	detailFrom := time.Date(2026, 6, 24, 9, 30, 0, 0, time.UTC)
	db := builderDetailDB(
		[]builderAggregateRow{sampleBuilderAggregateRow()},
		nil,
		[]builderSkippedRow{{Key: "other_rollup", Txs: 4, Blobs: 6}},
		nil,
		detailFrom,
	)
	a := newTestAPIWithDB(db)
	w := httptest.NewRecorder()
	a.GetBuilderByKey(w, builderKeyRequest("titan", "?range=30d"))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	data := decodeBuilderDetail(t, w)
	if data.SkippedDetailFrom == nil || !data.SkippedDetailFrom.Equal(detailFrom) {
		t.Fatalf("skipped_detail_from = %v, want %v", data.SkippedDetailFrom, detailFrom)
	}
	// The aggregate still describes the whole window; the two disagreeing is
	// exactly what the boundary explains.
	if data.Builder.Candidates == nil || data.Builder.Candidates.EligibleSkippedTxs == 0 {
		t.Fatalf("expected the permanent aggregate alongside the pruned detail, got %+v", data.Builder.Candidates)
	}
}

func TestGetBuilderByKey_NotFound(t *testing.T) {
	calls := 0
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			calls++
			if _, ok := dest.(*[]builderAggregateRow); !ok {
				t.Fatalf("unexpected follow-up read %T after an unknown key", dest)
			}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	w := httptest.NewRecorder()
	a.GetBuilderByKey(w, builderKeyRequest("nope", ""))

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
	// An unknown key short-circuits after the aggregate read.
	if calls != 1 {
		t.Errorf("expected 1 query for an unknown key, got %d", calls)
	}
	var resp Response
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ErrorCode != errCodeNotFound {
		t.Errorf("error_code = %q, want %q", resp.ErrorCode, errCodeNotFound)
	}
	// The 404 must not be cached: a builder that has just produced its first
	// block should resolve on the next request.
	w2 := httptest.NewRecorder()
	a.GetBuilderByKey(w2, builderKeyRequest("nope", ""))
	if calls != 2 {
		t.Errorf("expected the 404 to be recomputed, got %d total queries", calls)
	}
}

func TestGetBuilderByKey_EscapedKeyResolves(t *testing.T) {
	var gotKey interface{}
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if _, ok := dest.(*[]builderAggregateRow); ok {
				gotKey = args[3]
				row := sampleBuilderAggregateRow()
				row.BuilderKey = "extra:reth-v1-2-3"
				setSliceResult(dest, []builderAggregateRow{row})
				return nil
			}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	w := httptest.NewRecorder()
	a.GetBuilderByKey(w, builderKeyRequest("extra%3Areth-v1-2-3", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotKey != "extra:reth-v1-2-3" {
		t.Errorf("query key = %v, want the unescaped key", gotKey)
	}
}

func TestGetBuilderByKey_InvalidKey(t *testing.T) {
	for _, key := range []string{"", "   ", "%zz"} {
		t.Run(key, func(t *testing.T) {
			a := newTestAPIWithDB(&mockDB{})
			w := httptest.NewRecorder()
			a.GetBuilderByKey(w, builderKeyRequest(key, ""))

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for key %q, got %d", key, w.Code)
			}
		})
	}
}

func TestGetBuilderByKey_BadRangeAndNetwork(t *testing.T) {
	a := newTestAPIWithDB(&mockDB{})
	w := httptest.NewRecorder()
	a.GetBuilderByKey(w, builderKeyRequest("titan", "?range=all"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for range=all, got %d", w.Code)
	}

	a.networks = map[int]config.NetworkConfig{}
	w = httptest.NewRecorder()
	a.GetBuilderByKey(w, builderKeyRequest("titan", "?network=999"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unknown network, got %d", w.Code)
	}
}

func TestGetBuilderByKey_FollowUpQueryErrors(t *testing.T) {
	// Each follow-up read must surface as a 500 rather than a partially
	// populated payload.
	for _, failOn := range []string{"users", "skipped", "blocks", "detail_from"} {
		t.Run(failOn, func(t *testing.T) {
			db := &mockDB{
				getFn: func(_ context.Context, _ interface{}, _ string, _ ...interface{}) error {
					if failOn == "detail_from" {
						return fmt.Errorf("db error")
					}
					return nil
				},
				selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
					switch dest.(type) {
					case *[]builderAggregateRow:
						setSliceResult(dest, []builderAggregateRow{sampleBuilderAggregateRow()})
						return nil
					case *[]builderUserRow:
						if failOn == "users" {
							return fmt.Errorf("db error")
						}
					case *[]builderSkippedRow:
						if failOn == "skipped" {
							return fmt.Errorf("db error")
						}
					case *[]builderRecentBlockRow:
						if failOn == "blocks" {
							return fmt.Errorf("db error")
						}
					}
					return nil
				},
			}
			a := newTestAPIWithDB(db)
			w := httptest.NewRecorder()
			a.GetBuilderByKey(w, builderKeyRequest("titan", ""))
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("expected 500, got %d", w.Code)
			}
		})
	}
}

func TestGetBuilderByKey_AggregateQueryError(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return fmt.Errorf("db error")
		},
	}
	a := newTestAPIWithDB(db)
	w := httptest.NewRecorder()
	a.GetBuilderByKey(w, builderKeyRequest("titan", ""))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestBuilderKeyKnown(t *testing.T) {
	for key, want := range map[string]bool{
		"titan":                true,
		"beaverbuild":          true,
		"extra:geth":           false,
		"addr:0xabc":           false,
		"":                     true,
		"extraordinary":        true,
		"address-not-a-prefix": true,
	} {
		if got := builderKeyKnown(key); got != want {
			t.Errorf("builderKeyKnown(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestParseBuilderRange(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 34, 56, 0, time.UTC)
	for label, want := range map[string]time.Duration{
		"1h":  time.Hour,
		"24h": 24 * time.Hour,
		"7d":  7 * 24 * time.Hour,
		"30d": 30 * 24 * time.Hour,
		"":    24 * time.Hour,
		"24H": 24 * time.Hour,
	} {
		gotLabel, start, end, err := parseBuilderRange(builderRequest("?range="+label), now)
		if err != nil {
			t.Fatalf("range %q: %v", label, err)
		}
		if end.Sub(start) != want {
			t.Errorf("range %q width = %v, want %v", label, end.Sub(start), want)
		}
		if strings.TrimSpace(label) != "" && !strings.EqualFold(gotLabel, label) {
			t.Errorf("range %q echoed as %q", label, gotLabel)
		}
	}
	if _, _, _, err := parseBuilderRange(builderRequest("?range=all"), now); err == nil {
		t.Fatal("expected range=all to be rejected")
	}
}

func TestBuilderRatio(t *testing.T) {
	if got := builderRatio(5, 0); got != 0 {
		t.Errorf("builderRatio(5, 0) = %v, want 0", got)
	}
	if got := builderRatio(1, 3); got != 0.333333 {
		t.Errorf("builderRatio(1, 3) = %v, want 0.333333", got)
	}
}

func TestNullDecimalHelpers(t *testing.T) {
	if got := nullDecimal(sql.NullString{}); got != "" {
		t.Errorf("nullDecimal(NULL) = %q", got)
	}
	if got := nullDecimal(sql.NullString{String: "  7 ", Valid: true}); got != "7" {
		t.Errorf("nullDecimal trims: %q", got)
	}
	if nullDecimalPtr(sql.NullString{String: "   ", Valid: true}) != nil {
		t.Error("a blank decimal must yield a nil pointer")
	}
	if got := nullDecimalPtr(sql.NullString{String: "9", Valid: true}); got == nil || *got != "9" {
		t.Errorf("nullDecimalPtr = %v", got)
	}
}

func TestPrintableExtraDataText(t *testing.T) {
	for input, want := range map[string]string{
		"0x6265617665726275696c642e6f7267": "beaverbuild.org",
		"6265617665726275696c64":           "beaverbuild",
		"0x":                               "",
		"":                                 "",
		"0xdeadbeef":                       "", // not printable ASCII
		"0xzz":                             "", // not hex
		"0x2020":                           "", // whitespace only, trims to empty
		"0x206":                            "", // odd-length hex never decodes
		"0x546974616e2028746974616e6275696c6465722e78797a29": "Titan (titanbuilder.xyz)",
	} {
		if got := printableExtraDataText(input); got != want {
			t.Errorf("printableExtraDataText(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestToBlockBuilderResponse(t *testing.T) {
	payment := "42000000000000000"
	to := "0xproposer"
	pending := 5
	skippedTxs := 2
	skippedBlobs := 3
	maxTip := "5000000000"
	response := toBlockBuilderResponse(models.BlockBuilder{
		BlockNumber:           100,
		FeeRecipient:          "0xbuilder",
		ExtraData:             "0x6265617665726275696c642e6f7267",
		BuilderKey:            "beaverbuild",
		BuilderName:           "beaverbuild",
		TxCount:               180,
		ProposerPaymentWei:    &payment,
		ProposerPaymentTo:     &to,
		CandidateSnapshot:     true,
		PendingCandidateTxs:   &pending,
		EligibleSkippedTxs:    &skippedTxs,
		EligibleSkippedBlobs:  &skippedBlobs,
		EligibleSkippedMaxTip: &maxTip,
	})
	if !response.Known || response.ExtraDataText != "beaverbuild.org" {
		t.Errorf("unexpected response: %+v", response)
	}
	if response.ProposerPaymentEth != "0.042" || response.EligibleSkippedMaxTipGwei != "5" {
		t.Errorf("display fields = %q / %q", response.ProposerPaymentEth, response.EligibleSkippedMaxTipGwei)
	}

	// A builder row without a proposer payment leaves both the wei value and
	// its ETH companion off the wire.
	bare := toBlockBuilderResponse(models.BlockBuilder{BuilderKey: "addr:0xabc", ExtraData: "0x"})
	if bare.Known || bare.ProposerPaymentEth != "" || bare.ExtraDataText != "" {
		t.Errorf("unexpected bare response: %+v", bare)
	}
}
