-- Block builder attribution and blob inclusion timing.
--
-- Nothing indexed so far identifies who built a block. The indexer fetches
-- the full block, so the header fields that label the builder (fee recipient
-- and extra data) are in hand at index time and only need persisting. With
-- them, every confirmed blob can be attributed to the builder that included
-- it, which answers which builders include which senders and at what tip.
--
-- block_builders holds one row per indexed block:
--   * fee_recipient: header.coinbase. Under MEV-Boost this is usually the
--     builder's own address (the proposer is paid by the block's last tx),
--     for locally built blocks it is the proposer's fee recipient, so it is
--     ambiguous on its own.
--   * extra_data: header.extraData as 0x-hex. Major builders stamp a label
--     here ("Titan (titanbuilder.xyz)", "beaverbuild.org", ...); this is the
--     primary attribution signal.
--   * builder_key / builder_name: the grouping key and display name the
--     indexer's builder registry resolved from the two raw fields at index
--     time. Never NULL: unknown builders fall back to a key derived from the
--     printable extra data, or from the fee recipient when extra data is
--     empty, so grouping works before a builder is registered. A registry
--     change relabels rows in place (raw fields stay authoritative).
--   * proposer_payment_wei / proposer_payment_to: the value and recipient of
--     the block's last transaction when it is sent by the fee recipient, the
--     conventional MEV-Boost proposer payment. NULL when the last tx is not
--     from the fee recipient. Heuristic: some builders pay the proposer by
--     setting coinbase to the proposer's fee recipient instead.
--   * candidate snapshot aggregates (candidate_snapshot and the
--     eligible_skipped_* columns): what our node's pending blob pool looked
--     like when this block arrived, summarized permanently so the per-tx
--     detail in blob_inclusion_candidates can be pruned. candidate_snapshot
--     is FALSE for blocks indexed from history (the pool is meaningless for
--     a block that arrived long ago), and the aggregates are then NULL.
--
-- blob_inclusion_candidates is the per-transaction detail behind those
-- aggregates: every pending blob transaction our node had seen when a live
-- block arrived and that the block did not include, with the reason it is
-- classified under. Only reason = 'eligible' rows support a claim about
-- builder behavior; the other reasons explain why inclusion was impossible
-- or unlikely regardless of the builder:
--   too_recent          first seen less than candidate_min_age before the
--                       block's timestamp (slot start), so the builder may
--                       not have had it
--   nonce_gap           a lower-nonce pending blob tx from the same sender
--                       was also pending, so this one could not go first
--   priced_out_blob_fee max_fee_per_blob_gas below the block's blob base fee
--   priced_out_exec_fee max_fee_per_gas below the block's execution base fee
--   no_room             the block's remaining blob capacity could not hold
--                       this transaction's blobs
--   eligible            none of the above
-- Precedence when several apply is the order listed. The table is LOGGED
-- (the pool moves on, so rows cannot be reconstructed) and the indexer
-- prunes rows older than its retention window on the mempool cleanup
-- ticker, like blob_replacements.
--
-- Our node's mempool is not the builder's: private order flow and slow blob
-- propagation mean a candidate we saw may never have reached the builder.
-- candidate_min_age is the conservative floor; API consumers should present
-- these counts as "visible to our node and not included".
--
-- blobs gains two nullable columns:
--   * first_seen_at: the instant the indexer first saw the transaction
--     pending, copied from mempool_blobs.timestamp when the block promotes
--     the row. NULL when the tx was never seen pending (indexed from
--     history, or arrived with its block). block timestamp minus this is
--     the time to inclusion.
--   * tx_index: the transaction's position in its block, for ordering
--     analysis (where builders place blob transactions, and whether they
--     sort them by tip). NULL for rows indexed before this migration until
--     the priority fee backfill or a reindex revisits the block.
-- Neither column feeds any aggregate; the 000016 trigger guards compare an
-- explicit column list that excludes them, so backfilling them in place
-- stays a no-op for the rollup triggers.
--
-- blob_replacements gains the fee context of both sides of a fee bump, so
-- the log shows how much senders raise when they get stuck, plus the
-- replaced transaction's first-seen time. NULL on rows written before this
-- migration.
--
-- Nullable adds without defaults are metadata-only (no table rewrite).
-- The new tables start empty, so their indexes are instant. DDL only,
-- idempotent, no explicit transaction control; see README.md.

