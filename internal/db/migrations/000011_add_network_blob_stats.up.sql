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

CREATE OR REPLACE FUNCTION network_blob_stats_blobs_trigger()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'UPDATE'
        AND OLD.network_id IS NOT DISTINCT FROM NEW.network_id
        AND OLD.confirmed IS NOT DISTINCT FROM NEW.confirmed
        AND OLD.base_fee_per_blob_gas IS NOT DISTINCT FROM NEW.base_fee_per_blob_gas
        AND OLD.tip_per_blob_gas IS NOT DISTINCT FROM NEW.tip_per_blob_gas
        AND OLD.total_cost_eth IS NOT DISTINCT FROM NEW.total_cost_eth THEN
        RETURN NEW;
    END IF;

    IF TG_OP IN ('UPDATE', 'DELETE') AND OLD.confirmed = true THEN
        PERFORM network_blob_stats_apply_delta(
            OLD.network_id,
            -1,
            -OLD.base_fee_per_blob_gas,
            -OLD.tip_per_blob_gas,
            -OLD.total_cost_eth
        );
    END IF;

    IF TG_OP IN ('INSERT', 'UPDATE') AND NEW.confirmed = true THEN
        PERFORM network_blob_stats_apply_delta(
            NEW.network_id,
            1,
            NEW.base_fee_per_blob_gas,
            NEW.tip_per_blob_gas,
            NEW.total_cost_eth
        );
    END IF;

    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_network_blob_stats_blobs
AFTER INSERT OR UPDATE OR DELETE ON blobs
FOR EACH ROW
EXECUTE FUNCTION network_blob_stats_blobs_trigger();

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

CREATE OR REPLACE FUNCTION network_blob_stats_block_metrics_trigger()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'UPDATE' AND OLD.network_id <> NEW.network_id THEN
        PERFORM network_blob_stats_refresh_latest(OLD.network_id);
    END IF;

    IF TG_OP = 'DELETE' THEN
        PERFORM network_blob_stats_refresh_latest(OLD.network_id);
        RETURN OLD;
    END IF;

    PERFORM network_blob_stats_refresh_latest(NEW.network_id);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_network_blob_stats_block_metrics
AFTER INSERT OR UPDATE OR DELETE ON block_metrics
FOR EACH ROW
EXECUTE FUNCTION network_blob_stats_block_metrics_trigger();
