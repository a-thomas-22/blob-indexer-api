//go:build integration

// Package testdb hands each integration-test package its own database
// derived from TEST_DB_URL. go test runs package test binaries in parallel,
// and the db, api, and indexer integration suites all reset their schema
// with DROP SCHEMA public CASCADE — sharing one database across packages
// makes those resets race (a reset in one binary lands mid-migration in
// another, failing with `schema "public" does not exist`). Per-package
// databases make the resets safe without serializing the run with -p 1.
//
// The package is gated by the integration build tag so it stays out of
// regular builds and the coverage denominator.
package testdb

import (
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
)

// lockKey serializes CREATE DATABASE across concurrently running test
// binaries: Postgres refuses to copy a template database that another
// CREATE DATABASE is using, so unsynchronized first runs could still flake.
const lockKey = 0x626c6f62 // "blob"

// URL returns the connection string for a database dedicated to the calling
// test package: TEST_DB_URL with "_<suffix>" appended to the database name.
// The database is created if it does not exist. Skips the test when
// TEST_DB_URL is unset.
func URL(t *testing.T, suffix string) string {
	t.Helper()
	base := os.Getenv("TEST_DB_URL")
	if base == "" {
		t.Skip("TEST_DB_URL not set; skipping integration test")
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || len(strings.TrimPrefix(u.Path, "/")) == 0 {
		t.Fatalf("TEST_DB_URL must be a postgres://user:pass@host/dbname URL, got %q (err: %v)", base, err)
	}
	name := strings.TrimPrefix(u.Path, "/") + "_" + suffix
	ensureDatabase(t, base, name)
	u.Path = "/" + name
	return u.String()
}

// ensureDatabase creates the derived database when missing. It connects to
// the TEST_DB_URL database only to issue CREATE DATABASE; the connection
// works even while other binaries reset that database's schema.
func ensureDatabase(t *testing.T, adminURL, name string) {
	t.Helper()
	admin, err := sqlx.Connect("postgres", adminURL)
	if err != nil {
		t.Fatalf("connect to TEST_DB_URL for database creation: %v", err)
	}
	defer admin.Close()
	// pg_advisory_lock is session-scoped; pin the pool to one connection so
	// the lock, the existence check, and CREATE DATABASE share a session.
	admin.SetMaxOpenConns(1)

	if _, err := admin.Exec("SELECT pg_advisory_lock($1)", lockKey); err != nil {
		t.Fatalf("acquire test-database advisory lock: %v", err)
	}
	defer admin.Exec("SELECT pg_advisory_unlock($1)", lockKey)

	var exists bool
	if err := admin.Get(&exists, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name); err != nil {
		t.Fatalf("check database %q: %v", name, err)
	}
	if !exists {
		if _, err := admin.Exec("CREATE DATABASE " + pq.QuoteIdentifier(name)); err != nil {
			t.Fatalf("create database %q: %v", name, err)
		}
	}
}
