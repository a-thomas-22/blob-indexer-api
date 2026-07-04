-- Revert write-path cost reductions: restore the full-recount UPDATE/DELETE
-- triggers from 000001 and recreate the dropped indexes.

CREATE OR REPLACE FUNCTION blob_user_stats_blobs_delete_statement_trigger()
RETURNS trigger AS $$
DECLARE
    affected RECORD;
BEGIN
    FOR affected IN SELECT DISTINCT chain_id, from_address FROM old_blobs LOOP
        PERFORM blob_user_stats_refresh(affected.chain_id, affected.from_address);
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION blob_user_stats_blobs_update_statement_trigger()
RETURNS trigger AS $$
DECLARE
    affected RECORD;
BEGIN
    FOR affected IN
        SELECT DISTINCT chain_id, from_address FROM old_blobs
        UNION
        SELECT DISTINCT chain_id, from_address FROM new_blobs
    LOOP
        PERFORM blob_user_stats_refresh(affected.chain_id, affected.from_address);
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP FUNCTION IF EXISTS blob_user_stats_apply_signed_delta(INTEGER, TEXT, TEXT, BIGINT, NUMERIC, TIMESTAMP);

CREATE INDEX IF NOT EXISTS idx_blobs_chain_id ON blobs(chain_id);
CREATE INDEX IF NOT EXISTS idx_blobs_block_number ON blobs(block_number);
CREATE INDEX IF NOT EXISTS idx_blobs_from_address ON blobs(from_address);
CREATE INDEX IF NOT EXISTS idx_blobs_timestamp ON blobs(timestamp);
CREATE INDEX IF NOT EXISTS idx_blobs_confirmed ON blobs(confirmed);
CREATE INDEX IF NOT EXISTS idx_blobs_chain_timestamp ON blobs(chain_id, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_blobs_chain_confirmed_timestamp ON blobs(chain_id, confirmed, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_blobs_chain_confirmed_timestamp_cover
    ON blobs(chain_id, confirmed, timestamp DESC)
    INCLUDE (from_address, total_cost_wei, base_fee_per_blob_gas, blob_gas_used);

CREATE INDEX IF NOT EXISTS idx_block_metrics_chain_block ON block_metrics(chain_id, block_number DESC);

CREATE INDEX IF NOT EXISTS idx_blob_users_chain_id ON blob_users(chain_id);
CREATE INDEX IF NOT EXISTS idx_blob_users_address ON blob_users(address);
