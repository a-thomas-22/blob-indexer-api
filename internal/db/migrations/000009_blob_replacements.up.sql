-- Record each pending blob transaction the indexer evicts as replaced: a
-- fee bump reuses the sender's nonce under a new hash, and once the old rows
-- are deleted from mempool_blobs the observation is gone — the replaced hash
-- never confirms, so nothing else in the system remembers it existed.
--
-- One row per replaced hash, written in the same transaction as the eviction
-- (both in insertPendingBlobs, when the replacement is seen pending, and in
-- insertBlockData, when the replacement confirms without ever being seen).
-- A re-observed replacement upserts: the latest replacement hash wins.
--
-- Unlike mempool_blobs this table is LOGGED: replacement events are not
-- reconstructible from the node once the mempool moves on. Volume is low
-- (fee bumps are occasional), and the indexer prunes rows older than its
-- retention window on the mempool cleanup ticker, so it stays small.
--
-- DDL only, idempotent, no explicit transaction control — see README.md.

CREATE TABLE IF NOT EXISTS blob_replacements (
    chain_id INTEGER NOT NULL,
    replaced_tx_hash TEXT NOT NULL,
    replacement_tx_hash TEXT NOT NULL,
    from_address TEXT NOT NULL,
    nonce BIGINT NOT NULL,
    replaced_at TIMESTAMP NOT NULL,
    PRIMARY KEY (chain_id, replaced_tx_hash),
    CONSTRAINT fk_blob_replacements_network_chain_id
        FOREIGN KEY (chain_id) REFERENCES networks(chain_id)
        ON UPDATE RESTRICT ON DELETE RESTRICT
);

-- Serves the replaced_at-DESC list endpoint and the retention pruning.
CREATE INDEX IF NOT EXISTS idx_blob_replacements_chain_replaced_at
    ON blob_replacements(chain_id, replaced_at DESC);
