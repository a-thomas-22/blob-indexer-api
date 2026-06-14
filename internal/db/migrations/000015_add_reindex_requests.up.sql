CREATE TABLE IF NOT EXISTS block_reindex_requests (
    id BIGSERIAL PRIMARY KEY,
    network_id INTEGER NOT NULL,
    start_block BIGINT NOT NULL CHECK (start_block >= 0),
    end_block BIGINT NOT NULL CHECK (end_block >= start_block),
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'processing', 'completed', 'failed')),
    requested_by TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error TEXT,
    claimed_by TEXT,
    requested_at TIMESTAMP NOT NULL DEFAULT NOW(),
    started_at TIMESTAMP,
    completed_at TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_block_reindex_requests_network_chain_id
        FOREIGN KEY (network_id)
        REFERENCES networks(chain_id)
        ON UPDATE CASCADE
        ON DELETE RESTRICT
);

CREATE INDEX IF NOT EXISTS idx_block_reindex_requests_pending
    ON block_reindex_requests(network_id, requested_at, id)
    WHERE status = 'pending';

CREATE INDEX IF NOT EXISTS idx_block_reindex_requests_status
    ON block_reindex_requests(status, updated_at DESC);
