package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
)

func newInclusionRequest(txHash string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("txHash", txHash)
	req := httptest.NewRequest(http.MethodGet, "/blob/"+txHash+"/inclusion", http.NoBody)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func decodeInclusion(t *testing.T, w *httptest.ResponseRecorder) BlobInclusionResponse {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Success bool                  `json:"success"`
		Data    BlobInclusionResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Success {
		t.Fatal("expected success=true")
	}
	return resp.Data
}

var inclusionBlockTime = time.Date(2026, 9, 20, 3, 16, 23, 0, time.UTC)

func confirmedInclusionBlob(firstSeen *time.Time) models.Blob {
	txIndex := 325
	slot := int64(15254180)
	return models.Blob{
		ChainID:           42,
		BlockNumber:       26015973,
		TxHash:            validTestTxHash,
		FromAddress:       validTestAddress,
		BlobSizeBytes:     131072,
		BaseFeePerBlobGas: "5212673",
		TipPerBlobGas:     "42977537",
		TotalCostWei:      "683235475456",
		Timestamp:         inclusionBlockTime,
		Confirmed:         true,
		Slot:              &slot,
		TxIndex:           &txIndex,
		FirstSeenAt:       firstSeen,
	}
}

func nullTime(t time.Time) sql.NullTime { return sql.NullTime{Time: t, Valid: true} }
func nullStr(s string) sql.NullString   { return sql.NullString{String: s, Valid: true} }
func strPtr(s string) *string           { return &s }
func intPtr(i int) *int                 { return &i }
func int64Ptr(i int64) *int64           { return &i }

// includedRow is the row the blocks query's included arm yields for the
// confirmed fixture: titan built it, 4 of 9 blobs, base fee 5212673 wei.
func includedRow() blobInclusionBlockRow {
	return blobInclusionBlockRow{
		BlockNumber: 26015973, BlockTimestamp: nullTime(inclusionBlockTime), Included: true,
		FeeRecipient: nullStr("0xTitan"), ExtraData: nullStr("0x546974616e"),
		BuilderKey: nullStr("titan"), BuilderName: nullStr("Titan"), TxCount: intPtr(200),
		CandidateSnapshot: true, PendingCandidateTxs: intPtr(3),
		BlobCount: intPtr(4), BlobGasLimit: int64Ptr(1179648), BlobParamsMax: intPtr(9), BlobBaseFee: strPtr("5212673"),
	}
}

// inclusionMockDB routes the handler's three reads by destination type so a
// test can script each one independently.
type inclusionMockDB struct {
	blob    func(dest *models.Blob) error
	summary func(dest *blobInclusionSummaryRow, args []interface{}) error
	blocks  func(dest *[]blobInclusionBlockRow, args []interface{}) error
}

func (m *inclusionMockDB) db(t *testing.T) *mockDB {
	t.Helper()
	return &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			switch d := dest.(type) {
			case *models.Blob:
				if !strings.Contains(query, "FROM blobs") || args[0] != validTestTxHash || args[1] != 42 {
					t.Fatalf("unexpected blob lookup: %s %v", query, args)
				}
				return m.blob(d)
			case *blobInclusionSummaryRow:
				if !strings.Contains(query, "window_snapshot_blocks") {
					t.Fatalf("unexpected summary query: %s", query)
				}
				return m.summary(d, args)
			default:
				t.Fatalf("unexpected get destination %T", dest)
				return nil
			}
		},
		builderGetFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			t.Fatalf("the inclusion handler must not read the builder row separately: %s", query)
			return nil
		},
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			rows, ok := dest.(*[]blobInclusionBlockRow)
			if !ok || !strings.Contains(query, "UNION ALL") {
				t.Fatalf("unexpected select: %T %s", dest, query)
			}
			if args[3] != blobInclusionSkippedLimit {
				t.Fatalf("expected the skipped limit as $4, got %v", args[3])
			}
			return m.blocks(rows, args)
		},
	}
}

