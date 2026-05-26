-- Backfill for the per-blob row layout. Prior to this migration the indexer
-- collapsed each blob transaction (which may carry up to N blobs) into a
-- single row in the blobs table and recorded block_metrics.blob_count as the
-- number of blob *transactions* rather than blobs. This migration:
--   1. Splits any blobs row whose blob_gas_used exceeds one blob's worth
--      (131072 = params.BlobTxBlobGasPerBlob) into one row per blob.
--   2. Corrects block_metrics.blob_count to count blobs (blob_gas_used / 131072).
--
-- golang-migrate runs each migration file inside its own transaction, so no
-- explicit BEGIN/COMMIT is required here.

-- (1a) Insert the extra rows for each multi-blob source row. The new
-- blob_index values continue from MAX(blob_index) within the block and are
-- distributed deterministically across all multi-blob source rows so the
-- UNIQUE(network_id, block_number, blob_index) constraint is satisfied.
WITH multi_blob AS (
    SELECT
        b.id,
        b.network_id,
        b.block_number,
        b.blob_index,
        b.tx_hash,
        b.from_address,
        b.user_attribution,
        b.base_fee_per_blob_gas,
        b.tip_per_blob_gas,
        b.max_fee_per_blob_gas,
        b.timestamp,
        b.confirmed,
        b.indexer_version,
        (b.blob_gas_used / 131072)::int AS blob_count
    FROM blobs b
    WHERE b.blob_gas_used IS NOT NULL AND b.blob_gas_used > 131072
),
block_max AS (
    SELECT network_id, block_number, MAX(blob_index) AS max_blob_index
    FROM blobs
    GROUP BY network_id, block_number
),
expanded AS (
    SELECT
        m.*,
        bm.max_blob_index + ROW_NUMBER() OVER (
            PARTITION BY m.network_id, m.block_number
            ORDER BY m.blob_index, gs.idx
        ) AS new_blob_index
    FROM multi_blob m
    CROSS JOIN LATERAL generate_series(2, m.blob_count) AS gs(idx)
    JOIN block_max bm
        ON bm.network_id = m.network_id
       AND bm.block_number = m.block_number
)
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
FROM expanded;

-- (1b) Rewrite the original (now blob 0) row so its per-blob fields are correct.
UPDATE blobs
SET blob_gas_used = 131072,
    blob_size_bytes = 131072,
    total_cost_eth = base_fee_per_blob_gas * 131072
WHERE blob_gas_used IS NOT NULL AND blob_gas_used > 131072;

-- (2) Correct block_metrics.blob_count for every confirmed block that has any
-- blobs. Where blob_gas_used is zero we leave the row alone.
UPDATE block_metrics
SET blob_count = (blob_gas_used / 131072)::smallint
WHERE blob_gas_used > 0
  AND blob_count <> (blob_gas_used / 131072)::smallint;
