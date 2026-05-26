-- Restore the unique pending index. NOTE: this fails if the table contains
-- multiple pending rows per (network_id, tx_hash); operators must reconcile
-- pending data before rolling back.
DROP INDEX IF EXISTS idx_blobs_pending_network_tx_hash;

CREATE UNIQUE INDEX IF NOT EXISTS idx_blobs_network_id_tx_hash
    ON blobs(network_id, tx_hash)
    WHERE block_number < 0;
