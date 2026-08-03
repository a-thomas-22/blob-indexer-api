-- Historical leaderboards for GET /api/v1/records.
--
-- Three of the four leaderboards are already derivable from incrementally
-- maintained state; only the streak runs need new storage:
--
--   * base_fee_peaks   -> a (chain_id, blob_base_fee DESC, block_number DESC)
--                         index on block_metrics turns "highest blob base fee"
--                         into a top-N index scan.
--   * busiest_hours    -> block_metrics_rollups already carries sum_blob_count
--                         per hourly bucket and blob_chart_rollups carries
--                         total_cost_wei per (hour, sender); a partial index on
--                         the hourly block_metrics_rollups rows ranks the hours
--                         and the cost sum is then keyed to the top-N buckets.
--   * streaks          -> blob_block_streaks below.
--
-- blob_block_streaks stores one row per maximal run of consecutive indexed
-- blocks satisfying a predicate ('full' or 'above_target'). Runs are disjoint
-- and never adjacent (two adjacent runs would not be maximal), which is what
-- lets the maintenance function below expand an affected block range to whole
-- runs with two keyed lookups instead of walking the chain.
--
-- Maintenance is a single range-recompute primitive rather than per-event
-- merge/split logic, because block_metrics rows arrive out of order (the
-- indexer commits blocks concurrently), get rewritten by reindexes, and get
-- deleted in ranges by reorg rewinds. blob_block_streaks_recompute(chain, kind,
-- from, to) handles all of those uniformly: it widens the range to cover any
-- run touching or adjoining it, drops the runs inside, and rebuilds them from
-- block_metrics with a gaps-and-islands grouping. Deleted blocks need no
-- special case because the rebuild only sees the rows that survive.
--
-- The recompute is idempotent, so re-running a range converges. It is not
-- safe to interleave two recomputes over overlapping ranges, though: the
-- second would widen its range from a snapshot the first is about to replace.
-- Nothing extra is needed for that today because every block_metrics write
-- path in the indexer (block insert, reorg rewind, reindex delete) already
-- holds the per-network write lock, and so does the backfill below.
--
-- History is populated by the indexer's chunked startup backfill
-- (internal/indexer/records.go), not here: heavy backfills stay out of schema
-- migrations, see README.md.
--
-- Locking note: the two CREATE INDEX statements scan block_metrics and
-- block_metrics_rollups under a SHARE lock, blocking indexer writes for the
-- duration. block_metrics is one row per block, so this is seconds at the
-- current scale. If the table has grown large by deploy time, pre-run both
-- statements out-of-band with CREATE INDEX CONCURRENTLY IF NOT EXISTS using
-- the same names and definitions; this migration then no-ops.
--
-- DDL only, idempotent, no explicit transaction control -- see README.md.

-- ---------------------------------------------------------------------------
-- Predicate helpers
-- ---------------------------------------------------------------------------

-- blob_record_max_blobs / blob_record_target_blobs mirror blobSpaceLimit() in
-- internal/api/blob_handlers.go: the fork schedule's blob count when the
-- indexer recorded one, else the gas limit converted at 131072 gas per blob,
-- else 0 meaning "unknown" (which makes both predicates false).
CREATE OR REPLACE FUNCTION blob_record_max_blobs(p_blob_params_max INTEGER, p_blob_gas_limit BIGINT)
RETURNS INTEGER AS $$
    SELECT CASE
        WHEN p_blob_params_max > 0 THEN p_blob_params_max
        WHEN p_blob_gas_limit > 0 THEN (p_blob_gas_limit / 131072)::integer
        ELSE 0
    END;
$$ LANGUAGE sql IMMUTABLE;

CREATE OR REPLACE FUNCTION blob_record_target_blobs(p_blob_params_target INTEGER, p_blob_gas_target BIGINT)
RETURNS INTEGER AS $$
    SELECT CASE
        WHEN p_blob_params_target > 0 THEN p_blob_params_target
        WHEN p_blob_gas_target > 0 THEN (p_blob_gas_target / 131072)::integer
        ELSE 0
    END;
$$ LANGUAGE sql IMMUTABLE;

-- blob_record_streak_kinds is the catalog of maintained predicates. The
-- maintenance triggers loop over it, so adding a kind here plus a branch in
-- blob_block_streaks_recompute is all a new streak leaderboard needs.
CREATE OR REPLACE FUNCTION blob_record_streak_kinds()
RETURNS TABLE (kind TEXT) AS $$
    VALUES ('full'), ('above_target');
$$ LANGUAGE sql IMMUTABLE;

