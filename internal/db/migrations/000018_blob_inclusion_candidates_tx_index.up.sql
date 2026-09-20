-- Per-transaction inclusion history: /blob/{txHash}/inclusion lists every
-- block that passed a pending blob transaction by, with the reason each
-- classified it under. The primary key (chain_id, block_number, tx_hash)
-- and the (chain_id, block_timestamp) index both lead with a block
-- dimension, so a tx_hash probe had to walk every candidate row in the
-- transaction's waiting window — the whole pending pool times every block
-- it waited through. This index makes it a direct probe, ordered by block
-- for the timeline.
--
-- The table is pruned on indexer.candidate_retention (a week by default),
-- so the index stays bounded; at the pool sizes seen on mainnet it holds
-- well under a million rows and builds in seconds.
--
-- DDL only, idempotent, no explicit transaction control; see README.md.

CREATE INDEX IF NOT EXISTS idx_blob_inclusion_candidates_chain_tx_block
    ON blob_inclusion_candidates(chain_id, tx_hash, block_number);
