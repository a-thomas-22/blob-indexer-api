-- Store each blob transaction's EIP-4844 versioned blob hashes (one
-- 0x01-prefixed 32-byte hash per blob) so the API can answer "which
-- transaction carries blob X" lookups and expose the hashes on blob
-- responses.
--
-- The column holds the transaction's full ordered hash list, denormalized
-- onto every per-blob row (mirroring user_attribution): list endpoints read
-- it straight off whatever row they already fetch, with no per-row
-- aggregation over the transaction's sibling rows. Rows indexed before this
-- migration stay NULL — the API omits the field for them — and pick up
-- values only on reindex.
--
-- DDL only, idempotent, no explicit transaction control — see README.md.

ALTER TABLE blobs ADD COLUMN IF NOT EXISTS versioned_hashes TEXT[];
ALTER TABLE mempool_blobs ADD COLUMN IF NOT EXISTS versioned_hashes TEXT[];

-- Serves the /blob/by-hash containment lookup
-- (versioned_hashes @> ARRAY[$1]). mempool_blobs stays unindexed: it is a
-- tiny UNLOGGED table whose seq scan is cheaper than maintaining a GIN index
-- under its constant churn.
CREATE INDEX IF NOT EXISTS idx_blobs_versioned_hashes
    ON blobs USING GIN (versioned_hashes);
