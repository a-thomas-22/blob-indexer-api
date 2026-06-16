-- Consolidated baseline schema for the blob indexer.
--
-- This replaces the original 16 incremental migrations (the alpha database is
-- reset, so no data migration is required). The schema expresses the desired
-- end state directly:
--   * chain_id is the canonical network key (the dead networks.id SERIAL is
--     gone; networks.chain_id is the primary key). All child tables reference
--     networks(chain_id) ON UPDATE RESTRICT ON DELETE RESTRICT.
--   * blobs no longer carries the per-row indexer_version column.
--   * the wei-denominated cost column is named total_cost_wei (it always held
--     wei).
--   * blob_size_bytes is retained — it is summed into the calldata-cost chart
--     math and the chart rollups.
--
-- Pending (mempool) rows still use the internal block_number < 0 sentinel; that
-- is an indexer implementation detail and never appears on the wire (the API
-- serializes a pending block_number as null). DDL only, idempotent, no explicit
-- transaction control — see README.md.

-- ---------------------------------------------------------------------------
-- Core tables
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS networks (
    chain_id INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    start_block TEXT NOT NULL,
    is_enabled BOOLEAN DEFAULT TRUE,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS blobs (
    id SERIAL PRIMARY KEY,
    chain_id INTEGER NOT NULL,
    block_number BIGINT NOT NULL,
    blob_index INTEGER NOT NULL,
    tx_hash TEXT NOT NULL,
    from_address TEXT NOT NULL,
    user_attribution TEXT,
    blob_size_bytes BIGINT NOT NULL,
    base_fee_per_blob_gas NUMERIC NOT NULL,
    tip_per_blob_gas NUMERIC NOT NULL,
    total_cost_wei NUMERIC NOT NULL,
    timestamp TIMESTAMP NOT NULL,
    confirmed BOOLEAN DEFAULT TRUE,
    max_fee_per_blob_gas NUMERIC,
    blob_gas_used BIGINT,
    UNIQUE (chain_id, block_number, blob_index),
    CONSTRAINT fk_blobs_network_chain_id
        FOREIGN KEY (chain_id) REFERENCES networks(chain_id)
        ON UPDATE RESTRICT ON DELETE RESTRICT
);

CREATE TABLE IF NOT EXISTS blob_users (
    id SERIAL PRIMARY KEY,
    chain_id INTEGER NOT NULL,
    address TEXT NOT NULL,
    name TEXT NOT NULL,
    description TEXT,
    category TEXT,
    first_seen TIMESTAMP NOT NULL,
    last_seen TIMESTAMP NOT NULL,
    UNIQUE (chain_id, address),
    CONSTRAINT fk_blob_users_network_chain_id
        FOREIGN KEY (chain_id) REFERENCES networks(chain_id)
        ON UPDATE RESTRICT ON DELETE RESTRICT
);

CREATE TABLE IF NOT EXISTS indexer_metadata (
    id SERIAL PRIMARY KEY,
    chain_id INTEGER,
    key TEXT NOT NULL,
    value TEXT NOT NULL,
    UNIQUE (chain_id, key),
    CONSTRAINT fk_indexer_metadata_network_chain_id
        FOREIGN KEY (chain_id) REFERENCES networks(chain_id)
        ON UPDATE RESTRICT ON DELETE RESTRICT
);

-- PostgreSQL treats NULL as distinct in UNIQUE(chain_id, key), so global
-- metadata (chain_id IS NULL) needs its own partial unique index.
CREATE UNIQUE INDEX IF NOT EXISTS idx_indexer_metadata_global_key
    ON indexer_metadata(key)
    WHERE chain_id IS NULL;

CREATE TABLE IF NOT EXISTS indexed_blocks (
    chain_id INTEGER NOT NULL,
    block_number BIGINT NOT NULL,
    block_hash TEXT NOT NULL,
    parent_hash TEXT NOT NULL,
    indexed_at TIMESTAMP NOT NULL DEFAULT NOW(),
    PRIMARY KEY (chain_id, block_number),
    CONSTRAINT fk_indexed_blocks_network_chain_id
        FOREIGN KEY (chain_id) REFERENCES networks(chain_id)
        ON UPDATE RESTRICT ON DELETE RESTRICT
);

CREATE TABLE IF NOT EXISTS block_metrics (
    chain_id          INTEGER NOT NULL,
    block_number      BIGINT NOT NULL,
    block_timestamp   TIMESTAMP NOT NULL,
    blob_count        SMALLINT NOT NULL DEFAULT 0,
    blob_gas_used     BIGINT NOT NULL DEFAULT 0,
    blob_gas_target   BIGINT NOT NULL DEFAULT 0,
    blob_gas_limit    BIGINT NOT NULL DEFAULT 0,
    excess_blob_gas   BIGINT NOT NULL DEFAULT 0,
    blob_base_fee     NUMERIC NOT NULL DEFAULT 0,
    utilization_ratio NUMERIC(10, 6) NOT NULL DEFAULT 0,
    blob_params_target SMALLINT NOT NULL DEFAULT 3,
    blob_params_max    SMALLINT NOT NULL DEFAULT 6,
    update_fraction    BIGINT NOT NULL DEFAULT 3338477,
    PRIMARY KEY (chain_id, block_number),
    CONSTRAINT fk_block_metrics_network_chain_id
        FOREIGN KEY (chain_id) REFERENCES networks(chain_id)
        ON UPDATE RESTRICT ON DELETE RESTRICT
);

CREATE TABLE IF NOT EXISTS blob_attribution_claims (
    id SERIAL PRIMARY KEY,
    chain_id INTEGER NOT NULL,
    source TEXT NOT NULL,
    address TEXT NOT NULL,
    entity_id TEXT NOT NULL,
    name TEXT NOT NULL,
    category TEXT NOT NULL DEFAULT '',
    role TEXT NOT NULL DEFAULT '',
    confidence TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT '',
    valid_from_block BIGINT NOT NULL,
    valid_to_block BIGINT,
    updated_at TIMESTAMP NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_blob_attribution_claims_network_chain_id
        FOREIGN KEY (chain_id) REFERENCES networks(chain_id)
        ON UPDATE RESTRICT ON DELETE RESTRICT,
    UNIQUE (chain_id, source, address, entity_id, role, valid_from_block)
);

CREATE TABLE IF NOT EXISTS block_reindex_requests (
    id BIGSERIAL PRIMARY KEY,
    chain_id INTEGER NOT NULL,
    start_block BIGINT NOT NULL CHECK (start_block >= 0),
    end_block BIGINT NOT NULL CHECK (end_block >= start_block),
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'processing', 'completed', 'failed')),
    requested_by TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error TEXT,
    claimed_by TEXT,
    requested_at TIMESTAMP NOT NULL DEFAULT NOW(),
    started_at TIMESTAMP,
    completed_at TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_block_reindex_requests_network_chain_id
        FOREIGN KEY (chain_id) REFERENCES networks(chain_id)
        ON UPDATE RESTRICT ON DELETE RESTRICT
);

