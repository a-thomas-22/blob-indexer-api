-- Create indexed_blocks table for chain reorganization detection
CREATE TABLE IF NOT EXISTS indexed_blocks (
    network_id INTEGER NOT NULL,
    block_number BIGINT NOT NULL,
    block_hash TEXT NOT NULL,
    parent_hash TEXT NOT NULL,
    indexed_at TIMESTAMP NOT NULL DEFAULT NOW(),
    PRIMARY KEY (network_id, block_number)
);

-- Composite index for the hot "latest blobs" query path
-- Covers: WHERE confirmed = true AND network_id = $1 ORDER BY block_number DESC
CREATE INDEX IF NOT EXISTS idx_blobs_network_confirmed_block
    ON blobs(network_id, confirmed, block_number DESC);

-- Composite index for blob lookup by transaction hash
CREATE INDEX IF NOT EXISTS idx_blobs_network_txhash
    ON blobs(network_id, tx_hash);

-- Composite index for user attribution aggregation queries
CREATE INDEX IF NOT EXISTS idx_blobs_network_from_address
    ON blobs(network_id, from_address);
