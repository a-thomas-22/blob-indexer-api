-- Revert block builder attribution and blob inclusion timing.
--
-- DDL only, idempotent, no explicit transaction control; see README.md.

ALTER TABLE blob_replacements DROP COLUMN IF EXISTS replacement_max_fee_per_blob_gas;
ALTER TABLE blob_replacements DROP COLUMN IF EXISTS replacement_max_priority_fee_per_gas;
ALTER TABLE blob_replacements DROP COLUMN IF EXISTS replaced_first_seen_at;
ALTER TABLE blob_replacements DROP COLUMN IF EXISTS replaced_max_fee_per_blob_gas;
ALTER TABLE blob_replacements DROP COLUMN IF EXISTS replaced_max_priority_fee_per_gas;

ALTER TABLE blobs DROP COLUMN IF EXISTS tx_index;
ALTER TABLE blobs DROP COLUMN IF EXISTS first_seen_at;

DROP INDEX IF EXISTS idx_blob_inclusion_candidates_chain_timestamp;
DROP TABLE IF EXISTS blob_inclusion_candidates;

DROP INDEX IF EXISTS idx_block_builders_chain_key_timestamp;
DROP INDEX IF EXISTS idx_block_builders_chain_timestamp;
DROP TABLE IF EXISTS block_builders;