-- ---------------------------------------------------------------------------
-- network_blob_stats: denormalized running totals maintained by statement-level
-- triggers on blobs (confirmed deltas) and block_metrics (latest indexed block).
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS network_blob_stats (
    chain_id INTEGER PRIMARY KEY,
    total_confirmed_blobs BIGINT NOT NULL DEFAULT 0 CHECK (total_confirmed_blobs >= 0),
    sum_base_fee_per_blob_gas NUMERIC NOT NULL DEFAULT 0 CHECK (sum_base_fee_per_blob_gas >= 0),
    sum_tip_per_blob_gas NUMERIC NOT NULL DEFAULT 0 CHECK (sum_tip_per_blob_gas >= 0),
    sum_total_cost NUMERIC NOT NULL DEFAULT 0 CHECK (sum_total_cost >= 0),
    last_indexed_block BIGINT NOT NULL DEFAULT 0 CHECK (last_indexed_block >= 0),
    last_indexed_time TIMESTAMP NOT NULL DEFAULT '1970-01-01'::timestamp,
    updated_at TIMESTAMP NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_network_blob_stats_network_chain_id
        FOREIGN KEY (chain_id) REFERENCES networks(chain_id)
        ON UPDATE RESTRICT ON DELETE RESTRICT
);

CREATE OR REPLACE FUNCTION network_blob_stats_apply_delta(
    p_chain_id INTEGER,
    p_count_delta BIGINT,
    p_sum_base_fee_delta NUMERIC,
    p_sum_tip_delta NUMERIC,
    p_sum_total_cost_delta NUMERIC
) RETURNS void AS $$
BEGIN
    INSERT INTO network_blob_stats (chain_id)
    VALUES (p_chain_id)
    ON CONFLICT (chain_id) DO NOTHING;

    UPDATE network_blob_stats
    SET
        total_confirmed_blobs = GREATEST(total_confirmed_blobs + p_count_delta, 0::bigint),
        sum_base_fee_per_blob_gas = GREATEST(sum_base_fee_per_blob_gas + p_sum_base_fee_delta, 0::numeric),
        sum_tip_per_blob_gas = GREATEST(sum_tip_per_blob_gas + p_sum_tip_delta, 0::numeric),
        sum_total_cost = GREATEST(sum_total_cost + p_sum_total_cost_delta, 0::numeric),
        updated_at = NOW()
    WHERE chain_id = p_chain_id;
