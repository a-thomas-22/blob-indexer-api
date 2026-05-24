-- Ensure existing chain IDs have parent rows before adding foreign keys.
INSERT INTO networks (chain_id, name, start_block, is_enabled)
SELECT DISTINCT network_id, 'chain-' || network_id::text, '0', true
FROM (
    SELECT network_id FROM blobs
    UNION
    SELECT network_id FROM blob_users
    UNION
    SELECT network_id FROM indexer_metadata WHERE network_id IS NOT NULL
    UNION
    SELECT network_id FROM indexed_blocks
    UNION
    SELECT network_id FROM block_metrics
) AS existing_networks
WHERE network_id IS NOT NULL
ON CONFLICT (chain_id) DO NOTHING;

-- PostgreSQL treats NULL values as distinct for UNIQUE(network_id, key), so
-- global metadata needs its own partial unique index.
DELETE FROM indexer_metadata a
USING indexer_metadata b
WHERE a.network_id IS NULL
  AND b.network_id IS NULL
  AND a.key = b.key
  AND a.id < b.id;

CREATE UNIQUE INDEX IF NOT EXISTS idx_indexer_metadata_global_key
    ON indexer_metadata(key)
    WHERE network_id IS NULL;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'fk_blobs_network_chain_id'
    ) THEN
        ALTER TABLE blobs
            ADD CONSTRAINT fk_blobs_network_chain_id
            FOREIGN KEY (network_id)
            REFERENCES networks(chain_id)
            ON UPDATE CASCADE
            ON DELETE RESTRICT;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'fk_blob_users_network_chain_id'
    ) THEN
        ALTER TABLE blob_users
            ADD CONSTRAINT fk_blob_users_network_chain_id
            FOREIGN KEY (network_id)
            REFERENCES networks(chain_id)
            ON UPDATE CASCADE
            ON DELETE RESTRICT;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'fk_indexer_metadata_network_chain_id'
    ) THEN
        ALTER TABLE indexer_metadata
            ADD CONSTRAINT fk_indexer_metadata_network_chain_id
            FOREIGN KEY (network_id)
            REFERENCES networks(chain_id)
            ON UPDATE CASCADE
            ON DELETE RESTRICT;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'fk_indexed_blocks_network_chain_id'
    ) THEN
        ALTER TABLE indexed_blocks
            ADD CONSTRAINT fk_indexed_blocks_network_chain_id
            FOREIGN KEY (network_id)
            REFERENCES networks(chain_id)
            ON UPDATE CASCADE
            ON DELETE RESTRICT;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'fk_block_metrics_network_chain_id'
    ) THEN
        ALTER TABLE block_metrics
            ADD CONSTRAINT fk_block_metrics_network_chain_id
            FOREIGN KEY (network_id)
            REFERENCES networks(chain_id)
            ON UPDATE CASCADE
            ON DELETE RESTRICT;
    END IF;
END $$;
