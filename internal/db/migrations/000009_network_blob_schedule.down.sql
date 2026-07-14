-- Drop the learned blob schedule. Blob-parameter resolution degrades back to
-- the compiled go-ethereum chain config; arbitrary networks lose BPO awareness
-- until eth_config learning is available again.
--
-- DDL only, idempotent, no explicit transaction control — see README.md.

DROP TABLE IF EXISTS network_blob_schedule;
