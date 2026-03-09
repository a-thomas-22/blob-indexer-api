package attribution

import (
	"context"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/jmoiron/sqlx"

	"github.com/a-thomas-22/blob-indexer-api/internal/db"
)

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

func TestInitialize_LoadsKnownUsers(t *testing.T) {
	svc, mock := newMockService(t)
	rows := sqlmock.NewRows([]string{"id", "network_id", "address", "name", "description", "category", "first_seen", "last_seen"}).
		AddRow(1, 1, "0xABC", "Alice", "", "", time.Now(), time.Now()).
		AddRow(2, 1, "0xDef", "Bob", "", "", time.Now(), time.Now())

	mock.ExpectQuery("SELECT \\* FROM blob_users WHERE network_id = \\$1").
		WithArgs(1).
		WillReturnRows(rows)

	if err := svc.Initialize(context.TODO()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}

	if got := svc.GetUserAttribution("0xabc"); got != "Alice" {
		t.Errorf("expected Alice, got %q", got)
	}
	if got := svc.GetUserAttribution("0xDEF"); got != "Bob" {
		t.Errorf("expected Bob, got %q", got)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestAddKnownUser_InsertsNewUser(t *testing.T) {
	svc, mock := newMockService(t)

	mock.ExpectExec("INSERT INTO blob_users").
		WithArgs(1, "0xabc", "Alice", "desc", "infra", sqlmock.AnyArg(), sqlmock.AnyArg()).
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
		WithArgs("Alice", "desc", "infra", sqlmock.AnyArg(), "0xabc", 1).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := svc.AddKnownUser(context.TODO(), "0xABC", "Alice", "desc", "infra"); err != nil {
		t.Fatalf("AddKnownUser() update error = %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestGetKnownUsers_ReturnsRows(t *testing.T) {
	svc, mock := newMockService(t)
	rows := sqlmock.NewRows([]string{"id", "network_id", "address", "name", "description", "category", "first_seen", "last_seen"}).
		AddRow(1, 1, "0xabc", "Alice", "", "", time.Now(), time.Now())

	mock.ExpectQuery("SELECT \\* FROM blob_users WHERE network_id = \\$1 ORDER BY name").
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
	rows := sqlmock.NewRows([]string{"from_address", "user_attribution", "blob_count", "total_cost_eth", "last_timestamp"}).
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

func TestInitialize_ReturnsErrorOnQueryFailure(t *testing.T) {
	svc, mock := newMockService(t)
	mock.ExpectQuery("SELECT \\* FROM blob_users WHERE network_id = \\$1").
		WithArgs(1).
		WillReturnError(assertiveError("load failed"))

	if err := svc.Initialize(context.TODO()); err == nil {
		t.Fatal("expected Initialize() to return an error")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestUpdateUserLastSeen_KnownUser(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		svc, mock := newMockService(t)
		svc.knownUsers["0xabc"] = "Alice"

		mock.ExpectExec("UPDATE blob_users SET last_seen = \\$1 WHERE address = \\$2 AND network_id = \\$3").
			WithArgs(sqlmock.AnyArg(), "0xabc", 1).
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

		mock.ExpectExec("UPDATE blob_users SET last_seen = \\$1 WHERE address = \\$2 AND network_id = \\$3").
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

type assertiveError string

func (e assertiveError) Error() string { return string(e) }