func TestGetBlobInclusion_ConfirmedTimeline(t *testing.T) {
	firstSeen := inclusionBlockTime.Add(-19924 * time.Millisecond)
	var summaryArgs, blockArgs []interface{}
	m := &inclusionMockDB{
		blob: func(dest *models.Blob) error {
			*dest = confirmedInclusionBlob(&firstSeen)
			return nil
		},
		summary: func(dest *blobInclusionSummaryRow, args []interface{}) error {
			summaryArgs = args
			*dest = blobInclusionSummaryRow{
				FromBlock:             sql.NullInt64{Int64: 26015971, Valid: true},
				ToBlock:               sql.NullInt64{Int64: 26015972, Valid: true},
				WindowBlocks:          2,
				WindowSnapshotBlocks:  2,
				SkippedBlocks:         2,
				EligibleSkippedBlocks: 1,
			}
			return nil
		},
		blocks: func(dest *[]blobInclusionBlockRow, args []interface{}) error {
			blockArgs = args
			// Skipped rows newest first as the query orders them, then the
			// included arm's row last: the handler must not rely on order.
			*dest = []blobInclusionBlockRow{
				{
					BlockNumber: 26015972, BlockTimestamp: nullTime(inclusionBlockTime.Add(-12 * time.Second)), Reason: "eligible",
					FeeRecipient: nullStr("0xBeaver"), ExtraData: nullStr("0x6265617665726275696c642e6f7267"),
					BuilderKey: nullStr("beaverbuild"), BuilderName: nullStr("beaverbuild"),
					TxCount: intPtr(150), ProposerPaymentWei: strPtr("10000000000000000"), ProposerPaymentTo: strPtr("0xProposer"),
					CandidateSnapshot: true, PendingCandidateTxs: intPtr(5), EligibleSkippedTxs: intPtr(2),
					EligibleSkippedBlobs: intPtr(3), EligibleSkippedMaxTip: strPtr("7000000000"),
					BlobCount: intPtr(6), BlobGasLimit: int64Ptr(1179648), BlobParamsMax: intPtr(9), BlobBaseFee: strPtr("1500000000"),
				},
				{
					// No builder row and no metrics row: the reason stands alone.
					BlockNumber: 26015971, BlockTimestamp: nullTime(inclusionBlockTime.Add(-24 * time.Second)), Reason: "too_recent",
				},
				includedRow(),
			}
			return nil
		},
	}
	a := newTestAPIWithDB(m.db(t))
	w := httptest.NewRecorder()
	a.GetBlobInclusion(w, newInclusionRequest(validTestTxHash))
	got := decodeInclusion(t, w)

	if got.ChainID != 42 || got.NetworkName != "testnet" || got.TxHash != validTestTxHash || !got.Confirmed {
		t.Errorf("unexpected envelope: %+v", got)
	}
	if got.FirstSeenAt == nil || !got.FirstSeenAt.Equal(firstSeen) {
		t.Errorf("first_seen_at = %v, want %v", got.FirstSeenAt, firstSeen)
	}
	if got.TimeToInclusionMs == nil || *got.TimeToInclusionMs != 19924 {
		t.Errorf("time_to_inclusion_ms = %v, want 19924", got.TimeToInclusionMs)
	}

	// Both reads are keyed by the stored hash and bounded by the including
	// block.
	if len(summaryArgs) != 4 || summaryArgs[0] != 42 || summaryArgs[1] != validTestTxHash {
		t.Fatalf("summary args = %v", summaryArgs)
	}
	if seen := summaryArgs[2].(sql.NullTime); !seen.Valid || !seen.Time.Equal(firstSeen) {
		t.Errorf("summary first_seen arg = %+v, want %v", seen, firstSeen)
	}
	if at := summaryArgs[3].(sql.NullInt64); !at.Valid || at.Int64 != 26015973 {
		t.Errorf("summary included-at arg = %+v, want 26015973", at)
	}
	if blockArgs[0] != 42 || blockArgs[1] != validTestTxHash {
		t.Errorf("blocks args = %v", blockArgs)
	}
	if at := blockArgs[2].(sql.NullInt64); !at.Valid || at.Int64 != 26015973 {
		t.Errorf("blocks included-at arg = %+v, want 26015973", at)
	}

	inc := got.Included
	if inc == nil {
		t.Fatal("expected included block")
	}
	if inc.BlockNumber != 26015973 || !inc.BlockTimestamp.Equal(inclusionBlockTime) {
		t.Errorf("included block = %+v", inc)
	}
	if inc.Slot == nil || *inc.Slot != 15254180 || inc.TxIndex == nil || *inc.TxIndex != 325 {
		t.Errorf("included slot/tx_index = %v/%v", inc.Slot, inc.TxIndex)
	}
	if inc.BlobCount == nil || *inc.BlobCount != 4 || inc.MaxBlobs == nil || *inc.MaxBlobs != 9 {
		t.Errorf("included occupancy = %v/%v, want 4/9", inc.BlobCount, inc.MaxBlobs)
	}
	if inc.BlobBaseFee == nil || *inc.BlobBaseFee != "5212673" || inc.BlobBaseFeeGwei != "0.005212673" {
		t.Errorf("included base fee = %v (%s)", inc.BlobBaseFee, inc.BlobBaseFeeGwei)
	}
	if inc.Builder == nil || inc.Builder.Key != "titan" || inc.Builder.ExtraDataText != "Titan" || !inc.Builder.CandidateSnapshot {
		t.Errorf("included builder = %+v", inc.Builder)
	}

	if got.Window == nil || got.Window.FromBlock != 26015971 || got.Window.ToBlock != 26015972 ||
		got.Window.Blocks != 2 || got.Window.SnapshotBlocks != 2 {
		t.Errorf("window = %+v", got.Window)
	}
	if got.SkippedBlocks != 2 || got.EligibleSkippedBlocks != 1 || got.SkippedTruncated {
		t.Errorf("counts = %d/%d truncated=%v, want 2/1 false", got.SkippedBlocks, got.EligibleSkippedBlocks, got.SkippedTruncated)
	}

	// Oldest first on the wire, with the wait measured from first seen.
	if len(got.Skipped) != 2 {
		t.Fatalf("skipped = %+v, want 2 entries", got.Skipped)
	}
	first, second := got.Skipped[0], got.Skipped[1]
	if first.BlockNumber != 26015971 || first.Reason != "too_recent" || first.Builder != nil || first.BlobCount != nil || first.BlobBaseFee != nil {
		t.Errorf("skipped[0] = %+v, want a bare too_recent entry", first)
	}
	if first.WaitedMs == nil || *first.WaitedMs != 19924-24000 {
		t.Errorf("skipped[0].waited_ms = %v, want %d", first.WaitedMs, 19924-24000)
	}
	if second.BlockNumber != 26015972 || second.Reason != "eligible" || second.WaitedMs == nil || *second.WaitedMs != 19924-12000 {
		t.Errorf("skipped[1] = %+v", second)
	}
	if second.BlobCount == nil || *second.BlobCount != 6 || second.MaxBlobs == nil || *second.MaxBlobs != 9 || second.BlobBaseFeeGwei != "1.5" {
		t.Errorf("skipped[1] occupancy = %v/%v @ %s", second.BlobCount, second.MaxBlobs, second.BlobBaseFeeGwei)
	}
	b := second.Builder
	if b == nil || b.Key != "beaverbuild" || b.Name != "beaverbuild" || b.FeeRecipient != "0xBeaver" || b.ExtraDataText != "beaverbuild.org" ||
		b.TxCount != 150 || b.ProposerPaymentEth != "0.01" || b.ProposerPaymentTo == nil || *b.ProposerPaymentTo != "0xProposer" ||
		!b.CandidateSnapshot || b.PendingCandidateTxs == nil || *b.PendingCandidateTxs != 5 ||
		b.EligibleSkippedTxs == nil || *b.EligibleSkippedTxs != 2 || b.EligibleSkippedBlobs == nil || *b.EligibleSkippedBlobs != 3 ||
		b.EligibleSkippedMaxTipGwei != "7" {
		t.Errorf("skipped[1].builder = %+v", b)
	}

	wantCache := fmt.Sprintf("public, max-age=%d, s-maxage=%d",
		int(confirmedBlobCacheTTL.Seconds()), int(confirmedBlobEdgeTTL.Seconds()))
	if cc := w.Header().Get("Cache-Control"); cc != wantCache {
		t.Errorf("Cache-Control = %q, want %q", cc, wantCache)
	}
}

