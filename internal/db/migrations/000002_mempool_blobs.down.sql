-- Revert the dedicated mempool table. Pending rows are transient and
-- reconstructible from the node's mempool, so they are dropped rather than
-- copied back into blobs; an indexer running the old binary repopulates the
-- block_number < 0 sentinel rows on its next mempool poll.

DROP TABLE IF EXISTS mempool_blobs;

CREATE INDEX IF NOT EXISTS idx_blobs_pending_chain_tx_hash
    ON blobs(chain_id, tx_hash) WHERE block_number < 0;
