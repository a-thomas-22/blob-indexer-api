-- Revert the builder endpoints' covering index on blobs.
--
-- DDL only, idempotent, no explicit transaction control; see README.md.

DROP INDEX IF EXISTS idx_blobs_chain_timestamp_builder_cover;

ALTER TABLE blobs RESET (
    autovacuum_vacuum_insert_scale_factor,
    autovacuum_vacuum_insert_threshold
);
