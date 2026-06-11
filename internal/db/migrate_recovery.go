package db

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

// versionForcer is the subset of *migrate.Migrate used by dirty-schema
// recovery. Force sets the schema version and clears the dirty flag.
type versionForcer interface {
	Force(version int) error
}

// recoverDirtySchema attempts to clear a dirty schema_migrations row left
// behind by a migration run that was killed mid-flight (for example an Argo CD
// sync retry deleting a running migration Job).
//
// golang-migrate sets (version, dirty=true) before executing a migration and
// clears the flag afterwards. The postgres driver executes the whole .up.sql
// file as a single multi-statement Exec, which PostgreSQL runs in one implicit
// transaction — so when the connection dies mid-migration every statement
// rolls back and only the dirty flag persists. In that state it is safe to
// force the version back to the previous migration and re-run.
//
// That reasoning breaks if the file contains explicit transaction control
// (COMMIT, BEGIN, ...), which can split it into separately-committed parts, so
// recovery is refused for such files. It also assumes the dirty flag came from
// an up migration: cmd/migrate and the runtime binaries only ever migrate up,
// but a flag left by a manually-run down migration would make the re-run apply
// an already-applied migration — which is why migrations must stay idempotent
// (see internal/db/migrations/README.md).
func recoverDirtySchema(m versionForcer, db *sqlx.DB, migrationsPath string, dirtyVersion int) error {
	logger.Warn("Database schema is dirty; evaluating automatic recovery",
		zap.Int("dirty_version", dirtyVersion))

	// Re-check the recorded state: another migrator may have recovered or
	// finished while we were observing the error. If it is no longer dirty at
	// the version we saw, there is nothing to force — the caller retries Up.
	var row struct {
		Version int  `db:"version"`
		Dirty   bool `db:"dirty"`
	}
	if err := db.Get(&row, currentSchemaVersionQuery); err != nil {
		return fmt.Errorf("re-checking schema_migrations: %w", err)
	}
	if !row.Dirty || row.Version != dirtyVersion {
		logger.Warn("Schema is no longer dirty at the observed version; skipping force",
			zap.Int("observed_version", dirtyVersion),
			zap.Int("current_version", row.Version),
			zap.Bool("dirty", row.Dirty))
		return nil
	}

	forceTo, upFile, err := planDirtyRecovery(migrationsPath, dirtyVersion)
	if err != nil {
		return err
	}

	logger.Warn("Auto-recovering dirty schema: forcing version back and re-running the failed migration",
		zap.Int("dirty_version", dirtyVersion),
		zap.Int("force_to", forceTo),
		zap.String("migration", upFile))

	if err := m.Force(forceTo); err != nil {
		return fmt.Errorf("forcing schema version to %d: %w", forceTo, err)
	}
	return nil
}

// planDirtyRecovery decides whether the dirty version can be safely recovered
// and returns the version to force back to (-1 when the dirty migration is the
// first one) plus the up migration filename, or an error explaining why
// automatic recovery is refused.
func planDirtyRecovery(migrationsPath string, dirtyVersion int) (forceTo int, upFile string, err error) {
	if dirtyVersion < 1 {
		return 0, "", fmt.Errorf("dirty version %d does not correspond to an up migration", dirtyVersion)
	}

	files, err := upMigrationFiles(migrationsPath)
	if err != nil {
		return 0, "", err
	}

	upFile, ok := files[dirtyVersion]
	if !ok {
		return 0, "", fmt.Errorf("no bundled up migration for dirty version %d; refusing to guess", dirtyVersion)
	}

	content, err := os.ReadFile(filepath.Join(migrationsPath, upFile))
	if err != nil {
		return 0, "", fmt.Errorf("reading %s: %w", upFile, err)
	}
	if stmt, found := firstTransactionControlStatement(string(content)); found {
		return 0, "", fmt.Errorf(
			"migration %s contains explicit transaction control (%q); a killed run may have partially committed, refusing to auto-recover",
			upFile, stmt)
	}

	forceTo = -1 // golang-migrate's NilVersion: no migrations applied
	for v := range files {
		if v < dirtyVersion && v > forceTo {
			forceTo = v
		}
	}
	return forceTo, upFile, nil
}

// upMigrationFiles maps migration version -> up filename for every .up.sql in
// the migrations directory.
func upMigrationFiles(dir string) (map[int]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read migrations directory: %w", err)
	}

	files := make(map[int]string)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".up.sql") {
			continue
		}
		versionText, _, ok := strings.Cut(entry.Name(), "_")
		if !ok {
			return nil, fmt.Errorf("migration file %q does not start with a version prefix", entry.Name())
		}
		version, err := strconv.Atoi(versionText)
		if err != nil {
			return nil, fmt.Errorf("failed to parse migration version from %q: %w", entry.Name(), err)
		}
		files[version] = entry.Name()
	}
	return files, nil
}

