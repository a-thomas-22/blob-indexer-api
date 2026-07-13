-- Drop the replacement machinery: the event log and the pending-row nonce.
-- Eviction and recording degrade back to the TTL sweep; an old binary
-- ignores both either way.
--
-- DDL only, idempotent, no explicit transaction control — see README.md.

DROP TABLE IF EXISTS blob_replacements;
ALTER TABLE mempool_blobs DROP COLUMN IF EXISTS nonce;