END;
$$ LANGUAGE plpgsql;

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

DROP TRIGGER IF EXISTS trg_network_blob_stats_blobs_insert ON blobs;
CREATE TRIGGER trg_network_blob_stats_blobs_insert
AFTER INSERT ON blobs
REFERENCING NEW TABLE AS new_rows
FOR EACH STATEMENT
EXECUTE FUNCTION network_blob_stats_blobs_insert_trigger();

DROP TRIGGER IF EXISTS trg_network_blob_stats_blobs_update ON blobs;
CREATE TRIGGER trg_network_blob_stats_blobs_update
AFTER UPDATE ON blobs
REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows
FOR EACH STATEMENT
EXECUTE FUNCTION network_blob_stats_blobs_update_trigger();

DROP TRIGGER IF EXISTS trg_network_blob_stats_blobs_delete ON blobs;
CREATE TRIGGER trg_network_blob_stats_blobs_delete
AFTER DELETE ON blobs
REFERENCING OLD TABLE AS old_rows
FOR EACH STATEMENT
EXECUTE FUNCTION network_blob_stats_blobs_delete_trigger();

CREATE OR REPLACE FUNCTION network_blob_stats_refresh_latest(p_chain_id INTEGER)
RETURNS void AS $$
DECLARE
    latest RECORD;
BEGIN
    INSERT INTO network_blob_stats (chain_id)
    VALUES (p_chain_id)
    ON CONFLICT (chain_id) DO NOTHING;

    SELECT block_number, block_timestamp
    INTO latest
    FROM block_metrics
    WHERE chain_id = p_chain_id
    ORDER BY block_number DESC
    LIMIT 1;

    IF FOUND THEN
        UPDATE network_blob_stats
        SET last_indexed_block = latest.block_number,
            last_indexed_time = latest.block_timestamp,
            updated_at = NOW()
        WHERE chain_id = p_chain_id;
    ELSE
        UPDATE network_blob_stats
        SET last_indexed_block = 0,
            last_indexed_time = '1970-01-01'::timestamp,
            updated_at = NOW()
        WHERE chain_id = p_chain_id;
    END IF;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION network_blob_stats_block_metrics_insert_trigger()
RETURNS trigger AS $$
DECLARE
    affected RECORD;
BEGIN
    FOR affected IN SELECT DISTINCT chain_id FROM new_rows LOOP
        PERFORM network_blob_stats_refresh_latest(affected.chain_id);
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
        SELECT chain_id FROM old_rows
        UNION
        SELECT chain_id FROM new_rows
    LOOP
        PERFORM network_blob_stats_refresh_latest(affected.chain_id);
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION network_blob_stats_block_metrics_delete_trigger()
RETURNS trigger AS $$
DECLARE
    affected RECORD;
BEGIN
    FOR affected IN SELECT DISTINCT chain_id FROM old_rows LOOP
        PERFORM network_blob_stats_refresh_latest(affected.chain_id);
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_network_blob_stats_block_metrics_insert ON block_metrics;
CREATE TRIGGER trg_network_blob_stats_block_metrics_insert
AFTER INSERT ON block_metrics
REFERENCING NEW TABLE AS new_rows
FOR EACH STATEMENT
EXECUTE FUNCTION network_blob_stats_block_metrics_insert_trigger();

