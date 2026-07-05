-- Move pending (mempool) blobs out of the hot blobs table into a dedicated
-- mempool_blobs table.
--
-- Pending rows previously lived in blobs under the block_number < 0 sentinel,
-- which meant every mempool insert/re-poll UPDATE/promotion DELETE churned the
-- statement-level analytical triggers on blobs and deposited dead tuples at
-- the DESC-hot end of the confirmed-read indexes. With this table:
--   * blobs holds confirmed rows only (append-only plus reorg deletes), and
--     blob_user_stats now counts confirmed blobs only — pending rows no longer
--     flow through any trigger;
--   * mempool reads are a trivial small-table scan;
--   * promotion is DELETE FROM mempool_blobs + the existing confirmed INSERT
--     in the same transaction.
--
-- The table is UNLOGGED: its contents are reconstructible from the node's
-- mempool within seconds (WS pending-tx subscription or the 30s poll), so
-- losing it on crash recovery costs nothing and skipping WAL keeps the
-- high-churn writes cheap. blob_index is the per-transaction blob ordinal
-- (0..N-1), not the old pool-wide running counter.
--
-- DDL only, idempotent, no explicit transaction control — see README.md.

CREATE UNLOGGED TABLE IF NOT EXISTS mempool_blobs (
    chain_id INTEGER NOT NULL,
    tx_hash TEXT NOT NULL,
    blob_index INTEGER NOT NULL,
    from_address TEXT NOT NULL,
    user_attribution TEXT,
    blob_size_bytes BIGINT NOT NULL,
    base_fee_per_blob_gas NUMERIC NOT NULL,
    tip_per_blob_gas NUMERIC NOT NULL,
    total_cost_wei NUMERIC NOT NULL,
    timestamp TIMESTAMP NOT NULL,
    max_fee_per_blob_gas NUMERIC,
    blob_gas_used BIGINT,
    PRIMARY KEY (chain_id, tx_hash, blob_index),
    CONSTRAINT fk_mempool_blobs_network_chain_id
        FOREIGN KEY (chain_id) REFERENCES networks(chain_id)
        ON UPDATE RESTRICT ON DELETE RESTRICT
);

-- Serves the timestamp-DESC mempool list/pressure reads and the TTL cleanup.
CREATE INDEX IF NOT EXISTS idx_mempool_blobs_chain_timestamp
    ON mempool_blobs(chain_id, timestamp DESC);

-- Pending rows are transient (mempool TTL) and reconstructible, so drop the
-- old in-blobs rows rather than migrating them; the indexer repopulates
-- mempool_blobs immediately. The set is tiny (bounded by the live mempool),
-- so this is fast. Deleting also purges their contribution from
-- blob_user_stats via the existing delete trigger, aligning that rollup with
-- its new confirmed-only semantics.
DELETE FROM blobs WHERE block_number < 0;

-- The pending-lookup partial index on blobs is dead now.
DROP INDEX IF EXISTS idx_blobs_pending_chain_tx_hash;
