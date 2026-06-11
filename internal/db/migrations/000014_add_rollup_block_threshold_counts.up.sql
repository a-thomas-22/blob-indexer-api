-- block_metrics_rollups buckets did not record how many blocks ran above the
-- blob gas target or at the blob gas max, so rollup-served rolling windows
-- (wider than the 24h raw cutoff) reported zero for blocks_above_target and
-- blocks_at_max. Add per-bucket counters, teach the refresh function to
-- maintain them, and backfill existing buckets from raw block_metrics.
--
-- The effective target/max semantics mirror queryRollingStatsWindows in
-- internal/api/queries.go: prefer the per-block gas columns, fall back to
-- blob params * 131072 (params.BlobTxBlobGasPerBlob). Blocks with neither
-- stay unclassified and count toward neither bucket counter.

ALTER TABLE block_metrics_rollups
    ADD COLUMN blocks_above_target BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN blocks_at_max BIGINT NOT NULL DEFAULT 0;

-- Exact recompute of one block-metrics bucket. Bounded by the bucket span
-- (at most one day of block_metrics rows) and served by
-- idx_block_metrics_network_timestamp_cover. Same as migration 13 plus the
-- per-bucket threshold counters.
CREATE OR REPLACE FUNCTION block_metrics_rollups_refresh(
    p_network_id INTEGER,
    p_bucket_seconds INTEGER,
    p_bucket_start TIMESTAMP
)
RETURNS void AS $$
DECLARE
    agg RECORD;
BEGIN
    SELECT
        COUNT(*)::bigint AS block_count,
        COALESCE(MIN(block_number), 0) AS start_block,
        COALESCE(MAX(block_number), 0) AS end_block,
        COALESCE(SUM(blob_count), 0)::bigint AS sum_blob_count,
        COALESCE(SUM(blob_gas_used::numeric), 0) AS sum_blob_gas_used,
        COALESCE(SUM(blob_gas_target::numeric), 0) AS sum_blob_gas_target,
        COALESCE(SUM(blob_base_fee::numeric), 0) AS sum_blob_base_fee,
        COALESCE(SUM(utilization_ratio::numeric), 0) AS sum_utilization,
        COALESCE(percentile_disc(0.5) WITHIN GROUP (ORDER BY blob_base_fee::numeric), 0) AS median_blob_base_fee,
        COALESCE(percentile_disc(0.95) WITHIN GROUP (ORDER BY blob_base_fee::numeric), 0) AS p95_blob_base_fee,
        COUNT(*) FILTER (
            WHERE target_blob_gas > 0 AND effective_blob_gas_used > target_blob_gas
        )::bigint AS blocks_above_target,
        COUNT(*) FILTER (
            WHERE max_blob_gas > 0 AND effective_blob_gas_used >= max_blob_gas
        )::bigint AS blocks_at_max
    INTO agg
    FROM (
        SELECT
            block_number,
            blob_count,
            blob_gas_used,
            blob_gas_target,
            blob_base_fee,
            utilization_ratio,
            GREATEST(blob_gas_used, 0)::bigint AS effective_blob_gas_used,
            CASE
                WHEN blob_gas_target > 0 THEN blob_gas_target
                WHEN blob_params_target > 0 THEN blob_params_target::bigint * 131072
                ELSE 0
            END::bigint AS target_blob_gas,
            CASE
                WHEN blob_gas_limit > 0 THEN blob_gas_limit
                WHEN blob_params_max > 0 THEN blob_params_max::bigint * 131072
                ELSE 0
            END::bigint AS max_blob_gas
        FROM block_metrics
        WHERE network_id = p_network_id
            AND block_timestamp >= p_bucket_start
            AND block_timestamp < p_bucket_start + (p_bucket_seconds * INTERVAL '1 second')
    ) bm;

    IF agg.block_count = 0 THEN
        DELETE FROM block_metrics_rollups
        WHERE network_id = p_network_id
            AND bucket_seconds = p_bucket_seconds
            AND bucket_start = p_bucket_start;
        RETURN;
    END IF;

    INSERT INTO block_metrics_rollups (
        network_id,
        bucket_seconds,
        bucket_start,
        block_count,
        start_block,
        end_block,
        sum_blob_count,
        sum_blob_gas_used,
        sum_blob_gas_target,
        sum_blob_base_fee,
        sum_utilization,
        median_blob_base_fee,
        p95_blob_base_fee,
        blocks_above_target,
        blocks_at_max,
        updated_at
    )
    VALUES (
        p_network_id,
        p_bucket_seconds,
        p_bucket_start,
        agg.block_count,
        agg.start_block,
        agg.end_block,
        agg.sum_blob_count,
        agg.sum_blob_gas_used,
        agg.sum_blob_gas_target,
        agg.sum_blob_base_fee,
        agg.sum_utilization,
        agg.median_blob_base_fee,
        agg.p95_blob_base_fee,
        agg.blocks_above_target,
        agg.blocks_at_max,
        NOW()
    )
    ON CONFLICT (network_id, bucket_seconds, bucket_start) DO UPDATE SET
        block_count = EXCLUDED.block_count,
        start_block = EXCLUDED.start_block,
        end_block = EXCLUDED.end_block,
        sum_blob_count = EXCLUDED.sum_blob_count,
        sum_blob_gas_used = EXCLUDED.sum_blob_gas_used,
        sum_blob_gas_target = EXCLUDED.sum_blob_gas_target,
        sum_blob_base_fee = EXCLUDED.sum_blob_base_fee,
        sum_utilization = EXCLUDED.sum_utilization,
        median_blob_base_fee = EXCLUDED.median_blob_base_fee,
        p95_blob_base_fee = EXCLUDED.p95_blob_base_fee,
        blocks_above_target = EXCLUDED.blocks_above_target,
        blocks_at_max = EXCLUDED.blocks_at_max,
        updated_at = NOW();