// transactionControlKeywords are statements that split a multi-statement
// migration into separately-committed parts (or otherwise manipulate the
// implicit transaction golang-migrate relies on). Matching is on the first
// token of each top-level statement, so plpgsql BEGIN/END bodies (inside
// dollar quotes) and CASE ... END expressions (mid-statement) don't match.
//
//nolint:goconst // SQL keywords; the test file repeats them as expectations
var transactionControlKeywords = map[string]bool{
	"BEGIN":     true,
	"COMMIT":    true,
	"END":       true,
	"ROLLBACK":  true,
	"ABORT":     true,
	"START":     true, // START TRANSACTION
	"SAVEPOINT": true,
	"RELEASE":   true, // RELEASE SAVEPOINT
}

// firstTransactionControlStatement scans SQL for a top-level statement that
// begins with a transaction-control keyword, ignoring comments, string
// literals, quoted identifiers, and dollar-quoted bodies. It returns the
// offending keyword and true if one is found.
func firstTransactionControlStatement(sql string) (string, bool) {
	for _, stmt := range splitTopLevelStatements(sql) {
		first, second := firstTwoTokens(stmt)
		if transactionControlKeywords[first] {
			return first, true
		}
		// Two-phase commit: PREPARE TRANSACTION 'name'. Plain PREPARE (a
		// prepared statement) is fine.
		if first == "PREPARE" && second == "TRANSACTION" {
			return "PREPARE TRANSACTION", true
		}
	}
	return "", false
}

// splitTopLevelStatements splits SQL on semicolons that are outside comments,
// string literals, quoted identifiers, and dollar-quoted strings. Skipped
// regions are not included in the returned statement text.
func splitTopLevelStatements(sql string) []string {
	var (
		statements []string
		buf        strings.Builder
	)
	flush := func() {
		if s := strings.TrimSpace(buf.String()); s != "" {
			statements = append(statements, s)
		}
		buf.Reset()
	}

	for i := 0; i < len(sql); {
		switch {
		case strings.HasPrefix(sql[i:], "--"):
			if end := strings.IndexByte(sql[i:], '\n'); end >= 0 {
				i += end + 1
			} else {
				i = len(sql)
			}
			buf.WriteByte(' ')
		case strings.HasPrefix(sql[i:], "/*"):
			i += 2
			for depth := 1; depth > 0 && i < len(sql); {
				switch {
				case strings.HasPrefix(sql[i:], "/*"): // block comments nest in PostgreSQL
					depth++
					i += 2
				case strings.HasPrefix(sql[i:], "*/"):
					depth--
					i += 2
				default:
					i++
				}
			}
			buf.WriteByte(' ')
		case sql[i] == '\'':
			i++
			for i < len(sql) {
				if sql[i] == '\'' {
					if i+1 < len(sql) && sql[i+1] == '\'' { // '' escapes a quote
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			buf.WriteByte(' ')
		case sql[i] == '"':
			i++
			for i < len(sql) && sql[i] != '"' {
				i++
			}
			i++ // closing quote (or EOF)
			buf.WriteByte(' ')
		case sql[i] == '$':
			tag, ok := dollarQuoteTag(sql[i:])
			if !ok {
				buf.WriteByte(sql[i])
				i++
				break
			}
			i += len(tag)
			if end := strings.Index(sql[i:], tag); end >= 0 {
				i += end + len(tag)
			} else {
				i = len(sql) // unterminated dollar quote: skip the rest
			}
			buf.WriteByte(' ')
		case sql[i] == ';':
			flush()
			i++
		default:
			buf.WriteByte(sql[i])
			i++
		}
	}
	flush()
	return statements
}

// dollarQuoteTag reports whether s starts with a dollar-quote opening tag
// ($$, $tag$, ...) and returns the full tag including both dollar signs.
func dollarQuoteTag(s string) (string, bool) {
	if len(s) < 2 || s[0] != '$' {
		return "", false
	}
	for j := 1; j < len(s); j++ {
		c := s[j]
		switch {
		case c == '$':
			return s[:j+1], true
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z',
			j > 1 && c >= '0' && c <= '9':
			// valid tag character, keep scanning
		default:
			return "", false
		}
	}
	return "", false
}

// firstTwoTokens returns the first two whitespace-separated word tokens of a
// statement, uppercased.
func firstTwoTokens(stmt string) (first, second string) {
	fields := strings.FieldsFunc(stmt, func(r rune) bool {
		isWord := r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		return !isWord
	})
	if len(fields) > 0 {
		first = strings.ToUpper(fields[0])
	}
	if len(fields) > 1 {
		second = strings.ToUpper(fields[1])
	}
	return first, second
}
