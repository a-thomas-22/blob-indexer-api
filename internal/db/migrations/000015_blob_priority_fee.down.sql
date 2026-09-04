-- Revert the blob transaction execution-fee columns.
--
-- DDL only, idempotent, no explicit transaction control; see README.md.

DROP INDEX IF EXISTS idx_blobs_chain_timestamp_priced_cover;

ALTER TABLE blobs DROP COLUMN IF EXISTS max_priority_fee_per_gas;
ALTER TABLE blobs DROP COLUMN IF EXISTS max_fee_per_gas;
ALTER TABLE blobs DROP COLUMN IF EXISTS priority_fee_per_gas;

ALTER TABLE mempool_blobs DROP COLUMN IF EXISTS max_priority_fee_per_gas;
ALTER TABLE mempool_blobs DROP COLUMN IF EXISTS max_fee_per_gas;
ALTER TABLE mempool_blobs DROP COLUMN IF EXISTS priority_fee_per_gas;
