-- Composite index for rolling time-window stats queries.
CREATE INDEX IF NOT EXISTS idx_blobs_network_timestamp
    ON blobs(network_id, timestamp DESC);
