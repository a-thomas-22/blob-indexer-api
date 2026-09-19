package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	_ "github.com/a-thomas-22/blob-indexer-api/internal/testutil"
)

func testBlockBuilderRow(blockNumber int64) models.BlockBuilder {
	payment := "42000000000000000"
	to := "0xProposer"
	pending := 5
	skippedTxs := 2
	skippedBlobs := 3
	maxTip := "5000000000"
	return models.BlockBuilder{
		ChainID:               42,
		BlockNumber:           blockNumber,
		BlockTimestamp:        time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		FeeRecipient:          "0xBuilder",
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
	}
}

func testCandidateRow() models.BlobInclusionCandidate {
	nonce := int64(7)
	tip := "3000000000"
	maxFee := "40000000000"
	blobFee := "1000000000"
	return models.BlobInclusionCandidate{
		ChainID:              42,
		BlockNumber:          100,
		BlockTimestamp:       time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		TxHash:               validTestTxHash,
		FromAddress:          validTestAddress,
		UserAttribution:      "Fancy Rollup",
		Nonce:                &nonce,
		BlobCount:            2,
		MaxPriorityFeePerGas: &tip,
		MaxFeePerGas:         &maxFee,
		MaxFeePerBlobGas:     &blobFee,
		FirstSeenAt:          time.Date(2026, 6, 30, 23, 59, 55, 0, time.UTC),
		Reason:               models.CandidateEligible,
	}
}

// blockDetailDB serves the metrics, blobs, builder, and candidate reads of
// /block/{number}.
func blockDetailDB(builder *models.BlockBuilder, candidates []models.BlobInclusionCandidate) *mockDB {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			setStructResult(dest, testBlockMetrics(100, 1))
			return nil
		},
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			setSliceResult(dest, []models.Blob{blockTestBlob(100, 0)})
			return nil
		},
		builderSelectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if _, ok := dest.(*[]models.BlobInclusionCandidate); ok {
				setSliceResult(dest, candidates)
			}
			return nil
		},
	}
	if builder != nil {
		db.builderGetFn = func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			setStructResult(dest, *builder)
			return nil
		}
	}
	return db
}

func decodeBlockDetail(t *testing.T, w *httptest.ResponseRecorder) BlockDetailResponse {
	t.Helper()
	var resp struct {
		Success bool                `json:"success"`
		Data    BlockDetailResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode block detail: %v", err)
	}
	if !resp.Success {
		t.Fatal("expected success=true")
	}
	return resp.Data
}

func TestGetBlockByNumber_WithBuilderAndCandidates(t *testing.T) {
	builder := testBlockBuilderRow(100)
	db := blockDetailDB(&builder, []models.BlobInclusionCandidate{testCandidateRow()})
	a := newTestAPIWithDB(db)
	w := httptest.NewRecorder()
	a.GetBlockByNumber(w, newBlockRequest("100"))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	data := decodeBlockDetail(t, w)
	if data.Builder == nil {
		t.Fatal("expected a builder object")
	}
	if data.Builder.Key != "beaverbuild" || !data.Builder.Known || data.Builder.ExtraDataText != "beaverbuild.org" {
		t.Errorf("builder = %+v", data.Builder)
	}
	if data.Builder.TxCount != 180 || data.Builder.ProposerPaymentEth != "0.042" {
		t.Errorf("builder payment fields = %+v", data.Builder)
	}
	if !data.Builder.CandidateSnapshot || data.Builder.PendingCandidateTxs == nil || *data.Builder.PendingCandidateTxs != 5 {
		t.Errorf("builder snapshot fields = %+v", data.Builder)
	}
	if len(data.Candidates) != 1 {
		t.Fatalf("expected one candidate, got %+v", data.Candidates)
	}
	candidate := data.Candidates[0]
	if candidate.Reason != models.CandidateEligible || candidate.BlobCount != 2 ||
		candidate.MaxPriorityFeePerGasGwei != "3" || candidate.Nonce == nil || *candidate.Nonce != 7 {
		t.Errorf("candidate = %+v", candidate)
	}
	// The block payload keeps the WebSocket shape, so the shared fields must
	// still be present alongside the REST-only candidates list.
	if data.BlockNumber != 100 || data.Pricing == nil || len(data.Blobs) != 1 {
		t.Errorf("shared new_block fields = %+v", data.NewBlockData)
	}
}

