-- Revert the builder tables' autovacuum thresholds to the server defaults.
-- blobs keeps the insert-vacuum settings 000019 owns.
--
-- DDL only, idempotent, no explicit transaction control; see README.md.

ALTER TABLE block_builders RESET (
    autovacuum_analyze_scale_factor,
    autovacuum_analyze_threshold,
    autovacuum_vacuum_insert_scale_factor,
    autovacuum_vacuum_insert_threshold
);

ALTER TABLE block_metrics RESET (
    autovacuum_analyze_scale_factor,
    autovacuum_analyze_threshold,
    autovacuum_vacuum_insert_scale_factor,
    autovacuum_vacuum_insert_threshold
);

ALTER TABLE blobs RESET (
    autovacuum_analyze_scale_factor,
    autovacuum_analyze_threshold
);
