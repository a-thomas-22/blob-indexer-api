-- Follow-up to the network_blob_stats summary from migration 11:
--   * Add sender-level rollups for all-history /users paths.
--   * Add covering indexes for bounded window/chart scans.
--   * Best-effort enable database query telemetry where privileges allow it.

CREATE TABLE IF NOT EXISTS blob_user_stats (
    network_id INTEGER NOT NULL,
    from_address TEXT NOT NULL,
    user_attribution TEXT NOT NULL DEFAULT '',
    blob_count BIGINT NOT NULL DEFAULT 0 CHECK (blob_count >= 0),
    total_cost_eth NUMERIC NOT NULL DEFAULT 0 CHECK (total_cost_eth >= 0),
    last_timestamp TIMESTAMP NOT NULL DEFAULT '1970-01-01'::timestamp,
    updated_at TIMESTAMP NOT NULL DEFAULT NOW(),
    PRIMARY KEY (network_id, from_address),
    CONSTRAINT fk_blob_user_stats_network_chain_id
        FOREIGN KEY (network_id)
        REFERENCES networks(chain_id)
        ON UPDATE CASCADE
        ON DELETE RESTRICT
);

INSERT INTO blob_user_stats (
    network_id,
    from_address,
    user_attribution,
    blob_count,
    total_cost_eth,
    last_timestamp,
    updated_at
)
SELECT
    network_id,
    from_address,
    COALESCE(NULLIF(MAX(BTRIM(user_attribution)), ''), ''),
    COUNT(*)::bigint,
    COALESCE(SUM(total_cost_eth::numeric), 0),
    COALESCE(MAX(timestamp), '1970-01-01'::timestamp),
    NOW()
FROM blobs
GROUP BY network_id, from_address
ON CONFLICT (network_id, from_address) DO UPDATE SET
    user_attribution = EXCLUDED.user_attribution,
    blob_count = EXCLUDED.blob_count,
    total_cost_eth = EXCLUDED.total_cost_eth,
    last_timestamp = EXCLUDED.last_timestamp,
    updated_at = NOW();

CREATE INDEX IF NOT EXISTS idx_blob_user_stats_network_count
    ON blob_user_stats(network_id, blob_count DESC, total_cost_eth DESC, last_timestamp DESC);

CREATE INDEX IF NOT EXISTS idx_blob_user_stats_network_spend
    ON blob_user_stats(network_id, total_cost_eth DESC, blob_count DESC, last_timestamp DESC);

CREATE OR REPLACE FUNCTION blob_user_stats_refresh(p_network_id INTEGER, p_from_address TEXT)
RETURNS void AS $$
DECLARE
    stats RECORD;
BEGIN
    SELECT
        COUNT(*)::bigint AS blob_count,
        COALESCE(SUM(total_cost_eth::numeric), 0) AS total_cost_eth,
        COALESCE(MAX(timestamp), '1970-01-01'::timestamp) AS last_timestamp,
        COALESCE(NULLIF(MAX(BTRIM(user_attribution)), ''), '') AS user_attribution
    INTO stats
    FROM blobs
    WHERE network_id = p_network_id
        AND from_address = p_from_address;

    IF stats.blob_count = 0 THEN
        DELETE FROM blob_user_stats
        WHERE network_id = p_network_id
            AND from_address = p_from_address;
        RETURN;
    END IF;

    INSERT INTO blob_user_stats (
        network_id,
        from_address,
        user_attribution,
        blob_count,
        total_cost_eth,
        last_timestamp,
        updated_at
    )
    VALUES (
        p_network_id,
        p_from_address,
        stats.user_attribution,
        stats.blob_count,
        stats.total_cost_eth,
        stats.last_timestamp,
        NOW()
    )
    ON CONFLICT (network_id, from_address) DO UPDATE SET
        user_attribution = EXCLUDED.user_attribution,
        blob_count = EXCLUDED.blob_count,
        total_cost_eth = EXCLUDED.total_cost_eth,
        last_timestamp = EXCLUDED.last_timestamp,
        updated_at = NOW();
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION blob_user_stats_apply_insert_delta(
    p_network_id INTEGER,
    p_from_address TEXT,
    p_user_attribution TEXT,
    p_blob_count BIGINT,
    p_total_cost_eth NUMERIC,
    p_last_timestamp TIMESTAMP
)
RETURNS void AS $$
BEGIN
    INSERT INTO blob_user_stats (
        network_id,
        from_address,
        user_attribution,
        blob_count,
        total_cost_eth,
        last_timestamp,
        updated_at
    )
    VALUES (
        p_network_id,
        p_from_address,
        COALESCE(NULLIF(BTRIM(p_user_attribution), ''), ''),
        p_blob_count,
        p_total_cost_eth,
        p_last_timestamp,
        NOW()
    )
    ON CONFLICT (network_id, from_address) DO UPDATE SET
        user_attribution = COALESCE(
            NULLIF(BTRIM(EXCLUDED.user_attribution), ''),
            NULLIF(BTRIM(blob_user_stats.user_attribution), ''),
            ''
        ),
        blob_count = blob_user_stats.blob_count + EXCLUDED.blob_count,
        total_cost_eth = blob_user_stats.total_cost_eth + EXCLUDED.total_cost_eth,
        last_timestamp = GREATEST(blob_user_stats.last_timestamp, EXCLUDED.last_timestamp),
        updated_at = NOW();
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION blob_user_stats_blobs_insert_statement_trigger()
RETURNS trigger AS $$
DECLARE
    delta RECORD;
