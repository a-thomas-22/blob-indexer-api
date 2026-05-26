DROP TRIGGER IF EXISTS trg_network_blob_stats_block_metrics ON block_metrics;
DROP TRIGGER IF EXISTS trg_network_blob_stats_blobs ON blobs;

DROP FUNCTION IF EXISTS network_blob_stats_block_metrics_trigger();
DROP FUNCTION IF EXISTS network_blob_stats_refresh_latest(INTEGER);
DROP FUNCTION IF EXISTS network_blob_stats_blobs_trigger();
DROP FUNCTION IF EXISTS network_blob_stats_apply_delta(INTEGER, BIGINT, NUMERIC, NUMERIC, NUMERIC);

DROP TABLE IF EXISTS network_blob_stats;
