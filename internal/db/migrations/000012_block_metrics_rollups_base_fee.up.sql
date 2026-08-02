-- Add execution-layer base fee aggregates to block_metrics_rollups.
--
-- The cost-comparison chart prices the calldata equivalent of blob data at
-- each bucket's average execution-layer base fee (block_metrics.base_fee_wei,
-- added in 000010). The rollup-served chart paths need that average without
-- scanning raw block_metrics, so rollup rows carry the bucket's base fee sum
-- plus the count of blocks with a recorded (non-zero) fee. The API derives the
-- average as sum / count, which regroups exactly when a display bucket
-- combines several source buckets.
--
-- Going-forward only, matching 000010: block_metrics rows indexed before
-- 000010 hold base_fee_wei = 0, so there is no historical fee data to roll up
-- and no backfill runs here (heavy backfills stay out of schema migrations;
-- see README.md). base_fee_block_count counts only blocks with a non-zero
-- fee, so zero-fee legacy rows never dilute a bucket's average. Zero doubles
-- as the "not recorded" sentinel on purpose: 000010 defined the column as
-- NOT NULL DEFAULT 0 with exactly that meaning, and a genuine zero cannot
-- occur in indexed data because post-London EIP-1559 integer fee math never
-- decays the base fee to zero and every blob-carrying block is post-Cancun,
-- hence post-London. Rollup rows
-- written before this migration keep the default 0 / 0, which the API treats
-- as "no fee recorded" and prices with the blob-fee proxy, exactly the
-- behaviour before this migration. Fine (60s) buckets inside the 48h
-- retention window are recomputed by the indexer's startup backfill; coarse
-- buckets pick up the new aggregates whenever any row in the bucket changes
-- (reorg, reindex, or live writes).
--
-- Constant DEFAULTs keep the column adds metadata-only (no table rewrite) on
-- PostgreSQL 11+. DDL only, idempotent, no explicit transaction control; see
-- README.md.
ALTER TABLE block_metrics_rollups
    ADD COLUMN IF NOT EXISTS sum_base_fee_wei NUMERIC NOT NULL DEFAULT 0;
ALTER TABLE block_metrics_rollups
    ADD COLUMN IF NOT EXISTS base_fee_block_count BIGINT NOT NULL DEFAULT 0;

-- 000001's block_metrics_rollups_refresh extended with the two base fee
-- aggregates. Everything else matches the previous body.
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
        )::bigint AS blocks_at_max,
        COALESCE(SUM(base_fee_wei::numeric), 0) AS sum_base_fee_wei,
        COUNT(*) FILTER (WHERE base_fee_wei::numeric > 0)::bigint AS base_fee_block_count
    INTO agg
    FROM (
        SELECT
            block_number,
            blob_count,
            blob_gas_used,
            blob_gas_target,
            blob_base_fee,
            utilization_ratio,
            base_fee_wei,
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
        blocks_above_target, blocks_at_max, sum_base_fee_wei, base_fee_block_count, updated_at
    )
    VALUES (
        p_chain_id, p_bucket_seconds, p_bucket_start, agg.block_count, agg.start_block, agg.end_block,
        agg.sum_blob_count, agg.sum_blob_gas_used, agg.sum_blob_gas_target, agg.sum_blob_base_fee,
        agg.sum_utilization, agg.median_blob_base_fee, agg.p95_blob_base_fee,
        agg.blocks_above_target, agg.blocks_at_max, agg.sum_base_fee_wei, agg.base_fee_block_count, NOW()
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
        sum_base_fee_wei = EXCLUDED.sum_base_fee_wei,
        base_fee_block_count = EXCLUDED.base_fee_block_count,
        updated_at = NOW();
END;
$$ LANGUAGE plpgsql;
