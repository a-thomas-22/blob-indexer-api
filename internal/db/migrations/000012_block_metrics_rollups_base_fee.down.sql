-- Reverse 000012_block_metrics_rollups_base_fee.up.sql: restore the 000001
-- refresh body, then drop the base fee aggregate columns.
CREATE OR REPLACE FUNCTION block_metrics_rollups_refresh(
    p_chain_id INTEGER,
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
        WHERE chain_id = p_chain_id
            AND block_timestamp >= p_bucket_start
            AND block_timestamp < p_bucket_start + (p_bucket_seconds * INTERVAL '1 second')
    ) bm;

    IF agg.block_count = 0 THEN
        DELETE FROM block_metrics_rollups
        WHERE chain_id = p_chain_id
            AND bucket_seconds = p_bucket_seconds
            AND bucket_start = p_bucket_start;
        RETURN;
    END IF;

    INSERT INTO block_metrics_rollups (
        chain_id, bucket_seconds, bucket_start, block_count, start_block, end_block,
        sum_blob_count, sum_blob_gas_used, sum_blob_gas_target, sum_blob_base_fee,
        sum_utilization, median_blob_base_fee, p95_blob_base_fee,
        blocks_above_target, blocks_at_max, updated_at
    )
    VALUES (
        p_chain_id, p_bucket_seconds, p_bucket_start, agg.block_count, agg.start_block, agg.end_block,
        agg.sum_blob_count, agg.sum_blob_gas_used, agg.sum_blob_gas_target, agg.sum_blob_base_fee,
        agg.sum_utilization, agg.median_blob_base_fee, agg.p95_blob_base_fee,
        agg.blocks_above_target, agg.blocks_at_max, NOW()
    )
    ON CONFLICT (chain_id, bucket_seconds, bucket_start) DO UPDATE SET
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

ALTER TABLE block_metrics_rollups
    DROP COLUMN IF EXISTS sum_base_fee_wei;
ALTER TABLE block_metrics_rollups
    DROP COLUMN IF EXISTS base_fee_block_count;
