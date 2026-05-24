CREATE TABLE IF NOT EXISTS blob_attribution_claims (
    id SERIAL PRIMARY KEY,
    network_id INTEGER NOT NULL,
    source TEXT NOT NULL,
    address TEXT NOT NULL,
    entity_id TEXT NOT NULL,
    name TEXT NOT NULL,
    category TEXT NOT NULL DEFAULT '',
    role TEXT NOT NULL DEFAULT '',
    confidence TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT '',
    valid_from_block BIGINT NOT NULL,
    valid_to_block BIGINT,
    updated_at TIMESTAMP NOT NULL DEFAULT NOW(),
    UNIQUE(network_id, source, address, entity_id, role, valid_from_block)
);

CREATE INDEX IF NOT EXISTS idx_blob_attribution_claims_network_source
    ON blob_attribution_claims(network_id, source);

CREATE INDEX IF NOT EXISTS idx_blob_attribution_claims_address
    ON blob_attribution_claims(network_id, address);