DROP TRIGGER IF EXISTS trg_network_blob_stats_block_metrics_update ON block_metrics;
CREATE TRIGGER trg_network_blob_stats_block_metrics_update
AFTER UPDATE ON block_metrics
REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows
FOR EACH STATEMENT
EXECUTE FUNCTION network_blob_stats_block_metrics_update_trigger();

DROP TRIGGER IF EXISTS trg_network_blob_stats_block_metrics_delete ON block_metrics;
CREATE TRIGGER trg_network_blob_stats_block_metrics_delete
AFTER DELETE ON block_metrics
REFERENCING OLD TABLE AS old_rows
FOR EACH STATEMENT
EXECUTE FUNCTION network_blob_stats_block_metrics_delete_trigger();

-- ---------------------------------------------------------------------------
-- blob_user_stats: per-sender all-history rollups for the /users paths.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS blob_user_stats (
    chain_id INTEGER NOT NULL,
    from_address TEXT NOT NULL,
    user_attribution TEXT NOT NULL DEFAULT '',
    blob_count BIGINT NOT NULL DEFAULT 0 CHECK (blob_count >= 0),
    total_cost_wei NUMERIC NOT NULL DEFAULT 0 CHECK (total_cost_wei >= 0),
    last_timestamp TIMESTAMP NOT NULL DEFAULT '1970-01-01'::timestamp,
    updated_at TIMESTAMP NOT NULL DEFAULT NOW(),
    PRIMARY KEY (chain_id, from_address),
    CONSTRAINT fk_blob_user_stats_network_chain_id
        FOREIGN KEY (chain_id) REFERENCES networks(chain_id)
        ON UPDATE RESTRICT ON DELETE RESTRICT
);

CREATE OR REPLACE FUNCTION blob_user_stats_refresh(p_chain_id INTEGER, p_from_address TEXT)
RETURNS void AS $$
DECLARE
    stats RECORD;
BEGIN
    SELECT
        COUNT(*)::bigint AS blob_count,
        COALESCE(SUM(total_cost_wei::numeric), 0) AS total_cost_wei,
        COALESCE(MAX(timestamp), '1970-01-01'::timestamp) AS last_timestamp,
        COALESCE(NULLIF(MAX(BTRIM(user_attribution)), ''), '') AS user_attribution
    INTO stats
    FROM blobs
    WHERE chain_id = p_chain_id AND from_address = p_from_address;

    IF stats.blob_count = 0 THEN
        DELETE FROM blob_user_stats
        WHERE chain_id = p_chain_id AND from_address = p_from_address;
        RETURN;
    END IF;

    INSERT INTO blob_user_stats (
        chain_id, from_address, user_attribution, blob_count,
        total_cost_wei, last_timestamp, updated_at
    )
    VALUES (
        p_chain_id, p_from_address, stats.user_attribution, stats.blob_count,
        stats.total_cost_wei, stats.last_timestamp, NOW()
    )
    ON CONFLICT (chain_id, from_address) DO UPDATE SET
        user_attribution = EXCLUDED.user_attribution,
        blob_count = EXCLUDED.blob_count,
        total_cost_wei = EXCLUDED.total_cost_wei,
        last_timestamp = EXCLUDED.last_timestamp,
        updated_at = NOW();
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION blob_user_stats_apply_insert_delta(
    p_chain_id INTEGER,
    p_from_address TEXT,
    p_user_attribution TEXT,
    p_blob_count BIGINT,
    p_total_cost_wei NUMERIC,
    p_last_timestamp TIMESTAMP
)
RETURNS void AS $$
BEGIN
    INSERT INTO blob_user_stats (
        chain_id, from_address, user_attribution, blob_count,
        total_cost_wei, last_timestamp, updated_at
    )
    VALUES (
        p_chain_id, p_from_address,
        COALESCE(NULLIF(BTRIM(p_user_attribution), ''), ''),
        p_blob_count, p_total_cost_wei, p_last_timestamp, NOW()
    )
    ON CONFLICT (chain_id, from_address) DO UPDATE SET
        user_attribution = COALESCE(
            NULLIF(BTRIM(EXCLUDED.user_attribution), ''),
            NULLIF(BTRIM(blob_user_stats.user_attribution), ''),
            ''
        ),
        blob_count = blob_user_stats.blob_count + EXCLUDED.blob_count,
        total_cost_wei = blob_user_stats.total_cost_wei + EXCLUDED.total_cost_wei,
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
            chain_id,
            from_address,
            COALESCE(NULLIF(MAX(BTRIM(user_attribution)), ''), '') AS user_attribution,
            COUNT(*)::bigint AS blob_count,
            COALESCE(SUM(total_cost_wei::numeric), 0) AS total_cost_wei,
            COALESCE(MAX(timestamp), '1970-01-01'::timestamp) AS last_timestamp
        FROM new_blobs
        GROUP BY chain_id, from_address
    LOOP
        PERFORM blob_user_stats_apply_insert_delta(
            delta.chain_id, delta.from_address, delta.user_attribution,
            delta.blob_count, delta.total_cost_wei, delta.last_timestamp);
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION blob_user_stats_blobs_delete_statement_trigger()
RETURNS trigger AS $$
DECLARE
    affected RECORD;
