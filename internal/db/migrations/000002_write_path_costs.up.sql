-- Write-path cost reductions (see migrations/README.md for authoring rules).
--
-- 1. blob_user_stats UPDATE/DELETE triggers become delta-based. The original
--    triggers called blob_user_stats_refresh — a COUNT/SUM over the sender's
--    ENTIRE blob history — for every affected sender. Mempool churn fires
--    those paths nearly every slot (pending rows are UPDATEd each re-poll and
--    DELETEd on promotion inside the block-insert transaction), so for
--    high-volume rollup batchers the refresh re-scanned millions of rows per
--    block, a cost that grew without bound and serialized block ingestion.
--    Deltas make both paths O(rows touched by the statement) instead.
--
--    Accepted approximation, self-healing on the sender's next blob and
--    correctable by blob_user_stats_refresh (kept for reconciliation):
--    - last_timestamp is monotone: deleting a sender's newest rows (reorg)
--      leaves it overstated until their next insert.
--    user_attribution keeps the existing value unless the statement's new rows
--    carry a non-empty one (same precedence as the insert path) — EXCEPT when
--    a statement transitions a sender's attribution from non-empty to empty
--    (the attribution service revoking a delisted claim via
--    UPDATE blobs SET user_attribution = ''), which falls back to a full
--    refresh for that sender: keep-existing precedence could otherwise never
--    clear a stored name. Revocations are rare, per-address statements, so the
--    O(sender-history) refresh stays off the per-block hot path (mempool
--    re-polls rewrite pending rows with their current attribution, not '').
--
-- 2. Drop redundant indexes. The blobs table carried 15 indexes, roughly half
--    subsumed by composite/covering siblings; every one is maintained on every
--    insert plus the pending-row churn (insert as pending, delete on confirm,
--    insert as confirmed). Kept indexes cover every read path in
--    internal/api/queries.go, the chart/attribution SQL, and the indexer's
--    write/reorg paths.

-- ---------------------------------------------------------------------------
-- 1. Delta-based blob_user_stats maintenance
-- ---------------------------------------------------------------------------

-- Applies a signed delta to one sender's stats row, creating it when missing
-- and removing it when its count returns to zero. GREATEST(0, ...) guards
-- against drift ever violating the table's CHECK constraints.
CREATE OR REPLACE FUNCTION blob_user_stats_apply_signed_delta(
    p_chain_id INTEGER,
    p_from_address TEXT,
    p_user_attribution TEXT,
    p_blob_count_delta BIGINT,
    p_total_cost_wei_delta NUMERIC,
    p_last_timestamp TIMESTAMP
)
RETURNS void AS $$
BEGIN
    INSERT INTO blob_user_stats (
        chain_id, from_address, user_attribution, blob_count,
        total_cost_wei, last_timestamp, updated_at
    )
    VALUES (
        p_chain_id, p_from_address,
        COALESCE(NULLIF(BTRIM(p_user_attribution), ''), ''),
        GREATEST(p_blob_count_delta, 0),
        GREATEST(p_total_cost_wei_delta, 0),
        COALESCE(p_last_timestamp, '1970-01-01'::timestamp),
        NOW()
    )
    ON CONFLICT (chain_id, from_address) DO UPDATE SET
        user_attribution = COALESCE(
            NULLIF(BTRIM(EXCLUDED.user_attribution), ''),
            NULLIF(BTRIM(blob_user_stats.user_attribution), ''),
            ''
        ),
        blob_count = GREATEST(blob_user_stats.blob_count + p_blob_count_delta, 0),
        total_cost_wei = GREATEST(blob_user_stats.total_cost_wei + p_total_cost_wei_delta, 0),
        last_timestamp = GREATEST(
            blob_user_stats.last_timestamp,
            COALESCE(p_last_timestamp, '1970-01-01'::timestamp)
        ),
        updated_at = NOW();

    DELETE FROM blob_user_stats
    WHERE chain_id = p_chain_id
        AND from_address = p_from_address
        AND blob_count <= 0;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION blob_user_stats_blobs_delete_statement_trigger()
RETURNS trigger AS $$
DECLARE
    delta RECORD;
BEGIN
    FOR delta IN
        SELECT
            chain_id,
            from_address,
            COUNT(*)::bigint AS blob_count,
            COALESCE(SUM(total_cost_wei::numeric), 0) AS total_cost_wei
        FROM old_blobs
        GROUP BY chain_id, from_address
    LOOP
        PERFORM blob_user_stats_apply_signed_delta(
            delta.chain_id, delta.from_address, NULL,
            -delta.blob_count, -delta.total_cost_wei, NULL);
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

