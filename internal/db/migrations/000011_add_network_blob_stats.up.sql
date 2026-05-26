CREATE TABLE IF NOT EXISTS network_blob_stats (
    network_id INTEGER PRIMARY KEY,
    total_confirmed_blobs BIGINT NOT NULL DEFAULT 0 CHECK (total_confirmed_blobs >= 0),
    sum_base_fee_per_blob_gas NUMERIC NOT NULL DEFAULT 0 CHECK (sum_base_fee_per_blob_gas >= 0),
    sum_tip_per_blob_gas NUMERIC NOT NULL DEFAULT 0 CHECK (sum_tip_per_blob_gas >= 0),
    sum_total_cost NUMERIC NOT NULL DEFAULT 0 CHECK (sum_total_cost >= 0),
    last_indexed_block BIGINT NOT NULL DEFAULT 0 CHECK (last_indexed_block >= 0),
    last_indexed_time TIMESTAMP NOT NULL DEFAULT '1970-01-01'::timestamp,
    updated_at TIMESTAMP NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_network_blob_stats_network_chain_id
        FOREIGN KEY (network_id)
        REFERENCES networks(chain_id)
        ON UPDATE CASCADE
        ON DELETE RESTRICT
);

WITH confirmed AS (
    SELECT
        network_id,
        COUNT(*)::bigint AS total_confirmed_blobs,
        COALESCE(SUM(base_fee_per_blob_gas::numeric), 0) AS sum_base_fee_per_blob_gas,
        COALESCE(SUM(tip_per_blob_gas::numeric), 0) AS sum_tip_per_blob_gas,
        COALESCE(SUM(total_cost_eth::numeric), 0) AS sum_total_cost,
        MAX(timestamp) AS last_blob_time
    FROM blobs
    WHERE confirmed = true
    GROUP BY network_id
),
latest_block AS (
    SELECT DISTINCT ON (network_id)
        network_id,
        block_number AS last_indexed_block,
        block_timestamp AS last_indexed_time
    FROM block_metrics
    ORDER BY network_id, block_number DESC
)
INSERT INTO network_blob_stats (
    network_id,
    total_confirmed_blobs,
    sum_base_fee_per_blob_gas,
    sum_tip_per_blob_gas,
    sum_total_cost,
    last_indexed_block,
    last_indexed_time
)
SELECT
    n.chain_id,
    COALESCE(c.total_confirmed_blobs, 0),
    COALESCE(c.sum_base_fee_per_blob_gas, 0),
    COALESCE(c.sum_tip_per_blob_gas, 0),
    COALESCE(c.sum_total_cost, 0),
    COALESCE(lb.last_indexed_block, 0),
    COALESCE(lb.last_indexed_time, c.last_blob_time, '1970-01-01'::timestamp)
FROM networks n
LEFT JOIN confirmed c ON c.network_id = n.chain_id
LEFT JOIN latest_block lb ON lb.network_id = n.chain_id
ON CONFLICT (network_id) DO UPDATE SET
    total_confirmed_blobs = EXCLUDED.total_confirmed_blobs,
    sum_base_fee_per_blob_gas = EXCLUDED.sum_base_fee_per_blob_gas,
    sum_tip_per_blob_gas = EXCLUDED.sum_tip_per_blob_gas,
    sum_total_cost = EXCLUDED.sum_total_cost,
    last_indexed_block = EXCLUDED.last_indexed_block,
    last_indexed_time = EXCLUDED.last_indexed_time,
    updated_at = NOW();

CREATE OR REPLACE FUNCTION network_blob_stats_apply_delta(
    p_network_id INTEGER,
    p_count_delta BIGINT,
    p_sum_base_fee_delta NUMERIC,
    p_sum_tip_delta NUMERIC,
    p_sum_total_cost_delta NUMERIC
) RETURNS void AS $$
BEGIN
    INSERT INTO network_blob_stats (network_id)
    VALUES (p_network_id)
    ON CONFLICT (network_id) DO NOTHING;

    UPDATE network_blob_stats
    SET
        total_confirmed_blobs = GREATEST(total_confirmed_blobs + p_count_delta, 0::bigint),
        sum_base_fee_per_blob_gas = GREATEST(sum_base_fee_per_blob_gas + p_sum_base_fee_delta, 0::numeric),
        sum_tip_per_blob_gas = GREATEST(sum_tip_per_blob_gas + p_sum_tip_delta, 0::numeric),
        sum_total_cost = GREATEST(sum_total_cost + p_sum_total_cost_delta, 0::numeric),
        updated_at = NOW()
    WHERE network_id = p_network_id;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION network_blob_stats_blobs_insert_trigger()
RETURNS trigger AS $$
DECLARE
    delta RECORD;
