-- Revert 000006: drop the block_metrics NOTIFY trigger and its function.

DROP TRIGGER IF EXISTS block_metrics_notify_new_block_trigger ON block_metrics;
DROP FUNCTION IF EXISTS block_metrics_notify_new_block();