END;
$$ LANGUAGE plpgsql;

-- One-time backfill of the new counters for existing buckets: one grouped
-- UPDATE per granularity, each a single sequential scan of block_metrics plus
-- a hash aggregate and a join against that granularity's rollup rows. No
-- per-row sorts (unlike the migration-13 percentile backfill), so each pass is
-- scan-bound: measured ~0.4s per granularity per 1M rows warm, so at
-- production scale (~11.7M block_metrics rows) expect ~5s warm and tens of
-- seconds cold per granularity — roughly a minute total.
UPDATE block_metrics_rollups r
SET blocks_above_target = agg.blocks_above_target,
    blocks_at_max = agg.blocks_at_max,
    updated_at = NOW()
FROM (
    SELECT
        network_id,
        chart_rollup_bucket_start(block_timestamp, 3600) AS bucket_start,
        COUNT(*) FILTER (
            WHERE target_blob_gas > 0 AND effective_blob_gas_used > target_blob_gas
        )::bigint AS blocks_above_target,
        COUNT(*) FILTER (
            WHERE max_blob_gas > 0 AND effective_blob_gas_used >= max_blob_gas
        )::bigint AS blocks_at_max
    FROM (
        SELECT
            network_id,
            block_timestamp,
            GREATEST(blob_gas_used, 0)::bigint AS effective_blob_gas_used,
            CASE
                WHEN blob_gas_target > 0 THEN blob_gas_target
                WHEN blob_params_target > 0 THEN blob_params_target::bigint * 131072
                ELSE 0
            END::bigint AS target_blob_gas,
            CASE
                WHEN blob_gas_limit > 0 THEN blob_gas_limit
                WHEN blob_params_max > 0 THEN blob_params_max::bigint * 131072
                ELSE 0
            END::bigint AS max_blob_gas
        FROM block_metrics
    ) bm
    GROUP BY network_id, chart_rollup_bucket_start(block_timestamp, 3600)
) agg
WHERE r.network_id = agg.network_id
    AND r.bucket_seconds = 3600
    AND r.bucket_start = agg.bucket_start;

UPDATE block_metrics_rollups r
SET blocks_above_target = agg.blocks_above_target,
    blocks_at_max = agg.blocks_at_max,
    updated_at = NOW()
FROM (
    SELECT
        network_id,
        chart_rollup_bucket_start(block_timestamp, 21600) AS bucket_start,
        COUNT(*) FILTER (
            WHERE target_blob_gas > 0 AND effective_blob_gas_used > target_blob_gas
        )::bigint AS blocks_above_target,
        COUNT(*) FILTER (
            WHERE max_blob_gas > 0 AND effective_blob_gas_used >= max_blob_gas
        )::bigint AS blocks_at_max
    FROM (
        SELECT
            network_id,
            block_timestamp,
            GREATEST(blob_gas_used, 0)::bigint AS effective_blob_gas_used,
            CASE
                WHEN blob_gas_target > 0 THEN blob_gas_target
                WHEN blob_params_target > 0 THEN blob_params_target::bigint * 131072
                ELSE 0
            END::bigint AS target_blob_gas,
            CASE
                WHEN blob_gas_limit > 0 THEN blob_gas_limit
                WHEN blob_params_max > 0 THEN blob_params_max::bigint * 131072
                ELSE 0
            END::bigint AS max_blob_gas
        FROM block_metrics
    ) bm
    GROUP BY network_id, chart_rollup_bucket_start(block_timestamp, 21600)
) agg
WHERE r.network_id = agg.network_id
    AND r.bucket_seconds = 21600
    AND r.bucket_start = agg.bucket_start;

UPDATE block_metrics_rollups r
SET blocks_above_target = agg.blocks_above_target,
    blocks_at_max = agg.blocks_at_max,
    updated_at = NOW()
FROM (
    SELECT
        network_id,
        chart_rollup_bucket_start(block_timestamp, 86400) AS bucket_start,
        COUNT(*) FILTER (
            WHERE target_blob_gas > 0 AND effective_blob_gas_used > target_blob_gas
        )::bigint AS blocks_above_target,
        COUNT(*) FILTER (
            WHERE max_blob_gas > 0 AND effective_blob_gas_used >= max_blob_gas
        )::bigint AS blocks_at_max
    FROM (
        SELECT
            network_id,
            block_timestamp,
            GREATEST(blob_gas_used, 0)::bigint AS effective_blob_gas_used,
            CASE
                WHEN blob_gas_target > 0 THEN blob_gas_target
                WHEN blob_params_target > 0 THEN blob_params_target::bigint * 131072
                ELSE 0
            END::bigint AS target_blob_gas,
            CASE
                WHEN blob_gas_limit > 0 THEN blob_gas_limit
                WHEN blob_params_max > 0 THEN blob_params_max::bigint * 131072
                ELSE 0
            END::bigint AS max_blob_gas
        FROM block_metrics
    ) bm
    GROUP BY network_id, chart_rollup_bucket_start(block_timestamp, 86400)
) agg
WHERE r.network_id = agg.network_id
    AND r.bucket_seconds = 86400
    AND r.bucket_start = agg.bucket_start;
