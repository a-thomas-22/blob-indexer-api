package db

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func TestLatestMigrationVersionFromDir(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"000001_initial.up.sql":   "-- migration 1",
		"000010_latest.up.sql":    "-- migration 10",
		"000010_latest.down.sql":  "-- ignored",
		"README.md":               "ignored",
		"000002_extra.up.sql.bak": "ignored",
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
			t.Fatalf("failed to write test migration %s: %v", name, err)
		}
	}

	version, err := latestMigrationVersionFromDir(dir)
	if err != nil {
		t.Fatalf("latestMigrationVersionFromDir() error = %v", err)
	}
	if version != 10 {
		t.Fatalf("expected latest version 10, got %d", version)
	}
}

func TestCheckSchemaVersion(t *testing.T) {
	expected := latestMigrationVersionForTest(t)

	tests := []struct {
		name           string
		version        uint
		dirty          bool
		wantErr        error
		wantReady      bool
		wantMsgSnippet string
	}{
		{
			name:      "ready",
			version:   expected,
			wantReady: true,
		},
		{
			name:           "behind",
			version:        expected - 1,
			wantErr:        ErrSchemaNotReady,
			wantMsgSnippet: "behind",
		},
		{
			name:           "dirty",
			version:        expected,
			dirty:          true,
			wantErr:        ErrSchemaNotReady,
			wantMsgSnippet: "dirty",
		},
		{
			name:           "ahead",
			version:        expected + 1,
			wantErr:        ErrSchemaTooNew,
			wantMsgSnippet: "ahead",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db, mock := newMockDB(t)
			mock.ExpectQuery(regexp.QuoteMeta(currentSchemaVersionQuery)).
				WillReturnRows(sqlmock.NewRows([]string{"version", "dirty"}).AddRow(int64(tc.version), tc.dirty))

			status, err := db.CheckSchemaVersion(context.Background())
			if tc.wantErr == nil && err != nil {
				t.Fatalf("CheckSchemaVersion() error = %v", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("CheckSchemaVersion() error = %v, want %v", err, tc.wantErr)
			}
			if status.Ready != tc.wantReady {
				t.Fatalf("status.Ready = %v, want %v", status.Ready, tc.wantReady)
			}
			if status.ExpectedVersion != expected {
				t.Fatalf("status.ExpectedVersion = %d, want %d", status.ExpectedVersion, expected)
			}
			if tc.wantMsgSnippet != "" && !strings.Contains(status.Message, tc.wantMsgSnippet) {
				t.Fatalf("status.Message = %q, want substring %q", status.Message, tc.wantMsgSnippet)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("unmet expectations: %v", err)
			}
		})
	}
}

func TestCheckSchemaVersion_QueryErrorIsNotReady(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery(regexp.QuoteMeta(currentSchemaVersionQuery)).
		WillReturnError(sql.ErrNoRows)

	status, err := db.CheckSchemaVersion(context.Background())
	if !errors.Is(err, ErrSchemaNotReady) {
		t.Fatalf("CheckSchemaVersion() error = %v, want ErrSchemaNotReady", err)
	}
	if !strings.Contains(status.Message, "no version row") {
		t.Fatalf("status.Message = %q, want no version row", status.Message)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestWaitForSchemaRetriesUntilReady(t *testing.T) {
	expected := latestMigrationVersionForTest(t)
	db, mock := newMockDB(t)

	mock.ExpectQuery(regexp.QuoteMeta(currentSchemaVersionQuery)).
		WillReturnError(errors.New(`pq: relation "schema_migrations" does not exist`))
	mock.ExpectQuery(regexp.QuoteMeta(currentSchemaVersionQuery)).
		WillReturnRows(sqlmock.NewRows([]string{"version", "dirty"}).AddRow(int64(expected), false))

	err := db.WaitForSchema(context.Background(), 100*time.Millisecond, time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForSchema() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestWaitForSchemaRejectsNewerVersion(t *testing.T) {
	expected := latestMigrationVersionForTest(t)
	db, mock := newMockDB(t)

	mock.ExpectQuery(regexp.QuoteMeta(currentSchemaVersionQuery)).
		WillReturnRows(sqlmock.NewRows([]string{"version", "dirty"}).AddRow(int64(expected+1), false))

	err := db.WaitForSchema(context.Background(), time.Second, time.Millisecond)
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("WaitForSchema() error = %v, want ErrSchemaTooNew", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func latestMigrationVersionForTest(t *testing.T) uint {
	t.Helper()

	version, err := LatestMigrationVersion()
	if err != nil {
		t.Fatalf("LatestMigrationVersion() error = %v", err)
	}
	if version == 0 {
		t.Fatal("LatestMigrationVersion() returned 0")
	}
	return version
}
