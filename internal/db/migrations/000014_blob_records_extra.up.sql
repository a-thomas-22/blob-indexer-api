-- Additional /api/v1/records leaderboards.
--
-- Two new streak predicates and one new index. Everything else the endpoint
-- gained in this migration's companion change (busiest days, highest
-- utilization days, top spenders) reads existing trigger-maintained rollups
-- and needs no schema at all.
--
--   * 'drought'      -> runs of consecutive indexed blocks carrying no blobs.
--                       The inverse of the full-block card, and the one that
--                       stays interesting on quiet networks.
--   * 'below_target' -> runs of blocks strictly under their blob target.
--   * most expensive blocks -> an expression index ordering block_metrics by
--                       blob_base_fee * blob_count, which is the block's total
--                       blob spend up to the constant 131072 gas per blob.
--
-- Adding a streak kind is deliberately cheap: extend the kind catalog, the
-- CHECK constraint, and the recompute's CASE. The triggers, the range-widening
-- logic, the backfill, and the read query are all already parameterized by
-- kind.
--
-- DDL only, idempotent, no explicit transaction control -- see README.md.

-- ---------------------------------------------------------------------------
-- Kind catalog
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION blob_record_streak_kinds()
RETURNS TABLE (kind TEXT) AS $$
    VALUES ('full'), ('above_target'), ('drought'), ('below_target');
$$ LANGUAGE sql IMMUTABLE;

-- Bump this whenever a predicate's meaning changes without the kind list
-- changing. The indexer fingerprints (version, kind list) alongside its
-- backfill checkpoint and rebuilds history from scratch when the fingerprint
-- moves, which is what keeps a definition change from silently applying to
-- new blocks only. 000013 shipped no version function, which reads as
-- "unknown" and therefore also forces the one-time rebuild this migration
-- needs for its two new kinds.
CREATE OR REPLACE FUNCTION blob_record_streak_definition_version()
RETURNS INTEGER AS $$
    SELECT 2;
$$ LANGUAGE sql IMMUTABLE;

-- The constraint is replaced rather than altered in place; naming it explicitly
-- makes the drop idempotent across re-runs. Existing rows only ever hold the
-- two original kinds, so the revalidation scan cannot fail.
ALTER TABLE blob_block_streaks
    DROP CONSTRAINT IF EXISTS blob_block_streaks_kind_check;
ALTER TABLE blob_block_streaks
    ADD CONSTRAINT blob_block_streaks_kind_check
    CHECK (kind IN ('full', 'above_target', 'drought', 'below_target'));

-- ---------------------------------------------------------------------------
-- Recompute: same three steps as 000013, with two more predicates.
-- ---------------------------------------------------------------------------

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

    -- Step 2: drop the runs being rebuilt.
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
                -- A drought needs no fork schedule: an empty block is empty
                -- under any blob parameters, so unlike the other predicates
                -- this one classifies every indexed block.
                WHEN 'drought' THEN
                    blob_count <= 0
                WHEN 'below_target' THEN
                    blob_record_target_blobs(blob_params_target, blob_gas_target) > 0
                    AND blob_count < blob_record_target_blobs(blob_params_target, blob_gas_target)
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

-- ---------------------------------------------------------------------------
-- Most expensive blocks
-- ---------------------------------------------------------------------------

-- A block's total blob spend is blob_base_fee * blob_count * 131072. The
-- constant factor does not affect ordering, so the index carries the cheaper
-- product and the read multiplies it out for the wei value. This deliberately
-- ranks on blob_count rather than blob_gas_used so the figure agrees with the
-- blobs table's own cost accounting, which charges every blob the same
-- 131072 gas (see calculateBlobMetrics in internal/indexer).
--
-- Ordering by fee alone is already served by
-- idx_block_metrics_chain_blob_base_fee; this is a different ranking, since a
-- full block at a moderate fee can outspend a near-empty block at a peak fee.
--
-- Locking: same CREATE INDEX caveat as 000013. block_metrics is one row per
-- block, so the build is seconds at the current scale; pre-run it with
-- CREATE INDEX CONCURRENTLY IF NOT EXISTS under the same name and definition
-- if the table has grown, and this migration then no-ops.
CREATE INDEX IF NOT EXISTS idx_block_metrics_chain_blob_spend
    ON block_metrics(chain_id, ((blob_base_fee * blob_count)) DESC, block_number DESC)
    INCLUDE (block_timestamp, blob_count, blob_base_fee);
