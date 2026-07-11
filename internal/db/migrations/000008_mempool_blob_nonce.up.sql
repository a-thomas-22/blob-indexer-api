-- Track the sender's account nonce on pending blob rows so the indexer can
-- remove superseded rows when a transaction is replaced. A fee-bumped
-- replacement reuses the sender's nonce under a new hash, and the replaced
-- hash never confirms — so before this column the only thing that removed it
-- was the mempool TTL sweep, leaving a phantom pending tx in /blob/mempool
-- for up to mempool_ttl.
--
-- The column is nullable: rows written by a pre-nonce binary during the
-- deploy window stay NULL, never match a (from_address, nonce) cleanup
-- delete, and age out via the TTL sweep exactly as before. No index, for the
-- same reason versioned_hash has none here: the table is tiny (bounded by
-- the live mempool) and UNLOGGED with high write churn, so cleanup deletes
-- scan it via the chain_id prefix of its existing indexes.
--
-- DDL only, idempotent, no explicit transaction control — see README.md.

ALTER TABLE mempool_blobs ADD COLUMN IF NOT EXISTS nonce BIGINT;
