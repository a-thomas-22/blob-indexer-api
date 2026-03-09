DROP INDEX IF EXISTS idx_block_metrics_network_block;
DROP INDEX IF EXISTS idx_block_metrics_network_timestamp;

ALTER TABLE blobs DROP COLUMN IF EXISTS blob_gas_used;
ALTER TABLE blobs DROP COLUMN IF EXISTS max_fee_per_blob_gas;

DROP TABLE IF EXISTS block_metrics;
