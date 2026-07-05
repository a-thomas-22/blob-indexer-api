DROP INDEX IF EXISTS idx_blobs_versioned_hashes;
ALTER TABLE blobs DROP COLUMN IF EXISTS versioned_hashes;
ALTER TABLE mempool_blobs DROP COLUMN IF EXISTS versioned_hashes;
