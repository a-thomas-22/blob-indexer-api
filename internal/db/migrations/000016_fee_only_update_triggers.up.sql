-- Skip the blobs UPDATE aggregate triggers for fee-only updates.
--
-- The three statement-level UPDATE triggers on blobs maintain
-- network_blob_stats, blob_user_stats, and blob_chart_rollups, none of which
-- reads the execution-layer fee columns added in 000015. The priority fee
-- backfill updates only those three columns, in batches spanning many
-- blocks, so an unguarded trigger would rescan every touched sender's
-- hourly, six-hour, and daily rollup buckets per statement for no change in
-- any aggregate. Postgres does not allow a column list on a trigger that
-- uses transition tables, so the guard lives in each function: when no row
-- changed an aggregate-relevant column, return before doing any work. The
-- indexer's ON CONFLICT upserts set every listed column and still run the
-- full bodies, which are otherwise the 000003, 000004, and 000005 versions.
--
-- CREATE OR REPLACE FUNCTION only, idempotent, no explicit transaction
-- control; see README.md.

CREATE OR REPLACE FUNCTION network_blob_stats_blobs_update_trigger()
RETURNS trigger AS $$
DECLARE
    delta RECORD;
BEGIN
    -- Fee-only updates (the priority fee backfill writes just
    -- max_priority_fee_per_gas, max_fee_per_gas, and priority_fee_per_gas)
    -- change nothing this aggregate reads; skip the recompute entirely.
    IF NOT EXISTS (
        SELECT 1
        FROM new_rows n
        JOIN old_rows o ON o.id = n.id
        WHERE (n.chain_id, n.block_number, n.blob_index, n.tx_hash, n.from_address, n.user_attribution, n.blob_size_bytes, n.base_fee_per_blob_gas, n.tip_per_blob_gas, n.total_cost_wei, n.timestamp, n.max_fee_per_blob_gas, n.blob_gas_used, n.versioned_hash, n.slot)
            IS DISTINCT FROM
            (o.chain_id, o.block_number, o.blob_index, o.tx_hash, o.from_address, o.user_attribution, o.blob_size_bytes, o.base_fee_per_blob_gas, o.tip_per_blob_gas, o.total_cost_wei, o.timestamp, o.max_fee_per_blob_gas, o.blob_gas_used, o.versioned_hash, o.slot)
    ) THEN
        RETURN NULL;
    END IF;
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

CREATE OR REPLACE FUNCTION blob_user_stats_blobs_update_statement_trigger()
RETURNS trigger AS $$
DECLARE
    delta RECORD;
BEGIN
    -- Fee-only updates (the priority fee backfill writes just
    -- max_priority_fee_per_gas, max_fee_per_gas, and priority_fee_per_gas)
    -- change nothing this aggregate reads; skip the recompute entirely.
    IF NOT EXISTS (
        SELECT 1
        FROM new_blobs n
        JOIN old_blobs o ON o.id = n.id
        WHERE (n.chain_id, n.block_number, n.blob_index, n.tx_hash, n.from_address, n.user_attribution, n.blob_size_bytes, n.base_fee_per_blob_gas, n.tip_per_blob_gas, n.total_cost_wei, n.timestamp, n.max_fee_per_blob_gas, n.blob_gas_used, n.versioned_hash, n.slot)
            IS DISTINCT FROM
            (o.chain_id, o.block_number, o.blob_index, o.tx_hash, o.from_address, o.user_attribution, o.blob_size_bytes, o.base_fee_per_blob_gas, o.tip_per_blob_gas, o.total_cost_wei, o.timestamp, o.max_fee_per_blob_gas, o.blob_gas_used, o.versioned_hash, o.slot)
    ) THEN
        RETURN NULL;
    END IF;
    FOR delta IN
        SELECT
            chain_id,
            from_address,
            COALESCE(n.blob_count, 0) - COALESCE(o.blob_count, 0) AS blob_count,
            COALESCE(n.total_cost_wei, 0) - COALESCE(o.total_cost_wei, 0) AS total_cost_wei,
            n.user_attribution,
            n.last_timestamp,
            (COALESCE(o.user_attribution, '') <> '' AND COALESCE(n.user_attribution, '') = '')
                AS attribution_cleared
        FROM (
            SELECT
                chain_id,
                from_address,
                COUNT(*)::bigint AS blob_count,
                COALESCE(SUM(total_cost_wei::numeric), 0) AS total_cost_wei,
                COALESCE(NULLIF(MAX(BTRIM(user_attribution)), ''), '') AS user_attribution,
                MAX(timestamp) AS last_timestamp
            FROM new_blobs
            GROUP BY chain_id, from_address
        ) n
        FULL OUTER JOIN (
            SELECT
                chain_id,
                from_address,
                COUNT(*)::bigint AS blob_count,
                COALESCE(SUM(total_cost_wei::numeric), 0) AS total_cost_wei,
                COALESCE(NULLIF(MAX(BTRIM(user_attribution)), ''), '') AS user_attribution
            FROM old_blobs
            GROUP BY chain_id, from_address
        ) o USING (chain_id, from_address)
    LOOP
        IF delta.attribution_cleared THEN
            PERFORM blob_user_stats_refresh(delta.chain_id, delta.from_address);
        ELSE
            PERFORM blob_user_stats_apply_signed_delta(
                delta.chain_id, delta.from_address, delta.user_attribution,
                delta.blob_count, delta.total_cost_wei, delta.last_timestamp);
        END IF;
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION blob_chart_rollups_update_statement_trigger()
RETURNS trigger AS $$
DECLARE
    affected RECORD;
BEGIN
    -- Fee-only updates (the priority fee backfill writes just
    -- max_priority_fee_per_gas, max_fee_per_gas, and priority_fee_per_gas)
    -- change nothing this aggregate reads; skip the recompute entirely.
    IF NOT EXISTS (
        SELECT 1
        FROM new_blobs n
        JOIN old_blobs o ON o.id = n.id
        WHERE (n.chain_id, n.block_number, n.blob_index, n.tx_hash, n.from_address, n.user_attribution, n.blob_size_bytes, n.base_fee_per_blob_gas, n.tip_per_blob_gas, n.total_cost_wei, n.timestamp, n.max_fee_per_blob_gas, n.blob_gas_used, n.versioned_hash, n.slot)
            IS DISTINCT FROM
            (o.chain_id, o.block_number, o.blob_index, o.tx_hash, o.from_address, o.user_attribution, o.blob_size_bytes, o.base_fee_per_blob_gas, o.tip_per_blob_gas, o.total_cost_wei, o.timestamp, o.max_fee_per_blob_gas, o.blob_gas_used, o.versioned_hash, o.slot)
    ) THEN
        RETURN NULL;
    END IF;
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
        CROSS JOIN LATERAL chart_rollup_bucket_seconds_for(r.timestamp) g
    LOOP
        PERFORM blob_chart_rollups_refresh(
            affected.chain_id, affected.bucket_seconds, affected.bucket_start, affected.from_address);
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