BEGIN
    FOR affected IN SELECT DISTINCT chain_id, from_address FROM old_blobs LOOP
        PERFORM blob_user_stats_refresh(affected.chain_id, affected.from_address);
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
        SELECT DISTINCT chain_id, from_address FROM old_blobs
        UNION
        SELECT DISTINCT chain_id, from_address FROM new_blobs
    LOOP
        PERFORM blob_user_stats_refresh(affected.chain_id, affected.from_address);
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_blob_user_stats_blobs ON blobs;
CREATE TRIGGER trg_blob_user_stats_blobs
AFTER INSERT ON blobs
REFERENCING NEW TABLE AS new_blobs
FOR EACH STATEMENT
EXECUTE FUNCTION blob_user_stats_blobs_insert_statement_trigger();

DROP TRIGGER IF EXISTS trg_blob_user_stats_blobs_delete ON blobs;
CREATE TRIGGER trg_blob_user_stats_blobs_delete
AFTER DELETE ON blobs
REFERENCING OLD TABLE AS old_blobs
FOR EACH STATEMENT
EXECUTE FUNCTION blob_user_stats_blobs_delete_statement_trigger();

DROP TRIGGER IF EXISTS trg_blob_user_stats_blobs_update ON blobs;
CREATE TRIGGER trg_blob_user_stats_blobs_update
AFTER UPDATE ON blobs
REFERENCING OLD TABLE AS old_blobs NEW TABLE AS new_blobs
FOR EACH STATEMENT
EXECUTE FUNCTION blob_user_stats_blobs_update_statement_trigger();

-- ---------------------------------------------------------------------------
-- Chart rollups (block_metrics_rollups + blob_chart_rollups).
-- ---------------------------------------------------------------------------

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
    chain_id INTEGER NOT NULL,
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
    blocks_above_target BIGINT NOT NULL DEFAULT 0,
    blocks_at_max BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMP NOT NULL DEFAULT NOW(),
    PRIMARY KEY (chain_id, bucket_seconds, bucket_start),
    CONSTRAINT fk_block_metrics_rollups_network_chain_id
        FOREIGN KEY (chain_id) REFERENCES networks(chain_id)
        ON UPDATE RESTRICT ON DELETE RESTRICT
);

CREATE TABLE IF NOT EXISTS blob_chart_rollups (
    chain_id INTEGER NOT NULL,
    bucket_seconds INTEGER NOT NULL,
    bucket_start TIMESTAMP NOT NULL,
    from_address TEXT NOT NULL,
    user_attribution TEXT NOT NULL DEFAULT '',
    blob_count BIGINT NOT NULL DEFAULT 0 CHECK (blob_count >= 0),
    blob_bytes BIGINT NOT NULL DEFAULT 0,
    blob_gas_used BIGINT NOT NULL DEFAULT 0,
    total_cost_wei NUMERIC NOT NULL DEFAULT 0,
    sum_size_base_fee NUMERIC NOT NULL DEFAULT 0,
    updated_at TIMESTAMP NOT NULL DEFAULT NOW(),
    PRIMARY KEY (chain_id, bucket_seconds, bucket_start, from_address),
    CONSTRAINT fk_blob_chart_rollups_network_chain_id
        FOREIGN KEY (chain_id) REFERENCES networks(chain_id)
        ON UPDATE RESTRICT ON DELETE RESTRICT
);

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

