-- Record each blob's EIP-4844 versioned hash (0x01 || SHA256(KZG commitment)[1:])
-- so /api/v1/search can resolve a 64-hex query as a blob hash and disambiguate
-- it from a transaction hash. The hash is only observable from the transaction
-- at index time, so rows indexed before this migration stay NULL until their
-- blocks are reindexed; they simply never match a versioned-hash search. The
-- lookup index is partial for that reason — it stays empty at creation and only
-- grows with newly indexed blobs. mempool_blobs gets the column but no index:
-- the table is tiny (bounded by the live mempool) and UNLOGGED with high write
-- churn, so search scans it via the chain_id prefix of its existing indexes.
--
-- Locking: the partial predicate keeps the index EMPTY at build time (the
-- column was just added, every row is NULL), but CREATE INDEX still scans the
-- whole blobs heap under a SHARE lock, blocking indexer writes for the scan —
-- minutes at full historical scale (~48M rows) on slow disk. Deploy this soon
-- after the 2026-07-05 DB reset while blobs is still small. If the table has
-- re-grown by deploy time, pre-run both statements out-of-band first
-- (ALTER TABLE ... ADD COLUMN IF NOT EXISTS, then CREATE INDEX CONCURRENTLY
-- IF NOT EXISTS with the same name/definition); this migration then no-ops.
--
-- DDL only, idempotent, no explicit transaction control — see README.md.

ALTER TABLE blobs ADD COLUMN IF NOT EXISTS versioned_hash TEXT;
ALTER TABLE mempool_blobs ADD COLUMN IF NOT EXISTS versioned_hash TEXT;

CREATE INDEX IF NOT EXISTS idx_blobs_chain_versioned_hash
    ON blobs(chain_id, versioned_hash)
    WHERE versioned_hash IS NOT NULL;
