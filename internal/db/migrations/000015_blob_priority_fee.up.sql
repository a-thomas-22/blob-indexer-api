-- Record the execution-layer (EIP-1559) fees of each blob transaction.
--
-- Blob transactions compete for a block's few blob slots, and builders order
-- them by the priority fee they pay on execution gas, not by anything in the
-- blob fee market. tip_per_blob_gas is max_fee_per_blob_gas minus the blob
-- base fee (fee-cap headroom), which is never paid and says nothing about
-- inclusion priority; so when one rollup outbids the others and crowds their
-- batches out of blocks, nothing recorded so far shows it. These columns do:
--   * max_priority_fee_per_gas: the transaction's priority fee cap (tip cap);
--   * max_fee_per_gas: the transaction's total fee cap;
--   * priority_fee_per_gas: the priority fee actually paid per gas,
--     min(tip cap, fee cap - block base fee), known only once included, so
--     NULL on mempool_blobs.
-- All three are observable only from the transaction at index time, so rows
-- indexed before this migration stay NULL until their blocks are reindexed;
-- readers treat NULL as "not recorded" rather than zero.
--
-- The partial covering index serves /charts/blob-tips, which aggregates
-- priced rows over a time range directly from blobs (no rollup carries the
-- fee). Restricting it to priced rows keeps it EMPTY at creation, since the
-- column was just added and every row is NULL, and it only grows as new
-- blocks and reindexed history land; the INCLUDE list is exactly what the
-- query reads, so the scan is index-only once the visibility map catches up.
--
-- Locking: CREATE INDEX still scans the whole blobs heap under a SHARE lock
-- even though the result is empty, blocking indexer writes for the scan
-- (roughly a minute or two at ~22M rows). If that pause is unacceptable at
-- deploy time, pre-run the ALTER TABLE statements and then CREATE INDEX
-- CONCURRENTLY IF NOT EXISTS with the same name and definition out-of-band;
-- this migration then no-ops.
--
-- Nullable adds without defaults are metadata-only (no table rewrite). DDL
-- only, idempotent, no explicit transaction control; see README.md.

ALTER TABLE blobs ADD COLUMN IF NOT EXISTS max_priority_fee_per_gas NUMERIC;
ALTER TABLE blobs ADD COLUMN IF NOT EXISTS max_fee_per_gas NUMERIC;
ALTER TABLE blobs ADD COLUMN IF NOT EXISTS priority_fee_per_gas NUMERIC;

ALTER TABLE mempool_blobs ADD COLUMN IF NOT EXISTS max_priority_fee_per_gas NUMERIC;
ALTER TABLE mempool_blobs ADD COLUMN IF NOT EXISTS max_fee_per_gas NUMERIC;
ALTER TABLE mempool_blobs ADD COLUMN IF NOT EXISTS priority_fee_per_gas NUMERIC;

CREATE INDEX IF NOT EXISTS idx_blobs_chain_timestamp_priced_cover
    ON blobs(chain_id, timestamp DESC)
    INCLUDE (from_address, user_attribution, priority_fee_per_gas)
    WHERE priority_fee_per_gas IS NOT NULL;
