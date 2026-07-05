-- NOTIFY on block_metrics writes so the API can broadcast new_block WebSocket
-- events without polling indexer_metadata.
--
-- The previous poll-based detection read indexer_metadata.last_indexed_block,
-- which the indexer's concurrent workers advance monotonically to the MAX
-- completed block — a block that commits late (retry, slow worker) is passed
-- over by the poller's "block_number > last seen" cursor and never broadcast.
-- pg_notify queues inside the writing transaction and delivers on COMMIT, in
-- commit order, so every committed block produces exactly one delivered
-- notification and out-of-order commits cannot be skipped. Re-indexed blocks
-- (reorg replacements hit the ON CONFLICT DO UPDATE path, or re-insert after
-- the reorg delete) notify again, which lets clients receive corrected data
-- for replaced blocks.
--
-- Payload is compact JSON ({"chain_id":..,"block_number":..}); the API
-- listener treats the payload as a hint and falls back to a catch-up scan on
-- malformed or missed notifications.
--
-- DDL only, idempotent, no explicit transaction control — see README.md.

CREATE OR REPLACE FUNCTION block_metrics_notify_new_block()
RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify(
        'blob_indexer_new_block',
        json_build_object(
            'chain_id', NEW.chain_id,
            'block_number', NEW.block_number
        )::text
    );
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE TRIGGER block_metrics_notify_new_block_trigger
    AFTER INSERT OR UPDATE ON block_metrics
    FOR EACH ROW
    EXECUTE FUNCTION block_metrics_notify_new_block();
