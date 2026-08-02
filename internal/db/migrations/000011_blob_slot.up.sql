-- Record each confirmed blob's beacon slot so API consumers (blob-flow's
-- BlobArchive integration keys reads by slot) get the authoritative value
-- instead of re-deriving it client-side. The indexer computes it at index
-- time from the block timestamp and the network's beacon genesis time —
-- post-merge consensus makes that derivation exact. Rows indexed before this
-- migration stay NULL and are NOT backfilled in-place: the API derives the
-- slot at read time from the stored timestamp for them, so the wire field is
-- populated either way and the column converges as blocks are (re)indexed.
-- mempool_blobs gets no column: pending blobs have no slot until inclusion.
-- No index: nothing queries by slot.
--
-- DDL only, idempotent, no explicit transaction control — see README.md.

ALTER TABLE blobs ADD COLUMN IF NOT EXISTS slot BIGINT;
