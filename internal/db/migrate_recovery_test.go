package db

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func TestFirstTransactionControlStatement(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want string // "" means transaction-safe
	}{
		{"empty", "", ""},
		{"plain ddl", "CREATE TABLE foo (id INT);\nCREATE INDEX idx ON foo(id);", ""},
		{"top-level commit", "CREATE TABLE foo (id INT);\nCOMMIT;\nCREATE TABLE bar (id INT);", "COMMIT"},
		{"top-level begin", "BEGIN;\nCREATE TABLE foo (id INT);", "BEGIN"},
		{"begin transaction mixed case", "Begin Transaction;\nSELECT 1;", "BEGIN"},
		{"top-level end", "SELECT 1;\nEND;", "END"},
		{"rollback", "ROLLBACK;", "ROLLBACK"},
		{"abort", "ABORT;", "ABORT"},
		{"start transaction", "START TRANSACTION;", "START"},
		{"savepoint", "SAVEPOINT sp1;", "SAVEPOINT"},
		{"release savepoint", "RELEASE SAVEPOINT sp1;", "RELEASE"},
		{"prepare transaction", "PREPARE TRANSACTION 'gid';", "PREPARE TRANSACTION"},
		{"prepare statement is fine", "PREPARE q(int) AS SELECT $1;", ""},
		{"commit without trailing semicolon", "CREATE TABLE foo (id INT);\nCOMMIT", "COMMIT"},
		{"leading comment then commit", "-- note\nCOMMIT;", "COMMIT"},
		{"commit in line comment", "CREATE TABLE foo (id INT); -- COMMIT;\nSELECT 1;", ""},
		{"commit in block comment", "/* COMMIT; */ SELECT 1;", ""},
		{"commit in nested block comment", "/* outer /* COMMIT; */ still comment */ SELECT 1;", ""},
		{"commit in string literal", "INSERT INTO t (v) VALUES ('COMMIT; BEGIN;');", ""},
		{"escaped quote in string", "INSERT INTO t (v) VALUES ('it''s; COMMIT');", ""},
		{"commit in quoted identifier", `SELECT 1 AS "COMMIT";`, ""},
		{"case end mid-statement", "SELECT CASE WHEN x > 0 THEN 1 ELSE 0 END FROM t;", ""},
		{
			"plpgsql begin end inside dollar quotes",
			"CREATE FUNCTION f() RETURNS void AS $$\nBEGIN\n  PERFORM 1;\nEND;\n$$ LANGUAGE plpgsql;",
			"",
		},
		{
			"do block with tagged dollar quotes",
			"DO $body$\nBEGIN\n  COMMIT; -- still inside the body\nEND $body$;",
			"",
		},
		{"commit after dollar-quoted body", "DO $$ BEGIN PERFORM 1; END $$;\nCOMMIT;", "COMMIT"},
		{"dollar positional param is not a quote", "SELECT $1 + $2;\nCOMMIT;", "COMMIT"},
		{"unterminated dollar quote swallows rest", "DO $$ BEGIN END; COMMIT;", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found := firstTransactionControlStatement(tt.sql)
			if tt.want == "" {
				if found {
					t.Fatalf("expected transaction-safe SQL, got keyword %q", got)
				}
				return
			}
			if !found || got != tt.want {
				t.Fatalf("got (%q, %v), want (%q, true)", got, found, tt.want)
			}
		})
	}
}

