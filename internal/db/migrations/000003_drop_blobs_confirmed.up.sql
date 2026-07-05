-- Drop the vestigial blobs.confirmed column.
--
-- Since pending (mempool) rows moved to the dedicated mempool_blobs table
-- (000002), every row in blobs is confirmed by construction: the indexer only
-- writes confirmed rows, and 000002 deleted the historical pending sentinel
-- rows. The confirmed column is therefore a constant TRUE, the
-- confirmed-keyed indexes burn write amplification and cache for a key with
-- one value, and every `confirmed = true` predicate is a no-op. The API keeps
-- exposing `confirmed` on the wire (blob-flow reads it); it is now derived
-- from which table a row came from (blobs => true, mempool_blobs => false).
--
-- Replacement indexes are created BEFORE the confirmed-keyed ones are
-- dropped, and the whole file runs in one implicit transaction, so the hot
-- /blob/latest and chart reads never see a state without a usable index.
-- Sizing: plain CREATE INDEX takes a SHARE lock on blobs (reads proceed,
-- indexer writes stall and catch up afterwards). The two covering builds and
-- one plain build each scan blobs once; on the current production table
-- (weeks of data since the 000001 schema reset, single-digit millions of rows
-- at ~6 blobs/block) that is seconds per index, well inside the fast-DDL
-- budget. CREATE INDEX CONCURRENTLY is not an option inside migrations (see
-- README.md rule 1). If blobs ever grows to where these builds take minutes,
-- swap indexes out-of-band before running this style of migration.
--
-- DDL only, idempotent, no explicit transaction control — see README.md.

-- ---------------------------------------------------------------------------
-- 0. Safety net: purge any stray unconfirmed row before TRUE is baked in.
--    000002 already deleted all pending rows, so this is expected to hit
--    nothing. Guarded so the file stays idempotent after the column is gone.
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = current_schema()
            AND table_name = 'blobs' AND column_name = 'confirmed'
    ) THEN
        EXECUTE 'DELETE FROM blobs WHERE confirmed = false';
    END IF;
END $$;

-- ---------------------------------------------------------------------------
-- 1. Replacement indexes (created before the confirmed-keyed ones are
--    dropped). idx_blobs_chain_confirmed_timestamp needs no replacement:
--    baseline idx_blobs_chain_timestamp already covers (chain_id,
--    timestamp DESC). idx_blobs_confirmed indexes a constant and is dropped
--    without replacement.
-- ---------------------------------------------------------------------------

-- Replaces idx_blobs_chain_confirmed_block: /blob/latest keyset ordering.
CREATE INDEX IF NOT EXISTS idx_blobs_chain_block
    ON blobs(chain_id, block_number DESC);

-- Replaces idx_blobs_chain_confirmed_timestamp_cover: rolling-stats windows.
CREATE INDEX IF NOT EXISTS idx_blobs_chain_timestamp_cover
    ON blobs(chain_id, timestamp DESC)
    INCLUDE (from_address, total_cost_wei, base_fee_per_blob_gas, blob_gas_used);

-- Replaces idx_blobs_chain_confirmed_timestamp_chart_cover: raw chart scans.
CREATE INDEX IF NOT EXISTS idx_blobs_chain_timestamp_chart_cover
    ON blobs(chain_id, timestamp DESC)
    INCLUDE (from_address, user_attribution, total_cost_wei, base_fee_per_blob_gas, blob_gas_used, blob_size_bytes);

-- ---------------------------------------------------------------------------
-- 2. Trigger functions: drop the confirmed predicates. plpgsql bodies are
--    late-bound, so these must be replaced in the same transaction that drops
--    the column. Bodies are otherwise identical to 000001.
-- ---------------------------------------------------------------------------

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
            GROUP BY chain_id
            UNION ALL
            SELECT
                chain_id,
                COUNT(*)::bigint AS count_delta,
                COALESCE(SUM(base_fee_per_blob_gas::numeric), 0) AS sum_base_fee_delta,
                COALESCE(SUM(tip_per_blob_gas::numeric), 0) AS sum_tip_delta,
                COALESCE(SUM(total_cost_wei::numeric), 0) AS sum_total_cost_delta
            FROM new_rows
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

-- ---------------------------------------------------------------------------
-- 3. Drop the confirmed-keyed indexes, then the column. The explicit drops
--    are redundant with the column drop's cascade but keep intent visible.
-- ---------------------------------------------------------------------------

DROP INDEX IF EXISTS idx_blobs_confirmed;
DROP INDEX IF EXISTS idx_blobs_chain_confirmed_block;
DROP INDEX IF EXISTS idx_blobs_chain_confirmed_timestamp;
DROP INDEX IF EXISTS idx_blobs_chain_confirmed_timestamp_cover;
DROP INDEX IF EXISTS idx_blobs_chain_confirmed_timestamp_chart_cover;

ALTER TABLE blobs DROP COLUMN IF EXISTS confirmed;