-- Per-sender deltas between the statement's old and new row images. Exact for
-- blob_count/total_cost_wei even when a row's from_address changes: the old
-- address's aggregate appears only on the o side (negative delta) and the new
-- address's only on the n side (positive delta). A sender whose attribution
-- transitions non-empty -> empty (revocation) is fully refreshed instead,
-- because the delta upsert's keep-existing precedence can never clear a name.
CREATE OR REPLACE FUNCTION blob_user_stats_blobs_update_statement_trigger()
RETURNS trigger AS $$
DECLARE
    delta RECORD;
BEGIN
    FOR delta IN
        SELECT
            chain_id,
            from_address,
            COALESCE(n.blob_count, 0) - COALESCE(o.blob_count, 0) AS blob_count,
            COALESCE(n.total_cost_wei, 0) - COALESCE(o.total_cost_wei, 0) AS total_cost_wei,
            n.user_attribution,
            n.last_timestamp,
            (COALESCE(o.user_attribution, '') <> '' AND COALESCE(n.user_attribution, '') = '')
                AS attribution_cleared
        FROM (
            SELECT
                chain_id,
                from_address,
                COUNT(*)::bigint AS blob_count,
                COALESCE(SUM(total_cost_wei::numeric), 0) AS total_cost_wei,
                COALESCE(NULLIF(MAX(BTRIM(user_attribution)), ''), '') AS user_attribution,
                MAX(timestamp) AS last_timestamp
            FROM new_blobs
            GROUP BY chain_id, from_address
        ) n
        FULL OUTER JOIN (
            SELECT
                chain_id,
                from_address,
                COUNT(*)::bigint AS blob_count,
                COALESCE(SUM(total_cost_wei::numeric), 0) AS total_cost_wei,
                COALESCE(NULLIF(MAX(BTRIM(user_attribution)), ''), '') AS user_attribution
            FROM old_blobs
            GROUP BY chain_id, from_address
        ) o USING (chain_id, from_address)
    LOOP
        IF delta.attribution_cleared THEN
            PERFORM blob_user_stats_refresh(delta.chain_id, delta.from_address);
        ELSE
            PERFORM blob_user_stats_apply_signed_delta(
                delta.chain_id, delta.from_address, delta.user_attribution,
                delta.blob_count, delta.total_cost_wei, delta.last_timestamp);
        END IF;
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

-- ---------------------------------------------------------------------------
-- 2. Redundant index removal
-- ---------------------------------------------------------------------------

-- blobs. Kept: UNIQUE(chain_id, block_number, blob_index) — serves every
-- (chain_id, block_number) predicate incl. reorg deletes and per-block chart
-- joins; idx_blobs_chain_confirmed_block; idx_blobs_chain_txhash;
-- idx_blobs_chain_from_timestamp; idx_blobs_pending_chain_tx_hash (partial);
-- idx_blobs_chain_confirmed_timestamp_chart_cover — its key columns and
-- INCLUDE set are a superset of the dropped _cover variant, so it serves the
-- rolling-stats and chart raw scans as well as all plain
-- (chain_id, confirmed, timestamp) orderings; and
-- idx_blobs_chain_lower_from_address — required by the attribution refresher's
-- LOWER(from_address) UPDATE statements (internal/attribution/blob_list.go).
DROP INDEX IF EXISTS idx_blobs_chain_id;                       -- prefix of six composites
DROP INDEX IF EXISTS idx_blobs_block_number;                   -- subsumed by UNIQUE(chain_id, block_number, blob_index)
DROP INDEX IF EXISTS idx_blobs_from_address;                   -- no cross-chain address query exists
DROP INDEX IF EXISTS idx_blobs_timestamp;                      -- no cross-chain timestamp query exists
DROP INDEX IF EXISTS idx_blobs_confirmed;                      -- single-column boolean, never selective
DROP INDEX IF EXISTS idx_blobs_chain_timestamp;                -- every timestamp predicate also filters confirmed
DROP INDEX IF EXISTS idx_blobs_chain_confirmed_timestamp;      -- key-duplicate of the chart cover index
DROP INDEX IF EXISTS idx_blobs_chain_confirmed_timestamp_cover; -- INCLUDE subset of the chart cover index

-- block_metrics: (chain_id, block_number DESC) duplicates the primary key,
-- which a btree scans backwards at identical cost.
DROP INDEX IF EXISTS idx_block_metrics_chain_block;

-- blob_users: subsumed by UNIQUE(chain_id, address) and
-- idx_blob_users_chain_lower_address.
DROP INDEX IF EXISTS idx_blob_users_chain_id;
DROP INDEX IF EXISTS idx_blob_users_address;