// A block with no builder row omits the object entirely and still serves an
// explicit (empty) candidates list, so clients can branch on presence.
func TestGetBlockByNumber_WithoutBuilderRow(t *testing.T) {
	db := blockDetailDB(nil, nil)
	a := newTestAPIWithDB(db)
	w := httptest.NewRecorder()
	a.GetBlockByNumber(w, newBlockRequest("100"))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, `"builder"`) {
		t.Errorf("expected builder to be omitted, body: %s", body)
	}
	if !strings.Contains(body, `"candidates":[]`) {
		t.Errorf("expected candidates to serialize as [], body: %s", body)
	}
}

func TestGetBlockByNumber_BuilderReadErrors(t *testing.T) {
	testCases := []struct {
		name string
		db   *mockDB
	}{
		{
			name: "builder",
			db: func() *mockDB {
				db := blockDetailDB(nil, nil)
				db.builderGetFn = func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
					return fmt.Errorf("db error")
				}
				return db
			}(),
		},
		{
			name: "candidates",
			db: func() *mockDB {
				builder := testBlockBuilderRow(100)
				db := blockDetailDB(&builder, nil)
				db.builderSelectFn = func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
					return fmt.Errorf("db error")
				}
				return db
			}(),
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestAPIWithDB(tc.db)
			w := httptest.NewRecorder()
			a.GetBlockByNumber(w, newBlockRequest("100"))
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("expected 500, got %d", w.Code)
			}
		})
	}
}

// The torn-read retry predates builder attribution and must keep working:
// the builder and candidate reads happen after the retry loop settles, so a
// persistently torn pair is still served uncached, with its builder.
func TestGetBlockByNumber_TornReadKeepsBuilderUncached(t *testing.T) {
	builder := testBlockBuilderRow(100)
	selectCalls := 0
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			setStructResult(dest, testBlockMetrics(100, 2))
			return nil
		},
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			selectCalls++
			setSliceResult(dest, []models.Blob{blockTestBlob(100, 0)})
			return nil
		},
		builderGetFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			setStructResult(dest, builder)
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	w := httptest.NewRecorder()
	a.GetBlockByNumber(w, newBlockRequest("100"))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if selectCalls != 2 {
		t.Errorf("expected the torn pair to be retried once, got %d blob reads", selectCalls)
	}
	if got := w.Header().Get("Cache-Control"); got != "" {
		t.Errorf("expected no Cache-Control on a torn payload, got %q", got)
	}
	data := decodeBlockDetail(t, w)
	if data.Builder == nil || data.Builder.Key != "beaverbuild" {
		t.Errorf("expected the builder to survive a torn read, got %+v", data.Builder)
	}
}

// The WebSocket reconnect snapshot carries the same builder object as the
// live new_block event, keyed by block.
func TestBuildBlockSnapshot_IncludesBuilders(t *testing.T) {
	builder := testBlockBuilderRow(100)
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			switch dest.(type) {
			case *[]models.BlockMetrics:
				setSliceResult(dest, []models.BlockMetrics{testBlockMetrics(100, 1), testBlockMetrics(101, 1)})
			case *[]models.Blob:
				setSliceResult(dest, []models.Blob{blockTestBlob(100, 0), blockTestBlob(101, 0)})
			}
			return nil
		},
		builderSelectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if _, ok := dest.(*[]models.BlockBuilder); ok {
				setSliceResult(dest, []models.BlockBuilder{builder})
			}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	snapshot, err := a.buildBlockSnapshot(context.Background(), config.NetworkConfig{Name: "testnet", ChainID: 42})
	if err != nil {
		t.Fatalf("buildBlockSnapshot: %v", err)
	}
	if len(snapshot.Blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(snapshot.Blocks))
	}
	if snapshot.Blocks[0].Builder == nil || snapshot.Blocks[0].Builder.Key != "beaverbuild" {
		t.Errorf("block 100 builder = %+v", snapshot.Blocks[0].Builder)
	}
	// A block the backfill has not reached simply carries no builder.
	if snapshot.Blocks[1].Builder != nil {
		t.Errorf("block 101 should have no builder, got %+v", snapshot.Blocks[1].Builder)
	}
}

func TestBuildBlockSnapshot_BuilderQueryError(t *testing.T) {
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if _, ok := dest.(*[]models.BlockMetrics); ok {
				setSliceResult(dest, []models.BlockMetrics{testBlockMetrics(100, 0)})
			}
			return nil
		},
		builderSelectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return fmt.Errorf("db error")
		},
	}
	a := newTestAPIWithDB(db)
	if _, err := a.buildBlockSnapshot(context.Background(), config.NetworkConfig{Name: "testnet", ChainID: 42}); err == nil {
		t.Fatal("expected the builder read failure to surface")
	}
}

