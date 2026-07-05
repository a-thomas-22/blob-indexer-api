-- Dropping the columns also drops any dependent index, so this stays safe
-- even if the index migration's down was skipped.
ALTER TABLE blobs DROP COLUMN IF EXISTS versioned_hashes;
ALTER TABLE mempool_blobs DROP COLUMN IF EXISTS versioned_hashes;