CREATE OR REPLACE FUNCTION block_metrics_rollups_insert_statement_trigger()
RETURNS trigger AS $$
DECLARE
    affected RECORD;
BEGIN
    FOR affected IN
        SELECT DISTINCT
            r.chain_id,
            g.bucket_seconds,
            chart_rollup_bucket_start(r.block_timestamp, g.bucket_seconds) AS bucket_start
        FROM new_rows r
        CROSS JOIN chart_rollup_bucket_seconds() g
    LOOP
        PERFORM block_metrics_rollups_refresh(affected.chain_id, affected.bucket_seconds, affected.bucket_start);
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
            r.chain_id,
            g.bucket_seconds,
            chart_rollup_bucket_start(r.block_timestamp, g.bucket_seconds) AS bucket_start
        FROM old_rows r
        CROSS JOIN chart_rollup_bucket_seconds() g
    LOOP
        PERFORM block_metrics_rollups_refresh(affected.chain_id, affected.bucket_seconds, affected.bucket_start);
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
            r.chain_id,
            g.bucket_seconds,
            chart_rollup_bucket_start(r.block_timestamp, g.bucket_seconds) AS bucket_start
        FROM (
            SELECT chain_id, block_timestamp FROM old_rows
            UNION
            SELECT chain_id, block_timestamp FROM new_rows
        ) r
        CROSS JOIN chart_rollup_bucket_seconds() g
    LOOP
        PERFORM block_metrics_rollups_refresh(affected.chain_id, affected.bucket_seconds, affected.bucket_start);
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_block_metrics_rollups_insert ON block_metrics;
CREATE TRIGGER trg_block_metrics_rollups_insert
AFTER INSERT ON block_metrics
REFERENCING NEW TABLE AS new_rows
FOR EACH STATEMENT
EXECUTE FUNCTION block_metrics_rollups_insert_statement_trigger();

DROP TRIGGER IF EXISTS trg_block_metrics_rollups_update ON block_metrics;
CREATE TRIGGER trg_block_metrics_rollups_update
AFTER UPDATE ON block_metrics
REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows
FOR EACH STATEMENT
EXECUTE FUNCTION block_metrics_rollups_update_statement_trigger();

DROP TRIGGER IF EXISTS trg_block_metrics_rollups_delete ON block_metrics;
CREATE TRIGGER trg_block_metrics_rollups_delete
AFTER DELETE ON block_metrics
REFERENCING OLD TABLE AS old_rows
FOR EACH STATEMENT
EXECUTE FUNCTION block_metrics_rollups_delete_statement_trigger();

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

DROP TRIGGER IF EXISTS trg_blob_chart_rollups_insert ON blobs;
CREATE TRIGGER trg_blob_chart_rollups_insert
AFTER INSERT ON blobs
REFERENCING NEW TABLE AS new_blobs
FOR EACH STATEMENT
EXECUTE FUNCTION blob_chart_rollups_insert_statement_trigger();

DROP TRIGGER IF EXISTS trg_blob_chart_rollups_update ON blobs;
CREATE TRIGGER trg_blob_chart_rollups_update
AFTER UPDATE ON blobs
REFERENCING OLD TABLE AS old_blobs NEW TABLE AS new_blobs
FOR EACH STATEMENT
EXECUTE FUNCTION blob_chart_rollups_update_statement_trigger();

DROP TRIGGER IF EXISTS trg_blob_chart_rollups_delete ON blobs;
CREATE TRIGGER trg_blob_chart_rollups_delete
AFTER DELETE ON blobs
REFERENCING OLD TABLE AS old_blobs
FOR EACH STATEMENT
EXECUTE FUNCTION blob_chart_rollups_delete_statement_trigger();

-- ---------------------------------------------------------------------------
-- Indexes
-- ---------------------------------------------------------------------------