CREATE TABLE IF NOT EXISTS block_builders (
    chain_id                    INTEGER NOT NULL,
    block_number                BIGINT NOT NULL,
    block_timestamp             TIMESTAMP NOT NULL,
    fee_recipient               TEXT NOT NULL,
    extra_data                  TEXT NOT NULL,
    builder_key                 TEXT NOT NULL,
    builder_name                TEXT NOT NULL,
    tx_count                    INTEGER NOT NULL,
    proposer_payment_wei        NUMERIC,
    proposer_payment_to         TEXT,
    candidate_snapshot          BOOLEAN NOT NULL DEFAULT FALSE,
    pending_candidate_txs       INTEGER,
    eligible_skipped_txs        INTEGER,
    eligible_skipped_blobs      INTEGER,
    eligible_skipped_max_tip    NUMERIC,
    PRIMARY KEY (chain_id, block_number),
    CONSTRAINT fk_block_builders_network_chain_id
        FOREIGN KEY (chain_id) REFERENCES networks(chain_id)
        ON UPDATE RESTRICT ON DELETE RESTRICT
);

-- Range scans for the builder leaderboards and charts.
CREATE INDEX IF NOT EXISTS idx_block_builders_chain_timestamp
    ON block_builders(chain_id, block_timestamp DESC);

-- Per-builder detail pages and the relabel pass (which groups by the raw
-- fields to find rows a registry change affects).
CREATE INDEX IF NOT EXISTS idx_block_builders_chain_key_timestamp
    ON block_builders(chain_id, builder_key, block_timestamp DESC);

CREATE TABLE IF NOT EXISTS blob_inclusion_candidates (
    chain_id                    INTEGER NOT NULL,
    block_number                BIGINT NOT NULL,
    block_timestamp             TIMESTAMP NOT NULL,
    tx_hash                     TEXT NOT NULL,
    from_address                TEXT NOT NULL,
    user_attribution            TEXT,
    nonce                       BIGINT,
    blob_count                  INTEGER NOT NULL,
    max_priority_fee_per_gas    NUMERIC,
    max_fee_per_gas             NUMERIC,
    max_fee_per_blob_gas        NUMERIC,
    first_seen_at               TIMESTAMP NOT NULL,
    reason                      TEXT NOT NULL CHECK (reason IN (
                                    'eligible', 'too_recent', 'nonce_gap',
                                    'priced_out_blob_fee', 'priced_out_exec_fee', 'no_room')),
    PRIMARY KEY (chain_id, block_number, tx_hash),
    CONSTRAINT fk_blob_inclusion_candidates_network_chain_id
        FOREIGN KEY (chain_id) REFERENCES networks(chain_id)
        ON UPDATE RESTRICT ON DELETE RESTRICT
);

-- Serves the retention prune and time-range reads.
CREATE INDEX IF NOT EXISTS idx_blob_inclusion_candidates_chain_timestamp
    ON blob_inclusion_candidates(chain_id, block_timestamp DESC);

ALTER TABLE blobs ADD COLUMN IF NOT EXISTS first_seen_at TIMESTAMP;
ALTER TABLE blobs ADD COLUMN IF NOT EXISTS tx_index INTEGER;

ALTER TABLE blob_replacements ADD COLUMN IF NOT EXISTS replaced_max_priority_fee_per_gas NUMERIC;
ALTER TABLE blob_replacements ADD COLUMN IF NOT EXISTS replaced_max_fee_per_blob_gas NUMERIC;
ALTER TABLE blob_replacements ADD COLUMN IF NOT EXISTS replaced_first_seen_at TIMESTAMP;
ALTER TABLE blob_replacements ADD COLUMN IF NOT EXISTS replacement_max_priority_fee_per_gas NUMERIC;
ALTER TABLE blob_replacements ADD COLUMN IF NOT EXISTS replacement_max_fee_per_blob_gas NUMERIC;
