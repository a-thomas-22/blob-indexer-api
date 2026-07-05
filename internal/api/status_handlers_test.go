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

func TestGetIndexerStatus_Success(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetIndexerStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetIndexerStatus_IncludesFreshness(t *testing.T) {
	lastBlobTime := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	indexedAt := lastBlobTime.Add(5 * time.Second)
	backfillUpdatedAt := indexedAt.Add(3 * time.Second)
	earliestIndexed, latestIndexed := int64(100), int64(300)
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			switch query {
			case queryNetworkLastIndexedTime:
				timestamp := dest.(*time.Time)
				*timestamp = lastBlobTime
			case queryIndexedBlockCoverage:
				setStructResult(dest, indexedBlockCoverage{
					EarliestIndexedBlock: &earliestIndexed,
					LatestIndexedBlock:   &latestIndexed,
				})
			default:
				t.Fatalf("unexpected status query: %s", query)
			}
			return nil
		},
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			setSliceResult(dest, []freshnessMetadataRow{
				{Key: models.MetadataLastIndexedBlock, Value: "300"},
				{Key: models.MetadataCurrentChainHead, Value: "301"},
				{Key: models.MetadataLastIndexedAt, Value: models.FormatMetadataTimestamp(indexedAt)},
				{Key: models.MetadataBackfillActive, Value: "true"},
				{Key: models.MetadataBackfillStartBlock, Value: "100"},
				{Key: models.MetadataBackfillTargetBlock, Value: "301"},
				{Key: models.MetadataBackfillUpdatedAt, Value: models.FormatMetadataTimestamp(backfillUpdatedAt)},
			})
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetIndexerStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp struct {
		Success bool           `json:"success"`
		Data    StatusResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Data.LastIndexedBlock != 300 {
		t.Fatalf("expected last indexed block 300, got %d", resp.Data.LastIndexedBlock)
	}
	if !resp.Data.LastIndexedTime.Equal(lastBlobTime) {
		t.Fatalf("expected last indexed time %s, got %s", lastBlobTime, resp.Data.LastIndexedTime)
	}
	if resp.Data.CurrentChainHead == nil || *resp.Data.CurrentChainHead != 301 {
		t.Fatalf("expected current chain head 301, got %v", resp.Data.CurrentChainHead)
	}
	if resp.Data.IndexerLagBlocks == nil || *resp.Data.IndexerLagBlocks != 1 {
		t.Fatalf("expected indexer lag 1, got %v", resp.Data.IndexerLagBlocks)
	}
	if resp.Data.LastIndexedAt == nil || !resp.Data.LastIndexedAt.Equal(indexedAt) {
		t.Fatalf("expected last indexed at %s, got %v", indexedAt, resp.Data.LastIndexedAt)
	}
	if !resp.Data.Backfill.Active {
		t.Fatal("expected backfill to be active")
	}
	if resp.Data.Backfill.StartBlock == nil || *resp.Data.Backfill.StartBlock != 100 {
		t.Fatalf("expected backfill start block 100, got %v", resp.Data.Backfill.StartBlock)
	}
	if resp.Data.Backfill.CurrentBlock != 300 {
		t.Fatalf("expected backfill current block 300, got %d", resp.Data.Backfill.CurrentBlock)
	}
	if resp.Data.Backfill.TargetBlock == nil || *resp.Data.Backfill.TargetBlock != 301 {
		t.Fatalf("expected backfill target block 301, got %v", resp.Data.Backfill.TargetBlock)
	}
	if resp.Data.Backfill.RemainingBlocks == nil || *resp.Data.Backfill.RemainingBlocks != 1 {
		t.Fatalf("expected backfill remaining blocks 1, got %v", resp.Data.Backfill.RemainingBlocks)
	}
	if resp.Data.Backfill.ProgressPercent == nil || *resp.Data.Backfill.ProgressPercent <= 99 {
		t.Fatalf("expected backfill progress above 99%%, got %v", resp.Data.Backfill.ProgressPercent)
	}
	if resp.Data.Backfill.UpdatedAt == nil || !resp.Data.Backfill.UpdatedAt.Equal(backfillUpdatedAt) {
		t.Fatalf("expected backfill updated at %s, got %v", backfillUpdatedAt, resp.Data.Backfill.UpdatedAt)
	}
	if resp.Data.EarliestIndexedBlock == nil || *resp.Data.EarliestIndexedBlock != earliestIndexed {
		t.Fatalf("expected earliest indexed block %d, got %v", earliestIndexed, resp.Data.EarliestIndexedBlock)
	}
	if resp.Data.LatestIndexedBlock == nil || *resp.Data.LatestIndexedBlock != latestIndexed {
		t.Fatalf("expected latest indexed block %d, got %v", latestIndexed, resp.Data.LatestIndexedBlock)
	}
}

func TestGetIndexerStatus_CoverageOmittedWhenEmpty(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			// Coverage dest is left untouched: MIN/MAX over zero indexed
			// blocks scan as NULLs, so both bounds stay nil.
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetIndexerStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "earliest_indexed_block") || strings.Contains(body, "latest_indexed_block") {
		t.Fatalf("expected coverage fields omitted for empty coverage, got: %s", body)
	}
}

