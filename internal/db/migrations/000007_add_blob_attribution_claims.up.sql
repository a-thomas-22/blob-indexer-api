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
    CONSTRAINT fk_blob_attribution_claims_network_chain_id
        FOREIGN KEY (network_id)
        REFERENCES networks(chain_id)
        ON UPDATE CASCADE
        ON DELETE RESTRICT,
    UNIQUE(network_id, source, address, entity_id, role, valid_from_block)
);

CREATE INDEX IF NOT EXISTS idx_blob_attribution_claims_network_source
    ON blob_attribution_claims(network_id, source);

CREATE INDEX IF NOT EXISTS idx_blob_attribution_claims_address
    ON blob_attribution_claims(network_id, address);

CREATE INDEX IF NOT EXISTS idx_blobs_network_lower_from_address
    ON blobs(network_id, LOWER(from_address));
