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
-- The GIN lookup index lives in the next migration on purpose. ADD COLUMN
-- takes an ACCESS EXCLUSIVE lock on blobs, and golang-migrate runs each file
-- in a single transaction, so an index build in this file would hold that
-- lock — blocking all blob reads — for the full build. Kept alone, this file
-- is a catalog-only change (nullable columns, no default) and completes in
-- milliseconds.
--
-- DDL only, idempotent, no explicit transaction control — see README.md.

ALTER TABLE blobs ADD COLUMN IF NOT EXISTS versioned_hashes TEXT[];
ALTER TABLE mempool_blobs ADD COLUMN IF NOT EXISTS versioned_hashes TEXT[];