func TestGetBlobInclusion_PendingTimeline(t *testing.T) {
	seen := inclusionBlockTime.Add(-5 * time.Second)
	m := &inclusionMockDB{
		blob: func(dest *models.Blob) error {
			*dest = models.Blob{
				ChainID: 42, BlockNumber: models.PendingBlockNumber, TxHash: validTestTxHash,
				FromAddress: validTestAddress, Timestamp: seen, FirstSeenAt: &seen, Confirmed: false,
			}
			return nil
		},
		summary: func(dest *blobInclusionSummaryRow, args []interface{}) error {
			if at := args[3].(sql.NullInt64); at.Valid {
				t.Errorf("pending summary must pass a NULL including block, got %+v", at)
			}
			*dest = blobInclusionSummaryRow{
				FromBlock: sql.NullInt64{Int64: 500, Valid: true}, ToBlock: sql.NullInt64{Int64: 500, Valid: true},
				WindowBlocks: 1, WindowSnapshotBlocks: 1, SkippedBlocks: 1,
			}
			return nil
		},
		blocks: func(dest *[]blobInclusionBlockRow, args []interface{}) error {
			if at := args[2].(sql.NullInt64); at.Valid {
				t.Errorf("pending blocks read must pass a NULL including block, got %+v", at)
			}
			*dest = []blobInclusionBlockRow{{BlockNumber: 500, BlockTimestamp: nullTime(inclusionBlockTime), Reason: "nonce_gap"}}
			return nil
		},
	}
	a := newTestAPIWithDB(m.db(t))
	w := httptest.NewRecorder()
	a.GetBlobInclusion(w, newInclusionRequest(validTestTxHash))
	got := decodeInclusion(t, w)

	if got.Confirmed || got.Included != nil || got.TimeToInclusionMs != nil {
		t.Errorf("pending transaction must have no inclusion: %+v", got)
	}
	if got.FirstSeenAt == nil || !got.FirstSeenAt.Equal(seen) {
		t.Errorf("first_seen_at = %v, want %v", got.FirstSeenAt, seen)
	}
	if got.Window == nil || got.Window.FromBlock != 500 || got.Window.ToBlock != 500 {
		t.Errorf("window = %+v", got.Window)
	}
	if len(got.Skipped) != 1 || got.Skipped[0].Reason != "nonce_gap" || got.Skipped[0].WaitedMs == nil || *got.Skipped[0].WaitedMs != 5000 {
		t.Errorf("skipped = %+v", got.Skipped)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "" {
		t.Errorf("pending timeline must not be cached, got %q", cc)
	}
}

func TestGetBlobInclusion_NoFirstSeenLeavesWaitUnknown(t *testing.T) {
	m := &inclusionMockDB{
		blob: func(dest *models.Blob) error {
			*dest = confirmedInclusionBlob(nil)
			return nil
		},
		summary: func(dest *blobInclusionSummaryRow, args []interface{}) error {
			if seen := args[2].(sql.NullTime); seen.Valid {
				t.Errorf("unknown first_seen must be passed as NULL, got %+v", seen)
			}
			// Nothing bounds the window.
			*dest = blobInclusionSummaryRow{SkippedBlocks: 1}
			return nil
		},
		blocks: func(dest *[]blobInclusionBlockRow, args []interface{}) error {
			// The included arm with both joins missing carries no timestamp;
			// plus a stray row with no first-seen time to measure against.
			*dest = []blobInclusionBlockRow{
				{BlockNumber: 26015973, Included: true},
				{BlockNumber: 26015972, BlockTimestamp: nullTime(inclusionBlockTime), Reason: "eligible"},
			}
			return nil
		},
	}
	a := newTestAPIWithDB(m.db(t))
	w := httptest.NewRecorder()
	a.GetBlobInclusion(w, newInclusionRequest(validTestTxHash))
	got := decodeInclusion(t, w)

	if got.FirstSeenAt != nil || got.TimeToInclusionMs != nil {
		t.Errorf("expected no wait fields, got %+v", got)
	}
	if got.Window != nil {
		t.Errorf("expected no window, got %+v", got.Window)
	}
	if got.Included == nil || got.Included.Builder != nil || got.Included.BlobCount != nil || !got.Included.BlockTimestamp.Equal(inclusionBlockTime) {
		t.Errorf("included should carry the block alone, timestamped from the blob row: %+v", got.Included)
	}
	if len(got.Skipped) != 1 || got.Skipped[0].WaitedMs != nil {
		t.Errorf("waited_ms needs first_seen_at: %+v", got.Skipped)
	}
}

func TestGetBlobInclusion_TruncatedSkippedList(t *testing.T) {
	seen := inclusionBlockTime.Add(-time.Hour)
	m := &inclusionMockDB{
		blob: func(dest *models.Blob) error {
			*dest = confirmedInclusionBlob(&seen)
			return nil
		},
		summary: func(dest *blobInclusionSummaryRow, args []interface{}) error {
			*dest = blobInclusionSummaryRow{
				FromBlock: sql.NullInt64{Int64: 26015000, Valid: true}, ToBlock: sql.NullInt64{Int64: 26015972, Valid: true},
				WindowBlocks: 973, WindowSnapshotBlocks: 973, SkippedBlocks: 973, EligibleSkippedBlocks: 973,
			}
			return nil
		},
		blocks: func(dest *[]blobInclusionBlockRow, args []interface{}) error {
			rows := make([]blobInclusionBlockRow, 0, blobInclusionSkippedLimit+1)
			for i := 0; i < blobInclusionSkippedLimit; i++ {
				rows = append(rows, blobInclusionBlockRow{
					BlockNumber: 26015972 - int64(i), BlockTimestamp: nullTime(inclusionBlockTime), Reason: "eligible",
				})
			}
			rows = append(rows, includedRow())
			*dest = rows
			return nil
		},
	}
	a := newTestAPIWithDB(m.db(t))
	w := httptest.NewRecorder()
	a.GetBlobInclusion(w, newInclusionRequest(validTestTxHash))
	got := decodeInclusion(t, w)

	if !got.SkippedTruncated || got.SkippedBlocks != 973 || len(got.Skipped) != blobInclusionSkippedLimit {
		t.Errorf("truncated=%v skipped_blocks=%d len=%d", got.SkippedTruncated, got.SkippedBlocks, len(got.Skipped))
	}
	// Oldest first, and the retained entries are the newest ones.
	if got.Skipped[0].BlockNumber != 26015972-int64(blobInclusionSkippedLimit)+1 || got.Skipped[len(got.Skipped)-1].BlockNumber != 26015972 {
		t.Errorf("skipped spans %d..%d", got.Skipped[0].BlockNumber, got.Skipped[len(got.Skipped)-1].BlockNumber)
	}
	if got.Included == nil || got.Included.BlockNumber != 26015973 {
		t.Errorf("included = %+v", got.Included)
	}
}

func TestGetBlobInclusion_EmptySkippedIsArray(t *testing.T) {
	m := &inclusionMockDB{
		blob: func(dest *models.Blob) error {
			*dest = confirmedInclusionBlob(nil)
			return nil
		},
		summary: func(dest *blobInclusionSummaryRow, args []interface{}) error { return nil },
		blocks:  func(dest *[]blobInclusionBlockRow, args []interface{}) error { return nil },
	}
	a := newTestAPIWithDB(m.db(t))
	w := httptest.NewRecorder()
	a.GetBlobInclusion(w, newInclusionRequest(validTestTxHash))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// A confirmed transaction whose blocks read returned nothing still gets
	// its included block, known from the blob row.
	for _, want := range []string{`"skipped":[]`, `"window":null`, `"included":{"block_number":26015973`, `"skipped_truncated":false`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s: %s", want, body)
		}
	}
}

