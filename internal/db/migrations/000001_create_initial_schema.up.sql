-- Create networks table
CREATE TABLE IF NOT EXISTS networks (
    id SERIAL PRIMARY KEY,
    chain_id INTEGER NOT NULL UNIQUE,
    name TEXT NOT NULL,
    start_block TEXT NOT NULL,
    is_enabled BOOLEAN DEFAULT TRUE,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP NOT NULL DEFAULT NOW()
);

-- Create blobs table
CREATE TABLE IF NOT EXISTS blobs (
    id SERIAL PRIMARY KEY,
    network_id INTEGER NOT NULL,
    block_number BIGINT NOT NULL,
    blob_index SMALLINT NOT NULL,
    tx_hash TEXT NOT NULL,
    from_address TEXT NOT NULL,
    user_attribution TEXT,
    blob_size_bytes BIGINT NOT NULL,
    base_fee_per_blob_gas NUMERIC NOT NULL,
    tip_per_blob_gas NUMERIC NOT NULL,
    total_cost_eth NUMERIC NOT NULL,
    timestamp TIMESTAMP NOT NULL,
    confirmed BOOLEAN DEFAULT TRUE,
    indexer_version TEXT DEFAULT 'v1.0.0',
    UNIQUE(network_id, block_number, blob_index)
);

-- Create blob_users table
CREATE TABLE IF NOT EXISTS blob_users (
    id SERIAL PRIMARY KEY,
    network_id INTEGER NOT NULL,
    address TEXT NOT NULL,
    name TEXT NOT NULL,
    description TEXT,
    category TEXT,
    first_seen TIMESTAMP NOT NULL,
    last_seen TIMESTAMP NOT NULL,
    UNIQUE(network_id, address)
);

-- Create indexer_metadata table
CREATE TABLE IF NOT EXISTS indexer_metadata (
    id SERIAL PRIMARY KEY,
    network_id INTEGER,
    key TEXT NOT NULL,
    value TEXT NOT NULL,
    UNIQUE(network_id, key)
);

-- Create indexes
CREATE INDEX IF NOT EXISTS idx_blobs_network_id ON blobs(network_id);
CREATE INDEX IF NOT EXISTS idx_blobs_block_number ON blobs(block_number);
CREATE INDEX IF NOT EXISTS idx_blobs_from_address ON blobs(from_address);
CREATE INDEX IF NOT EXISTS idx_blobs_timestamp ON blobs(timestamp);
CREATE INDEX IF NOT EXISTS idx_blobs_confirmed ON blobs(confirmed);
CREATE INDEX IF NOT EXISTS idx_blob_users_network_id ON blob_users(network_id);
CREATE INDEX IF NOT EXISTS idx_blob_users_address ON blob_users(address);

-- Add unique constraint on tx_hash and network_id for pending transactions
CREATE UNIQUE INDEX IF NOT EXISTS idx_blobs_network_id_tx_hash ON blobs(network_id, tx_hash) WHERE block_number < 0;

-- Insert default network (mainnet)
INSERT INTO networks (chain_id, name, start_block, is_enabled)
VALUES (1, 'mainnet', 'LATEST-1000', true)
ON CONFLICT (chain_id) DO NOTHING;
