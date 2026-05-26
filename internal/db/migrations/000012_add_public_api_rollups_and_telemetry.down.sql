DROP TRIGGER IF EXISTS trg_blob_user_stats_blobs_update ON blobs;
DROP TRIGGER IF EXISTS trg_blob_user_stats_blobs_delete ON blobs;
DROP TRIGGER IF EXISTS trg_blob_user_stats_blobs ON blobs;

DROP FUNCTION IF EXISTS blob_user_stats_blobs_update_statement_trigger();
DROP FUNCTION IF EXISTS blob_user_stats_blobs_delete_statement_trigger();
DROP FUNCTION IF EXISTS blob_user_stats_blobs_insert_statement_trigger();
DROP FUNCTION IF EXISTS blob_user_stats_apply_insert_delta(INTEGER, TEXT, TEXT, BIGINT, NUMERIC, TIMESTAMP);
DROP FUNCTION IF EXISTS blob_user_stats_refresh(INTEGER, TEXT);

CREATE INDEX IF NOT EXISTS idx_block_metrics_network_timestamp
    ON block_metrics(network_id, block_timestamp DESC);

CREATE INDEX IF NOT EXISTS idx_blobs_network_from_address
    ON blobs(network_id, from_address);

DROP INDEX IF EXISTS idx_block_metrics_network_timestamp_cover;
DROP INDEX IF EXISTS idx_blobs_network_confirmed_timestamp_cover;
DROP INDEX IF EXISTS idx_blobs_network_from_timestamp;

DROP INDEX IF EXISTS idx_blob_user_stats_network_spend;
DROP INDEX IF EXISTS idx_blob_user_stats_network_count;
DROP TABLE IF EXISTS blob_user_stats;