BEGIN
    FOR delta IN
        SELECT
            network_id,
            from_address,
            COALESCE(NULLIF(MAX(BTRIM(user_attribution)), ''), '') AS user_attribution,
            COUNT(*)::bigint AS blob_count,
            COALESCE(SUM(total_cost_eth::numeric), 0) AS total_cost_eth,
            COALESCE(MAX(timestamp), '1970-01-01'::timestamp) AS last_timestamp
        FROM new_blobs
        GROUP BY network_id, from_address
    LOOP
        PERFORM blob_user_stats_apply_insert_delta(
            delta.network_id,
            delta.from_address,
            delta.user_attribution,
            delta.blob_count,
            delta.total_cost_eth,
            delta.last_timestamp
        );
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION blob_user_stats_blobs_delete_statement_trigger()
RETURNS trigger AS $$
DECLARE
    affected RECORD;
BEGIN
    FOR affected IN
        SELECT DISTINCT network_id, from_address FROM old_blobs
    LOOP
        PERFORM blob_user_stats_refresh(affected.network_id, affected.from_address);
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION blob_user_stats_blobs_update_statement_trigger()
RETURNS trigger AS $$
DECLARE
    affected RECORD;
BEGIN
    FOR affected IN
        SELECT DISTINCT network_id, from_address FROM old_blobs
        UNION
        SELECT DISTINCT network_id, from_address FROM new_blobs
    LOOP
        PERFORM blob_user_stats_refresh(affected.network_id, affected.from_address);
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_blob_user_stats_blobs ON blobs;
DROP TRIGGER IF EXISTS trg_blob_user_stats_blobs_delete ON blobs;
DROP TRIGGER IF EXISTS trg_blob_user_stats_blobs_update ON blobs;

CREATE TRIGGER trg_blob_user_stats_blobs
AFTER INSERT ON blobs
REFERENCING NEW TABLE AS new_blobs
FOR EACH STATEMENT
EXECUTE FUNCTION blob_user_stats_blobs_insert_statement_trigger();

CREATE TRIGGER trg_blob_user_stats_blobs_delete
AFTER DELETE ON blobs
REFERENCING OLD TABLE AS old_blobs
FOR EACH STATEMENT
EXECUTE FUNCTION blob_user_stats_blobs_delete_statement_trigger();

CREATE TRIGGER trg_blob_user_stats_blobs_update
AFTER UPDATE ON blobs
REFERENCING OLD TABLE AS old_blobs NEW TABLE AS new_blobs
FOR EACH STATEMENT
EXECUTE FUNCTION blob_user_stats_blobs_update_statement_trigger();

CREATE INDEX IF NOT EXISTS idx_blobs_network_from_timestamp
    ON blobs(network_id, from_address, timestamp DESC);

CREATE INDEX IF NOT EXISTS idx_blobs_network_confirmed_timestamp_cover
    ON blobs(network_id, confirmed, timestamp DESC)
    INCLUDE (from_address, total_cost_eth, base_fee_per_blob_gas, blob_gas_used);

CREATE INDEX IF NOT EXISTS idx_block_metrics_network_timestamp_cover
    ON block_metrics(network_id, block_timestamp DESC)
    INCLUDE (block_number, blob_count, blob_gas_used, blob_gas_target, blob_base_fee, utilization_ratio);

DO $$
BEGIN
    BEGIN
        CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
    EXCEPTION WHEN OTHERS THEN
        RAISE NOTICE 'pg_stat_statements could not be created by this role: %', SQLERRM;
    END;

    BEGIN
        EXECUTE format('ALTER DATABASE %I SET log_min_duration_statement = %L', current_database(), '500ms');
        EXECUTE format('ALTER DATABASE %I SET track_io_timing = %L', current_database(), 'on');
        EXECUTE format('ALTER DATABASE %I SET pg_stat_statements.track = %L', current_database(), 'all');
    EXCEPTION WHEN OTHERS THEN
        RAISE NOTICE 'database telemetry settings could not be applied by this role: %', SQLERRM;
    END;
END $$;
