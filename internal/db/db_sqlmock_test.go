package db

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/jmoiron/sqlx"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
)

func newMockDB(t *testing.T) (*DB, sqlmock.Sqlmock) {
	t.Helper()

	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	t.Cleanup(func() {
		_ = sqlDB.Close()
	})

	return &DB{DB: sqlx.NewDb(sqlDB, "sqlmock")}, mock
}

func TestExecSelectAndGetContext(t *testing.T) {
	db, mock := newMockDB(t)
	ctx := context.Background()

	mock.ExpectExec(regexp.QuoteMeta("UPDATE test SET value = $1 WHERE id = $2")).
		WithArgs("v", 10).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if _, err := db.ExecContext(ctx, "UPDATE test SET value = $1 WHERE id = $2", "v", 10); err != nil {
		t.Fatalf("ExecContext() error = %v", err)
	}

	rows := sqlmock.NewRows([]string{"id", "name"}).AddRow(1, "alice").AddRow(2, "bob")
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, name FROM test WHERE id > $1")).
		WithArgs(0).
		WillReturnRows(rows)

	var result []struct {
		ID   int    `db:"id"`
		Name string `db:"name"`
	}
	if err := db.SelectContext(ctx, &result, "SELECT id, name FROM test WHERE id > $1", 0); err != nil {
		t.Fatalf("SelectContext() error = %v", err)
	}
	if len(result) != 2 || result[0].Name != "alice" || result[1].Name != "bob" {
		t.Fatalf("unexpected select result: %+v", result)
	}

	oneRow := sqlmock.NewRows([]string{"name"}).AddRow("value")
	mock.ExpectQuery(regexp.QuoteMeta("SELECT name FROM test WHERE id = $1")).
		WithArgs(1).
		WillReturnRows(oneRow)

	var name string
	if err := db.GetContext(ctx, &name, "SELECT name FROM test WHERE id = $1", 1); err != nil {
		t.Fatalf("GetContext() error = %v", err)
	}
	if name != "value" {
		t.Fatalf("unexpected GetContext value: %q", name)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestGetMetadata(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		db, mock := newMockDB(t)
		rows := sqlmock.NewRows([]string{"value"}).AddRow("123")
		mock.ExpectQuery(regexp.QuoteMeta("SELECT value FROM indexer_metadata WHERE key = $1")).
			WithArgs("k").
			WillReturnRows(rows)

		value, err := db.GetMetadata(context.Background(), "k")
		if err != nil {
			t.Fatalf("GetMetadata() error = %v", err)
		}
		if value != "123" {
			t.Fatalf("expected 123, got %q", value)
		}
	})

	t.Run("error", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectQuery(regexp.QuoteMeta("SELECT value FROM indexer_metadata WHERE key = $1")).
			WithArgs("k").
			WillReturnError(sql.ErrNoRows)

		_, err := db.GetMetadata(context.Background(), "k")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "failed to get metadata for key k") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestGetNetworkMetadata(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		db, mock := newMockDB(t)
		rows := sqlmock.NewRows([]string{"value"}).AddRow("456")
		mock.ExpectQuery(regexp.QuoteMeta("SELECT value FROM indexer_metadata WHERE network_id = $1 AND key = $2")).
			WithArgs(1, "k").
			WillReturnRows(rows)

		value, err := db.GetNetworkMetadata(context.Background(), 1, "k")
		if err != nil {
			t.Fatalf("GetNetworkMetadata() error = %v", err)
		}
		if value != "456" {
			t.Fatalf("expected 456, got %q", value)
		}
	})

	t.Run("error", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectQuery(regexp.QuoteMeta("SELECT value FROM indexer_metadata WHERE network_id = $1 AND key = $2")).
			WithArgs(1, "k").
			WillReturnError(sql.ErrNoRows)

		_, err := db.GetNetworkMetadata(context.Background(), 1, "k")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "failed to get metadata for key k and network 1") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestSetMetadata(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectExec("INSERT INTO indexer_metadata").
			WithArgs("k", "v").
			WillReturnResult(sqlmock.NewResult(1, 1))

		if err := db.SetMetadata(context.Background(), "k", "v"); err != nil {
			t.Fatalf("SetMetadata() error = %v", err)
		}
	})

	t.Run("error", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectExec("INSERT INTO indexer_metadata").
			WithArgs("k", "v").
			WillReturnError(errors.New("write failed"))

		err := db.SetMetadata(context.Background(), "k", "v")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "failed to set metadata for key k") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestSetNetworkMetadata(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectExec("INSERT INTO indexer_metadata").
			WithArgs(1, "k", "v").
			WillReturnResult(sqlmock.NewResult(1, 1))

		if err := db.SetNetworkMetadata(context.Background(), 1, "k", "v"); err != nil {
			t.Fatalf("SetNetworkMetadata() error = %v", err)
		}
	})

	t.Run("error", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectExec("INSERT INTO indexer_metadata").
			WithArgs(1, "k", "v").
			WillReturnError(errors.New("write failed"))

		err := db.SetNetworkMetadata(context.Background(), 1, "k", "v")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "failed to set metadata for key k and network 1") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestUpsertNetworks(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		db, mock := newMockDB(t)
		networks := []config.NetworkConfig{
			{Name: "mainnet", ChainID: 1, StartBlock: "LATEST-1000", Enabled: true},
			{Name: "sepolia", ChainID: 11155111, StartBlock: "LATEST-100", Enabled: false},
		}

		mock.ExpectExec("INSERT INTO networks").
			WithArgs(1, "mainnet", "LATEST-1000", true).
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectExec("INSERT INTO networks").
			WithArgs(11155111, "sepolia", "LATEST-100", false).
			WillReturnResult(sqlmock.NewResult(1, 1))

		if err := db.UpsertNetworks(context.Background(), networks); err != nil {
			t.Fatalf("UpsertNetworks() error = %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("error", func(t *testing.T) {
		db, mock := newMockDB(t)
		networks := []config.NetworkConfig{
			{Name: "sepolia", ChainID: 11155111, StartBlock: "LATEST-100", Enabled: true},
		}

		mock.ExpectExec("INSERT INTO networks").
			WithArgs(11155111, "sepolia", "LATEST-100", true).
			WillReturnError(errors.New("write failed"))

		err := db.UpsertNetworks(context.Background(), networks)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "failed to upsert network sepolia") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestGetIndexedBlockHash(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		db, mock := newMockDB(t)
		rows := sqlmock.NewRows([]string{"block_hash"}).AddRow("0xabc")
		mock.ExpectQuery(regexp.QuoteMeta("SELECT block_hash FROM indexed_blocks WHERE network_id = $1 AND block_number = $2")).
			WithArgs(1, uint64(10)).
			WillReturnRows(rows)

		hash, err := db.GetIndexedBlockHash(context.Background(), 1, 10)
		if err != nil {
			t.Fatalf("GetIndexedBlockHash() error = %v", err)
		}
		if hash != "0xabc" {
			t.Fatalf("expected hash 0xabc, got %q", hash)
		}
	})

	t.Run("error passes through", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectQuery(regexp.QuoteMeta("SELECT block_hash FROM indexed_blocks WHERE network_id = $1 AND block_number = $2")).
			WithArgs(1, uint64(10)).
			WillReturnError(sql.ErrNoRows)

		_, err := db.GetIndexedBlockHash(context.Background(), 1, 10)
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("expected sql.ErrNoRows, got %v", err)
		}
	})
}

