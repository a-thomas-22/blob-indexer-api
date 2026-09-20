-- Covering index for the builder endpoints' per-transaction reads.
--
-- /builders, /builders/{key} and the builder users breakdown collapse the
-- window's blob rows to one row per transaction (builderTxSourceSQL and
-- builderEntityKeyedTxsSQL in internal/api/builders_queries.go) and need
-- block_number, tx_hash, the sender and its attribution, the execution
-- tip and first_seen_at for each. None of the existing (chain_id,
-- timestamp) indexes carries that set, so the scan went to the heap for
-- every row in the window: at 7d on mainnet that is ~280k rows across
-- ~40k scattered heap pages, 7-8 seconds cold on the production volume
-- and a 503 after the 5s aggregate timeout. With the columns in the
-- INCLUDE list the read is index-only: a few thousand leaf pages for the
-- same window.
--
-- The visibility map has to be current for the index-only path to skip
-- the heap, and the builder windows read the newest pages, which are
-- exactly the ones the default autovacuum leaves unvacuumed longest: the
-- insert-triggered vacuum fires at 20% of the table (~17M rows), which at
-- the steady-state live rate — ~40k mainnet blob rows a day, ~280k per 7d
-- window, plus the testnets sharing the table — is more than a year, so
-- the recent tail would never be all-visible and the index-only scan
-- would fetch its heap pages anyway. The storage parameters below drop
-- the scale factor entirely and fire on a fixed 50k inserts, about daily
-- for mainnet alone and a few times a day with the testnets; an
-- insert-triggered vacuum skips the pages the map already covers and,
-- with no dead tuples to remove, skips the index passes too, so each run
-- only visits the new tail and stays cheap.
--
-- Locking: CREATE INDEX scans the whole blobs heap under a SHARE lock and
-- blocks indexer writes for the duration. On a production-sized table
-- pre-run CREATE INDEX CONCURRENTLY IF NOT EXISTS with this exact name
-- and definition out-of-band; the migration then no-ops.
--
-- DDL only, idempotent, no explicit transaction control; see README.md.

CREATE INDEX IF NOT EXISTS idx_blobs_chain_timestamp_builder_cover
    ON blobs(chain_id, timestamp DESC)
    INCLUDE (block_number, tx_hash, from_address, user_attribution, priority_fee_per_gas, first_seen_at);

ALTER TABLE blobs SET (
    autovacuum_vacuum_insert_scale_factor = 0,
    autovacuum_vacuum_insert_threshold = 50000
);