func TestGetIndexerStatus_CoverageQueryErrorDegrades(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			if query == queryIndexedBlockCoverage {
				return fmt.Errorf("coverage query failed")
			}
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetIndexerStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 when coverage query fails, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"success":true`) {
		t.Fatalf("expected success:true in response, got: %s", body)
	}
	if strings.Contains(body, "earliest_indexed_block") || strings.Contains(body, "latest_indexed_block") {
		t.Fatalf("expected coverage fields omitted on query error, got: %s", body)
	}
}

func TestGetIndexerStatus_BackfillCurrentBlockOverridesLiveHead(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return nil
		},
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			setSliceResult(dest, []freshnessMetadataRow{
				{Key: models.MetadataLastIndexedBlock, Value: "1000"},
				{Key: models.MetadataCurrentChainHead, Value: "1000"},
				{Key: models.MetadataBackfillActive, Value: "true"},
				{Key: models.MetadataBackfillStartBlock, Value: "100"},
				{Key: models.MetadataBackfillCurrentBlock, Value: "250"},
				{Key: models.MetadataBackfillTargetBlock, Value: "1000"},
			})
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetIndexerStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp struct {
		Success bool           `json:"success"`
		Data    StatusResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Data.Backfill.Active {
		t.Fatal("expected backfill to stay active")
	}
	if resp.Data.Backfill.CurrentBlock != 250 {
		t.Fatalf("expected backfill current block 250, got %d", resp.Data.Backfill.CurrentBlock)
	}
	if resp.Data.Backfill.RemainingBlocks == nil || *resp.Data.Backfill.RemainingBlocks != 750 {
		t.Fatalf("expected remaining blocks 750, got %v", resp.Data.Backfill.RemainingBlocks)
	}
}

func TestGetIndexerStatus_BackfillFallback(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return nil
		},
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			setSliceResult(dest, []freshnessMetadataRow{
				{Key: models.MetadataLastIndexedBlock, Value: "10"},
				{Key: models.MetadataCurrentChainHead, Value: "25"},
			})
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetIndexerStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp struct {
		Success bool           `json:"success"`
		Data    StatusResponse `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Data.Backfill.Active {
		t.Fatal("expected lag fallback to mark backfill active")
	}
	if resp.Data.Backfill.TargetBlock == nil || *resp.Data.Backfill.TargetBlock != 25 {
		t.Fatalf("expected target block 25, got %v", resp.Data.Backfill.TargetBlock)
	}
	if resp.Data.Backfill.RemainingBlocks == nil || *resp.Data.Backfill.RemainingBlocks != 15 {
		t.Fatalf("expected remaining blocks 15, got %v", resp.Data.Backfill.RemainingBlocks)
	}
}

func TestGetIndexerStatus_DBError(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			return fmt.Errorf("db error")
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetIndexerStatus(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestGetIndexerStatus_EmptyDatabase(t *testing.T) {
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			// Simulate SQL NULL from MAX(timestamp) on empty table — dest remains nil
			return nil
		},
	}
	a := newTestAPIWithDB(db)
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	a.GetIndexerStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for empty database, got %d: %s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	if !strings.Contains(body, `"success":true`) {
		t.Fatalf("expected success:true in response, got: %s", body)
	}
}

func TestGetIndexerStatus_BadNetwork(t *testing.T) {
	a := newTestAPI()
	a.networks = map[int]config.NetworkConfig{}
	req := httptest.NewRequest(http.MethodGet, "/?network=999", http.NoBody)
	w := httptest.NewRecorder()
	a.GetIndexerStatus(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}