BEGIN
    FOR delta IN
        SELECT
            network_id,
            COUNT(*)::bigint AS count_delta,
            COALESCE(SUM(base_fee_per_blob_gas::numeric), 0) AS sum_base_fee_delta,
            COALESCE(SUM(tip_per_blob_gas::numeric), 0) AS sum_tip_delta,
            COALESCE(SUM(total_cost_eth::numeric), 0) AS sum_total_cost_delta
        FROM new_rows
        WHERE confirmed = true
        GROUP BY network_id
    LOOP
        PERFORM network_blob_stats_apply_delta(
            delta.network_id,
            delta.count_delta,
            delta.sum_base_fee_delta,
            delta.sum_tip_delta,
            delta.sum_total_cost_delta
        );
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
            network_id,
            -COUNT(*)::bigint AS count_delta,
            -COALESCE(SUM(base_fee_per_blob_gas::numeric), 0) AS sum_base_fee_delta,
            -COALESCE(SUM(tip_per_blob_gas::numeric), 0) AS sum_tip_delta,
            -COALESCE(SUM(total_cost_eth::numeric), 0) AS sum_total_cost_delta
        FROM old_rows
        WHERE confirmed = true
        GROUP BY network_id
    LOOP
        PERFORM network_blob_stats_apply_delta(
            delta.network_id,
            delta.count_delta,
            delta.sum_base_fee_delta,
            delta.sum_tip_delta,
            delta.sum_total_cost_delta
        );
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
            network_id,
            SUM(count_delta)::bigint AS count_delta,
            SUM(sum_base_fee_delta) AS sum_base_fee_delta,
            SUM(sum_tip_delta) AS sum_tip_delta,
            SUM(sum_total_cost_delta) AS sum_total_cost_delta
        FROM (
            SELECT
                network_id,
                -COUNT(*)::bigint AS count_delta,
                -COALESCE(SUM(base_fee_per_blob_gas::numeric), 0) AS sum_base_fee_delta,
                -COALESCE(SUM(tip_per_blob_gas::numeric), 0) AS sum_tip_delta,
                -COALESCE(SUM(total_cost_eth::numeric), 0) AS sum_total_cost_delta
            FROM old_rows
            WHERE confirmed = true
            GROUP BY network_id

            UNION ALL

            SELECT
                network_id,
                COUNT(*)::bigint AS count_delta,
                COALESCE(SUM(base_fee_per_blob_gas::numeric), 0) AS sum_base_fee_delta,
                COALESCE(SUM(tip_per_blob_gas::numeric), 0) AS sum_tip_delta,
                COALESCE(SUM(total_cost_eth::numeric), 0) AS sum_total_cost_delta
            FROM new_rows
            WHERE confirmed = true
            GROUP BY network_id
        ) deltas
        GROUP BY network_id
        HAVING SUM(count_delta) <> 0
            OR SUM(sum_base_fee_delta) <> 0
            OR SUM(sum_tip_delta) <> 0
            OR SUM(sum_total_cost_delta) <> 0
    LOOP
        PERFORM network_blob_stats_apply_delta(
            delta.network_id,
            delta.count_delta,
            delta.sum_base_fee_delta,
            delta.sum_tip_delta,
            delta.sum_total_cost_delta
        );
    END LOOP;

    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_network_blob_stats_blobs_insert
AFTER INSERT ON blobs
REFERENCING NEW TABLE AS new_rows
FOR EACH STATEMENT
EXECUTE FUNCTION network_blob_stats_blobs_insert_trigger();

CREATE TRIGGER trg_network_blob_stats_blobs_update
AFTER UPDATE ON blobs
REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows
FOR EACH STATEMENT
EXECUTE FUNCTION network_blob_stats_blobs_update_trigger();

CREATE TRIGGER trg_network_blob_stats_blobs_delete
AFTER DELETE ON blobs
REFERENCING OLD TABLE AS old_rows
FOR EACH STATEMENT
EXECUTE FUNCTION network_blob_stats_blobs_delete_trigger();

CREATE OR REPLACE FUNCTION network_blob_stats_refresh_latest(p_network_id INTEGER)
RETURNS void AS $$
DECLARE
    latest RECORD;
BEGIN
    INSERT INTO network_blob_stats (network_id)
    VALUES (p_network_id)
    ON CONFLICT (network_id) DO NOTHING;

    SELECT block_number, block_timestamp
    INTO latest
    FROM block_metrics
    WHERE network_id = p_network_id
    ORDER BY block_number DESC
    LIMIT 1;

    IF FOUND THEN
        UPDATE network_blob_stats
        SET
            last_indexed_block = latest.block_number,
            last_indexed_time = latest.block_timestamp,
            updated_at = NOW()
        WHERE network_id = p_network_id;
    ELSE
        UPDATE network_blob_stats
        SET
            last_indexed_block = 0,
            last_indexed_time = '1970-01-01'::timestamp,
            updated_at = NOW()
        WHERE network_id = p_network_id;
    END IF;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION network_blob_stats_block_metrics_insert_trigger()
RETURNS trigger AS $$
DECLARE
    affected RECORD;
BEGIN
    FOR affected IN
        SELECT DISTINCT network_id
        FROM new_rows
    LOOP
        PERFORM network_blob_stats_refresh_latest(affected.network_id);
    END LOOP;

    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION network_blob_stats_block_metrics_update_trigger()
RETURNS trigger AS $$
DECLARE
    affected RECORD;
BEGIN
    FOR affected IN
        SELECT network_id FROM old_rows
        UNION
        SELECT network_id FROM new_rows
    LOOP
        PERFORM network_blob_stats_refresh_latest(affected.network_id);
    END LOOP;

    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION network_blob_stats_block_metrics_delete_trigger()
RETURNS trigger AS $$
DECLARE
    affected RECORD;
BEGIN
    FOR affected IN
        SELECT DISTINCT network_id
        FROM old_rows
    LOOP
        PERFORM network_blob_stats_refresh_latest(affected.network_id);
    END LOOP;

    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_network_blob_stats_block_metrics_insert
AFTER INSERT ON block_metrics
REFERENCING NEW TABLE AS new_rows
FOR EACH STATEMENT
EXECUTE FUNCTION network_blob_stats_block_metrics_insert_trigger();

CREATE TRIGGER trg_network_blob_stats_block_metrics_update
AFTER UPDATE ON block_metrics
REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows
FOR EACH STATEMENT
EXECUTE FUNCTION network_blob_stats_block_metrics_update_trigger();

CREATE TRIGGER trg_network_blob_stats_block_metrics_delete
AFTER DELETE ON block_metrics
REFERENCING OLD TABLE AS old_rows
FOR EACH STATEMENT
EXECUTE FUNCTION network_blob_stats_block_metrics_delete_trigger();
