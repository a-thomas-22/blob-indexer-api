-- WARNING: narrowing back to SMALLINT will fail if any row has a blob_index
-- outside the int16 range. Operators must clean those up first.
ALTER TABLE blobs ALTER COLUMN blob_index TYPE SMALLINT;
