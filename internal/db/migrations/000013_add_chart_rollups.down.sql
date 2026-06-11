DROP TRIGGER IF EXISTS trg_blob_chart_rollups_delete ON blobs;
DROP TRIGGER IF EXISTS trg_blob_chart_rollups_update ON blobs;
DROP TRIGGER IF EXISTS trg_blob_chart_rollups_insert ON blobs;

DROP TRIGGER IF EXISTS trg_block_metrics_rollups_delete ON block_metrics;
DROP TRIGGER IF EXISTS trg_block_metrics_rollups_update ON block_metrics;
DROP TRIGGER IF EXISTS trg_block_metrics_rollups_insert ON block_metrics;

DROP FUNCTION IF EXISTS blob_chart_rollups_update_statement_trigger();
DROP FUNCTION IF EXISTS blob_chart_rollups_delete_statement_trigger();
DROP FUNCTION IF EXISTS blob_chart_rollups_insert_statement_trigger();
DROP FUNCTION IF EXISTS blob_chart_rollups_refresh(INTEGER, INTEGER, TIMESTAMP, TEXT);

DROP FUNCTION IF EXISTS block_metrics_rollups_update_statement_trigger();
DROP FUNCTION IF EXISTS block_metrics_rollups_delete_statement_trigger();
DROP FUNCTION IF EXISTS block_metrics_rollups_insert_statement_trigger();
DROP FUNCTION IF EXISTS block_metrics_rollups_refresh(INTEGER, INTEGER, TIMESTAMP);

DROP TABLE IF EXISTS blob_chart_rollups;
DROP TABLE IF EXISTS block_metrics_rollups;

DROP FUNCTION IF EXISTS chart_rollup_bucket_seconds();
DROP FUNCTION IF EXISTS chart_rollup_bucket_start(TIMESTAMP, INTEGER);
