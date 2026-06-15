-- Short-range attribution and cost-comparison charts read raw blob rows for
-- minute buckets. Cover the bounded timestamp scan with every blob column those
-- queries aggregate, and index the normalized attribution lookup.

CREATE INDEX IF NOT EXISTS idx_blobs_network_confirmed_timestamp_chart_cover
    ON blobs(network_id, confirmed, timestamp DESC)
    INCLUDE (
        from_address,
        user_attribution,
        total_cost_eth,
        base_fee_per_blob_gas,
        blob_gas_used,
        blob_size_bytes
    );

CREATE INDEX IF NOT EXISTS idx_blob_users_network_lower_address
    ON blob_users(network_id, LOWER(address))
    INCLUDE (name, category);
