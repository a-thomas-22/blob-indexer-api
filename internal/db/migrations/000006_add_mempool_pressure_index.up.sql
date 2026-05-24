-- Composite index for pending blob mempool pressure queries.
CREATE INDEX IF NOT EXISTS idx_blobs_network_confirmed_timestamp
    ON blobs(network_id, confirmed, timestamp DESC);