func TestDeleteFromBlockMethods(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE network_id = $1 AND block_number >= $2")).
		WithArgs(1, int64(100)).
		WillReturnResult(sqlmock.NewResult(0, 2))
	if err := db.DeleteBlobsFromBlock(context.Background(), 1, 100); err != nil {
		t.Fatalf("DeleteBlobsFromBlock() error = %v", err)
	}

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM indexed_blocks WHERE network_id = $1 AND block_number >= $2")).
		WithArgs(1, uint64(100)).
		WillReturnResult(sqlmock.NewResult(0, 2))
	if err := db.DeleteIndexedBlocksFromBlock(context.Background(), 1, 100); err != nil {
		t.Fatalf("DeleteIndexedBlocksFromBlock() error = %v", err)
	}

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM block_metrics WHERE network_id = $1 AND block_number >= $2")).
		WithArgs(1, int64(100)).
		WillReturnResult(sqlmock.NewResult(0, 2))
	if err := db.DeleteBlockMetricsFromBlock(context.Background(), 1, 100); err != nil {
		t.Fatalf("DeleteBlockMetricsFromBlock() error = %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestDeleteStalePendingBlobs(t *testing.T) {
	db, mock := newMockDB(t)
	cutoff := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM blobs WHERE network_id = $1 AND block_number < 0 AND timestamp < $2")).
		WithArgs(1, cutoff).
		WillReturnResult(sqlmock.NewResult(0, 5))

	deleted, err := db.DeleteStalePendingBlobs(context.Background(), 1, cutoff)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deleted != 5 {
		t.Errorf("expected 5 deleted, got %d", deleted)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestConnectAndMigrations_InvalidURL(t *testing.T) {
	ctx := context.Background()
	dbCfg := config.DatabaseConfig{
		URL:             "postgres://invalid:invalid@127.0.0.1:1/invalid?sslmode=disable&connect_timeout=1",
		MaxOpenConns:    25,
		MaxIdleConns:    10,
		ConnMaxLifetime: 5 * time.Minute,
		ConnMaxIdleTime: time.Minute,
	}
	_, err := Connect(ctx, dbCfg)
	if err == nil {
		t.Fatal("expected Connect() to fail")
	}

	err = RunMigrations("postgres://invalid:invalid@127.0.0.1:1/invalid?sslmode=disable&connect_timeout=1")
	if err == nil {
		t.Fatal("expected RunMigrations() to fail")
	}
}
