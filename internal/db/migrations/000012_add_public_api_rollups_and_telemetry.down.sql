DROP TRIGGER IF EXISTS trg_blob_user_stats_blobs_update ON blobs;
DROP TRIGGER IF EXISTS trg_blob_user_stats_blobs_delete ON blobs;
DROP TRIGGER IF EXISTS trg_blob_user_stats_blobs ON blobs;

DROP FUNCTION IF EXISTS blob_user_stats_blobs_update_statement_trigger();
DROP FUNCTION IF EXISTS blob_user_stats_blobs_delete_statement_trigger();
DROP FUNCTION IF EXISTS blob_user_stats_blobs_insert_statement_trigger();
DROP FUNCTION IF EXISTS blob_user_stats_apply_insert_delta(INTEGER, TEXT, TEXT, BIGINT, NUMERIC, TIMESTAMP);
DROP FUNCTION IF EXISTS blob_user_stats_refresh(INTEGER, TEXT);

DROP INDEX IF EXISTS idx_block_metrics_network_timestamp_cover;
DROP INDEX IF EXISTS idx_blobs_network_confirmed_timestamp_cover;
DROP INDEX IF EXISTS idx_blobs_network_from_timestamp;

DROP INDEX IF EXISTS idx_blob_user_stats_network_spend;
DROP INDEX IF EXISTS idx_blob_user_stats_network_count;
DROP TABLE IF EXISTS blob_user_stats;

DROP TRIGGER IF EXISTS trg_network_blob_stats_block_metrics_update ON block_metrics;
DROP TRIGGER IF EXISTS trg_network_blob_stats_block_metrics_delete ON block_metrics;
DROP TRIGGER IF EXISTS trg_network_blob_stats_block_metrics ON block_metrics;
DROP TRIGGER IF EXISTS trg_network_blob_stats_blobs_update ON blobs;
DROP TRIGGER IF EXISTS trg_network_blob_stats_blobs_delete ON blobs;
DROP TRIGGER IF EXISTS trg_network_blob_stats_blobs ON blobs;

DROP FUNCTION IF EXISTS network_blob_stats_block_metrics_update_statement_trigger();
DROP FUNCTION IF EXISTS network_blob_stats_block_metrics_delete_statement_trigger();
DROP FUNCTION IF EXISTS network_blob_stats_block_metrics_insert_statement_trigger();
DROP FUNCTION IF EXISTS network_blob_stats_blobs_update_statement_trigger();
DROP FUNCTION IF EXISTS network_blob_stats_blobs_delete_statement_trigger();
DROP FUNCTION IF EXISTS network_blob_stats_blobs_insert_statement_trigger();

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

CREATE TRIGGER trg_network_blob_stats_blobs
AFTER INSERT OR UPDATE OR DELETE ON blobs
FOR EACH ROW
EXECUTE FUNCTION network_blob_stats_blobs_trigger();

CREATE TRIGGER trg_network_blob_stats_block_metrics
AFTER INSERT OR UPDATE OR DELETE ON block_metrics
FOR EACH ROW
EXECUTE FUNCTION network_blob_stats_block_metrics_trigger();

DO $$
BEGIN
    BEGIN
        EXECUTE format('ALTER DATABASE %I RESET log_min_duration_statement', current_database());
        EXECUTE format('ALTER DATABASE %I RESET track_io_timing', current_database());
        EXECUTE format('ALTER DATABASE %I RESET pg_stat_statements.track', current_database());
    EXCEPTION WHEN OTHERS THEN
        RAISE NOTICE 'database telemetry settings could not be reset by this role: %', SQLERRM;
    END;
END $$;
