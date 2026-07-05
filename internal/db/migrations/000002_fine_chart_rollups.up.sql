-- Fine-grained (60-second) chart rollup bucket with bounded retention.
--
-- The API serves rolling-stats windows of 24h and shorter, and sub-hour chart
-- granularities, from raw blobs/block_metrics scans today. A per-minute rollup
-- bucket lets those reads stay O(buckets) like the wider windows already
-- served from hourly rollups. Unlike the coarse buckets, per-minute rows are
-- only useful for ~2 days of history, so they carry a retention window:
--   * chart_rollup_bucket_seconds_for(ts) hands the statement triggers the
--     bucket sizes applicable to a row timestamp — fine buckets are skipped
--     for rows older than chart_rollup_fine_retention(), so reindexes of deep
--     history neither create per-minute rows nor loop over per-minute buckets
--     on delete/update.
--   * The indexer prunes expired fine buckets on a timer and backfills the
--     retention window in bucket-aligned chunks on startup (heavy backfills
--     stay out of schema migrations; see README.md). Until that backfill
--     completes, the API detects missing fine coverage and falls back to raw
--     scans.
--
-- DDL only, idempotent, no explicit transaction control — see README.md.

-- ---------------------------------------------------------------------------
-- Bucket catalogue
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION chart_rollup_fine_bucket_seconds()
RETURNS INTEGER AS $$
    SELECT 60;
$$ LANGUAGE sql IMMUTABLE;

-- Mirrored by db.FineChartRollupRetention in Go (prune cutoff and backfill
-- span); keep the two in sync.
CREATE OR REPLACE FUNCTION chart_rollup_fine_retention()
RETURNS INTERVAL AS $$
    SELECT INTERVAL '48 hours';
$$ LANGUAGE sql IMMUTABLE;

CREATE OR REPLACE FUNCTION chart_rollup_bucket_seconds()
RETURNS TABLE (bucket_seconds INTEGER) AS $$
    VALUES (60), (3600), (21600), (86400);
$$ LANGUAGE sql IMMUTABLE;

-- Bucket sizes the triggers maintain for a row with the given timestamp: all
-- coarse buckets always, the fine bucket only within its retention window.
CREATE OR REPLACE FUNCTION chart_rollup_bucket_seconds_for(p_timestamp TIMESTAMP)
RETURNS TABLE (bucket_seconds INTEGER) AS $$
    SELECT g.bucket_seconds
    FROM chart_rollup_bucket_seconds() g
    WHERE g.bucket_seconds <> chart_rollup_fine_bucket_seconds()
        OR p_timestamp >= NOW() - chart_rollup_fine_retention();
$$ LANGUAGE sql STABLE;

-- ---------------------------------------------------------------------------
-- block_metrics trigger functions: swap the static bucket list for the
-- retention-aware per-timestamp list. Bodies otherwise match the baseline.
-- ---------------------------------------------------------------------------

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
        CROSS JOIN LATERAL chart_rollup_bucket_seconds_for(r.block_timestamp) g
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
        CROSS JOIN LATERAL chart_rollup_bucket_seconds_for(r.block_timestamp) g
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
        CROSS JOIN LATERAL chart_rollup_bucket_seconds_for(r.block_timestamp) g
    LOOP
        PERFORM block_metrics_rollups_refresh(affected.chain_id, affected.bucket_seconds, affected.bucket_start);
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

-- ---------------------------------------------------------------------------
-- blobs trigger functions: same swap.
-- ---------------------------------------------------------------------------

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
    CROSS JOIN LATERAL chart_rollup_bucket_seconds_for(r.timestamp) g
    WHERE r.confirmed = true
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
        CROSS JOIN LATERAL chart_rollup_bucket_seconds_for(r.timestamp) g
        WHERE r.confirmed = true
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
            SELECT chain_id, timestamp, from_address FROM old_blobs WHERE confirmed = true
            UNION
            SELECT chain_id, timestamp, from_address FROM new_blobs WHERE confirmed = true
        ) r
        CROSS JOIN LATERAL chart_rollup_bucket_seconds_for(r.timestamp) g
    LOOP
        PERFORM blob_chart_rollups_refresh(
            affected.chain_id, affected.bucket_seconds, affected.bucket_start, affected.from_address);
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
