-- Add the block's execution-layer (EIP-1559) base fee to block_metrics.
--
-- The pricing endpoint's next-fee prediction reconstructs the next block's
-- excess blob gas with go-ethereum's fork-aware eip4844.CalcExcessBlobGas.
-- Post-Osaka that formula has an EIP-7918 reserve-price branch which compares
-- the blob base fee against a reserve derived from the *execution* base fee, so
-- the prediction needs each block's execution base fee, which nothing recorded
-- before now.
--
-- Prediction-only column: no historical backfill is required. Rows indexed
-- before this migration keep the default 0, which makes the EIP-7918 reserve
-- price zero. A zero reserve can never exceed the blob price, so the reserve
-- branch is skipped and the estimate falls back to the legacy pre-Osaka formula
-- — identical to the behaviour before this column existed.
--
-- A constant DEFAULT makes this a metadata-only add (no table rewrite) on
-- PostgreSQL 11+. DDL only, idempotent, no explicit transaction control — see
-- README.md.
ALTER TABLE block_metrics
    ADD COLUMN IF NOT EXISTS base_fee_wei NUMERIC NOT NULL DEFAULT 0;