-- ---------------------------------------------------------------------------
-- blob_block_streaks
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS blob_block_streaks (
    chain_id INTEGER NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('full', 'above_target')),
    start_block BIGINT NOT NULL,
    end_block BIGINT NOT NULL CHECK (end_block >= start_block),
    length BIGINT NOT NULL CHECK (length > 0),
    start_timestamp TIMESTAMP NOT NULL,
    end_timestamp TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL DEFAULT NOW(),
    PRIMARY KEY (chain_id, kind, start_block),
    CONSTRAINT fk_blob_block_streaks_network_chain_id
        FOREIGN KEY (chain_id) REFERENCES networks(chain_id)
        ON UPDATE RESTRICT ON DELETE RESTRICT
);

-- Serves the top-N leaderboard read in its exact sort order (length desc,
-- end_block desc), so /records never sorts a result set.
CREATE INDEX IF NOT EXISTS idx_blob_block_streaks_chain_kind_length
    ON blob_block_streaks(chain_id, kind, length DESC, end_block DESC);

-- ---------------------------------------------------------------------------
-- Range recompute
-- ---------------------------------------------------------------------------

-- blob_block_streaks_recompute rebuilds every maximal run overlapping
-- [p_from_block, p_to_block] for one predicate.
--
-- Step 1 widens the range so no run is left half-rebuilt. A run overlapping or
-- adjoining the range on the left is found by taking the last run starting at
-- or before p_from_block; on the right, by taking the last run starting at or
-- before p_to_block + 1. Runs are disjoint and ordered, so those two probes see
-- every run that could merge with the rebuilt region, and every run intersecting
-- the widened range then starts inside it.
--
-- Step 3 groups by (block_number - row_number()), which is constant exactly
-- across blocks that are both consecutively numbered and consecutively
-- qualifying. A block that is missing from block_metrics (an unindexed gap or a
-- reorged-out block) and a block that is present but does not qualify therefore
-- both break the run, which is the documented contract.
CREATE OR REPLACE FUNCTION blob_block_streaks_recompute(
    p_chain_id INTEGER,
    p_kind TEXT,
    p_from_block BIGINT,
    p_to_block BIGINT
)
RETURNS void AS $$
DECLARE
    v_from BIGINT := LEAST(p_from_block, p_to_block);
    v_to BIGINT := GREATEST(p_from_block, p_to_block);
    v_edge RECORD;
BEGIN
    -- Step 1: widen to whole runs.
    SELECT start_block, end_block INTO v_edge
    FROM blob_block_streaks
    WHERE chain_id = p_chain_id AND kind = p_kind AND start_block <= v_from
    ORDER BY start_block DESC
    LIMIT 1;
    IF FOUND AND v_edge.end_block >= v_from - 1 THEN
        v_from := LEAST(v_from, v_edge.start_block);
        v_to := GREATEST(v_to, v_edge.end_block);
    END IF;

    SELECT start_block, end_block INTO v_edge
    FROM blob_block_streaks
    WHERE chain_id = p_chain_id AND kind = p_kind AND start_block <= v_to + 1
    ORDER BY start_block DESC
    LIMIT 1;
    IF FOUND THEN
        v_to := GREATEST(v_to, v_edge.end_block);
    END IF;

    -- Step 2: drop the runs being rebuilt. Every run intersecting the widened
    -- range starts inside it, so the primary key range is exhaustive.
    DELETE FROM blob_block_streaks
    WHERE chain_id = p_chain_id
        AND kind = p_kind
        AND start_block >= v_from
        AND start_block <= v_to;

    -- Step 3: rebuild from the surviving block_metrics rows.
    INSERT INTO blob_block_streaks (
        chain_id, kind, start_block, end_block, length,
        start_timestamp, end_timestamp, updated_at
    )
    SELECT
        p_chain_id,
        p_kind,
        MIN(q.block_number),
        MAX(q.block_number),
        COUNT(*)::bigint,
        MIN(q.block_timestamp),
        MAX(q.block_timestamp),
        NOW()
    FROM (
        SELECT
            block_number,
            block_timestamp,
            block_number - ROW_NUMBER() OVER (ORDER BY block_number) AS run_key
        FROM block_metrics
        WHERE chain_id = p_chain_id
            AND block_number >= v_from
            AND block_number <= v_to
            AND CASE p_kind
                WHEN 'full' THEN
                    blob_record_max_blobs(blob_params_max, blob_gas_limit) > 0
                    AND blob_count >= blob_record_max_blobs(blob_params_max, blob_gas_limit)
                WHEN 'above_target' THEN
                    blob_record_target_blobs(blob_params_target, blob_gas_target) > 0
                    AND blob_count > blob_record_target_blobs(blob_params_target, blob_gas_target)
                ELSE false
            END
    ) q
    GROUP BY q.run_key
    ON CONFLICT (chain_id, kind, start_block) DO UPDATE SET
        end_block = EXCLUDED.end_block,
        length = EXCLUDED.length,
        start_timestamp = EXCLUDED.start_timestamp,
        end_timestamp = EXCLUDED.end_timestamp,
        updated_at = NOW();
