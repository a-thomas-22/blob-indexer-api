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

func TestLatestMigrationVersionFromDirErrors(t *testing.T) {
	tests := []struct {
		name        string
		setupDir    func(t *testing.T) string
		wantSnippet string
	}{
		{
			name: "missing directory",
			setupDir: func(t *testing.T) string {
				t.Helper()
				return filepath.Join(t.TempDir(), "missing")
			},
			wantSnippet: "failed to read migrations directory",
		},
		{
			name: "no up migrations",
			setupDir: func(t *testing.T) string {
				t.Helper()
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("ignored"), 0o600); err != nil {
					t.Fatalf("failed to write test file: %v", err)
				}
				if err := os.Mkdir(filepath.Join(dir, "000001_dir.up.sql"), 0o700); err != nil {
					t.Fatalf("failed to write test directory: %v", err)
				}
				return dir
			},
			wantSnippet: "no up migrations found",
		},
		{
			name: "missing version separator",
			setupDir: func(t *testing.T) string {
				t.Helper()
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "000001.up.sql"), []byte("-- bad"), 0o600); err != nil {
					t.Fatalf("failed to write test migration: %v", err)
				}
				return dir
			},
			wantSnippet: "does not start with a version prefix",
		},
		{
			name: "invalid version prefix",
			setupDir: func(t *testing.T) string {
				t.Helper()
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "not-a-number_bad.up.sql"), []byte("-- bad"), 0o600); err != nil {
					t.Fatalf("failed to write test migration: %v", err)
				}
				return dir
			},
			wantSnippet: "failed to parse migration version",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := latestMigrationVersionFromDir(tc.setupDir(t))
			if err == nil {
				t.Fatal("expected latestMigrationVersionFromDir() error")
			}
			if !strings.Contains(err.Error(), tc.wantSnippet) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantSnippet)
			}
		})
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

func TestNormalizeSchemaWait(t *testing.T) {
	tests := []struct {
		name             string
		timeout          time.Duration
		pollInterval     time.Duration
		wantTimeout      time.Duration
		wantPollInterval time.Duration
	}{
		{
			name:             "preserves positive values",
			timeout:          time.Second,
			pollInterval:     10 * time.Millisecond,
			wantTimeout:      time.Second,
			wantPollInterval: 10 * time.Millisecond,
		},
		{
			name:             "defaults timeout",
			pollInterval:     10 * time.Millisecond,
			wantTimeout:      DefaultSchemaWaitTimeout,
			wantPollInterval: 10 * time.Millisecond,
		},
		{
			name:             "defaults poll interval",
			timeout:          time.Second,
			wantTimeout:      time.Second,
			wantPollInterval: DefaultSchemaPollInterval,
		},
		{
			name:             "defaults non-positive values",
			timeout:          -time.Second,
			pollInterval:     -time.Millisecond,
			wantTimeout:      DefaultSchemaWaitTimeout,
			wantPollInterval: DefaultSchemaPollInterval,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotTimeout, gotPollInterval := normalizeSchemaWait(tc.timeout, tc.pollInterval)
			if gotTimeout != tc.wantTimeout {
				t.Fatalf("timeout = %s, want %s", gotTimeout, tc.wantTimeout)
			}
			if gotPollInterval != tc.wantPollInterval {
				t.Fatalf("pollInterval = %s, want %s", gotPollInterval, tc.wantPollInterval)
			}
		})
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
