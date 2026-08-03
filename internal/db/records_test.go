package db

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestBackfillBlobBlockStreaksChunk(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectExec("SELECT blob_block_streaks_recompute_all").
		WithArgs(1, int64(100), int64(50099)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := db.BackfillBlobBlockStreaksChunk(context.Background(), 1, 100, 50099); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestBackfillBlobBlockStreaksChunk_RejectsInvertedBounds(t *testing.T) {
	db, _ := newMockDB(t)

	err := db.BackfillBlobBlockStreaksChunk(context.Background(), 1, 500, 100)
	if err == nil || !strings.Contains(err.Error(), "inverted bounds") {
		t.Fatalf("expected an inverted-bounds error, got %v", err)
	}
}

func TestBackfillBlobBlockStreaksChunk_Error(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectExec("SELECT blob_block_streaks_recompute_all").
		WillReturnError(errors.New("recompute failed"))

	err := db.BackfillBlobBlockStreaksChunk(context.Background(), 1, 100, 200)
	if err == nil || !strings.Contains(err.Error(), "backfill blob block streaks") {
		t.Fatalf("expected a backfill error, got %v", err)
	}
}

func TestIndexedBlockBounds(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery("SELECT MIN\\(block_number\\) AS min_block").
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"min_block", "max_block"}).AddRow(100, 999))

	bounds, err := db.IndexedBlockBounds(context.Background(), 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bounds.HasBlocks || bounds.Min != 100 || bounds.Max != 999 {
		t.Fatalf("unexpected bounds: %+v", bounds)
	}
}

// A network with no indexed blocks yields NULL aggregates, which must read as
// "no history" rather than the block range [0, 0].
func TestIndexedBlockBounds_NoIndexedBlocks(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery("SELECT MIN\\(block_number\\) AS min_block").
		WithArgs(7).
		WillReturnRows(sqlmock.NewRows([]string{"min_block", "max_block"}).AddRow(nil, nil))

	bounds, err := db.IndexedBlockBounds(context.Background(), 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bounds.HasBlocks {
		t.Fatalf("expected no indexed blocks, got %+v", bounds)
	}
}

func TestIndexedBlockBounds_Error(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery("SELECT MIN\\(block_number\\) AS min_block").
		WillReturnError(errors.New("probe failed"))

	if _, err := db.IndexedBlockBounds(context.Background(), 1); err == nil {
		t.Fatal("expected an error")
	}
}