END;
$$ LANGUAGE plpgsql;

-- blob_block_streaks_recompute_all runs every cataloged predicate over one
-- block range.
CREATE OR REPLACE FUNCTION blob_block_streaks_recompute_all(
    p_chain_id INTEGER,
    p_from_block BIGINT,
    p_to_block BIGINT
)
RETURNS void AS $$
DECLARE
    k RECORD;
BEGIN
    FOR k IN SELECT kind FROM blob_record_streak_kinds() LOOP
        PERFORM blob_block_streaks_recompute(p_chain_id, k.kind, p_from_block, p_to_block);
    END LOOP;
END;
$$ LANGUAGE plpgsql;

-- ---------------------------------------------------------------------------
-- Statement triggers on block_metrics
-- ---------------------------------------------------------------------------

-- One recompute per (chain, kind) over the statement's block range. The indexer
-- commits one block per statement on the live path, so the range is a single
-- block there; bulk writes (seeding, reindex ranges) collapse into one pass.
CREATE OR REPLACE FUNCTION blob_block_streaks_insert_statement_trigger()
RETURNS trigger AS $$
DECLARE
    affected RECORD;
BEGIN
    FOR affected IN
        SELECT chain_id, MIN(block_number) AS from_block, MAX(block_number) AS to_block
        FROM new_rows
        GROUP BY chain_id
    LOOP
        PERFORM blob_block_streaks_recompute_all(affected.chain_id, affected.from_block, affected.to_block);
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION blob_block_streaks_delete_statement_trigger()
RETURNS trigger AS $$
DECLARE
    affected RECORD;
BEGIN
    FOR affected IN
        SELECT chain_id, MIN(block_number) AS from_block, MAX(block_number) AS to_block
        FROM old_rows
        GROUP BY chain_id
    LOOP
        PERFORM blob_block_streaks_recompute_all(affected.chain_id, affected.from_block, affected.to_block);
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION blob_block_streaks_update_statement_trigger()
RETURNS trigger AS $$
DECLARE
    affected RECORD;
BEGIN
    FOR affected IN
        SELECT chain_id, MIN(block_number) AS from_block, MAX(block_number) AS to_block
        FROM (
            SELECT chain_id, block_number FROM old_rows
            UNION ALL
            SELECT chain_id, block_number FROM new_rows
        ) r
        GROUP BY chain_id
    LOOP
        PERFORM blob_block_streaks_recompute_all(affected.chain_id, affected.from_block, affected.to_block);
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_blob_block_streaks_insert ON block_metrics;
CREATE TRIGGER trg_blob_block_streaks_insert
AFTER INSERT ON block_metrics
REFERENCING NEW TABLE AS new_rows
FOR EACH STATEMENT
EXECUTE FUNCTION blob_block_streaks_insert_statement_trigger();

DROP TRIGGER IF EXISTS trg_blob_block_streaks_update ON block_metrics;
CREATE TRIGGER trg_blob_block_streaks_update
AFTER UPDATE ON block_metrics
REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows
FOR EACH STATEMENT
EXECUTE FUNCTION blob_block_streaks_update_statement_trigger();

DROP TRIGGER IF EXISTS trg_blob_block_streaks_delete ON block_metrics;
CREATE TRIGGER trg_blob_block_streaks_delete
AFTER DELETE ON block_metrics
REFERENCING OLD TABLE AS old_rows
FOR EACH STATEMENT
EXECUTE FUNCTION blob_block_streaks_delete_statement_trigger();

-- ---------------------------------------------------------------------------
-- Indexes for the remaining leaderboards
-- ---------------------------------------------------------------------------

-- Top-N blocks by blob base fee, in the response's exact order. The INCLUDE
-- columns make the peaks read index-only.
CREATE INDEX IF NOT EXISTS idx_block_metrics_chain_blob_base_fee
    ON block_metrics(chain_id, blob_base_fee DESC, block_number DESC)
    INCLUDE (block_timestamp, blob_count);

-- Top-N hourly buckets by blob count. Partial on the hourly bucket size, which
-- is the only one /records ranks.
CREATE INDEX IF NOT EXISTS idx_block_metrics_rollups_hourly_blob_count
    ON block_metrics_rollups(chain_id, sum_blob_count DESC, bucket_start DESC)
    WHERE bucket_seconds = 3600;
