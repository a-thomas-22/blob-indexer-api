-- A blob transaction can carry up to 6 (or more under post-Pectra params) blobs.
-- The previous partial unique index allowed only one pending row per tx, which
-- forced the indexer to collapse multi-blob txs into a single row. Replace the
-- unique partial index with a non-unique partial index so the mempool path can
-- store one row per blob.
DROP INDEX IF EXISTS idx_blobs_network_id_tx_hash;

CREATE INDEX IF NOT EXISTS idx_blobs_pending_network_tx_hash
    ON blobs(network_id, tx_hash)
    WHERE block_number < 0;
