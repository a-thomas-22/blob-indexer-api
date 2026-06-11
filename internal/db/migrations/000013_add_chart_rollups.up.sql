-- Pre-aggregated chart rollups so /charts/* and wide-window reads stay
-- O(buckets) instead of O(raw rows):
--   * block_metrics_rollups: per (network, bucket) block-side aggregates with
--     exact per-bucket median/p95 blob base fees.
--   * blob_chart_rollups: per (network, bucket, sender) confirmed-blob
--     aggregates serving cost totals, unique-sender counts, attribution
--     series, and calldata-equivalent sums.
-- Buckets are maintained at the hour-and-coarser granularities the chart API
-- serves (3600s, 21600s, 86400s); sub-hour buckets stay on raw scans, which
-- the covering indexes from migration 12 bound to at most 24 hours of rows.

CREATE OR REPLACE FUNCTION chart_rollup_bucket_start(p_timestamp TIMESTAMP, p_bucket_seconds INTEGER)
RETURNS TIMESTAMP AS $$
    SELECT TIMESTAMP 'epoch' + (
        FLOOR(EXTRACT(EPOCH FROM p_timestamp) / p_bucket_seconds)::bigint
        * (p_bucket_seconds * INTERVAL '1 second')
    );
$$ LANGUAGE sql IMMUTABLE;

CREATE OR REPLACE FUNCTION chart_rollup_bucket_seconds()
RETURNS TABLE (bucket_seconds INTEGER) AS $$
    VALUES (3600), (21600), (86400);
$$ LANGUAGE sql IMMUTABLE;

CREATE TABLE IF NOT EXISTS block_metrics_rollups (
    network_id INTEGER NOT NULL,
    bucket_seconds INTEGER NOT NULL,
    bucket_start TIMESTAMP NOT NULL,
    block_count BIGINT NOT NULL DEFAULT 0 CHECK (block_count >= 0),
    start_block BIGINT NOT NULL DEFAULT 0,
    end_block BIGINT NOT NULL DEFAULT 0,
    sum_blob_count BIGINT NOT NULL DEFAULT 0,
    sum_blob_gas_used NUMERIC NOT NULL DEFAULT 0,
    sum_blob_gas_target NUMERIC NOT NULL DEFAULT 0,
    sum_blob_base_fee NUMERIC NOT NULL DEFAULT 0,
    sum_utilization NUMERIC NOT NULL DEFAULT 0,
    median_blob_base_fee NUMERIC NOT NULL DEFAULT 0,
    p95_blob_base_fee NUMERIC NOT NULL DEFAULT 0,
    updated_at TIMESTAMP NOT NULL DEFAULT NOW(),
    PRIMARY KEY (network_id, bucket_seconds, bucket_start),
    CONSTRAINT fk_block_metrics_rollups_network_chain_id
        FOREIGN KEY (network_id)
        REFERENCES networks(chain_id)
        ON UPDATE CASCADE
        ON DELETE RESTRICT
);

CREATE TABLE IF NOT EXISTS blob_chart_rollups (
    network_id INTEGER NOT NULL,
    bucket_seconds INTEGER NOT NULL,
    bucket_start TIMESTAMP NOT NULL,
    from_address TEXT NOT NULL,
    user_attribution TEXT NOT NULL DEFAULT '',
    blob_count BIGINT NOT NULL DEFAULT 0 CHECK (blob_count >= 0),
    blob_bytes BIGINT NOT NULL DEFAULT 0,
    blob_gas_used BIGINT NOT NULL DEFAULT 0,
    total_cost_eth NUMERIC NOT NULL DEFAULT 0,
    sum_size_base_fee NUMERIC NOT NULL DEFAULT 0,
    updated_at TIMESTAMP NOT NULL DEFAULT NOW(),
    PRIMARY KEY (network_id, bucket_seconds, bucket_start, from_address),
    CONSTRAINT fk_blob_chart_rollups_network_chain_id
        FOREIGN KEY (network_id)
        REFERENCES networks(chain_id)
        ON UPDATE CASCADE
        ON DELETE RESTRICT
);

-- Exact recompute of one block-metrics bucket. Bounded by the bucket span
-- (at most one day of block_metrics rows) and served by
-- idx_block_metrics_network_timestamp_cover.
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
        COALESCE(percentile_disc(0.95) WITHIN GROUP (ORDER BY blob_base_fee::numeric), 0) AS p95_blob_base_fee
    INTO agg
    FROM block_metrics
    WHERE network_id = p_network_id
        AND block_timestamp >= p_bucket_start
        AND block_timestamp < p_bucket_start + (p_bucket_seconds * INTERVAL '1 second');

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
        updated_at = NOW();
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION block_metrics_rollups_insert_statement_trigger()
RETURNS trigger AS $$
DECLARE
    affected RECORD;
BEGIN
    FOR affected IN
        SELECT DISTINCT
            r.network_id,
            g.bucket_seconds,
            chart_rollup_bucket_start(r.block_timestamp, g.bucket_seconds) AS bucket_start
        FROM new_rows r
        CROSS JOIN chart_rollup_bucket_seconds() g
    LOOP
        PERFORM block_metrics_rollups_refresh(affected.network_id, affected.bucket_seconds, affected.bucket_start);
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
            r.network_id,
            g.bucket_seconds,
            chart_rollup_bucket_start(r.block_timestamp, g.bucket_seconds) AS bucket_start
        FROM old_rows r
        CROSS JOIN chart_rollup_bucket_seconds() g
    LOOP
        PERFORM block_metrics_rollups_refresh(affected.network_id, affected.bucket_seconds, affected.bucket_start);
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
            r.network_id,
            g.bucket_seconds,
            chart_rollup_bucket_start(r.block_timestamp, g.bucket_seconds) AS bucket_start
        FROM (
            SELECT network_id, block_timestamp FROM old_rows
            UNION
            SELECT network_id, block_timestamp FROM new_rows
        ) r
        CROSS JOIN chart_rollup_bucket_seconds() g
    LOOP
        PERFORM block_metrics_rollups_refresh(affected.network_id, affected.bucket_seconds, affected.bucket_start);
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_block_metrics_rollups_insert ON block_metrics;
DROP TRIGGER IF EXISTS trg_block_metrics_rollups_update ON block_metrics;
DROP TRIGGER IF EXISTS trg_block_metrics_rollups_delete ON block_metrics;

CREATE TRIGGER trg_block_metrics_rollups_insert
AFTER INSERT ON block_metrics
REFERENCING NEW TABLE AS new_rows
FOR EACH STATEMENT
EXECUTE FUNCTION block_metrics_rollups_insert_statement_trigger();

CREATE TRIGGER trg_block_metrics_rollups_update
AFTER UPDATE ON block_metrics
REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows
FOR EACH STATEMENT
EXECUTE FUNCTION block_metrics_rollups_update_statement_trigger();

CREATE TRIGGER trg_block_metrics_rollups_delete
AFTER DELETE ON block_metrics
REFERENCING OLD TABLE AS old_rows
FOR EACH STATEMENT
EXECUTE FUNCTION block_metrics_rollups_delete_statement_trigger();

-- Exact recompute of one (bucket, sender) blob rollup row. Bounded by one
-- sender's rows inside one bucket and served by
-- idx_blobs_network_from_timestamp.
CREATE OR REPLACE FUNCTION blob_chart_rollups_refresh(
    p_network_id INTEGER,
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
        COALESCE(SUM(total_cost_eth::numeric), 0) AS total_cost_eth,
        COALESCE(SUM(blob_size_bytes::numeric * base_fee_per_blob_gas::numeric), 0) AS sum_size_base_fee
    INTO agg
    FROM blobs
    WHERE network_id = p_network_id
        AND from_address = p_from_address
        AND confirmed = true
        AND timestamp >= p_bucket_start
        AND timestamp < p_bucket_start + (p_bucket_seconds * INTERVAL '1 second');

    IF agg.blob_count = 0 THEN
        DELETE FROM blob_chart_rollups
        WHERE network_id = p_network_id
            AND bucket_seconds = p_bucket_seconds
            AND bucket_start = p_bucket_start
            AND from_address = p_from_address;
        RETURN;
    END IF;

    INSERT INTO blob_chart_rollups (
        network_id,
        bucket_seconds,
        bucket_start,
        from_address,
        user_attribution,
        blob_count,
        blob_bytes,
        blob_gas_used,
        total_cost_eth,
        sum_size_base_fee,
        updated_at
    )
    VALUES (
        p_network_id,
        p_bucket_seconds,
        p_bucket_start,
        p_from_address,
        agg.user_attribution,
        agg.blob_count,
        agg.blob_bytes,
        agg.blob_gas_used,
        agg.total_cost_eth,
        agg.sum_size_base_fee,
        NOW()
    )
    ON CONFLICT (network_id, bucket_seconds, bucket_start, from_address) DO UPDATE SET
        user_attribution = EXCLUDED.user_attribution,
        blob_count = EXCLUDED.blob_count,
        blob_bytes = EXCLUDED.blob_bytes,
        blob_gas_used = EXCLUDED.blob_gas_used,
        total_cost_eth = EXCLUDED.total_cost_eth,
        sum_size_base_fee = EXCLUDED.sum_size_base_fee,
        updated_at = NOW();
END;
$$ LANGUAGE plpgsql;

-- Inserts are the hot path (one statement per indexed block), so they apply
-- pure additive deltas instead of recomputing buckets.
CREATE OR REPLACE FUNCTION blob_chart_rollups_insert_statement_trigger()
RETURNS trigger AS $$
BEGIN
    INSERT INTO blob_chart_rollups (
        network_id,
        bucket_seconds,
        bucket_start,
        from_address,
        user_attribution,
        blob_count,
        blob_bytes,
        blob_gas_used,
        total_cost_eth,
        sum_size_base_fee,
        updated_at
    )
    SELECT
        r.network_id,
        g.bucket_seconds,
        chart_rollup_bucket_start(r.timestamp, g.bucket_seconds),
        r.from_address,
        COALESCE(NULLIF(MAX(BTRIM(r.user_attribution)), ''), ''),
        COUNT(*)::bigint,
        COALESCE(SUM(r.blob_size_bytes), 0)::bigint,
        COALESCE(SUM(COALESCE(r.blob_gas_used, 0)), 0)::bigint,
        COALESCE(SUM(r.total_cost_eth::numeric), 0),
        COALESCE(SUM(r.blob_size_bytes::numeric * r.base_fee_per_blob_gas::numeric), 0),
        NOW()
    FROM new_blobs r
    CROSS JOIN chart_rollup_bucket_seconds() g
    WHERE r.confirmed = true
    GROUP BY r.network_id, g.bucket_seconds, chart_rollup_bucket_start(r.timestamp, g.bucket_seconds), r.from_address
    ON CONFLICT (network_id, bucket_seconds, bucket_start, from_address) DO UPDATE SET
        user_attribution = COALESCE(
            NULLIF(BTRIM(EXCLUDED.user_attribution), ''),
            NULLIF(BTRIM(blob_chart_rollups.user_attribution), ''),
            ''
        ),
        blob_count = blob_chart_rollups.blob_count + EXCLUDED.blob_count,
        blob_bytes = blob_chart_rollups.blob_bytes + EXCLUDED.blob_bytes,
        blob_gas_used = blob_chart_rollups.blob_gas_used + EXCLUDED.blob_gas_used,
        total_cost_eth = blob_chart_rollups.total_cost_eth + EXCLUDED.total_cost_eth,
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
            r.network_id,
            g.bucket_seconds,
            chart_rollup_bucket_start(r.timestamp, g.bucket_seconds) AS bucket_start,
            r.from_address
        FROM old_blobs r
        CROSS JOIN chart_rollup_bucket_seconds() g
        WHERE r.confirmed = true
    LOOP
        PERFORM blob_chart_rollups_refresh(
            affected.network_id,
            affected.bucket_seconds,
            affected.bucket_start,
            affected.from_address
        );
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

-- Updates cover the pending→confirmed transition (confirmed flips and the
-- timestamp may move buckets), so both the old and new keys are recomputed.
CREATE OR REPLACE FUNCTION blob_chart_rollups_update_statement_trigger()
RETURNS trigger AS $$
DECLARE
    affected RECORD;
BEGIN
    FOR affected IN
        SELECT DISTINCT
            r.network_id,
            g.bucket_seconds,
            chart_rollup_bucket_start(r.timestamp, g.bucket_seconds) AS bucket_start,
            r.from_address
        FROM (
            SELECT network_id, timestamp, from_address FROM old_blobs WHERE confirmed = true
            UNION
            SELECT network_id, timestamp, from_address FROM new_blobs WHERE confirmed = true
        ) r
        CROSS JOIN chart_rollup_bucket_seconds() g
    LOOP
        PERFORM blob_chart_rollups_refresh(
            affected.network_id,
            affected.bucket_seconds,
            affected.bucket_start,
            affected.from_address
        );
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_blob_chart_rollups_insert ON blobs;
DROP TRIGGER IF EXISTS trg_blob_chart_rollups_update ON blobs;
DROP TRIGGER IF EXISTS trg_blob_chart_rollups_delete ON blobs;

CREATE TRIGGER trg_blob_chart_rollups_insert
AFTER INSERT ON blobs
REFERENCING NEW TABLE AS new_blobs
FOR EACH STATEMENT
EXECUTE FUNCTION blob_chart_rollups_insert_statement_trigger();

CREATE TRIGGER trg_blob_chart_rollups_update
AFTER UPDATE ON blobs
REFERENCING OLD TABLE AS old_blobs NEW TABLE AS new_blobs
FOR EACH STATEMENT
EXECUTE FUNCTION blob_chart_rollups_update_statement_trigger();

CREATE TRIGGER trg_blob_chart_rollups_delete
AFTER DELETE ON blobs
REFERENCING OLD TABLE AS old_blobs
FOR EACH STATEMENT
EXECUTE FUNCTION blob_chart_rollups_delete_statement_trigger();

-- One-time backfill from existing raw data. May take minutes on large
-- databases; runs inside the migration so the API never serves partial
-- rollups.
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
    p95_blob_base_fee
)
SELECT
    network_id,
    g.bucket_seconds,
    chart_rollup_bucket_start(block_timestamp, g.bucket_seconds),
    COUNT(*)::bigint,
    COALESCE(MIN(block_number), 0),
    COALESCE(MAX(block_number), 0),
    COALESCE(SUM(blob_count), 0)::bigint,
    COALESCE(SUM(blob_gas_used::numeric), 0),
    COALESCE(SUM(blob_gas_target::numeric), 0),
    COALESCE(SUM(blob_base_fee::numeric), 0),
    COALESCE(SUM(utilization_ratio::numeric), 0),
    COALESCE(percentile_disc(0.5) WITHIN GROUP (ORDER BY blob_base_fee::numeric), 0),
    COALESCE(percentile_disc(0.95) WITHIN GROUP (ORDER BY blob_base_fee::numeric), 0)
FROM block_metrics
CROSS JOIN chart_rollup_bucket_seconds() g
GROUP BY network_id, g.bucket_seconds, chart_rollup_bucket_start(block_timestamp, g.bucket_seconds)
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
    updated_at = NOW();

INSERT INTO blob_chart_rollups (
    network_id,
    bucket_seconds,
    bucket_start,
    from_address,
    user_attribution,
    blob_count,
    blob_bytes,
    blob_gas_used,
    total_cost_eth,
    sum_size_base_fee
)
SELECT
    network_id,
    g.bucket_seconds,
    chart_rollup_bucket_start(timestamp, g.bucket_seconds),
    from_address,
    COALESCE(NULLIF(MAX(BTRIM(user_attribution)), ''), ''),
    COUNT(*)::bigint,
    COALESCE(SUM(blob_size_bytes), 0)::bigint,
    COALESCE(SUM(COALESCE(blob_gas_used, 0)), 0)::bigint,
    COALESCE(SUM(total_cost_eth::numeric), 0),
    COALESCE(SUM(blob_size_bytes::numeric * base_fee_per_blob_gas::numeric), 0)
FROM blobs
CROSS JOIN chart_rollup_bucket_seconds() g
WHERE confirmed = true
GROUP BY network_id, g.bucket_seconds, chart_rollup_bucket_start(timestamp, g.bucket_seconds), from_address
ON CONFLICT (network_id, bucket_seconds, bucket_start, from_address) DO UPDATE SET
    user_attribution = EXCLUDED.user_attribution,
    blob_count = EXCLUDED.blob_count,
    blob_bytes = EXCLUDED.blob_bytes,
    blob_gas_used = EXCLUDED.blob_gas_used,
    total_cost_eth = EXCLUDED.total_cost_eth,
    sum_size_base_fee = EXCLUDED.sum_size_base_fee,
    updated_at = NOW();
