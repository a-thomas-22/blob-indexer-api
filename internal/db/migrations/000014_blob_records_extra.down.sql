-- Reverse 000014_blob_records_extra.up.sql: drop the new leaderboard index,
-- remove the rows for the kinds 000013 did not know about, and restore the
-- two-kind catalog, constraint, and recompute body.
--
-- The definition-version function goes away with it. The indexer reads a
-- missing function as "unknown", which does not match any stored fingerprint,
-- so the next startup rebuilds the two surviving kinds from scratch rather
-- than trusting a checkpoint advanced under four-kind definitions.

DROP INDEX IF EXISTS idx_block_metrics_chain_blob_spend;

DROP FUNCTION IF EXISTS blob_record_streak_definition_version();

CREATE OR REPLACE FUNCTION blob_record_streak_kinds()
RETURNS TABLE (kind TEXT) AS $$
    VALUES ('full'), ('above_target');
$$ LANGUAGE sql IMMUTABLE;

DELETE FROM blob_block_streaks WHERE kind NOT IN ('full', 'above_target');

ALTER TABLE blob_block_streaks
    DROP CONSTRAINT IF EXISTS blob_block_streaks_kind_check;
ALTER TABLE blob_block_streaks
    ADD CONSTRAINT blob_block_streaks_kind_check
    CHECK (kind IN ('full', 'above_target'));

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

    DELETE FROM blob_block_streaks
    WHERE chain_id = p_chain_id
        AND kind = p_kind
        AND start_block >= v_from
        AND start_block <= v_to;

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