CREATE INDEX IF NOT EXISTS idx_blobs_chain_id ON blobs(chain_id);
CREATE INDEX IF NOT EXISTS idx_blobs_block_number ON blobs(block_number);
CREATE INDEX IF NOT EXISTS idx_blobs_from_address ON blobs(from_address);
CREATE INDEX IF NOT EXISTS idx_blobs_timestamp ON blobs(timestamp);
CREATE INDEX IF NOT EXISTS idx_blobs_confirmed ON blobs(confirmed);
CREATE INDEX IF NOT EXISTS idx_blobs_chain_confirmed_block ON blobs(chain_id, confirmed, block_number DESC);
CREATE INDEX IF NOT EXISTS idx_blobs_chain_txhash ON blobs(chain_id, tx_hash);
CREATE INDEX IF NOT EXISTS idx_blobs_chain_timestamp ON blobs(chain_id, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_blobs_chain_confirmed_timestamp ON blobs(chain_id, confirmed, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_blobs_chain_lower_from_address ON blobs(chain_id, LOWER(from_address));
CREATE INDEX IF NOT EXISTS idx_blobs_chain_from_timestamp ON blobs(chain_id, from_address, timestamp DESC);
-- Pending (mempool) rows use the block_number < 0 sentinel; this non-unique
-- partial index serves the mempool delete/lookup path. Multiple pending rows
-- per tx (one per blob) are allowed.
CREATE INDEX IF NOT EXISTS idx_blobs_pending_chain_tx_hash
    ON blobs(chain_id, tx_hash) WHERE block_number < 0;
CREATE INDEX IF NOT EXISTS idx_blobs_chain_confirmed_timestamp_cover
    ON blobs(chain_id, confirmed, timestamp DESC)
    INCLUDE (from_address, total_cost_wei, base_fee_per_blob_gas, blob_gas_used);
CREATE INDEX IF NOT EXISTS idx_blobs_chain_confirmed_timestamp_chart_cover
    ON blobs(chain_id, confirmed, timestamp DESC)
    INCLUDE (from_address, user_attribution, total_cost_wei, base_fee_per_blob_gas, blob_gas_used, blob_size_bytes);

CREATE INDEX IF NOT EXISTS idx_blob_users_chain_id ON blob_users(chain_id);
CREATE INDEX IF NOT EXISTS idx_blob_users_address ON blob_users(address);
CREATE INDEX IF NOT EXISTS idx_blob_users_chain_lower_address
    ON blob_users(chain_id, LOWER(address)) INCLUDE (name, category);

CREATE INDEX IF NOT EXISTS idx_block_metrics_chain_block ON block_metrics(chain_id, block_number DESC);
CREATE INDEX IF NOT EXISTS idx_block_metrics_chain_timestamp_cover
    ON block_metrics(chain_id, block_timestamp DESC)
    INCLUDE (block_number, blob_count, blob_gas_used, blob_gas_target, blob_base_fee, utilization_ratio);

CREATE INDEX IF NOT EXISTS idx_blob_attribution_claims_chain_source
    ON blob_attribution_claims(chain_id, source);
CREATE INDEX IF NOT EXISTS idx_blob_attribution_claims_address
    ON blob_attribution_claims(chain_id, address);

CREATE INDEX IF NOT EXISTS idx_blob_user_stats_chain_count
    ON blob_user_stats(chain_id, blob_count DESC, total_cost_wei DESC, last_timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_blob_user_stats_chain_spend
    ON blob_user_stats(chain_id, total_cost_wei DESC, blob_count DESC, last_timestamp DESC);

CREATE INDEX IF NOT EXISTS idx_block_reindex_requests_pending
    ON block_reindex_requests(chain_id, requested_at, id) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_block_reindex_requests_status
    ON block_reindex_requests(status, updated_at DESC);

-- Best-effort: install pg_stat_statements where role privileges allow it.
DO $$
BEGIN
    BEGIN
        CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
    EXCEPTION WHEN OTHERS THEN
        RAISE NOTICE 'pg_stat_statements could not be created by this role: %', SQLERRM;
    END;
END $$;

-- Default network (mainnet).
INSERT INTO networks (chain_id, name, start_block, is_enabled)
VALUES (1, 'mainnet', 'LATEST-1000', true)
ON CONFLICT (chain_id) DO NOTHING;
