-- Drop the pending-blob nonce column. Replacement cleanup degrades back to
-- the TTL sweep; an old binary ignores the column either way.
--
-- DDL only, idempotent, no explicit transaction control — see README.md.

ALTER TABLE mempool_blobs DROP COLUMN IF EXISTS nonce;
