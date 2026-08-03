-- Reverse 000013_blob_records.up.sql. Dropping the streak table loses only
-- derived state: the indexer's startup backfill rebuilds it from block_metrics
-- if the migration is re-applied.

DROP TRIGGER IF EXISTS trg_blob_block_streaks_insert ON block_metrics;
DROP TRIGGER IF EXISTS trg_blob_block_streaks_update ON block_metrics;
DROP TRIGGER IF EXISTS trg_blob_block_streaks_delete ON block_metrics;

DROP FUNCTION IF EXISTS blob_block_streaks_insert_statement_trigger();
DROP FUNCTION IF EXISTS blob_block_streaks_update_statement_trigger();
DROP FUNCTION IF EXISTS blob_block_streaks_delete_statement_trigger();
DROP FUNCTION IF EXISTS blob_block_streaks_recompute_all(INTEGER, BIGINT, BIGINT);
DROP FUNCTION IF EXISTS blob_block_streaks_recompute(INTEGER, TEXT, BIGINT, BIGINT);
DROP FUNCTION IF EXISTS blob_record_streak_kinds();
DROP FUNCTION IF EXISTS blob_record_target_blobs(INTEGER, BIGINT);
DROP FUNCTION IF EXISTS blob_record_max_blobs(INTEGER, BIGINT);

DROP INDEX IF EXISTS idx_block_metrics_rollups_hourly_blob_count;
DROP INDEX IF EXISTS idx_block_metrics_chain_blob_base_fee;
DROP INDEX IF EXISTS idx_blob_block_streaks_chain_kind_length;

DROP TABLE IF EXISTS blob_block_streaks;
