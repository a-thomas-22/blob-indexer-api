-- Serves the /blob/by-hash containment lookup
-- (versioned_hashes @> ARRAY[$1]). Separate from the column-add migration so
-- the build runs in its own transaction: CREATE INDEX takes only a SHARE
-- lock, so blob reads keep flowing while it scans the table, and only the
-- indexer's writes wait (it catches up via its normal retry/gap machinery).
-- Bundling it with ADD COLUMN would instead hold that migration's ACCESS
-- EXCLUSIVE lock across the whole build, blocking reads too. The scan is
-- cheap besides: every pre-existing row is NULL in versioned_hashes, so the
-- index starts effectively empty.
--
-- CREATE INDEX CONCURRENTLY is not an option here — it cannot run inside a
-- transaction (migration rule 1 in README.md).
--
-- mempool_blobs stays unindexed: it is a tiny UNLOGGED table whose seq scan
-- is cheaper than maintaining a GIN index under its constant churn.

CREATE INDEX IF NOT EXISTS idx_blobs_versioned_hashes
    ON blobs USING GIN (versioned_hashes);
