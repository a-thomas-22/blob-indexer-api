-- Evict fee-bumped (replaced) blob transactions from the pending view and
-- remember that they happened. A replacement reuses the sender's nonce under
-- a new hash, and the replaced hash never confirms, so before this migration
-- the only thing that removed its pending rows was the mempool TTL sweep —
-- blob-flow showed a phantom pending tx for up to mempool_ttl — and once the
-- rows were gone, no trace of the replacement existed anywhere.
--
-- mempool_blobs.nonce keys the eviction. It is nullable: rows written by a
-- pre-nonce binary during the deploy window stay NULL, never match a
-- (from_address, nonce) cleanup delete, and age out via the TTL sweep
-- exactly as before. No index, for the same reason versioned_hash has none
-- here: the table is tiny (bounded by the live mempool) and UNLOGGED with
-- high write churn, so cleanup deletes scan it via the chain_id prefix of
-- its existing indexes.
--
-- mempool_blobs.last_seen separates liveness from age: timestamp stays the
-- first-seen instant (API ordering and pending-age metrics), while
-- last_seen is the liveness watermark the TTL sweep reaps on — the indexer
-- bumps it for every tracked tx the node still reports as pending, so the
-- sweep means "the node stopped reporting this tx for mempool_ttl" rather
-- than "first seen more than mempool_ttl ago". Nullable for the same
-- deploy-window reason as nonce; the sweep falls back to timestamp
-- (COALESCE) for NULL rows, which is exactly the old behavior.
--
-- blob_replacements records each eviction: one row per replaced hash,
-- written in the same transaction as the eviction (in insertPendingBlobs
-- when the replacement is seen pending, in insertBlockData when it confirms
-- without ever having been seen). A re-observed replacement upserts: the
-- latest replacement hash wins. Unlike mempool_blobs this table is LOGGED:
-- replacement events are not reconstructible from the node once the mempool
-- moves on. Volume is low (fee bumps are occasional), and the indexer prunes
-- rows older than its retention window on the mempool cleanup ticker, so it
-- stays small.
--
-- DDL only, idempotent, no explicit transaction control — see README.md.

ALTER TABLE mempool_blobs ADD COLUMN IF NOT EXISTS nonce BIGINT;
ALTER TABLE mempool_blobs ADD COLUMN IF NOT EXISTS last_seen TIMESTAMP;

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