func TestGetBlobInclusion_NotFound(t *testing.T) {
	m := &inclusionMockDB{
		blob: func(dest *models.Blob) error { return sql.ErrNoRows },
	}
	a := newTestAPIWithDB(m.db(t))
	w := httptest.NewRecorder()
	a.GetBlobInclusion(w, newInclusionRequest(validTestTxHash))
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetBlobInclusion_InvalidHash(t *testing.T) {
	a := newTestAPI()
	for _, hash := range []string{"", "abc", "0x1234", "0x" + strings.Repeat("zz", 32)} {
		w := httptest.NewRecorder()
		a.GetBlobInclusion(w, newInclusionRequest(hash))
		if w.Code != http.StatusBadRequest {
			t.Errorf("hash %q: expected 400, got %d", hash, w.Code)
		}
	}
}

func TestGetBlobInclusion_BadNetwork(t *testing.T) {
	a := newTestAPI()
	req := newInclusionRequest(validTestTxHash)
	q := req.URL.Query()
	q.Set("network", "nope")
	req.URL.RawQuery = q.Encode()
	w := httptest.NewRecorder()
	a.GetBlobInclusion(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetBlobInclusion_DBErrors(t *testing.T) {
	boom := errors.New("boom")
	ok := func() *inclusionMockDB {
		return &inclusionMockDB{
			blob: func(dest *models.Blob) error {
				*dest = confirmedInclusionBlob(nil)
				return nil
			},
			summary: func(dest *blobInclusionSummaryRow, args []interface{}) error { return nil },
			blocks:  func(dest *[]blobInclusionBlockRow, args []interface{}) error { return nil },
		}
	}
	cases := map[string]func(m *inclusionMockDB){
		"blob": func(m *inclusionMockDB) { m.blob = func(*models.Blob) error { return boom } },
		"summary": func(m *inclusionMockDB) {
			m.summary = func(*blobInclusionSummaryRow, []interface{}) error { return boom }
		},
		"blocks": func(m *inclusionMockDB) {
			m.blocks = func(*[]blobInclusionBlockRow, []interface{}) error { return boom }
		},
	}
	for name, breakStep := range cases {
		t.Run(name, func(t *testing.T) {
			m := ok()
			breakStep(m)
			a := newTestAPIWithDB(m.db(t))
			w := httptest.NewRecorder()
			a.GetBlobInclusion(w, newInclusionRequest(validTestTxHash))
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
			}
			if cc := w.Header().Get("Cache-Control"); cc != "" {
				t.Errorf("errors must not be cached, got %q", cc)
			}
		})
	}
}

func TestToBlobInclusionBlock_OccupancyFallbacks(t *testing.T) {
	// blob_params_max NULL falls back to the gas limit; a blank base fee is
	// omitted rather than rendered as zero gwei; a missing timestamp takes
	// the fallback.
	row := blobInclusionBlockRow{
		BlockNumber: 7, Reason: "no_room",
		BlobCount: intPtr(6), BlobGasLimit: int64Ptr(786432), BlobBaseFee: strPtr(" "),
	}
	got := toBlobInclusionBlock(row, inclusionBlockTime)
	if got.MaxBlobs == nil || *got.MaxBlobs != 6 {
		t.Errorf("max_blobs = %v, want 6 from the gas limit", got.MaxBlobs)
	}
	if got.BlobBaseFee != nil || got.BlobBaseFeeGwei != "" {
		t.Errorf("blank base fee should be omitted, got %v/%q", got.BlobBaseFee, got.BlobBaseFeeGwei)
	}
	if !got.BlockTimestamp.Equal(inclusionBlockTime) {
		t.Errorf("timestamp = %v, want the fallback", got.BlockTimestamp)
	}
	skipped := toBlobInclusionSkipped(row, nil)
	if skipped.WaitedMs != nil || skipped.Reason != "no_room" {
		t.Errorf("no first-seen time: waited_ms should be nil, got %+v", skipped)
	}
}
