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

-- The indexer's checkpoints describe rows this migration just dropped. Left
-- behind, a roll forward would resume the builder backfill below its floor
-- (or, for the oldest-first checkpoint earlier releases kept, past the tip)
-- and never refill the recreated table, and the relabel pass would skip the
-- rows it never wrote. Deleting them makes down-then-up start over.
DELETE FROM indexer_metadata
WHERE key IN ('block_builder_backfill_floor', 'block_builder_backfill_block', 'block_builder_registry_version');
