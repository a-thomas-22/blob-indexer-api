-- Block-level blob pricing metrics for detailed pricing analysis
CREATE TABLE IF NOT EXISTS block_metrics (
    network_id        INTEGER NOT NULL,
    block_number      BIGINT NOT NULL,
    block_timestamp   TIMESTAMP NOT NULL,

    -- Blob counts and gas
    blob_count        SMALLINT NOT NULL DEFAULT 0,
    blob_gas_used     BIGINT NOT NULL DEFAULT 0,
    blob_gas_target   BIGINT NOT NULL DEFAULT 0,
    blob_gas_limit    BIGINT NOT NULL DEFAULT 0,

    -- Pricing algorithm inputs
    excess_blob_gas   BIGINT NOT NULL DEFAULT 0,
    blob_base_fee     NUMERIC NOT NULL DEFAULT 0,

    -- Derived metrics
    utilization_ratio NUMERIC(10, 6) NOT NULL DEFAULT 0,

    -- Fork parameters active at this block
    blob_params_target SMALLINT NOT NULL DEFAULT 3,
    blob_params_max    SMALLINT NOT NULL DEFAULT 6,
    update_fraction    BIGINT NOT NULL DEFAULT 3338477,

    PRIMARY KEY (network_id, block_number)
);

-- Per-transaction pricing fields on blobs table
ALTER TABLE blobs ADD COLUMN IF NOT EXISTS max_fee_per_blob_gas NUMERIC;
ALTER TABLE blobs ADD COLUMN IF NOT EXISTS blob_gas_used BIGINT;

-- Indexes for time-series pricing queries
CREATE INDEX IF NOT EXISTS idx_block_metrics_network_timestamp
    ON block_metrics(network_id, block_timestamp DESC);

CREATE INDEX IF NOT EXISTS idx_block_metrics_network_block
    ON block_metrics(network_id, block_number DESC);