func TestToBlobInclusionCandidateResponses_OmitsMissingCaps(t *testing.T) {
	bare := models.BlobInclusionCandidate{
		TxHash:      validTestTxHash,
		FromAddress: validTestAddress,
		BlobCount:   1,
		FirstSeenAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		Reason:      models.CandidateTooRecent,
	}
	responses := toBlobInclusionCandidateResponses([]models.BlobInclusionCandidate{bare})
	if len(responses) != 1 {
		t.Fatalf("expected one response, got %d", len(responses))
	}
	got := responses[0]
	if got.MaxPriorityFeePerGas != nil || got.MaxPriorityFeePerGasGwei != "" ||
		got.MaxFeePerGas != nil || got.MaxFeePerBlobGas != nil || got.Nonce != nil {
		t.Errorf("expected the missing caps to stay unset, got %+v", got)
	}
	if got.Reason != models.CandidateTooRecent {
		t.Errorf("reason = %q", got.Reason)
	}
	// An empty candidate list still serializes as an array, never null.
	payload, err := json.Marshal(toBlobInclusionCandidateResponses(nil))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(payload) != "[]" {
		t.Errorf("empty candidates = %s, want []", payload)
	}
}

func TestBlockBuildersByNumber(t *testing.T) {
	byNumber := blockBuildersByNumber([]models.BlockBuilder{testBlockBuilderRow(100), testBlockBuilderRow(101)})
	if len(byNumber) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(byNumber))
	}
	if _, ok := byNumber[100]; !ok {
		t.Error("expected block 100 to be indexed")
	}
	if len(blockBuildersByNumber(nil)) != 0 {
		t.Error("expected an empty map for no rows")
	}
}

// Blob rows gained first_seen_at, tx_index, and the derived wait; the derived
// field is signed, and pending rows never carry it.
func TestToBlobResponse_InclusionFields(t *testing.T) {
	blockTime := time.Date(2026, 7, 1, 0, 0, 12, 0, time.UTC)
	seen := blockTime.Add(-4200 * time.Millisecond)
	index := 12
	blob := models.Blob{
		ChainID:           42,
		BlockNumber:       100,
		TxHash:            validTestTxHash,
		FromAddress:       validTestAddress,
		BaseFeePerBlobGas: "1",
		TipPerBlobGas:     "1",
		TotalCostWei:      "1",
		Timestamp:         blockTime,
		Confirmed:         true,
		FirstSeenAt:       &seen,
		TxIndex:           &index,
	}
	network := config.NetworkConfig{Name: "testnet", ChainID: 42}

	got := toBlobResponse(blob, network)
	if got.TxIndex == nil || *got.TxIndex != 12 {
		t.Errorf("tx_index = %v", got.TxIndex)
	}
	if got.FirstSeenAt == nil || !got.FirstSeenAt.Equal(seen) {
		t.Errorf("first_seen_at = %v", got.FirstSeenAt)
	}
	if got.TimeToInclusionMs == nil || *got.TimeToInclusionMs != 4200 {
		t.Errorf("time_to_inclusion_ms = %v, want 4200", got.TimeToInclusionMs)
	}

	// Seen after slot start: the wait is negative and must survive.
	late := blockTime.Add(1500 * time.Millisecond)
	blob.FirstSeenAt = &late
	if got := toBlobResponse(blob, network); got.TimeToInclusionMs == nil || *got.TimeToInclusionMs != -1500 {
		t.Errorf("negative time_to_inclusion_ms = %v, want -1500", got.TimeToInclusionMs)
	}

	// No first-seen time (indexed from history): the derived field is absent.
	blob.FirstSeenAt = nil
	response := toBlobResponse(blob, network)
	if response.TimeToInclusionMs != nil {
		t.Errorf("expected no wait without first_seen_at, got %v", *response.TimeToInclusionMs)
	}
	payload, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, field := range []string{"first_seen_at", "time_to_inclusion_ms"} {
		if strings.Contains(string(payload), field) {
			t.Errorf("expected %q to be omitted, payload: %s", field, payload)
		}
	}

	// Pending rows project their own first-seen time; the wait would be a
	// constant zero, so it is omitted rather than reported.
	pending := models.Blob{
		ChainID:           42,
		BlockNumber:       models.PendingBlockNumber,
		TxHash:            validTestTxHash,
		FromAddress:       validTestAddress,
		BaseFeePerBlobGas: "1",
		TipPerBlobGas:     "1",
		TotalCostWei:      "1",
		Timestamp:         blockTime,
		FirstSeenAt:       &blockTime,
	}
	if got := toBlobResponse(pending, network); got.TimeToInclusionMs != nil || got.FirstSeenAt == nil {
		t.Errorf("pending blob inclusion fields = %+v / %v", got.TimeToInclusionMs, got.FirstSeenAt)
	}
}

