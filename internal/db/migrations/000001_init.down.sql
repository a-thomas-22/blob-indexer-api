-- Teardown for the consolidated baseline. Dropping the tables CASCADE removes
-- their triggers and indexes; the trigger functions are dropped explicitly.

DROP TABLE IF EXISTS blob_chart_rollups CASCADE;
DROP TABLE IF EXISTS block_metrics_rollups CASCADE;
DROP TABLE IF EXISTS blob_user_stats CASCADE;
DROP TABLE IF EXISTS network_blob_stats CASCADE;
DROP TABLE IF EXISTS block_reindex_requests CASCADE;
DROP TABLE IF EXISTS blob_attribution_claims CASCADE;
DROP TABLE IF EXISTS block_metrics CASCADE;
DROP TABLE IF EXISTS indexed_blocks CASCADE;
DROP TABLE IF EXISTS indexer_metadata CASCADE;
DROP TABLE IF EXISTS blob_users CASCADE;
DROP TABLE IF EXISTS blobs CASCADE;
DROP TABLE IF EXISTS networks CASCADE;

DROP FUNCTION IF EXISTS blob_chart_rollups_update_statement_trigger();
DROP FUNCTION IF EXISTS blob_chart_rollups_delete_statement_trigger();
DROP FUNCTION IF EXISTS blob_chart_rollups_insert_statement_trigger();
DROP FUNCTION IF EXISTS blob_chart_rollups_refresh(INTEGER, INTEGER, TIMESTAMP, TEXT);
DROP FUNCTION IF EXISTS block_metrics_rollups_update_statement_trigger();
DROP FUNCTION IF EXISTS block_metrics_rollups_delete_statement_trigger();
DROP FUNCTION IF EXISTS block_metrics_rollups_insert_statement_trigger();
DROP FUNCTION IF EXISTS block_metrics_rollups_refresh(INTEGER, INTEGER, TIMESTAMP);
DROP FUNCTION IF EXISTS chart_rollup_bucket_seconds();
DROP FUNCTION IF EXISTS chart_rollup_bucket_start(TIMESTAMP, INTEGER);
DROP FUNCTION IF EXISTS blob_user_stats_blobs_update_statement_trigger();
DROP FUNCTION IF EXISTS blob_user_stats_blobs_delete_statement_trigger();
DROP FUNCTION IF EXISTS blob_user_stats_blobs_insert_statement_trigger();
DROP FUNCTION IF EXISTS blob_user_stats_apply_insert_delta(INTEGER, TEXT, TEXT, BIGINT, NUMERIC, TIMESTAMP);
DROP FUNCTION IF EXISTS blob_user_stats_refresh(INTEGER, TEXT);
DROP FUNCTION IF EXISTS network_blob_stats_block_metrics_delete_trigger();
DROP FUNCTION IF EXISTS network_blob_stats_block_metrics_update_trigger();
DROP FUNCTION IF EXISTS network_blob_stats_block_metrics_insert_trigger();
DROP FUNCTION IF EXISTS network_blob_stats_refresh_latest(INTEGER);
DROP FUNCTION IF EXISTS network_blob_stats_blobs_update_trigger();
DROP FUNCTION IF EXISTS network_blob_stats_blobs_delete_trigger();
DROP FUNCTION IF EXISTS network_blob_stats_blobs_insert_trigger();
DROP FUNCTION IF EXISTS network_blob_stats_apply_delta(INTEGER, BIGINT, NUMERIC, NUMERIC, NUMERIC);
