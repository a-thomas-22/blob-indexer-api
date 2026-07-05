-- Revert the fine (60-second) chart rollup bucket: restore the pre-000004
-- trigger functions (block_metrics bodies from 000001, blobs bodies from
-- 000003 — no confirmed predicates) and the coarse bucket list, drop the
-- retention helpers, and remove the per-minute rows (bounded by the ~48h
-- retention window, so this delete stays small). Idempotent, no explicit
-- transaction control — see README.md.

CREATE OR REPLACE FUNCTION chart_rollup_bucket_seconds()
RETURNS TABLE (bucket_seconds INTEGER) AS $$
    VALUES (3600), (21600), (86400);
$$ LANGUAGE sql IMMUTABLE;

CREATE OR REPLACE FUNCTION block_metrics_rollups_insert_statement_trigger()
RETURNS trigger AS $$
DECLARE
    affected RECORD;
BEGIN
    FOR affected IN
        SELECT DISTINCT
            r.chain_id,
            g.bucket_seconds,
            chart_rollup_bucket_start(r.block_timestamp, g.bucket_seconds) AS bucket_start
        FROM new_rows r
        CROSS JOIN chart_rollup_bucket_seconds() g
    LOOP
        PERFORM block_metrics_rollups_refresh(affected.chain_id, affected.bucket_seconds, affected.bucket_start);
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION block_metrics_rollups_delete_statement_trigger()
RETURNS trigger AS $$
DECLARE
    affected RECORD;
BEGIN
    FOR affected IN
        SELECT DISTINCT
            r.chain_id,
            g.bucket_seconds,
            chart_rollup_bucket_start(r.block_timestamp, g.bucket_seconds) AS bucket_start
        FROM old_rows r
        CROSS JOIN chart_rollup_bucket_seconds() g
    LOOP
        PERFORM block_metrics_rollups_refresh(affected.chain_id, affected.bucket_seconds, affected.bucket_start);
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION block_metrics_rollups_update_statement_trigger()
RETURNS trigger AS $$
DECLARE
    affected RECORD;
BEGIN
    FOR affected IN
        SELECT DISTINCT
            r.chain_id,
            g.bucket_seconds,
            chart_rollup_bucket_start(r.block_timestamp, g.bucket_seconds) AS bucket_start
        FROM (
            SELECT chain_id, block_timestamp FROM old_rows
            UNION
            SELECT chain_id, block_timestamp FROM new_rows
        ) r
        CROSS JOIN chart_rollup_bucket_seconds() g
    LOOP
        PERFORM block_metrics_rollups_refresh(affected.chain_id, affected.bucket_seconds, affected.bucket_start);
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION blob_chart_rollups_insert_statement_trigger()
RETURNS trigger AS $$
BEGIN
    INSERT INTO blob_chart_rollups (
        chain_id, bucket_seconds, bucket_start, from_address, user_attribution,
        blob_count, blob_bytes, blob_gas_used, total_cost_wei, sum_size_base_fee, updated_at
    )
    SELECT
        r.chain_id,
        g.bucket_seconds,
        chart_rollup_bucket_start(r.timestamp, g.bucket_seconds),
        r.from_address,
        COALESCE(NULLIF(MAX(BTRIM(r.user_attribution)), ''), ''),
        COUNT(*)::bigint,
        COALESCE(SUM(r.blob_size_bytes), 0)::bigint,
        COALESCE(SUM(COALESCE(r.blob_gas_used, 0)), 0)::bigint,
        COALESCE(SUM(r.total_cost_wei::numeric), 0),
        COALESCE(SUM(r.blob_size_bytes::numeric * r.base_fee_per_blob_gas::numeric), 0),
        NOW()
    FROM new_blobs r
    CROSS JOIN chart_rollup_bucket_seconds() g
    GROUP BY r.chain_id, g.bucket_seconds, chart_rollup_bucket_start(r.timestamp, g.bucket_seconds), r.from_address
    ON CONFLICT (chain_id, bucket_seconds, bucket_start, from_address) DO UPDATE SET
        user_attribution = COALESCE(
            NULLIF(BTRIM(EXCLUDED.user_attribution), ''),
            NULLIF(BTRIM(blob_chart_rollups.user_attribution), ''),
            ''
        ),
        blob_count = blob_chart_rollups.blob_count + EXCLUDED.blob_count,
        blob_bytes = blob_chart_rollups.blob_bytes + EXCLUDED.blob_bytes,
        blob_gas_used = blob_chart_rollups.blob_gas_used + EXCLUDED.blob_gas_used,
        total_cost_wei = blob_chart_rollups.total_cost_wei + EXCLUDED.total_cost_wei,
        sum_size_base_fee = blob_chart_rollups.sum_size_base_fee + EXCLUDED.sum_size_base_fee,
        updated_at = NOW();
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION blob_chart_rollups_delete_statement_trigger()
RETURNS trigger AS $$
DECLARE
    affected RECORD;
BEGIN
    FOR affected IN
        SELECT DISTINCT
            r.chain_id,
            g.bucket_seconds,
            chart_rollup_bucket_start(r.timestamp, g.bucket_seconds) AS bucket_start,
            r.from_address
        FROM old_blobs r
        CROSS JOIN chart_rollup_bucket_seconds() g
    LOOP
        PERFORM blob_chart_rollups_refresh(
            affected.chain_id, affected.bucket_seconds, affected.bucket_start, affected.from_address);
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION blob_chart_rollups_update_statement_trigger()
RETURNS trigger AS $$
DECLARE
    affected RECORD;
BEGIN
    FOR affected IN
        SELECT DISTINCT
            r.chain_id,
            g.bucket_seconds,
            chart_rollup_bucket_start(r.timestamp, g.bucket_seconds) AS bucket_start,
            r.from_address
        FROM (
            SELECT chain_id, timestamp, from_address FROM old_blobs
            UNION
            SELECT chain_id, timestamp, from_address FROM new_blobs
        ) r
        CROSS JOIN chart_rollup_bucket_seconds() g
    LOOP
        PERFORM blob_chart_rollups_refresh(
            affected.chain_id, affected.bucket_seconds, affected.bucket_start, affected.from_address);
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP FUNCTION IF EXISTS chart_rollup_bucket_seconds_for(TIMESTAMP);
DROP FUNCTION IF EXISTS chart_rollup_fine_retention();
DROP FUNCTION IF EXISTS chart_rollup_fine_bucket_seconds();

DELETE FROM blob_chart_rollups WHERE bucket_seconds = 60;
DELETE FROM block_metrics_rollups WHERE bucket_seconds = 60;