func TestGetBlobReplacements_FeeContextFields(t *testing.T) {
	replacedTip := "1000000000"
	replacedBlobFee := "2000000000"
	replacementTip := "3000000000"
	replacementBlobFee := "4000000000"
	firstSeen := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	db := &mockDB{
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			setSliceResult(dest, []models.BlobReplacement{
				{
					ReplacedTxHash:                  validTestTxHash,
					ReplacementTxHash:               validTestTxHash,
					FromAddress:                     validTestAddress,
					Nonce:                           7,
					ReplacedAt:                      firstSeen,
					ReplacedMaxPriorityFeePerGas:    &replacedTip,
					ReplacedMaxFeePerBlobGas:        &replacedBlobFee,
					ReplacedFirstSeenAt:             &firstSeen,
					ReplacementMaxPriorityFeePerGas: &replacementTip,
					ReplacementMaxFeePerBlobGas:     &replacementBlobFee,
				},
				// A pre-migration row carries none of the fee context.
				{
					ReplacedTxHash:    validTestTxHash,
					ReplacementTxHash: validTestTxHash,
					FromAddress:       validTestAddress,
					Nonce:             8,
					ReplacedAt:        firstSeen,
				},
			})
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	w := httptest.NewRecorder()
	a.GetBlobReplacements(w, httptest.NewRequest(http.MethodGet, "/blob/replacements", http.NoBody))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data []BlobReplacementResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("expected 2 events, got %d", len(resp.Data))
	}
	bumped := resp.Data[0]
	if bumped.ReplacedMaxPriorityFeePerGasGwei != "1" || bumped.ReplacementMaxPriorityFeePerGasGwei != "3" {
		t.Errorf("tip gwei companions = %q / %q", bumped.ReplacedMaxPriorityFeePerGasGwei, bumped.ReplacementMaxPriorityFeePerGasGwei)
	}
	if bumped.ReplacedMaxFeePerBlobGasGwei != "2" || bumped.ReplacementMaxFeePerBlobGasGwei != "4" {
		t.Errorf("blob fee gwei companions = %q / %q", bumped.ReplacedMaxFeePerBlobGasGwei, bumped.ReplacementMaxFeePerBlobGasGwei)
	}
	if bumped.ReplacedFirstSeenAt == nil || !bumped.ReplacedFirstSeenAt.Equal(firstSeen) {
		t.Errorf("replaced_first_seen_at = %v", bumped.ReplacedFirstSeenAt)
	}
	legacy := resp.Data[1]
	if legacy.ReplacedMaxPriorityFeePerGas != nil || legacy.ReplacedFirstSeenAt != nil ||
		legacy.ReplacementMaxFeePerBlobGas != nil || legacy.ReplacedMaxPriorityFeePerGasGwei != "" {
		t.Errorf("expected a pre-migration row to carry no fee context, got %+v", legacy)
	}
}

// The projections that scan models.Blob must all select the two new columns,
// or sqlx fails at scan time against a real database.
func TestBlobProjectionsCarryInclusionColumns(t *testing.T) {
	for name, query := range map[string]string{
		"confirmed": blobSelectColumns,
		"mempool":   mempoolBlobSelectColumns,
		"byAddress": queryLatestBlobsByAddresses,
	} {
		for _, column := range []string{"first_seen_at", "tx_index"} {
			if !strings.Contains(query, column) {
				t.Errorf("%s projection is missing %s", name, column)
			}
		}
	}
	for _, column := range []string{
		"replaced_max_priority_fee_per_gas",
		"replaced_max_fee_per_blob_gas",
		"replaced_first_seen_at",
		"replacement_max_priority_fee_per_gas",
		"replacement_max_fee_per_blob_gas",
	} {
		if !strings.Contains(blobReplacementSelectColumns, column) {
			t.Errorf("replacement projection is missing %s", column)
		}
	}
}

func TestBlockBuilderSelectColumnsCoverModel(t *testing.T) {
	// A column missing here would scan as the zero value and silently drop a
	// field from every block payload.
	for _, column := range []string{
		"fee_recipient", "extra_data", "builder_key", "builder_name", "tx_count",
		"proposer_payment_wei", "proposer_payment_to", "candidate_snapshot",
		"pending_candidate_txs", "eligible_skipped_txs", "eligible_skipped_blobs",
		"eligible_skipped_max_tip",
	} {
		if !strings.Contains(blockBuilderSelectColumns, column) {
			t.Errorf("block builder projection is missing %s", column)
		}
	}
}
