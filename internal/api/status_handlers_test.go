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
	db := &mockDB{
		getFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			timestamp := dest.(**time.Time)
			*timestamp = &lastBlobTime
			return nil
		},
		selectFn: func(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
			setSliceResult(dest, []freshnessMetadataRow{
				{Key: models.MetadataLastIndexedBlock, Value: "300"},
				{Key: models.MetadataCurrentChainHead, Value: "301"},
				{Key: models.MetadataLastIndexedAt, Value: models.FormatMetadataTimestamp(indexedAt)},
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
