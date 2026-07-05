-- Restore blobs.confirmed and the confirmed-keyed indexes/trigger predicates.
--
-- Every surviving row in blobs is confirmed (pending rows live in
-- mempool_blobs since 000002), so ADD COLUMN ... DEFAULT TRUE backfills the
-- correct value for free (metadata-only fill). Trigger function bodies are
-- restored verbatim from 000001. The replacement indexes added by the up
-- migration are dropped last, after their confirmed-keyed equivalents are
-- back. DDL only, idempotent, no explicit transaction control — see README.md.

ALTER TABLE blobs ADD COLUMN IF NOT EXISTS confirmed BOOLEAN DEFAULT TRUE;

-- Recreate the confirmed-keyed indexes before dropping their replacements.
CREATE INDEX IF NOT EXISTS idx_blobs_confirmed ON blobs(confirmed);
CREATE INDEX IF NOT EXISTS idx_blobs_chain_confirmed_block ON blobs(chain_id, confirmed, block_number DESC);
CREATE INDEX IF NOT EXISTS idx_blobs_chain_confirmed_timestamp ON blobs(chain_id, confirmed, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_blobs_chain_confirmed_timestamp_cover
    ON blobs(chain_id, confirmed, timestamp DESC)
    INCLUDE (from_address, total_cost_wei, base_fee_per_blob_gas, blob_gas_used);
CREATE INDEX IF NOT EXISTS idx_blobs_chain_confirmed_timestamp_chart_cover
    ON blobs(chain_id, confirmed, timestamp DESC)
    INCLUDE (from_address, user_attribution, total_cost_wei, base_fee_per_blob_gas, blob_gas_used, blob_size_bytes);

-- Restore the 000001 trigger function bodies (confirmed predicates included).

CREATE OR REPLACE FUNCTION network_blob_stats_blobs_insert_trigger()
RETURNS trigger AS $$
DECLARE
    delta RECORD;
BEGIN
    FOR delta IN
        SELECT
            chain_id,
            COUNT(*)::bigint AS count_delta,
            COALESCE(SUM(base_fee_per_blob_gas::numeric), 0) AS sum_base_fee_delta,
            COALESCE(SUM(tip_per_blob_gas::numeric), 0) AS sum_tip_delta,
            COALESCE(SUM(total_cost_wei::numeric), 0) AS sum_total_cost_delta
        FROM new_rows
        WHERE confirmed = true
        GROUP BY chain_id
    LOOP
        PERFORM network_blob_stats_apply_delta(
            delta.chain_id, delta.count_delta, delta.sum_base_fee_delta,
            delta.sum_tip_delta, delta.sum_total_cost_delta);
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION network_blob_stats_blobs_delete_trigger()
RETURNS trigger AS $$
DECLARE
    delta RECORD;
BEGIN
    FOR delta IN
        SELECT
            chain_id,
            -COUNT(*)::bigint AS count_delta,
            -COALESCE(SUM(base_fee_per_blob_gas::numeric), 0) AS sum_base_fee_delta,
            -COALESCE(SUM(tip_per_blob_gas::numeric), 0) AS sum_tip_delta,
            -COALESCE(SUM(total_cost_wei::numeric), 0) AS sum_total_cost_delta
        FROM old_rows
        WHERE confirmed = true
        GROUP BY chain_id
    LOOP
        PERFORM network_blob_stats_apply_delta(
            delta.chain_id, delta.count_delta, delta.sum_base_fee_delta,
            delta.sum_tip_delta, delta.sum_total_cost_delta);
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION network_blob_stats_blobs_update_trigger()
RETURNS trigger AS $$
DECLARE
    delta RECORD;
BEGIN
    FOR delta IN
        SELECT
            chain_id,
            SUM(count_delta)::bigint AS count_delta,
            SUM(sum_base_fee_delta) AS sum_base_fee_delta,
            SUM(sum_tip_delta) AS sum_tip_delta,
            SUM(sum_total_cost_delta) AS sum_total_cost_delta
        FROM (
            SELECT
                chain_id,
                -COUNT(*)::bigint AS count_delta,
                -COALESCE(SUM(base_fee_per_blob_gas::numeric), 0) AS sum_base_fee_delta,
                -COALESCE(SUM(tip_per_blob_gas::numeric), 0) AS sum_tip_delta,
                -COALESCE(SUM(total_cost_wei::numeric), 0) AS sum_total_cost_delta
            FROM old_rows
            WHERE confirmed = true
            GROUP BY chain_id
            UNION ALL
            SELECT
                chain_id,
                COUNT(*)::bigint AS count_delta,
                COALESCE(SUM(base_fee_per_blob_gas::numeric), 0) AS sum_base_fee_delta,
                COALESCE(SUM(tip_per_blob_gas::numeric), 0) AS sum_tip_delta,
                COALESCE(SUM(total_cost_wei::numeric), 0) AS sum_total_cost_delta
            FROM new_rows
            WHERE confirmed = true
            GROUP BY chain_id
        ) deltas
        GROUP BY chain_id
        HAVING SUM(count_delta) <> 0
            OR SUM(sum_base_fee_delta) <> 0
            OR SUM(sum_tip_delta) <> 0
            OR SUM(sum_total_cost_delta) <> 0
    LOOP
        PERFORM network_blob_stats_apply_delta(
            delta.chain_id, delta.count_delta, delta.sum_base_fee_delta,
            delta.sum_tip_delta, delta.sum_total_cost_delta);
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION blob_chart_rollups_refresh(
    p_chain_id INTEGER,
    p_bucket_seconds INTEGER,
    p_bucket_start TIMESTAMP,
    p_from_address TEXT
)
RETURNS void AS $$
DECLARE
    agg RECORD;
BEGIN
    SELECT
        COUNT(*)::bigint AS blob_count,
        COALESCE(NULLIF(MAX(BTRIM(user_attribution)), ''), '') AS user_attribution,
        COALESCE(SUM(blob_size_bytes), 0)::bigint AS blob_bytes,
        COALESCE(SUM(COALESCE(blob_gas_used, 0)), 0)::bigint AS blob_gas_used,
        COALESCE(SUM(total_cost_wei::numeric), 0) AS total_cost_wei,
        COALESCE(SUM(blob_size_bytes::numeric * base_fee_per_blob_gas::numeric), 0) AS sum_size_base_fee
    INTO agg
    FROM blobs
    WHERE chain_id = p_chain_id
        AND from_address = p_from_address
        AND confirmed = true
        AND timestamp >= p_bucket_start
        AND timestamp < p_bucket_start + (p_bucket_seconds * INTERVAL '1 second');

    IF agg.blob_count = 0 THEN
        DELETE FROM blob_chart_rollups
        WHERE chain_id = p_chain_id
            AND bucket_seconds = p_bucket_seconds
            AND bucket_start = p_bucket_start
            AND from_address = p_from_address;
        RETURN;
    END IF;

    INSERT INTO blob_chart_rollups (
        chain_id, bucket_seconds, bucket_start, from_address, user_attribution,
        blob_count, blob_bytes, blob_gas_used, total_cost_wei, sum_size_base_fee, updated_at
    )
    VALUES (
        p_chain_id, p_bucket_seconds, p_bucket_start, p_from_address, agg.user_attribution,
        agg.blob_count, agg.blob_bytes, agg.blob_gas_used, agg.total_cost_wei, agg.sum_size_base_fee, NOW()
    )
    ON CONFLICT (chain_id, bucket_seconds, bucket_start, from_address) DO UPDATE SET
        user_attribution = EXCLUDED.user_attribution,
        blob_count = EXCLUDED.blob_count,
        blob_bytes = EXCLUDED.blob_bytes,
        blob_gas_used = EXCLUDED.blob_gas_used,
        total_cost_wei = EXCLUDED.total_cost_wei,
        sum_size_base_fee = EXCLUDED.sum_size_base_fee,
        updated_at = NOW();
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
        CROSS JOIN chart_rollup_bucket_seconds() g
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
        CROSS JOIN chart_rollup_bucket_seconds() g
    LOOP
        PERFORM blob_chart_rollups_refresh(
            affected.chain_id, affected.bucket_seconds, affected.bucket_start, affected.from_address);
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

-- Drop the replacement indexes added by the up migration.
DROP INDEX IF EXISTS idx_blobs_chain_block;
DROP INDEX IF EXISTS idx_blobs_chain_timestamp_cover;
DROP INDEX IF EXISTS idx_blobs_chain_timestamp_chart_cover;
