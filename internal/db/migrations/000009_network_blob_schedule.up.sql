-- Persist the per-network blob-parameter schedule the indexer learns from the
-- node via eth_config (EIP-7910). Each row is one fork boundary: the blob
-- target/max/update-fraction that becomes active at activation_time and stays
-- active until the next boundary. The indexer upserts current/next/last on a
-- poll ticker; both the indexer (fork-aware indexing) and the API (pricing,
-- fork stage, next-fee prediction) read it back to build a ChainConfig that
-- reflects the real schedule for any network — including BPO forks and chains
-- go-ethereum does not ship a compiled config for — without a code change.
--
-- Keyed by (chain_id, activation_time): re-observing the same boundary upserts
-- the latest advertised parameters. LOGGED and tiny (a handful of rows per
-- network), so no extra indexes beyond the primary key.
--
-- DDL only, idempotent, no explicit transaction control — see README.md.

CREATE TABLE IF NOT EXISTS network_blob_schedule (
    chain_id INTEGER NOT NULL,
    activation_time BIGINT NOT NULL,
    target INTEGER NOT NULL,
    max INTEGER NOT NULL,
    update_fraction BIGINT NOT NULL,
    source TEXT NOT NULL,
    observed_at TIMESTAMP NOT NULL,
    PRIMARY KEY (chain_id, activation_time),
    CONSTRAINT fk_network_blob_schedule_network_chain_id
        FOREIGN KEY (chain_id) REFERENCES networks(chain_id)
        ON UPDATE RESTRICT ON DELETE RESTRICT
);
