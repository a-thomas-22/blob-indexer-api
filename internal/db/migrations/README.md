# Migration authoring rules

Migrations run via golang-migrate from a Kubernetes Job (an Argo CD `PreSync`
hook in production). A migration run can be killed at any moment — node
drain, deploy retry, operator action — so every migration must tolerate dying
mid-flight. These rules exist because migration 12's multi-minute backfill was
killed by an Argo CD sync retry on 2026-06-11, which left the schema dirty and
wedged all deploys until manual recovery.

## 1. Keep schema migrations fast

A migration file should contain DDL (tables, indexes, triggers, functions) and
only trivial DML. Target seconds, not minutes. Anything that scans or rewrites
a large table (the `blobs` table is ~48M rows and growing) does **not** belong
in a migration file.

Heavy backfills instead run **outside** the schema migration, chunked and
idempotently:

- The migration creates the destination table/columns and any triggers that
  keep *new* data consistent, plus (if needed) a progress-tracking table.
- The backfill recomputes in bounded chunks (per address range, per block
  range, per bucket), records progress as it goes, and uses
  `ON CONFLICT ... DO UPDATE` / recompute-from-source semantics so re-running
  a chunk is harmless.
- Run the backfill either from the migrate binary after `m.Up()` returns, or
  as a separate post-install Job — never inside the `.up.sql`.

A kill mid-backfill then costs one chunk of progress, not the schema state.

## 2. No explicit transaction control

The postgres driver executes each `.sql` file as a single multi-statement
`Exec`, which PostgreSQL wraps in one implicit transaction. That guarantee is
what makes automatic dirty-flag recovery safe (`recoverDirtySchema` in
`internal/db/migrate_recovery.go`): if a run is killed, the whole file rolled
back, so forcing the version back and re-running cannot double-apply anything.

Therefore migration files must not contain top-level `BEGIN`, `COMMIT`,
`ROLLBACK`, `END`, `SAVEPOINT`, `START TRANSACTION`, etc. (plpgsql
`BEGIN/END` inside `$$` bodies is fine). This also rules out statements that
cannot run inside a transaction, such as `CREATE INDEX CONCURRENTLY`.
`TestBundledMigrationsAreTransactionSafe` enforces this in CI.

## 3. Write idempotent migrations

Use `IF NOT EXISTS` / `IF EXISTS` / `CREATE OR REPLACE` /
`ON CONFLICT ... DO UPDATE` everywhere they apply. The dirty-flag recovery
re-runs the failed migration; in the (tiny) window where a migration committed
but the dirty flag was never cleared, the re-run must be a no-op rather than
an error. The same applies if an operator manually forces versions around.

## Dirty-state recovery

If a migration run dies between writing the dirty flag and clearing it,
`db.RunMigrations` detects `Dirty database version N`, verifies that
migration N is bundled and transaction-safe (rule 2), logs loudly, forces the
version back to N-1, and re-runs. Manual recovery
(`migrate ... force <N-1>` + re-run, see the runbook in ops memory) is only
needed if the migration file violates these rules.
