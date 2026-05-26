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
