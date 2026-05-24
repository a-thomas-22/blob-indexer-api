ALTER TABLE IF EXISTS block_metrics
    DROP CONSTRAINT IF EXISTS fk_block_metrics_network_chain_id;

ALTER TABLE IF EXISTS indexed_blocks
    DROP CONSTRAINT IF EXISTS fk_indexed_blocks_network_chain_id;

ALTER TABLE IF EXISTS indexer_metadata
    DROP CONSTRAINT IF EXISTS fk_indexer_metadata_network_chain_id;

ALTER TABLE IF EXISTS blob_users
    DROP CONSTRAINT IF EXISTS fk_blob_users_network_chain_id;

ALTER TABLE IF EXISTS blobs
    DROP CONSTRAINT IF EXISTS fk_blobs_network_chain_id;

DROP INDEX IF EXISTS idx_indexer_metadata_global_key;
