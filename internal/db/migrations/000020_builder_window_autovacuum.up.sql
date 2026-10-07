-- Keep planner statistics and the visibility map current at the tip of the
-- tables the builder windows read.
--
-- block_builders, block_metrics and blobs are append-mostly and tens of
-- millions of rows long, while the builder endpoints only ever read their
-- newest slice (1h to 30d). Autovacuum's defaults scale with table size:
-- ANALYZE fires after 10% of the table changes and the insert-triggered
-- VACUUM after 20%. At the steady-state live rate — ~21k block rows a day
-- across mainnet, sepolia and hoodi on a ~17M-row table, ~250k blob rows a
-- day on ~97M — that is two to three months between ANALYZEs and longer
-- between vacuums. None of the three tables has an index led by its
-- timestamp (only (chain_id, timestamp)), so the planner cannot probe the
-- column's live maximum either: any window newer than the last ANALYZE's
-- histogram is estimated at one row. In October 2026, six days after the
-- last ANALYZE, that sent every builder range above 1h on every network to
-- a quadratic nested loop and a 503 (sepolia 24h: 54s). A manual ANALYZE of
-- the three tables took under two seconds each.
--
-- Fixed thresholds instead of scale factors:
--   * ANALYZE every 2,000 block rows (~2h across the three networks) and
--     every 10,000 blob rows (~1h), so the histogram's upper bound is never
--     more than a couple of hours behind the tip.
--   * Insert-triggered VACUUM of block_builders and block_metrics every
--     5,000 rows (~6h), so the recent pages are all-visible and the
--     builder-share chart's index-only scan of
--     idx_block_metrics_chain_timestamp_cover does not fetch the heap.
--     blobs already has this from 000019.
--
-- Both are cheap: ANALYZE samples a fixed 30k rows whatever the table size,
-- and an insert-triggered vacuum skips the pages the visibility map already
-- covers.
--
-- DDL only, idempotent, no explicit transaction control; see README.md.

ALTER TABLE block_builders SET (
    autovacuum_analyze_scale_factor = 0,
    autovacuum_analyze_threshold = 2000,
    autovacuum_vacuum_insert_scale_factor = 0,
    autovacuum_vacuum_insert_threshold = 5000
);

ALTER TABLE block_metrics SET (
    autovacuum_analyze_scale_factor = 0,
    autovacuum_analyze_threshold = 2000,
    autovacuum_vacuum_insert_scale_factor = 0,
    autovacuum_vacuum_insert_threshold = 5000
);

ALTER TABLE blobs SET (
    autovacuum_analyze_scale_factor = 0,
    autovacuum_analyze_threshold = 10000
);
