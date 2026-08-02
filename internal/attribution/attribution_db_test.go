package attribution

import (
	"context"
	"database/sql/driver"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/jmoiron/sqlx"

	"github.com/a-thomas-22/blob-indexer-api/internal/db"
)

// utcTimeArg matches a time.Time argument only if it is pinned to UTC.
// first_seen/last_seen are timezone-less TIMESTAMP columns, which discard the
// offset lib/pq encodes — a local-zone time would be stored shifted on
// non-UTC hosts.
type utcTimeArg struct{}

func (utcTimeArg) Match(v driver.Value) bool {
	t, ok := v.(time.Time)
	return ok && t.Location() == time.UTC
}

func newMockService(t *testing.T) (*Service, sqlmock.Sqlmock) {
	t.Helper()

	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	t.Cleanup(func() {
		_ = sqlDB.Close()
	})

	xdb := sqlx.NewDb(sqlDB, "sqlmock")
	return NewService(&db.DB{DB: xdb}), mock
}

func TestInitialize_DoesNotLoadBlobUsersAsFallback(t *testing.T) {
	svc, mock := newMockService(t)

	if err := svc.Initialize(context.TODO()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}

	if got := svc.GetUserAttribution("0xabc"); got != "" {
		t.Errorf("expected no DB-backed fallback attribution, got %q", got)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestAddKnownUser_InsertsNewUser(t *testing.T) {
	svc, mock := newMockService(t)

	mock.ExpectExec("INSERT INTO blob_users").
		WithArgs(1, "0xabc", "Alice", "desc", "infra", utcTimeArg{}, utcTimeArg{}).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := svc.AddKnownUser(context.TODO(), "0xAbC", "Alice", "desc", "infra"); err != nil {
		t.Fatalf("AddKnownUser() error = %v", err)
	}

	if got := svc.GetUserAttribution("0xabc"); got != "Alice" {
		t.Errorf("expected known user to be stored, got %q", got)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestAddKnownUser_UpdatesExistingUser(t *testing.T) {
	svc, mock := newMockService(t)
	svc.knownUsers["0xabc"] = "OldName"

	mock.ExpectExec("UPDATE blob_users").
		WithArgs("Alice", "desc", "infra", utcTimeArg{}, "0xabc", 1).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := svc.AddKnownUser(context.TODO(), "0xABC", "Alice", "desc", "infra"); err != nil {
		t.Fatalf("AddKnownUser() update error = %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestAddKnownUser_ErrorPaths(t *testing.T) {
	t.Run("insert error", func(t *testing.T) {
		svc, mock := newMockService(t)
		mock.ExpectExec("INSERT INTO blob_users").
			WithArgs(1, "0xabc", "Alice", "desc", "infra", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnError(assertiveError("insert failed"))

		if err := svc.AddKnownUser(context.TODO(), "0xAbC", "Alice", "desc", "infra"); err == nil {
			t.Fatal("expected AddKnownUser() to return error on insert failure")
		}

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("update error", func(t *testing.T) {
		svc, mock := newMockService(t)
		svc.knownUsers["0xabc"] = "OldName"

		mock.ExpectExec("UPDATE blob_users").
			WithArgs("Alice", "desc", "infra", sqlmock.AnyArg(), "0xabc", 1).
			WillReturnError(assertiveError("update failed"))

		if err := svc.AddKnownUser(context.TODO(), "0xABC", "Alice", "desc", "infra"); err == nil {
			t.Fatal("expected AddKnownUser() to return error on update failure")
		}

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})
}

func TestGetKnownUsers_ReturnsRows(t *testing.T) {
	svc, mock := newMockService(t)
	rows := sqlmock.NewRows([]string{"id", "chain_id", "address", "name", "description", "category", "first_seen", "last_seen"}).
		AddRow(1, 1, "0xabc", "Alice", "", "", time.Now(), time.Now())

	mock.ExpectQuery("SELECT \\* FROM blob_users WHERE chain_id = \\$1 ORDER BY name").
		WithArgs(1).
		WillReturnRows(rows)

	users, err := svc.GetKnownUsers(context.TODO())
	if err != nil {
		t.Fatalf("GetKnownUsers() error = %v", err)
	}
	if len(users) != 1 || users[0].Name != "Alice" {
		t.Fatalf("unexpected users result: %+v", users)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestGetTopBlobUsers_ReturnsRows(t *testing.T) {
	svc, mock := newMockService(t)
	now := time.Now()
	rows := sqlmock.NewRows([]string{"from_address", "user_attribution", "blob_count", "total_cost_wei", "last_timestamp"}).
		AddRow("0xabc", "Alice", 3, "1.5", now)

	mock.ExpectQuery("SELECT").WithArgs(1, 5, 0).WillReturnRows(rows)

	stats, err := svc.GetTopBlobUsers(context.TODO(), 5, 0)
	if err != nil {
		t.Fatalf("GetTopBlobUsers() error = %v", err)
	}
	if len(stats) != 1 || stats[0].Address != "0xabc" || stats[0].BlobCount != 3 {
		t.Fatalf("unexpected stats result: %+v", stats)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestInitialize_BlobListDisabledDoesNotQuery(t *testing.T) {
	svc, mock := newMockService(t)

	if err := svc.Initialize(context.TODO()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestUpdateUserLastSeen_KnownUser(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		svc, mock := newMockService(t)
		svc.knownUsers["0xabc"] = "Alice"

		mock.ExpectExec("UPDATE blob_users SET last_seen = \\$1 WHERE address = \\$2 AND chain_id = \\$3").
			WithArgs(utcTimeArg{}, "0xabc", 1).
			WillReturnResult(sqlmock.NewResult(0, 1))

		if err := svc.UpdateUserLastSeen(context.TODO(), "0xABC"); err != nil {
			t.Fatalf("UpdateUserLastSeen() error = %v", err)
		}

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("db error", func(t *testing.T) {
		svc, mock := newMockService(t)
		svc.knownUsers["0xabc"] = "Alice"

		mock.ExpectExec("UPDATE blob_users SET last_seen = \\$1 WHERE address = \\$2 AND chain_id = \\$3").
			WithArgs(sqlmock.AnyArg(), "0xabc", 1).
			WillReturnError(assertiveError("update failed"))

		if err := svc.UpdateUserLastSeen(context.TODO(), "0xABC"); err == nil {
			t.Fatal("expected UpdateUserLastSeen() to return error")
		}

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})
}

func TestBatchUpdateUserLastSeen(t *testing.T) {
	t.Run("empty input returns nil", func(t *testing.T) {
		svc, mock := newMockService(t)
		err := svc.BatchUpdateUserLastSeen(context.TODO(), nil)
		if err != nil {
			t.Fatalf("BatchUpdateUserLastSeen() error = %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unexpected db expectations: %v", err)
		}
	})

	t.Run("no known addresses returns nil", func(t *testing.T) {
		svc, mock := newMockService(t)
		svc.knownUsers["0xabc"] = "Alice"

		err := svc.BatchUpdateUserLastSeen(context.TODO(), []string{"0xdef", "0x123"})
		if err != nil {
			t.Fatalf("BatchUpdateUserLastSeen() error = %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unexpected db expectations: %v", err)
		}
	})

	t.Run("updates known addresses in single query", func(t *testing.T) {
		svc, mock := newMockService(t)
		svc.knownUsers["0xabc"] = "Alice"
		svc.knownUsers["0xdef"] = "Bob"

		mock.ExpectExec("UPDATE blob_users SET last_seen = \\$1 WHERE address = ANY\\(\\$2\\) AND chain_id = \\$3").
			WithArgs(utcTimeArg{}, sqlmock.AnyArg(), 1).
			WillReturnResult(sqlmock.NewResult(0, 2))

		err := svc.BatchUpdateUserLastSeen(context.TODO(), []string{"0xABC", "0xdef", "0xunknown"})
		if err != nil {
			t.Fatalf("BatchUpdateUserLastSeen() error = %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("returns db error", func(t *testing.T) {
		svc, mock := newMockService(t)
		svc.knownUsers["0xabc"] = "Alice"

		mock.ExpectExec("UPDATE blob_users SET last_seen = \\$1 WHERE address = ANY\\(\\$2\\) AND chain_id = \\$3").
			WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), 1).
			WillReturnError(assertiveError("batch update failed"))

		err := svc.BatchUpdateUserLastSeen(context.TODO(), []string{"0xabc"})
		if err == nil {
			t.Fatal("expected BatchUpdateUserLastSeen() to return error")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})
}

type assertiveError string

func (e assertiveError) Error() string { return string(e) }
