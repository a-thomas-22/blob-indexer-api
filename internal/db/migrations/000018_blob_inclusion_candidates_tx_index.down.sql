-- Revert the per-transaction inclusion history index.
--
-- DDL only, idempotent, no explicit transaction control; see README.md.

DROP INDEX IF EXISTS idx_blob_inclusion_candidates_chain_tx_block;
