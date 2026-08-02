-- Remove the beacon slot column. The value is re-derivable from timestamp +
-- beacon genesis time, so the rollback loses nothing that a re-migration and
-- reindex cannot restore.
ALTER TABLE blobs DROP COLUMN IF EXISTS slot;
