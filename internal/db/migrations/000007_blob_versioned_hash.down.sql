-- Revert the blob versioned-hash storage added for /api/v1/search.
--
-- DDL only, idempotent, no explicit transaction control — see README.md.

DROP INDEX IF EXISTS idx_blobs_chain_versioned_hash;

ALTER TABLE blobs DROP COLUMN IF EXISTS versioned_hash;
ALTER TABLE mempool_blobs DROP COLUMN IF EXISTS versioned_hash;
