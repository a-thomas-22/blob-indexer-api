-- Backfill for the per-blob row layout. Prior to this migration the indexer
-- collapsed each blob transaction (which may carry several blobs) into a
-- single row in the blobs table, and recorded block_metrics.blob_count as the
-- number of blob *transactions* rather than blobs.
--
-- This migration rewrites every row of every affected block (blocks where at
-- least one row was a collapsed multi-blob record) so that blob_index values
-- are contiguous 0..N-1 and ordered by the source tx's original position then
-- by blob ordinal within the tx. That matches the invariant the runtime
-- indexer now produces and is the order /api/v1/blob/* consumers expect when
-- sorting by (block_number, blob_index).
--
-- Strategy:
--   1. Build a temp table of every blob row that *should* exist in any
--      affected block, with a freshly-assigned contiguous blob_index.
--   2. Delete the legacy rows in those blocks.
--   3. Insert the temp table back into blobs.
--   4. Correct block_metrics.blob_count for every block (blob_gas_used / 131072).
--
-- golang-migrate wraps the file in a transaction, so all four steps commit
-- together or not at all.

-- (1) Materialise the new shape of every affected block.
CREATE TEMPORARY TABLE _backfill_rows ON COMMIT DROP AS
WITH legacy AS (
    -- One row per legacy blobs row in any confirmed block. Pending rows
    -- (block_number < 0) are excluded — they will be re-detected by the
    -- mempool poller after the fix lands.
    SELECT
        b.network_id,
        b.block_number,
        b.blob_index AS legacy_blob_index,
        b.tx_hash,
        b.from_address,
        b.user_attribution,
        b.base_fee_per_blob_gas,
        b.tip_per_blob_gas,
        b.max_fee_per_blob_gas,
        b.timestamp,
        b.confirmed,
        b.indexer_version,
        GREATEST(COALESCE(b.blob_gas_used, 131072) / 131072, 1)::int AS blob_count
    FROM blobs b
    WHERE b.confirmed = true AND b.block_number >= 0
),
affected_blocks AS (
    -- Only blocks that had at least one collapsed multi-blob row need
    -- rewriting. Untouched blocks already have one row per blob with the
    -- correct blob_index.
    SELECT DISTINCT network_id, block_number
    FROM legacy
    WHERE blob_count > 1
),
expanded AS (
    -- Cross-join each legacy row with generate_series(1, blob_count) to
    -- produce one expanded row per blob. Within each (network_id,
    -- block_number) partition, order by the legacy_blob_index (which was
    -- the source tx's position in the block) and then by gs.idx (the
    -- blob's ordinal within the tx). ROW_NUMBER - 1 gives contiguous
    -- 0..N-1 indices that match the runtime convention.
    SELECT
        l.network_id,
        l.block_number,
        l.tx_hash,
        l.from_address,
        l.user_attribution,
        l.base_fee_per_blob_gas,
        l.tip_per_blob_gas,
        l.max_fee_per_blob_gas,
        l.timestamp,
        l.confirmed,
        l.indexer_version,
        (ROW_NUMBER() OVER (
            PARTITION BY l.network_id, l.block_number
            ORDER BY l.legacy_blob_index, gs.idx
        ) - 1)::int AS new_blob_index
    FROM legacy l
    JOIN affected_blocks ab
      ON ab.network_id = l.network_id
     AND ab.block_number = l.block_number
    CROSS JOIN LATERAL generate_series(1, l.blob_count) AS gs(idx)
)
SELECT * FROM expanded;

-- (2) Delete the legacy rows in affected blocks. We touch only confirmed rows
--     so pending rows are unaffected (their indices live in block_number < 0,
--     a disjoint key space).
DELETE FROM blobs
WHERE confirmed = true
  AND block_number >= 0
  AND (network_id, block_number) IN (
      SELECT DISTINCT network_id, block_number FROM _backfill_rows
  );

-- (3) Insert the rewritten rows.
INSERT INTO blobs (
    network_id, block_number, blob_index, tx_hash, from_address, user_attribution,
    blob_size_bytes, base_fee_per_blob_gas, tip_per_blob_gas, total_cost_eth,
    timestamp, confirmed, indexer_version, max_fee_per_blob_gas, blob_gas_used
)
SELECT
    network_id,
    block_number,
    new_blob_index,
    tx_hash,
    from_address,
    user_attribution,
    131072,
    base_fee_per_blob_gas,
    tip_per_blob_gas,
    base_fee_per_blob_gas * 131072,
    timestamp,
    confirmed,
    indexer_version,
    max_fee_per_blob_gas,
    131072
FROM _backfill_rows;

-- (4) Correct block_metrics.blob_count for every block that has any blobs.
--     This is independent of the blobs rewrite — block_metrics was wrong even
--     in blocks where every blob tx happened to carry exactly one blob, just
--     because it counted tx-rows rather than blob-rows. blob_gas_used (from
--     the chain header) is authoritative.
UPDATE block_metrics
SET blob_count = (blob_gas_used / 131072)::smallint
WHERE blob_gas_used > 0
  AND blob_count <> (blob_gas_used / 131072)::smallint;
