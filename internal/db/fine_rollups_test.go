package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestBackfillFineChartRollupsChunk(t *testing.T) {
	db, mock := newMockDB(t)
	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	mock.ExpectExec("INSERT INTO blob_chart_rollups").
		WithArgs(1, start, end, FineChartRollupBucketSeconds).
		WillReturnResult(sqlmock.NewResult(0, 12))
	mock.ExpectExec("INSERT INTO block_metrics_rollups").
		WithArgs(1, start, end, FineChartRollupBucketSeconds).
		WillReturnResult(sqlmock.NewResult(0, 60))

	if err := db.BackfillFineChartRollupsChunk(context.Background(), 1, start, end); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestBackfillFineChartRollupsChunk_RejectsUnalignedBounds(t *testing.T) {
	db, _ := newMockDB(t)
	aligned := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)

	for _, bounds := range [][2]time.Time{
		{aligned.Add(30 * time.Second), aligned.Add(time.Hour)},
		{aligned, aligned.Add(time.Hour + 30*time.Second)},
	} {
		err := db.BackfillFineChartRollupsChunk(context.Background(), 1, bounds[0], bounds[1])
		if err == nil || !strings.Contains(err.Error(), "aligned") {
			t.Fatalf("expected alignment error for bounds %v, got %v", bounds, err)
		}
	}
}

func TestBackfillFineChartRollupsChunk_Errors(t *testing.T) {
	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	t.Run("blob statement fails", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectExec("INSERT INTO blob_chart_rollups").
			WillReturnError(errors.New("blob insert failed"))
		err := db.BackfillFineChartRollupsChunk(context.Background(), 1, start, end)
		if err == nil || !strings.Contains(err.Error(), "fine blob chart rollups") {
			t.Fatalf("expected blob backfill error, got %v", err)
		}
	})

	t.Run("block metrics statement fails", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectExec("INSERT INTO blob_chart_rollups").
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec("INSERT INTO block_metrics_rollups").
			WillReturnError(errors.New("metrics insert failed"))
		err := db.BackfillFineChartRollupsChunk(context.Background(), 1, start, end)
		if err == nil || !strings.Contains(err.Error(), "fine block metrics rollups") {
			t.Fatalf("expected block metrics backfill error, got %v", err)
		}
	})
}

func TestPruneFineChartRollups(t *testing.T) {
	db, mock := newMockDB(t)
	cutoff := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)

	mock.ExpectExec("DELETE FROM blob_chart_rollups WHERE chain_id = \\$1 AND bucket_seconds = \\$2 AND bucket_start < \\$3").
		WithArgs(1, FineChartRollupBucketSeconds, cutoff).
		WillReturnResult(sqlmock.NewResult(0, 7))
	mock.ExpectExec("DELETE FROM block_metrics_rollups WHERE chain_id = \\$1 AND bucket_seconds = \\$2 AND bucket_start < \\$3").
		WithArgs(1, FineChartRollupBucketSeconds, cutoff).
		WillReturnResult(sqlmock.NewResult(0, 5))

	deleted, err := db.PruneFineChartRollups(context.Background(), 1, cutoff)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deleted != 12 {
		t.Fatalf("expected 12 deleted rows, got %d", deleted)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPruneFineChartRollups_Error(t *testing.T) {
	db, mock := newMockDB(t)
	cutoff := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)

	mock.ExpectExec("DELETE FROM blob_chart_rollups").
		WillReturnError(errors.New("delete failed"))

	if _, err := db.PruneFineChartRollups(context.Background(), 1, cutoff); err == nil {
		t.Fatal("expected prune error")
	}
}
