-- Drop indexes
DROP INDEX IF EXISTS idx_blobs_network_id_tx_hash;
DROP INDEX IF EXISTS idx_blob_users_address;
DROP INDEX IF EXISTS idx_blob_users_network_id;
DROP INDEX IF EXISTS idx_blobs_confirmed;
DROP INDEX IF EXISTS idx_blobs_timestamp;
DROP INDEX IF EXISTS idx_blobs_from_address;
DROP INDEX IF EXISTS idx_blobs_block_number;
DROP INDEX IF EXISTS idx_blobs_network_id;

-- Drop tables
DROP TABLE IF EXISTS indexer_metadata;
DROP TABLE IF EXISTS blob_users;
DROP TABLE IF EXISTS blobs;
DROP TABLE IF EXISTS networks;
