-- Reverse 000010_block_metrics_base_fee.up.sql.
ALTER TABLE block_metrics
    DROP COLUMN IF EXISTS base_fee_wei;