// TestBundledMigrationsAreTransactionSafe enforces the migration authoring
// rule from internal/db/migrations/README.md: no explicit transaction control
// in any migration file. golang-migrate runs each file in one implicit
// transaction, and dirty-schema auto-recovery (recoverDirtySchema) depends on
// that holding so a killed run cannot leave partially-committed state.
func TestBundledMigrationsAreTransactionSafe(t *testing.T) {
	dir, err := migrationsDir()
	if err != nil {
		t.Fatalf("migrationsDir: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	checked := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		if stmt, found := firstTransactionControlStatement(string(content)); found {
			t.Errorf("%s contains explicit transaction control (%q); migrations must run in golang-migrate's single implicit transaction", entry.Name(), stmt)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no migration files checked")
	}
}

func writeMigrationFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func TestPlanDirtyRecovery(t *testing.T) {
	dir := writeMigrationFiles(t, map[string]string{
		"000001_first.up.sql":    "CREATE TABLE a (id INT);",
		"000001_first.down.sql":  "DROP TABLE a;",
		"000002_second.up.sql":   "CREATE TABLE b (id INT);",
		"000002_second.down.sql": "DROP TABLE b;",
		"000004_fourth.up.sql":   "CREATE TABLE d (id INT);",
	})

	t.Run("forces back to previous version", func(t *testing.T) {
		forceTo, upFile, err := planDirtyRecovery(dir, 2)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if forceTo != 1 || upFile != "000002_second.up.sql" {
			t.Fatalf("got (%d, %s), want (1, 000002_second.up.sql)", forceTo, upFile)
		}
	})

	t.Run("skips version gaps", func(t *testing.T) {
		forceTo, _, err := planDirtyRecovery(dir, 4)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if forceTo != 2 {
			t.Fatalf("forceTo = %d, want 2", forceTo)
		}
	})

	t.Run("first migration forces to nil version", func(t *testing.T) {
		forceTo, _, err := planDirtyRecovery(dir, 1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if forceTo != -1 {
			t.Fatalf("forceTo = %d, want -1", forceTo)
		}
	})

	t.Run("refuses unknown version", func(t *testing.T) {
		if _, _, err := planDirtyRecovery(dir, 3); err == nil {
			t.Fatal("expected error for version with no bundled migration")
		}
	})

	t.Run("refuses non-positive version", func(t *testing.T) {
		for _, v := range []int{0, -1} {
			if _, _, err := planDirtyRecovery(dir, v); err == nil {
				t.Fatalf("expected error for dirty version %d", v)
			}
		}
	})

	t.Run("refuses missing directory", func(t *testing.T) {
		if _, _, err := planDirtyRecovery(filepath.Join(dir, "missing"), 1); err == nil {
			t.Fatal("expected error for missing migrations directory")
		}
	})

	t.Run("refuses unparseable version prefix", func(t *testing.T) {
		bad := writeMigrationFiles(t, map[string]string{
			"abc_bad.up.sql": "SELECT 1;",
		})
		if _, _, err := planDirtyRecovery(bad, 1); err == nil {
			t.Fatal("expected error for unparseable migration filename")
		}
	})

	t.Run("refuses filename without version prefix", func(t *testing.T) {
		bad := writeMigrationFiles(t, map[string]string{
			"noprefix.up.sql": "SELECT 1;",
		})
		if _, _, err := planDirtyRecovery(bad, 1); err == nil {
			t.Fatal("expected error for filename without version prefix")
		}
	})

	t.Run("refuses migration with explicit transaction control", func(t *testing.T) {
		unsafe := writeMigrationFiles(t, map[string]string{
			"000001_unsafe.up.sql": "CREATE TABLE a (id INT);\nCOMMIT;\nCREATE TABLE b (id INT);",
		})
		_, _, err := planDirtyRecovery(unsafe, 1)
		if err == nil || !strings.Contains(err.Error(), "transaction control") {
			t.Fatalf("expected transaction-control refusal, got %v", err)
		}
	})
}

type fakeForcer struct {
	forcedTo []int
	err      error
}

func (f *fakeForcer) Force(version int) error {
	f.forcedTo = append(f.forcedTo, version)
	return f.err
}

func expectSchemaVersionQuery(mock sqlmock.Sqlmock, version int, dirty bool) {
	mock.ExpectQuery(regexp.QuoteMeta(currentSchemaVersionQuery)).
		WillReturnRows(sqlmock.NewRows([]string{"version", "dirty"}).AddRow(version, dirty))
}

func TestRecoverDirtySchema(t *testing.T) {
	dir := writeMigrationFiles(t, map[string]string{
		"000001_first.up.sql":  "CREATE TABLE a (id INT);",
		"000002_second.up.sql": "CREATE TABLE b (id INT);",
	})

	t.Run("forces back to previous version while dirty", func(t *testing.T) {
		db, mock := newMockDB(t)
		expectSchemaVersionQuery(mock, 2, true)

		forcer := &fakeForcer{}
		if err := recoverDirtySchema(forcer, db.DB, dir, 2); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(forcer.forcedTo) != 1 || forcer.forcedTo[0] != 1 {
			t.Fatalf("forced to %v, want [1]", forcer.forcedTo)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("no-op when no longer dirty", func(t *testing.T) {
		db, mock := newMockDB(t)
		expectSchemaVersionQuery(mock, 2, false)

		forcer := &fakeForcer{}
		if err := recoverDirtySchema(forcer, db.DB, dir, 2); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(forcer.forcedTo) != 0 {
			t.Fatalf("expected no force, got %v", forcer.forcedTo)
		}
	})

	t.Run("no-op when version moved on", func(t *testing.T) {
		db, mock := newMockDB(t)
		expectSchemaVersionQuery(mock, 3, true)

		forcer := &fakeForcer{}
		if err := recoverDirtySchema(forcer, db.DB, dir, 2); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(forcer.forcedTo) != 0 {
			t.Fatalf("expected no force, got %v", forcer.forcedTo)
		}
	})

	t.Run("propagates re-check query error", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectQuery(regexp.QuoteMeta(currentSchemaVersionQuery)).
			WillReturnError(errors.New("connection lost"))

		if err := recoverDirtySchema(&fakeForcer{}, db.DB, dir, 2); err == nil {
			t.Fatal("expected error from schema_migrations re-check")
		}
	})

	t.Run("propagates plan refusal", func(t *testing.T) {
		db, mock := newMockDB(t)
		expectSchemaVersionQuery(mock, 99, true)

		forcer := &fakeForcer{}
		if err := recoverDirtySchema(forcer, db.DB, dir, 99); err == nil {
			t.Fatal("expected refusal for unknown dirty version")
		}
		if len(forcer.forcedTo) != 0 {
			t.Fatalf("expected no force, got %v", forcer.forcedTo)
		}
	})

	t.Run("propagates force error", func(t *testing.T) {
		db, mock := newMockDB(t)
		expectSchemaVersionQuery(mock, 2, true)

		forcer := &fakeForcer{err: errors.New("force failed")}
		if err := recoverDirtySchema(forcer, db.DB, dir, 2); err == nil {
			t.Fatal("expected force error to propagate")
		}
	})
}
