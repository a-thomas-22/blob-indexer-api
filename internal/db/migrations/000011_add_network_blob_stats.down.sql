DROP TRIGGER IF EXISTS trg_network_blob_stats_block_metrics_delete ON block_metrics;
DROP TRIGGER IF EXISTS trg_network_blob_stats_block_metrics_update ON block_metrics;
DROP TRIGGER IF EXISTS trg_network_blob_stats_block_metrics_insert ON block_metrics;
DROP TRIGGER IF EXISTS trg_network_blob_stats_blobs_delete ON blobs;
DROP TRIGGER IF EXISTS trg_network_blob_stats_blobs_update ON blobs;
DROP TRIGGER IF EXISTS trg_network_blob_stats_blobs_insert ON blobs;

DROP FUNCTION IF EXISTS network_blob_stats_block_metrics_delete_trigger();
DROP FUNCTION IF EXISTS network_blob_stats_block_metrics_update_trigger();
DROP FUNCTION IF EXISTS network_blob_stats_block_metrics_insert_trigger();
DROP FUNCTION IF EXISTS network_blob_stats_refresh_latest(INTEGER);
DROP FUNCTION IF EXISTS network_blob_stats_blobs_update_trigger();
DROP FUNCTION IF EXISTS network_blob_stats_blobs_delete_trigger();
DROP FUNCTION IF EXISTS network_blob_stats_blobs_insert_trigger();
DROP FUNCTION IF EXISTS network_blob_stats_apply_delta(INTEGER, BIGINT, NUMERIC, NUMERIC, NUMERIC);

DROP TABLE IF EXISTS network_blob_stats;
