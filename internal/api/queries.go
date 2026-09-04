package api

import "github.com/a-thomas-22/blob-indexer-api/internal/db/models"

// blobSelectColumns projects blobs rows into the models.Blob shape. The
// blobs table holds confirmed rows only (pending rows live in mempool_blobs),
// so the wire-visible confirmed flag is the projected literal true rather
// than a stored column. versioned_hashes is computed, not stored: it gathers
// the transaction's full ordered hash list from the sibling rows'
// versioned_hash values — one idx_blobs_chain_txhash probe over the tx's
// <= a-few rows per projected row, bounded by the small LIMITs of every query
// using this projection. Rows indexed before the versioned-hash migration
// have NULL hashes, filtered out here, so pre-migration transactions yield an
// empty list (omitted on the wire).
const blobSelectColumns = `
	id,
	chain_id,
	block_number,
	blob_index,
	tx_hash,
	from_address,
	user_attribution,
	blob_size_bytes,
	base_fee_per_blob_gas,
	tip_per_blob_gas,
	total_cost_wei,
	timestamp,
	true AS confirmed,
	max_fee_per_blob_gas,
	blob_gas_used,
	versioned_hash,
	slot,
	max_priority_fee_per_gas,
	max_fee_per_gas,
	priority_fee_per_gas,
	ARRAY(
		SELECT b2.versioned_hash FROM blobs b2
		WHERE b2.chain_id = blobs.chain_id AND b2.tx_hash = blobs.tx_hash
			AND b2.versioned_hash IS NOT NULL
		ORDER BY b2.blob_index
	) AS versioned_hashes
`

// mempoolBlobSelectColumns projects mempool_blobs rows into the models.Blob
// shape. Pending rows carry the internal block-number sentinel
// (models.PendingBlockNumber) and are never confirmed, so downstream
// serialization (JSON null block_number, confirmed=false) is unchanged.
// versioned_hashes gathers the transaction's hash list from sibling rows via
// the mempool_blobs primary key (chain_id, tx_hash, blob_index) — see
// blobSelectColumns.
const mempoolBlobSelectColumns = `
	0 AS id,
	chain_id,
	-1 AS block_number,
	blob_index,
	tx_hash,
	from_address,
	user_attribution,
	blob_size_bytes,
	base_fee_per_blob_gas,
	tip_per_blob_gas,
	total_cost_wei,
	timestamp,
	false AS confirmed,
	max_fee_per_blob_gas,
	blob_gas_used,
	versioned_hash,
	NULL::bigint AS slot,
	max_priority_fee_per_gas,
	max_fee_per_gas,
	priority_fee_per_gas,
	ARRAY(
		SELECT m2.versioned_hash FROM mempool_blobs m2
		WHERE m2.chain_id = mempool_blobs.chain_id AND m2.tx_hash = mempool_blobs.tx_hash
			AND m2.versioned_hash IS NOT NULL
		ORDER BY m2.blob_index
	) AS versioned_hashes
`

const blockMetricsSelectColumns = `
	chain_id,
	block_number,
	block_timestamp,
	blob_count,
	blob_gas_used,
	blob_gas_target,
	blob_gas_limit,
	excess_blob_gas,
	blob_base_fee,
	base_fee_wei,
	utilization_ratio,
	blob_params_target,
	blob_params_max,
	update_fraction
`

// SQL query constants used by API handlers.
const (
	// queryLatestBlobs retrieves confirmed blobs ordered by block number descending.
	queryLatestBlobs = `
		SELECT ` + blobSelectColumns + ` FROM blobs
		WHERE chain_id = $1
		ORDER BY block_number DESC, blob_index ASC
		LIMIT $2 OFFSET $3
	`

	// queryMempoolBlobs retrieves pending (mempool) blobs ordered by timestamp descending.
	queryMempoolBlobs = `
		SELECT ` + mempoolBlobSelectColumns + ` FROM mempool_blobs
		WHERE chain_id = $1
		ORDER BY timestamp DESC
		LIMIT $2 OFFSET $3
	`

	// queryMempoolPressure computes bounded aggregate pressure metrics for pending blobs.
	queryMempoolPressure = `
		WITH limited_pending AS (
			SELECT
				from_address,
				timestamp,
				max_fee_per_blob_gas,
				COALESCE(blob_gas_used, blob_size_bytes / 128, 0) AS blob_gas_used
			FROM mempool_blobs
			WHERE chain_id = $1
			ORDER BY timestamp DESC
			LIMIT $2
		),
		pending AS (
			SELECT * FROM limited_pending
			ORDER BY timestamp DESC
			LIMIT $3
		)
		SELECT
			COUNT(*) AS pending_blob_count,
			COALESCE(SUM(blob_gas_used), 0)::bigint AS pending_blob_gas,
			COUNT(DISTINCT from_address) AS pending_unique_senders,
			COALESCE(MIN(max_fee_per_blob_gas::numeric) FILTER (WHERE max_fee_per_blob_gas IS NOT NULL), 0::numeric) AS max_fee_min,
			COALESCE(AVG(max_fee_per_blob_gas::numeric) FILTER (WHERE max_fee_per_blob_gas IS NOT NULL), 0::numeric) AS max_fee_avg,
			COALESCE(PERCENTILE_DISC(0.5) WITHIN GROUP (ORDER BY max_fee_per_blob_gas::numeric) FILTER (WHERE max_fee_per_blob_gas IS NOT NULL), 0::numeric) AS max_fee_median,
			COALESCE(PERCENTILE_DISC(0.95) WITHIN GROUP (ORDER BY max_fee_per_blob_gas::numeric) FILTER (WHERE max_fee_per_blob_gas IS NOT NULL), 0::numeric) AS max_fee_p95,
			COALESCE(MAX(max_fee_per_blob_gas::numeric) FILTER (WHERE max_fee_per_blob_gas IS NOT NULL), 0::numeric) AS max_fee_max,
			COALESCE(GREATEST(EXTRACT(EPOCH FROM ((statement_timestamp() AT TIME ZONE 'UTC') - MIN(timestamp))), 0), 0)::double precision AS oldest_age_seconds,
			COALESCE(GREATEST(EXTRACT(EPOCH FROM ((statement_timestamp() AT TIME ZONE 'UTC') - MAX(timestamp))), 0), 0)::double precision AS newest_age_seconds,
			COALESCE(AVG(GREATEST(EXTRACT(EPOCH FROM ((statement_timestamp() AT TIME ZONE 'UTC') - timestamp)), 0)), 0)::double precision AS average_age_seconds,
			MIN(timestamp) AS oldest_timestamp,
			MAX(timestamp) AS newest_timestamp,
			COUNT(*) FILTER (
				WHERE $4::numeric IS NOT NULL
					AND max_fee_per_blob_gas IS NOT NULL
					AND max_fee_per_blob_gas::numeric >= $4::numeric
			) AS likely_includable_count,
			COUNT(*) FILTER (
				WHERE $4::numeric IS NOT NULL
					AND max_fee_per_blob_gas IS NOT NULL
					AND max_fee_per_blob_gas::numeric < $4::numeric
			) AS underpriced_count,
			COUNT(*) FILTER (
				WHERE $4::numeric IS NULL
					OR max_fee_per_blob_gas IS NULL
			) AS unknown_pricing_count,
			EXISTS (SELECT 1 FROM limited_pending OFFSET $3) AS sample_truncated
		FROM pending
	`

	// queryLatestBlobBaseFee retrieves the newest indexed blob base fee for includability estimates.
	queryLatestBlobBaseFee = `
		SELECT blob_base_fee FROM block_metrics
		WHERE chain_id = $1
		ORDER BY block_number DESC
		LIMIT 1
	`

	// queryBlobByTxHash retrieves a single blob by transaction hash and network,
	// checking confirmed rows first and falling back to the mempool. Multi-blob
	// transactions return their first blob.
	queryBlobByTxHash = `
		SELECT ` + blobSelectColumns + ` FROM blobs WHERE tx_hash = $1 AND chain_id = $2
		UNION ALL
		SELECT ` + mempoolBlobSelectColumns + ` FROM mempool_blobs WHERE tx_hash = $1 AND chain_id = $2
		ORDER BY confirmed DESC, blob_index ASC
		LIMIT 1
	`

	// queryBlobByVersionedHash retrieves a single blob by EIP-4844 versioned
	// blob hash and network — an equality probe on the per-row versioned_hash
	// column, served by the partial idx_blobs_chain_versioned_hash (the same
	// index behind /search's blob resolution); the mempool arm is a bounded
	// scan of the tiny pending set. The matched row is the blob itself, so
	// blob_index identifies the carrying position within its transaction. When
	// identical blob content was posted more than once (same content ⇒ same
	// versioned hash): confirmed rows win over pending ones, then the newest
	// inclusion by block, with timestamp DESC breaking pending ties
	// (block_number is the shared -1 sentinel there), tx_hash making
	// same-block/same-poll ties deterministic, and blob_index settling
	// duplicate hashes within one transaction. Rows indexed before the
	// versioned-hash migration have NULL versioned_hash and never match.
	queryBlobByVersionedHash = `
		SELECT ` + blobSelectColumns + ` FROM blobs
		WHERE chain_id = $2 AND versioned_hash = $1
		UNION ALL
		SELECT ` + mempoolBlobSelectColumns + ` FROM mempool_blobs
		WHERE chain_id = $2 AND versioned_hash = $1
		ORDER BY confirmed DESC, block_number DESC, timestamp DESC, tx_hash ASC, blob_index ASC
		LIMIT 1
	`

	// queryTopBlobUsersWithOptions aggregates windowed sender usage ($4 is '1h',
	// '24h', '7d', or '30d'; all-history reads use the queryTopBlobUsersAll*
	// variants) from chart rollups so windows stay O(buckets x senders) instead
	// of scanning raw blobs. The 1h window reads fine (60s) buckets aligned down
	// to the minute; wider windows read hourly buckets aligned down to the hour.
	// Windows have only the aligned lower bound and extend through the
	// in-progress bucket: results stay current through now (a leaderboard that
	// excluded the open bucket would lag by up to a full bucket), while the
	// bound itself moves only at bucket boundaries, so request-time jitter
	// never shifts the window and every cache layer (in-process, ETag, edge,
	// browser) serves one entry per URL between data changes. The traded cost
	// is that a window spans up to one extra bucket of history beyond its
	// nominal width. Fine buckets are trigger-maintained in the same
	// transaction as blob inserts (48h retention), so a 1h window is fully
	// covered on any database whose fine-rollup migration is at least an hour
	// old. Windows cover confirmed blobs only; exact last-seen times come from
	// blob_user_stats. The trailing from_address sort key makes ordering — and
	// therefore pagination — fully deterministic across ties.
	queryTopBlobUsersWithOptions = `
		WITH window_params AS (
			SELECT
				CASE WHEN $4 = '1h' THEN 60 ELSE 3600 END AS bucket_seconds,
				-- bucket_start is a naive UTC timestamp, so the bound must be
				-- computed in UTC wall time; bare NOW() would shift the window
				-- by the session TimeZone offset.
				CASE
					WHEN $4 = '1h' THEN date_trunc('minute', (NOW() AT TIME ZONE 'UTC') - INTERVAL '1 hour')
					WHEN $4 = '24h' THEN date_trunc('hour', (NOW() AT TIME ZONE 'UTC') - INTERVAL '24 hours')
					WHEN $4 = '30d' THEN date_trunc('hour', (NOW() AT TIME ZONE 'UTC') - INTERVAL '30 days')
					ELSE date_trunc('hour', (NOW() AT TIME ZONE 'UTC') - INTERVAL '7 days')
				END AS start_time
		),
		user_totals AS (
			SELECT
				r.from_address,
				COALESCE(NULLIF(MAX(BTRIM(r.user_attribution)), ''), NULLIF(MAX(BTRIM(bu.name)), ''), '') AS user_attribution,
				COALESCE(NULLIF(MAX(BTRIM(bu.category)), ''), 'unknown') AS category,
				COALESCE(SUM(r.blob_count), 0)::bigint AS blob_count,
				COALESCE(SUM(r.total_cost_wei), 0) AS total_cost_wei,
				MAX(r.bucket_start) AS last_bucket_start
			FROM blob_chart_rollups r
			CROSS JOIN window_params wp
			LEFT JOIN blob_users bu
				ON bu.chain_id = r.chain_id
				AND LOWER(bu.address) = LOWER(r.from_address)
			WHERE r.chain_id = $1
				AND r.bucket_seconds = wp.bucket_seconds
				AND r.bucket_start >= wp.start_time
			GROUP BY r.from_address
		),
		totals AS (
			SELECT
				COALESCE(SUM(blob_count), 0) AS total_blobs,
				COALESCE(SUM(total_cost_wei), 0) AS total_spend
			FROM user_totals
		)
		SELECT
			user_totals.from_address,
			user_totals.user_attribution,
			user_totals.category,
			user_totals.blob_count,
			user_totals.total_cost_wei::text AS total_cost_wei,
			COALESCE(s.last_timestamp, user_totals.last_bucket_start) AS last_timestamp,
			CASE
				WHEN totals.total_blobs > 0 THEN ROUND((user_totals.blob_count::numeric / totals.total_blobs::numeric) * 100, 6)::float8
				ELSE 0
			END AS blob_share_percent,
			CASE
				WHEN totals.total_spend > 0 THEN ROUND((user_totals.total_cost_wei / totals.total_spend) * 100, 6)::float8
				ELSE 0
			END AS spend_share_percent
		FROM user_totals
		LEFT JOIN blob_user_stats s
			ON s.chain_id = $1
			AND s.from_address = user_totals.from_address
		CROSS JOIN totals
		ORDER BY
			CASE WHEN $5 = 'count' THEN user_totals.blob_count END DESC,
			CASE WHEN $5 = 'spend' THEN user_totals.total_cost_wei END DESC,
			user_totals.blob_count DESC,
			user_totals.total_cost_wei DESC,
			user_totals.from_address ASC
		LIMIT $2 OFFSET $3
	`

	// queryTopBlobUsersAllBase reads all-history sender usage from maintained
	// rollups. The ORDER BY is appended statically per sort option (ByCount /
	// BySpend below) rather than via CASE expressions on a parameter, so the
	// planner can serve the ordered LIMIT scan straight from
	// idx_blob_user_stats_chain_count / idx_blob_user_stats_chain_spend.
	queryTopBlobUsersAllBase = `
		WITH user_totals AS (
			SELECT
				s.from_address,
				COALESCE(NULLIF(BTRIM(s.user_attribution), ''), NULLIF(BTRIM(bu.name), ''), '') AS user_attribution,
				COALESCE(NULLIF(BTRIM(bu.category), ''), 'unknown') AS category,
				s.blob_count,
				s.total_cost_wei,
				s.last_timestamp
			FROM blob_user_stats s
			LEFT JOIN blob_users bu
				ON bu.chain_id = s.chain_id
				AND LOWER(bu.address) = LOWER(s.from_address)
			WHERE s.chain_id = $1
				AND $4::text = 'all'
		),
		pending AS (
			SELECT
				COUNT(*)::bigint AS total_pending_blobs,
				COALESCE(SUM(total_cost_wei::numeric), 0) AS pending_total_cost
			FROM mempool_blobs
			WHERE chain_id = $1
		),
		totals AS (
			SELECT
				(COALESCE(s.total_confirmed_blobs, 0) + p.total_pending_blobs) AS total_blobs,
				(COALESCE(s.sum_total_cost, 0) + p.pending_total_cost) AS total_spend
			FROM pending p
			LEFT JOIN network_blob_stats s ON s.chain_id = $1
		)
		SELECT
			user_totals.from_address,
			user_totals.user_attribution,
			user_totals.category,
			user_totals.blob_count,
			user_totals.total_cost_wei::text AS total_cost_wei,
			user_totals.last_timestamp,
			CASE
				WHEN totals.total_blobs > 0 THEN ROUND((user_totals.blob_count::numeric / totals.total_blobs::numeric) * 100, 6)::float8
				ELSE 0
			END AS blob_share_percent,
			CASE
				WHEN totals.total_spend > 0 THEN ROUND((user_totals.total_cost_wei / totals.total_spend) * 100, 6)::float8
				ELSE 0
			END AS spend_share_percent
		FROM user_totals
		CROSS JOIN totals
	`

	queryTopBlobUsersAllByCount = queryTopBlobUsersAllBase + userSortByCountClause
	queryTopBlobUsersAllBySpend = queryTopBlobUsersAllBase + userSortBySpendClause

	// queryTopUnattributedBlobUsersWithOptions aggregates windowed sender usage
	// for addresses without either indexed attribution or a known blob_users
	// entry. Same rollup-backed window semantics as queryTopBlobUsersWithOptions.
	queryTopUnattributedBlobUsersWithOptions = `
		WITH window_params AS (
			SELECT
				CASE WHEN $4 = '1h' THEN 60 ELSE 3600 END AS bucket_seconds,
				-- bucket_start is a naive UTC timestamp, so the bound must be
				-- computed in UTC wall time; bare NOW() would shift the window
				-- by the session TimeZone offset.
				CASE
					WHEN $4 = '1h' THEN date_trunc('minute', (NOW() AT TIME ZONE 'UTC') - INTERVAL '1 hour')
					WHEN $4 = '24h' THEN date_trunc('hour', (NOW() AT TIME ZONE 'UTC') - INTERVAL '24 hours')
					WHEN $4 = '30d' THEN date_trunc('hour', (NOW() AT TIME ZONE 'UTC') - INTERVAL '30 days')
					ELSE date_trunc('hour', (NOW() AT TIME ZONE 'UTC') - INTERVAL '7 days')
				END AS start_time
		),
		user_totals AS (
			SELECT
				r.from_address,
				'' AS user_attribution,
				'unknown' AS category,
				COALESCE(SUM(r.blob_count), 0)::bigint AS blob_count,
				COALESCE(SUM(r.total_cost_wei), 0) AS total_cost_wei,
				MAX(r.bucket_start) AS last_bucket_start
			FROM blob_chart_rollups r
			CROSS JOIN window_params wp
			LEFT JOIN blob_users bu
				ON bu.chain_id = r.chain_id
				AND LOWER(bu.address) = LOWER(r.from_address)
			WHERE r.chain_id = $1
				AND r.bucket_seconds = wp.bucket_seconds
				AND r.bucket_start >= wp.start_time
			GROUP BY r.from_address
			HAVING NULLIF(MAX(BTRIM(r.user_attribution)), '') IS NULL
				AND MAX(bu.id) IS NULL
		),
		totals AS (
			SELECT
				COALESCE(SUM(blob_count), 0) AS total_blobs,
				COALESCE(SUM(total_cost_wei), 0) AS total_spend
			FROM user_totals
		)
		SELECT
			user_totals.from_address,
			user_totals.user_attribution,
			user_totals.category,
			user_totals.blob_count,
			user_totals.total_cost_wei::text AS total_cost_wei,
			COALESCE(s.last_timestamp, user_totals.last_bucket_start) AS last_timestamp,
			CASE
				WHEN totals.total_blobs > 0 THEN ROUND((user_totals.blob_count::numeric / totals.total_blobs::numeric) * 100, 6)::float8
				ELSE 0
			END AS blob_share_percent,
			CASE
				WHEN totals.total_spend > 0 THEN ROUND((user_totals.total_cost_wei / totals.total_spend) * 100, 6)::float8
				ELSE 0
			END AS spend_share_percent
		FROM user_totals
		LEFT JOIN blob_user_stats s
			ON s.chain_id = $1
			AND s.from_address = user_totals.from_address
		CROSS JOIN totals
		ORDER BY
			CASE WHEN $5 = 'count' THEN user_totals.blob_count END DESC,
			CASE WHEN $5 = 'spend' THEN user_totals.total_cost_wei END DESC,
			user_totals.blob_count DESC,
			user_totals.total_cost_wei DESC,
			user_totals.from_address ASC
		LIMIT $2 OFFSET $3
	`

	// queryTopUnattributedBlobUsersAllBase reads all-history unattributed sender
	// usage from maintained sender rollups. Same static-ORDER BY scheme as
	// queryTopBlobUsersAllBase.
	queryTopUnattributedBlobUsersAllBase = `
		WITH user_totals AS (
			SELECT
				s.from_address,
				'' AS user_attribution,
				'unknown' AS category,
				s.blob_count,
				s.total_cost_wei,
				s.last_timestamp
			FROM blob_user_stats s
			LEFT JOIN blob_users bu
				ON bu.chain_id = s.chain_id
				AND LOWER(bu.address) = LOWER(s.from_address)
			WHERE s.chain_id = $1
				AND $4::text = 'all'
				AND NULLIF(BTRIM(s.user_attribution), '') IS NULL
				AND bu.id IS NULL
		),
		totals AS (
			SELECT
				COALESCE(SUM(blob_count), 0) AS total_blobs,
				COALESCE(SUM(total_cost_wei), 0) AS total_spend
			FROM user_totals
		)
		SELECT
			user_totals.from_address,
			user_totals.user_attribution,
			user_totals.category,
			user_totals.blob_count,
			user_totals.total_cost_wei::text AS total_cost_wei,
			user_totals.last_timestamp,
			CASE
				WHEN totals.total_blobs > 0 THEN ROUND((user_totals.blob_count::numeric / totals.total_blobs::numeric) * 100, 6)::float8
				ELSE 0
			END AS blob_share_percent,
			CASE
				WHEN totals.total_spend > 0 THEN ROUND((user_totals.total_cost_wei / totals.total_spend) * 100, 6)::float8
				ELSE 0
			END AS spend_share_percent
		FROM user_totals
		CROSS JOIN totals
	`

	queryTopUnattributedBlobUsersAllByCount = queryTopUnattributedBlobUsersAllBase + userSortByCountClause
	queryTopUnattributedBlobUsersAllBySpend = queryTopUnattributedBlobUsersAllBase + userSortBySpendClause

	// userGroupTailSQL is the shared tail of the entity-grouped /users queries
	// (group=entity). It expects a `keyed` CTE of per-address rows
	// (from_address, user_attribution, category, blob_count, total_cost_wei,
	// last_timestamp, entity_slug — '' when the address is unattributed or its
	// attribution slugs to nothing) and a `totals` CTE carrying the same share
	// denominators as the per-address variant. Attributed rows collapse into
	// one row per entity slug — the same key scheme as the
	// /charts/attribution-usage series, so leaderboard rows and chart series
	// cross-reference — while unattributed rows pass through keyed by address.
	// Ordering and LIMIT/OFFSET run AFTER grouping, which is the point of the
	// grouped mode: an entity spread across many addresses ranks by its
	// combined totals instead of being truncated because individual addresses
	// fell below the per-address cutoff. Shares are computed from the exact
	// grouped sums, which equals summing the per-address shares without their
	// per-row rounding. Member addresses are ordered busiest first so the
	// first element serves as the group's primary address; the trailing
	// group_key + is_entity sort keys keep ordering and pagination
	// deterministic across ties, including the theoretical case of an
	// address-keyed row tying an entity whose slug equals that address
	// (attribution names are curated, so a hex-address-shaped name should
	// never actually ship). The bare entity_slug joins group_key in GROUP BY
	// both to license the slug references in the select list (the group-key
	// CASE alone would not) and to keep that colliding pair as two rows;
	// within a group the slug is constant, so the partitioning is unchanged.
	// The display name is always the busiest member's attribution spelling:
	// members can differ in case while sharing a slug (busiest-first is
	// deterministic where MIN would be collation-dependent), unattributed
	// rows yield '' naturally, and a name whose slug is degenerate keeps its
	// spelling on its address-keyed row instead of being blanked into a fake
	// unattributed sender.
	userGroupTailSQL = `,
		grouped AS (
			SELECT
				CASE WHEN k.entity_slug = '' THEN k.from_address ELSE k.entity_slug END AS group_key,
				(k.entity_slug <> '') AS is_entity,
				(array_agg(k.user_attribution ORDER BY k.blob_count DESC, k.total_cost_wei DESC, k.from_address ASC))[1] AS user_attribution,
				COALESCE(NULLIF(MIN(NULLIF(k.category, 'unknown')), ''), 'unknown') AS category,
				array_agg(k.from_address ORDER BY k.blob_count DESC, k.total_cost_wei DESC, k.from_address ASC) AS addresses,
				COALESCE(SUM(k.blob_count), 0)::bigint AS blob_count,
				COALESCE(SUM(k.total_cost_wei), 0) AS total_cost_wei,
				MAX(k.last_timestamp) AS last_timestamp
			FROM keyed k
			GROUP BY
				CASE WHEN k.entity_slug = '' THEN k.from_address ELSE k.entity_slug END,
				k.entity_slug
		)
		SELECT
			grouped.group_key,
			grouped.user_attribution,
			grouped.category,
			grouped.addresses,
			grouped.blob_count,
			grouped.total_cost_wei::text AS total_cost_wei,
			grouped.last_timestamp,
			CASE
				WHEN totals.total_blobs > 0 THEN ROUND((grouped.blob_count::numeric / totals.total_blobs::numeric) * 100, 6)::float8
				ELSE 0
			END AS blob_share_percent,
			CASE
				WHEN totals.total_spend > 0 THEN ROUND((grouped.total_cost_wei / totals.total_spend) * 100, 6)::float8
				ELSE 0
			END AS spend_share_percent
		FROM grouped
		CROSS JOIN totals
		ORDER BY
			CASE WHEN $5 = 'count' THEN grouped.blob_count END DESC,
			CASE WHEN $5 = 'spend' THEN grouped.total_cost_wei END DESC,
			grouped.blob_count DESC,
			grouped.total_cost_wei DESC,
			grouped.group_key ASC,
			grouped.is_entity DESC
		LIMIT $2 OFFSET $3
	`

	// userGroupSlugExpr normalizes an attribution name into its entity slug,
	// mirroring the /charts/attribution-usage key scheme
	// (attributionEntityBaseSQL): lowercase, non-ASCII-alphanumeric runs
	// collapsed to '_', outer underscores trimmed. Unattributed rows and
	// degenerate names that slug to nothing (e.g. fully non-ASCII) yield ''
	// so the tail keys them by address — such rows keep their attribution
	// name but cannot merge across addresses. This deliberately diverges from
	// the chart, which buckets empty slugs under its aggregate 'unknown'
	// series: a leaderboard row must stay addressable, and folding distinct
	// degenerate names into one pseudo-entity would fabricate a merged row.
	userGroupSlugExpr = `COALESCE(NULLIF(TRIM(BOTH '_' FROM regexp_replace(lower(ut.user_attribution), '[^a-z0-9]+', '_', 'g')), ''), '')`

	// queryTopBlobUserGroupsWithOptions is the entity-grouped variant of
	// queryTopBlobUsersWithOptions: identical per-address window aggregation
	// and share denominator (every sender in the window), then collapsed by
	// attribution entity via userGroupTailSQL. Same rollup-backed window
	// semantics and argument list ($1 chain, $2 limit, $3 offset, $4 window,
	// $5 sort) as the per-address query.
	queryTopBlobUserGroupsWithOptions = `
		WITH window_params AS (
			SELECT
				CASE WHEN $4 = '1h' THEN 60 ELSE 3600 END AS bucket_seconds,
				-- bucket_start is a naive UTC timestamp, so the bound must be
				-- computed in UTC wall time; bare NOW() would shift the window
				-- by the session TimeZone offset.
				CASE
					WHEN $4 = '1h' THEN date_trunc('minute', (NOW() AT TIME ZONE 'UTC') - INTERVAL '1 hour')
					WHEN $4 = '24h' THEN date_trunc('hour', (NOW() AT TIME ZONE 'UTC') - INTERVAL '24 hours')
					WHEN $4 = '30d' THEN date_trunc('hour', (NOW() AT TIME ZONE 'UTC') - INTERVAL '30 days')
					ELSE date_trunc('hour', (NOW() AT TIME ZONE 'UTC') - INTERVAL '7 days')
				END AS start_time
		),
		user_totals AS (
			SELECT
				r.from_address,
				COALESCE(NULLIF(MAX(BTRIM(r.user_attribution)), ''), NULLIF(MAX(BTRIM(bu.name)), ''), '') AS user_attribution,
				COALESCE(NULLIF(MAX(BTRIM(bu.category)), ''), 'unknown') AS category,
				COALESCE(SUM(r.blob_count), 0)::bigint AS blob_count,
				COALESCE(SUM(r.total_cost_wei), 0) AS total_cost_wei,
				MAX(r.bucket_start) AS last_bucket_start
			FROM blob_chart_rollups r
			CROSS JOIN window_params wp
			LEFT JOIN blob_users bu
				ON bu.chain_id = r.chain_id
				AND LOWER(bu.address) = LOWER(r.from_address)
			WHERE r.chain_id = $1
				AND r.bucket_seconds = wp.bucket_seconds
				AND r.bucket_start >= wp.start_time
			GROUP BY r.from_address
		),
		totals AS (
			SELECT
				COALESCE(SUM(blob_count), 0) AS total_blobs,
				COALESCE(SUM(total_cost_wei), 0) AS total_spend
			FROM user_totals
		),
		keyed AS (
			SELECT
				ut.from_address,
				ut.user_attribution,
				ut.category,
				ut.blob_count,
				ut.total_cost_wei,
				COALESCE(s.last_timestamp, ut.last_bucket_start) AS last_timestamp,
				` + userGroupSlugExpr + ` AS entity_slug
			FROM user_totals ut
			LEFT JOIN blob_user_stats s
				ON s.chain_id = $1
				AND s.from_address = ut.from_address
		)` + userGroupTailSQL

	// queryTopBlobUserGroupsAll is the entity-grouped variant of the
	// all-history queryTopBlobUsersAll* queries: identical per-address rollup
	// read and share denominator (network totals plus the pending set), then
	// collapsed by attribution entity via userGroupTailSQL. Unlike the
	// per-address all-history variants there is no static-ORDER BY split —
	// grouping has to aggregate every sender row anyway, so the ordered-LIMIT
	// index shortcut does not apply and the sort arrives as $5 like the
	// windowed query.
	queryTopBlobUserGroupsAll = `
		WITH user_totals AS (
			SELECT
				s.from_address,
				COALESCE(NULLIF(BTRIM(s.user_attribution), ''), NULLIF(BTRIM(bu.name), ''), '') AS user_attribution,
				COALESCE(NULLIF(BTRIM(bu.category), ''), 'unknown') AS category,
				s.blob_count,
				s.total_cost_wei,
				s.last_timestamp
			FROM blob_user_stats s
			LEFT JOIN blob_users bu
				ON bu.chain_id = s.chain_id
				AND LOWER(bu.address) = LOWER(s.from_address)
			WHERE s.chain_id = $1
				AND $4::text = 'all'
		),
		pending AS (
			SELECT
				COUNT(*)::bigint AS total_pending_blobs,
				COALESCE(SUM(total_cost_wei::numeric), 0) AS pending_total_cost
			FROM mempool_blobs
			WHERE chain_id = $1
		),
		totals AS (
			SELECT
				(COALESCE(s.total_confirmed_blobs, 0) + p.total_pending_blobs) AS total_blobs,
				(COALESCE(s.sum_total_cost, 0) + p.pending_total_cost) AS total_spend
			FROM pending p
			LEFT JOIN network_blob_stats s ON s.chain_id = $1
		),
		keyed AS (
			SELECT
				ut.from_address,
				ut.user_attribution,
				ut.category,
				ut.blob_count,
				ut.total_cost_wei,
				ut.last_timestamp,
				` + userGroupSlugExpr + ` AS entity_slug
			FROM user_totals ut
		)` + userGroupTailSQL

	// queryBlobUserCategoryBreakdown aggregates windowed blob usage by known user
	// category ($2 is '1h', '24h', '7d', or '30d'; all-history reads use
	// queryBlobUserCategoryBreakdownAll). Same rollup-backed window semantics as
	// queryTopBlobUsersWithOptions.
	queryBlobUserCategoryBreakdown = `
		WITH window_params AS (
			SELECT
				CASE WHEN $2 = '1h' THEN 60 ELSE 3600 END AS bucket_seconds,
				-- bucket_start is a naive UTC timestamp, so the bound must be
				-- computed in UTC wall time; bare NOW() would shift the window
				-- by the session TimeZone offset.
				CASE
					WHEN $2 = '1h' THEN date_trunc('minute', (NOW() AT TIME ZONE 'UTC') - INTERVAL '1 hour')
					WHEN $2 = '24h' THEN date_trunc('hour', (NOW() AT TIME ZONE 'UTC') - INTERVAL '24 hours')
					WHEN $2 = '30d' THEN date_trunc('hour', (NOW() AT TIME ZONE 'UTC') - INTERVAL '30 days')
					ELSE date_trunc('hour', (NOW() AT TIME ZONE 'UTC') - INTERVAL '7 days')
				END AS start_time
		),
		category_totals AS (
			SELECT
				COALESCE(NULLIF(BTRIM(bu.category), ''), 'unknown') AS category,
				COALESCE(SUM(r.blob_count), 0)::bigint AS blob_count,
				COALESCE(SUM(r.total_cost_wei), 0) AS total_cost_wei
			FROM blob_chart_rollups r
			CROSS JOIN window_params wp
			LEFT JOIN blob_users bu
				ON bu.chain_id = r.chain_id
				AND LOWER(bu.address) = LOWER(r.from_address)
			WHERE r.chain_id = $1
				AND r.bucket_seconds = wp.bucket_seconds
				AND r.bucket_start >= wp.start_time
			GROUP BY COALESCE(NULLIF(BTRIM(bu.category), ''), 'unknown')
		),
		totals AS (
			SELECT
				COALESCE(SUM(blob_count), 0) AS total_blobs,
				COALESCE(SUM(total_cost_wei), 0) AS total_spend
			FROM category_totals
		)
		SELECT
			category_totals.category,
			category_totals.blob_count,
			category_totals.total_cost_wei::text AS total_cost_wei,
			CASE
				WHEN totals.total_blobs > 0 THEN ROUND((category_totals.blob_count::numeric / totals.total_blobs::numeric) * 100, 6)::float8
				ELSE 0
			END AS blob_share_percent,
			CASE
				WHEN totals.total_spend > 0 THEN ROUND((category_totals.total_cost_wei / totals.total_spend) * 100, 6)::float8
				ELSE 0
			END AS spend_share_percent
		FROM category_totals
		CROSS JOIN totals
		ORDER BY category_totals.blob_count DESC, category_totals.total_cost_wei DESC, category_totals.category ASC
	`

	// queryBlobUserCategoryBreakdownAll reads all-history category share from
	// maintained sender rollups.
	queryBlobUserCategoryBreakdownAll = `
		WITH category_totals AS (
			SELECT
				COALESCE(NULLIF(BTRIM(bu.category), ''), 'unknown') AS category,
				COALESCE(SUM(s.blob_count), 0) AS blob_count,
				COALESCE(SUM(s.total_cost_wei), 0) AS total_cost_wei
			FROM blob_user_stats s
			LEFT JOIN blob_users bu
				ON bu.chain_id = s.chain_id
				AND LOWER(bu.address) = LOWER(s.from_address)
			WHERE s.chain_id = $1
				AND $2::text = 'all'
			GROUP BY COALESCE(NULLIF(BTRIM(bu.category), ''), 'unknown')
		),
		pending AS (
			SELECT
				COUNT(*)::bigint AS total_pending_blobs,
				COALESCE(SUM(total_cost_wei::numeric), 0) AS pending_total_cost
			FROM mempool_blobs
			WHERE chain_id = $1
		),
		totals AS (
			SELECT
				(COALESCE(s.total_confirmed_blobs, 0) + p.total_pending_blobs) AS total_blobs,
				(COALESCE(s.sum_total_cost, 0) + p.pending_total_cost) AS total_spend
			FROM pending p
			LEFT JOIN network_blob_stats s ON s.chain_id = $1
		)
		SELECT
			category_totals.category,
			category_totals.blob_count,
			category_totals.total_cost_wei::text AS total_cost_wei,
			CASE
				WHEN totals.total_blobs > 0 THEN ROUND((category_totals.blob_count::numeric / totals.total_blobs::numeric) * 100, 6)::float8
				ELSE 0
			END AS blob_share_percent,
			CASE
				WHEN totals.total_spend > 0 THEN ROUND((category_totals.total_cost_wei / totals.total_spend) * 100, 6)::float8
				ELSE 0
			END AS spend_share_percent
		FROM category_totals
		CROSS JOIN totals
		ORDER BY category_totals.blob_count DESC, category_totals.total_cost_wei DESC, category_totals.category ASC
	`

	// queryBlobStats reads whole-history statistics from the maintained network summary.
	// The pending set is a trivially small dedicated table, so it is folded in at read time.
	queryBlobStats = `
		WITH pending AS (
			SELECT
				COUNT(*)::bigint AS total_pending_blobs,
				COALESCE(SUM(base_fee_per_blob_gas::numeric), 0) AS pending_base_fee,
				COALESCE(SUM(tip_per_blob_gas::numeric), 0) AS pending_tip,
				COALESCE(SUM(total_cost_wei::numeric), 0) AS pending_total_cost
			FROM mempool_blobs
			WHERE chain_id = $1
		)
		SELECT
			(COALESCE(s.total_confirmed_blobs, 0) + p.total_pending_blobs) AS total_blobs,
			COALESCE(s.total_confirmed_blobs, 0) AS total_confirmed_blobs,
			p.total_pending_blobs AS total_pending_blobs,
			CASE
				WHEN (COALESCE(s.total_confirmed_blobs, 0) + p.total_pending_blobs) > 0
				THEN (
					(COALESCE(s.sum_base_fee_per_blob_gas, 0) + p.pending_base_fee)
					/ (COALESCE(s.total_confirmed_blobs, 0) + p.total_pending_blobs)
				)::text
				ELSE '0'
			END AS average_base_fee,
			CASE
				WHEN (COALESCE(s.total_confirmed_blobs, 0) + p.total_pending_blobs) > 0
				THEN (
					(COALESCE(s.sum_tip_per_blob_gas, 0) + p.pending_tip)
					/ (COALESCE(s.total_confirmed_blobs, 0) + p.total_pending_blobs)
				)::text
				ELSE '0'
			END AS average_tip,
			CASE
				WHEN (COALESCE(s.total_confirmed_blobs, 0) + p.total_pending_blobs) > 0
				THEN (
					(COALESCE(s.sum_total_cost, 0) + p.pending_total_cost)
					/ (COALESCE(s.total_confirmed_blobs, 0) + p.total_pending_blobs)
				)::text
				ELSE '0'
			END AS average_total_cost,
			COALESCE(s.last_indexed_block, 0) AS last_indexed_block,
			COALESCE(s.last_indexed_time, '1970-01-01'::timestamp) AS last_indexed_time
		FROM pending p
		LEFT JOIN network_blob_stats s ON s.chain_id = $1
	`

	// queryRollingStatsWindows computes time-windowed blob market statistics
	// with one bounded materialized scan per source table.
	queryRollingStatsWindows = `
		WITH requested_windows AS (
			SELECT window_label, duration_seconds, ord
			FROM unnest($2::text[], $3::bigint[]) WITH ORDINALITY AS u(window_label, duration_seconds, ord)
		),
		window_bounds AS (
			SELECT
				window_label,
				duration_seconds,
				ord,
				$4::timestamp - (duration_seconds * INTERVAL '1 second') AS start_time,
				$4::timestamp AS end_time
			FROM requested_windows
		),
		blob_source AS MATERIALIZED (
			SELECT
				b.timestamp,
				b.from_address,
				b.base_fee_per_blob_gas::numeric AS base_fee_per_blob_gas,
				b.total_cost_wei::numeric AS total_cost_wei,
				COALESCE(b.blob_gas_used, 0)::bigint AS blob_gas_used
			FROM blobs b
			WHERE b.chain_id = $1
				AND b.timestamp >= (SELECT MIN(start_time) FROM window_bounds)
				AND b.timestamp < $4::timestamp
		),
		blob_windows AS (
			SELECT
				wb.ord,
				COUNT(bs.timestamp) AS total_blobs,
				COALESCE(SUM(bs.blob_gas_used), 0) AS total_blob_gas_used,
				COALESCE(SUM(bs.total_cost_wei), 0) AS total_cost_wei,
				COUNT(DISTINCT bs.from_address) AS unique_senders,
				COALESCE(AVG(bs.base_fee_per_blob_gas), 0) AS average_blob_base_fee,
				COALESCE(percentile_disc(0.5) WITHIN GROUP (ORDER BY bs.base_fee_per_blob_gas), 0) AS median_blob_base_fee,
				COALESCE(percentile_disc(0.95) WITHIN GROUP (ORDER BY bs.base_fee_per_blob_gas), 0) AS p95_blob_base_fee
			FROM window_bounds wb
			LEFT JOIN blob_source bs
				ON bs.timestamp >= wb.start_time
				AND bs.timestamp < wb.end_time
			GROUP BY wb.ord
		),
		metric_source AS MATERIALIZED (
			SELECT
				bm.block_timestamp,
				bm.utilization_ratio::numeric AS utilization_ratio,
				GREATEST(bm.blob_gas_used, 0)::bigint AS blob_gas_used,
				-- Effective target/max mirror effectiveBlobTargetGas/effectiveBlobMaxGas:
				-- prefer the per-block gas columns, fall back to blob params * 131072
				-- (params.BlobTxBlobGasPerBlob). Blocks with neither are left unclassified.
				CASE
					WHEN bm.blob_gas_target > 0 THEN bm.blob_gas_target
					WHEN bm.blob_params_target > 0 THEN bm.blob_params_target::bigint * 131072
					ELSE 0
				END::bigint AS target_blob_gas,
				CASE
					WHEN bm.blob_gas_limit > 0 THEN bm.blob_gas_limit
					WHEN bm.blob_params_max > 0 THEN bm.blob_params_max::bigint * 131072
					ELSE 0
				END::bigint AS max_blob_gas
			FROM block_metrics bm
			WHERE bm.chain_id = $1
				AND bm.block_timestamp >= (SELECT MIN(start_time) FROM window_bounds)
				AND bm.block_timestamp < $4::timestamp
		),
		metric_windows AS (
			SELECT
				wb.ord,
				COALESCE(AVG(ms.utilization_ratio), 0) AS average_utilization,
				COUNT(ms.block_timestamp) AS total_blocks,
				COUNT(*) FILTER (
					WHERE ms.target_blob_gas > 0 AND ms.blob_gas_used > ms.target_blob_gas
				) AS blocks_above_target,
				COUNT(*) FILTER (
					WHERE ms.max_blob_gas > 0 AND ms.blob_gas_used >= ms.max_blob_gas
				) AS blocks_at_max
			FROM window_bounds wb
			LEFT JOIN metric_source ms
				ON ms.block_timestamp >= wb.start_time
				AND ms.block_timestamp < wb.end_time
			GROUP BY wb.ord
		)
		SELECT
			wb.window_label AS stats_window,
			wb.duration_seconds,
			wb.start_time,
			wb.end_time,
			bs.average_blob_base_fee,
			bs.median_blob_base_fee,
			bs.p95_blob_base_fee,
			bs.total_blobs,
			bs.total_blob_gas_used,
			bs.total_cost_wei,
			bs.unique_senders,
			bms.average_utilization,
			bms.total_blocks,
			bms.blocks_above_target,
			bms.blocks_at_max
		FROM window_bounds wb
		LEFT JOIN blob_windows bs ON bs.ord = wb.ord
		LEFT JOIN metric_windows bms ON bms.ord = wb.ord
		ORDER BY wb.ord
	`

	// queryFineRollupCoverageStart reports the earliest fine (60s) rollup
	// bucket for a network. Fine buckets are trigger-maintained from the
	// fine_chart_rollups migration onward and backfilled across the retention
	// window by the indexer, so a
	// window or chart range starting before this timestamp is not fully
	// covered and must fall back to raw scans. block_metrics_rollups is the
	// coverage signal because blocks are continuous while blob buckets are
	// sparse. MIN is a sound signal because the backfill runs newest-first in
	// atomic chunks: completed fine coverage is always contiguous from this
	// timestamp to now, even if a backfill run aborted partway.
	queryFineRollupCoverageStart = `
		SELECT MIN(bucket_start) FROM block_metrics_rollups
		WHERE chain_id = $1 AND bucket_seconds = 60
	`

	// queryRollingStatsWindowsFine serves rolling windows up to the raw cutoff
	// from fine (60s) chart rollups instead of raw scans, reading O(minutes)
	// pre-aggregated rows. Windows align down to the last completed minute, so
	// results lag real time by up to 60 seconds and always span exactly the
	// advertised duration. Sums, averages,
	// and unique-sender counts are exact over the aligned window (per-sender
	// bucket rows keep COUNT(DISTINCT) exact); median and p95 are estimated
	// from per-minute medians: the window median is the median of minute
	// medians, and the window p95 is the 95th percentile of minute medians.
	// Minute buckets (~5 blocks) are much finer than fee variation, so a
	// minute's median stands in for its blocks and percentiles across minutes
	// track the true per-blob percentiles closely — unlike the hourly path's
	// median-of-bucket-p95s, which at this granularity would collapse toward
	// the median because a single minute has almost no internal spread.
	queryRollingStatsWindowsFine = `
		WITH requested_windows AS (
			SELECT window_label, duration_seconds, ord
			FROM unnest($2::text[], $3::bigint[]) WITH ORDINALITY AS u(window_label, duration_seconds, ord)
		),
		window_bounds AS (
			SELECT
				window_label,
				duration_seconds,
				ord,
				date_trunc('minute', $4::timestamp) - (duration_seconds * INTERVAL '1 second') AS start_time,
				date_trunc('minute', $4::timestamp) AS end_time
			FROM requested_windows
		),
		blob_windows AS (
			SELECT
				wb.ord,
				COALESCE(SUM(r.blob_count), 0)::bigint AS total_blobs,
				COALESCE(SUM(r.blob_gas_used), 0)::bigint AS total_blob_gas_used,
				COALESCE(SUM(r.total_cost_wei), 0) AS total_cost_wei,
				COUNT(DISTINCT r.from_address) AS unique_senders,
				CASE
					WHEN COALESCE(SUM(r.blob_bytes), 0) > 0 THEN SUM(r.sum_size_base_fee) / SUM(r.blob_bytes)
					ELSE 0
				END AS average_blob_base_fee
			FROM window_bounds wb
			LEFT JOIN blob_chart_rollups r
				ON r.chain_id = $1
				AND r.bucket_seconds = 60
				AND r.bucket_start >= wb.start_time
				AND r.bucket_start < wb.end_time
			GROUP BY wb.ord
		),
		metric_windows AS (
			SELECT
				wb.ord,
				CASE
					WHEN COALESCE(SUM(r.block_count), 0) > 0 THEN SUM(r.sum_utilization) / SUM(r.block_count)
					ELSE 0
				END AS average_utilization,
				COALESCE(SUM(r.block_count), 0)::bigint AS total_blocks,
				COALESCE(SUM(r.blocks_above_target), 0)::bigint AS blocks_above_target,
				COALESCE(SUM(r.blocks_at_max), 0)::bigint AS blocks_at_max,
				COALESCE(percentile_disc(0.5) WITHIN GROUP (ORDER BY r.median_blob_base_fee), 0) AS median_blob_base_fee,
				COALESCE(percentile_disc(0.95) WITHIN GROUP (ORDER BY r.median_blob_base_fee), 0) AS p95_blob_base_fee
			FROM window_bounds wb
			LEFT JOIN block_metrics_rollups r
				ON r.chain_id = $1
				AND r.bucket_seconds = 60
				AND r.bucket_start >= wb.start_time
				AND r.bucket_start < wb.end_time
			GROUP BY wb.ord
		)
		SELECT
			wb.window_label AS stats_window,
			wb.duration_seconds,
			wb.start_time,
			wb.end_time,
			COALESCE(bs.average_blob_base_fee, 0) AS average_blob_base_fee,
			COALESCE(bms.median_blob_base_fee, 0) AS median_blob_base_fee,
			COALESCE(bms.p95_blob_base_fee, 0) AS p95_blob_base_fee,
			COALESCE(bs.total_blobs, 0) AS total_blobs,
			COALESCE(bs.total_blob_gas_used, 0) AS total_blob_gas_used,
			COALESCE(bs.total_cost_wei, 0) AS total_cost_wei,
			COALESCE(bs.unique_senders, 0) AS unique_senders,
			COALESCE(bms.average_utilization, 0) AS average_utilization,
			COALESCE(bms.total_blocks, 0) AS total_blocks,
			COALESCE(bms.blocks_above_target, 0) AS blocks_above_target,
			COALESCE(bms.blocks_at_max, 0) AS blocks_at_max
		FROM window_bounds wb
		LEFT JOIN blob_windows bs ON bs.ord = wb.ord
		LEFT JOIN metric_windows bms ON bms.ord = wb.ord
		ORDER BY wb.ord
	`

	// queryRollingStatsWindowsRollup serves rolling windows longer than the raw
	// cutoff from hourly chart rollups, staying O(buckets x senders) instead of
	// scanning raw rows. Windows align down to the rollup hour: the end is the
	// last completed hour boundary and the start is derived from it, so the
	// served window always spans exactly the advertised duration (the
	// in-progress hour is excluded, which at these widths is noise). Rollups
	// cover confirmed blobs only. Sums, averages, and unique-sender counts are
	// exact over the aligned window; median and p95 are estimated from
	// per-bucket values (median of hourly medians/p95s), which discards
	// within-vs-across hour weighting but tracks the true percentile closely
	// at these widths.
	queryRollingStatsWindowsRollup = `
		WITH requested_windows AS (
			SELECT window_label, duration_seconds, ord
			FROM unnest($2::text[], $3::bigint[]) WITH ORDINALITY AS u(window_label, duration_seconds, ord)
		),
		window_bounds AS (
			SELECT
				window_label,
				duration_seconds,
				ord,
				date_trunc('hour', $4::timestamp) - (duration_seconds * INTERVAL '1 second') AS start_time,
				date_trunc('hour', $4::timestamp) AS end_time
			FROM requested_windows
		),
		blob_windows AS (
			SELECT
				wb.ord,
				COALESCE(SUM(r.blob_count), 0)::bigint AS total_blobs,
				COALESCE(SUM(r.blob_gas_used), 0)::bigint AS total_blob_gas_used,
				COALESCE(SUM(r.total_cost_wei), 0) AS total_cost_wei,
				COUNT(DISTINCT r.from_address) AS unique_senders,
				CASE
					WHEN COALESCE(SUM(r.blob_bytes), 0) > 0 THEN SUM(r.sum_size_base_fee) / SUM(r.blob_bytes)
					ELSE 0
				END AS average_blob_base_fee
			FROM window_bounds wb
			LEFT JOIN blob_chart_rollups r
				ON r.chain_id = $1
				AND r.bucket_seconds = 3600
				AND r.bucket_start >= wb.start_time
				AND r.bucket_start < wb.end_time
			GROUP BY wb.ord
		),
		metric_windows AS (
			SELECT
				wb.ord,
				CASE
					WHEN COALESCE(SUM(r.block_count), 0) > 0 THEN SUM(r.sum_utilization) / SUM(r.block_count)
					ELSE 0
				END AS average_utilization,
				COALESCE(SUM(r.block_count), 0)::bigint AS total_blocks,
				COALESCE(SUM(r.blocks_above_target), 0)::bigint AS blocks_above_target,
				COALESCE(SUM(r.blocks_at_max), 0)::bigint AS blocks_at_max,
				COALESCE(percentile_disc(0.5) WITHIN GROUP (ORDER BY r.median_blob_base_fee), 0) AS median_blob_base_fee,
				COALESCE(percentile_disc(0.5) WITHIN GROUP (ORDER BY r.p95_blob_base_fee), 0) AS p95_blob_base_fee
			FROM window_bounds wb
			LEFT JOIN block_metrics_rollups r
				ON r.chain_id = $1
				AND r.bucket_seconds = 3600
				AND r.bucket_start >= wb.start_time
				AND r.bucket_start < wb.end_time
			GROUP BY wb.ord
		)
		SELECT
			wb.window_label AS stats_window,
			wb.duration_seconds,
			wb.start_time,
			wb.end_time,
			COALESCE(bs.average_blob_base_fee, 0) AS average_blob_base_fee,
			COALESCE(bms.median_blob_base_fee, 0) AS median_blob_base_fee,
			COALESCE(bms.p95_blob_base_fee, 0) AS p95_blob_base_fee,
			COALESCE(bs.total_blobs, 0) AS total_blobs,
			COALESCE(bs.total_blob_gas_used, 0) AS total_blob_gas_used,
			COALESCE(bs.total_cost_wei, 0) AS total_cost_wei,
			COALESCE(bs.unique_senders, 0) AS unique_senders,
			COALESCE(bms.average_utilization, 0) AS average_utilization,
			COALESCE(bms.total_blocks, 0) AS total_blocks,
			COALESCE(bms.blocks_above_target, 0) AS blocks_above_target,
			COALESCE(bms.blocks_at_max, 0) AS blocks_at_max
		FROM window_bounds wb
		LEFT JOIN blob_windows bs ON bs.ord = wb.ord
		LEFT JOIN metric_windows bms ON bms.ord = wb.ord
		ORDER BY wb.ord
	`

	// queryBlockMetrics retrieves recent block metrics for pricing data: the N
	// most recently indexed blocks, newest first. Every indexed block has a row
	// (including zero-blob blocks), so gaps appear only where a slot was missed
	// or a block's commit is still in flight — the indexer commits blocks
	// concurrently, so a block can briefly land before its predecessor. The
	// (chain_id, block_number) primary key serves this as a single backward
	// range scan.
	queryBlockMetrics = `
		SELECT ` + blockMetricsSelectColumns + ` FROM block_metrics
		WHERE chain_id = $1
		ORDER BY block_number DESC
		LIMIT $2
	`

	// queryBlockMetricsByNumber retrieves block metrics for specific block numbers.
	queryBlockMetricsByNumber = `
		SELECT ` + blockMetricsSelectColumns + ` FROM block_metrics
		WHERE chain_id = $1 AND block_number = ANY($2::bigint[])
	`

	// queryBlockMetricsForBlock retrieves the block_metrics row of a single
	// block for /block/{number}; every indexed block has a row, so no row
	// means the block is not indexed.
	queryBlockMetricsForBlock = `
		SELECT ` + blockMetricsSelectColumns + ` FROM block_metrics
		WHERE chain_id = $1 AND block_number = $2
	`

	// queryLatestBlobsByAddress retrieves confirmed blobs for a specific sender
	// address. Ordered by timestamp — for confirmed rows block-timestamp order
	// is block order — so idx_blobs_chain_from_timestamp serves the top-N
	// directly; ordering by block_number has no matching index and degrades to
	// fetching the sender's entire history and sorting it.
	queryLatestBlobsByAddress = `
		SELECT ` + blobSelectColumns + ` FROM blobs
		WHERE chain_id = $1 AND from_address = $2
		ORDER BY timestamp DESC, blob_index ASC
		LIMIT $3 OFFSET $4
	`

	// queryMempoolBlobsByAddress retrieves pending blobs for a specific sender address.
	queryMempoolBlobsByAddress = `
		SELECT ` + mempoolBlobSelectColumns + ` FROM mempool_blobs
		WHERE chain_id = $1 AND from_address = $2
		ORDER BY timestamp DESC
		LIMIT $3 OFFSET $4
	`

	// queryBlobReplacements lists observed replacement events, newest first.
	queryBlobReplacements = `
		SELECT replaced_tx_hash, replacement_tx_hash, from_address, nonce, replaced_at
		FROM blob_replacements
		WHERE chain_id = $1
		ORDER BY replaced_at DESC
		LIMIT $2 OFFSET $3
	`

	// queryBlobReplacementsByTxHash resolves the replacement events touching
	// one transaction hash, on either side of the replacement.
	queryBlobReplacementsByTxHash = `
		SELECT replaced_tx_hash, replacement_tx_hash, from_address, nonce, replaced_at
		FROM blob_replacements
		WHERE chain_id = $1 AND (replaced_tx_hash = $2 OR replacement_tx_hash = $2)
		ORDER BY replaced_at DESC
		LIMIT $3 OFFSET $4
	`

	// queryUserByAddress retrieves aggregated stats for a single sender address.
	queryUserByAddress = `
		WITH selected_user AS (
			SELECT
				s.from_address,
				COALESCE(NULLIF(BTRIM(s.user_attribution), ''), NULLIF(BTRIM(bu.name), ''), '') AS user_attribution,
				COALESCE(NULLIF(BTRIM(bu.category), ''), 'unknown') AS category,
				s.blob_count,
				s.total_cost_wei,
				s.last_timestamp
			FROM blob_user_stats s
			LEFT JOIN blob_users bu
				ON bu.chain_id = s.chain_id
				AND LOWER(bu.address) = LOWER(s.from_address)
			WHERE s.chain_id = $1 AND s.from_address = $2
		),
		pending AS (
			SELECT
				COUNT(*)::bigint AS total_pending_blobs,
				COALESCE(SUM(total_cost_wei::numeric), 0) AS pending_total_cost
			FROM mempool_blobs
			WHERE chain_id = $1
		),
		totals AS (
			SELECT
				(COALESCE(s.total_confirmed_blobs, 0) + p.total_pending_blobs) AS total_blobs,
				(COALESCE(s.sum_total_cost, 0) + p.pending_total_cost) AS total_spend
			FROM pending p
			LEFT JOIN network_blob_stats s ON s.chain_id = $1
		)
		SELECT
			selected_user.from_address,
			selected_user.user_attribution,
			selected_user.category,
			selected_user.blob_count,
			selected_user.total_cost_wei::text AS total_cost_wei,
			selected_user.last_timestamp,
			CASE
				WHEN totals.total_blobs > 0 THEN ROUND((selected_user.blob_count::numeric / totals.total_blobs::numeric) * 100, 6)::float8
				ELSE 0
			END AS blob_share_percent,
			CASE
				WHEN totals.total_spend > 0 THEN ROUND((selected_user.total_cost_wei / totals.total_spend) * 100, 6)::float8
				ELSE 0
			END AS spend_share_percent
		FROM selected_user
		CROSS JOIN totals
	`

	// queryLastIndexedTimeCoalesce retrieves the most recent confirmed blob timestamp,
	// defaulting to epoch if no blobs exist.
	queryLastIndexedTimeCoalesce = "SELECT COALESCE(MAX(timestamp), '1970-01-01'::timestamp) FROM blobs WHERE chain_id = $1"

	// queryNetworkLastIndexedTime reads the trigger-maintained last indexed
	// block timestamp from the single-row network summary — a primary-key
	// lookup, unlike a MAX() probe over the blobs table. MAX() over the 0-or-1
	// matching rows folds the missing-network case to epoch without ErrNoRows
	// handling.
	queryNetworkLastIndexedTime = "SELECT COALESCE(MAX(last_indexed_time), '1970-01-01'::timestamp) FROM network_blob_stats WHERE chain_id = $1"

	// queryIndexedBlockCoverage reports the indexed block range for a network
	// from indexed_blocks, the canonical per-block coverage record (the indexer
	// writes one row per indexed block and uses it for gap detection). The
	// (chain_id, block_number) primary key turns both aggregates into single
	// index probes, so this stays cheap under /status polling. The bounds are
	// sparse extremes, not a contiguity claim: the range can contain interior
	// gaps while failed blocks retry and the gap scanner backfills. Both
	// bounds are NULL when the network has no indexed blocks.
	queryIndexedBlockCoverage = `
		SELECT
			MIN(block_number) AS earliest_indexed_block,
			MAX(block_number) AS latest_indexed_block
		FROM indexed_blocks
		WHERE chain_id = $1
	`

	// userSortByCountClause / userSortBySpendClause terminate the all-history
	// top-user queries with static sort keys that line up with
	// idx_blob_user_stats_chain_count and idx_blob_user_stats_chain_spend. The
	// trailing from_address key makes ordering fully deterministic for
	// pagination; it sits outside the indexes, but full three-key ties are rare
	// enough that the incremental sort it forces almost never runs.
	userSortByCountClause = `
		ORDER BY
			user_totals.blob_count DESC,
			user_totals.total_cost_wei DESC,
			user_totals.last_timestamp DESC,
			user_totals.from_address ASC
		LIMIT $2 OFFSET $3
	`
	userSortBySpendClause = `
		ORDER BY
			user_totals.total_cost_wei DESC,
			user_totals.blob_count DESC,
			user_totals.last_timestamp DESC,
			user_totals.from_address ASC
		LIMIT $2 OFFSET $3
	`

	// queryTableSize retrieves the total size of a table in bytes.
	queryTableSize = `
		SELECT pg_total_relation_size($1)
	`

	// queryIndexCount counts the number of indexes on a table.
	queryIndexCount = `
		SELECT COUNT(*)
		FROM pg_indexes
		WHERE tablename = $1
	`

	// queryDatabaseSize retrieves the total size of the current database.
	queryDatabaseSize = `
		SELECT pg_database_size(current_database())
	`

	// queryLastIndexedBlock retrieves the last indexed block number from indexer metadata.
	queryLastIndexedBlock = "SELECT value FROM indexer_metadata WHERE chain_id = $1 AND key = '" + models.MetadataLastIndexedBlock + "'"

	// queryNetworkFreshnessMetadata retrieves frontend freshness metadata for a network.
	queryNetworkFreshnessMetadata = `
		SELECT key, value
		FROM indexer_metadata
		WHERE chain_id = $1
			AND key IN (
				'` + models.MetadataLastIndexedBlock + `',
					'` + models.MetadataCurrentChainHead + `',
					'` + models.MetadataChainHeadUpdatedAt + `',
					'` + models.MetadataLastIndexedAt + `',
					'` + models.MetadataWebSocketFreshnessAt + `',
					'` + models.MetadataBackfillActive + `',
					'` + models.MetadataBackfillStartBlock + `',
					'` + models.MetadataBackfillCurrentBlock + `',
					'` + models.MetadataBackfillTargetBlock + `',
					'` + models.MetadataBackfillUpdatedAt + `',
					'` + models.MetadataBackfillCompletedAt + `'
				)
		`

	// queryRecentBlockMetricsNumbers lists the newest block numbers with a
	// block_metrics row for a network. The WebSocket poller's startup baseline
	// seeds its seen-set from one trailing window of these so the first
	// catch-up scan does not replay already-indexed history.
	queryRecentBlockMetricsNumbers = `
		SELECT block_number FROM block_metrics
		WHERE chain_id = $1
		ORDER BY block_number DESC
		LIMIT $2
	`

	// queryBlockMetricsNumbersSince lists block numbers with a block_metrics row
	// after a given block. The WebSocket poller's catch-up scan uses it to find
	// committed blocks whose NOTIFY was missed (listener down or reconnecting),
	// including late out-of-order commits behind the broadcast head.
	queryBlockMetricsNumbersSince = `
		SELECT block_number FROM block_metrics
		WHERE chain_id = $1 AND block_number > $2
		ORDER BY block_number ASC
		LIMIT $3
	`

	// queryBlobsByBlockNumber retrieves the confirmed blobs of a single block
	// for a new_block WebSocket broadcast.
	queryBlobsByBlockNumber = `
		SELECT ` + blobSelectColumns + ` FROM blobs
		WHERE chain_id = $1 AND block_number = $2
		ORDER BY blob_index ASC
	`

	// queryBlobsByBlockNumbers retrieves confirmed blobs for a set of blocks —
	// used to assemble the block_snapshot sent to newly connected WebSocket
	// clients.
	queryBlobsByBlockNumbers = `
		SELECT ` + blobSelectColumns + ` FROM blobs
		WHERE chain_id = $1 AND block_number = ANY($2::bigint[])
		ORDER BY block_number DESC, blob_index ASC
	`

	// queryPendingBlobTxHashes lists the distinct tx hashes of every pending
	// blob for a network — an index-only scan of the mempool_blobs primary key
	// (chain_id, tx_hash, blob_index) — so the WebSocket poller can diff the
	// mempool each tick without fetching full rows. DISTINCT collapses
	// multi-blob transactions (one pending row per blob) to one hash.
	queryPendingBlobTxHashes = "SELECT DISTINCT tx_hash FROM mempool_blobs WHERE chain_id = $1"

	// queryPendingBlobsByTxHashes fetches full pending rows for the (typically
	// zero or few) tx hashes that are new since the poller's previous tick.
	// blob_index ASC lets the caller keep the first blob per tx, mirroring
	// queryBlobByTxHash's first-blob convention for multi-blob transactions.
	queryPendingBlobsByTxHashes = `
		SELECT ` + mempoolBlobSelectColumns + ` FROM mempool_blobs
		WHERE chain_id = $1 AND tx_hash = ANY($2)
		ORDER BY timestamp DESC, blob_index ASC
	`

	// querySearchBlockByNumber probes whether a block is indexed — a
	// block_metrics primary-key lookup. block_metrics is the canonical
	// per-block table: every indexed block has a row, including zero-blob
	// blocks.
	querySearchBlockByNumber = `
		SELECT block_number FROM block_metrics
		WHERE chain_id = $1 AND block_number = $2
	`

	// querySearchTxByHash resolves a 64-hex search query as a blob transaction
	// hash, preferring a confirmed row over a pending one (pending rows sort
	// last via the block-number sentinel). Served by idx_blobs_chain_txhash and
	// the mempool_blobs primary key.
	querySearchTxByHash = `
		SELECT tx_hash, block_number FROM (
			SELECT tx_hash, block_number FROM blobs
			WHERE chain_id = $1 AND tx_hash = $2
			UNION ALL
			SELECT tx_hash, -1 AS block_number FROM mempool_blobs
			WHERE chain_id = $1 AND tx_hash = $2
		) matches
		ORDER BY block_number DESC
		LIMIT 1
	`

	// querySearchBlobByVersionedHash resolves a 64-hex search query as an
	// EIP-4844 blob versioned hash, preferring the newest confirmed occurrence
	// (the same blob can be resubmitted in multiple transactions) over a
	// pending one. The trailing tx_hash sort key keeps the response
	// deterministic when the same hash lands in two transactions of one block.
	// Served by the partial idx_blobs_chain_versioned_hash; rows indexed
	// before the versioned-hash migration have NULL versioned_hash and never
	// match. The mempool arm is a bounded scan of the tiny pending set.
	querySearchBlobByVersionedHash = `
		SELECT versioned_hash, tx_hash, block_number FROM (
			SELECT versioned_hash, tx_hash, block_number FROM blobs
			WHERE chain_id = $1 AND versioned_hash = $2
			UNION ALL
			SELECT versioned_hash, tx_hash, -1 AS block_number FROM mempool_blobs
			WHERE chain_id = $1 AND versioned_hash = $2
		) matches
		ORDER BY block_number DESC, tx_hash ASC
		LIMIT 1
	`

	// querySearchSenderByAddress probes whether an address has sent confirmed
	// blobs — a blob_user_stats primary-key lookup — with the same
	// attribution-name coalescing as queryUserByAddress.
	querySearchSenderByAddress = `
		SELECT
			s.from_address,
			COALESCE(NULLIF(BTRIM(s.user_attribution), ''), NULLIF(BTRIM(bu.name), ''), '') AS user_attribution
		FROM blob_user_stats s
		LEFT JOIN blob_users bu
			ON bu.chain_id = s.chain_id
			AND LOWER(bu.address) = LOWER(s.from_address)
		WHERE s.chain_id = $1 AND s.from_address = $2
	`

	// querySearchRollupsByName matches free-text search input against the
	// known rollup attribution names synced from the blob-list (see
	// internal/attribution) as a case-insensitive prefix. $2 must be a
	// LIKE-escaped lowercase prefix pattern. The escape literal is an
	// E-string so its meaning does not depend on the server's
	// standard_conforming_strings setting.
	//
	// The address set is blob_users unioned with non-disputed
	// blob_attribution_claims: blob_users alone is only the currently-active
	// registry projection (the blob-list sync deletes retired senders from
	// it), while claims retain every address whose validity range attributed
	// historical blobs, so a rollup match lists retired senders too. The
	// result is a superset of the addresses /users can report: registry
	// addresses with no indexed blobs yet are listed, and if the registry
	// carried conflicting overlapping claims for one address, each
	// non-disputed name would list it even though per-block resolution
	// awards the blobs to a single winner. Disputed claims never attribute
	// blobs and stay excluded. Both tables hold one row per attributed
	// address/claim, so the scan is bounded by the (small) known-user set
	// rather than any per-blob table.
	querySearchRollupsByName = `
		SELECT name, array_agg(address ORDER BY address) AS addresses
		FROM (
			SELECT name, address
			FROM blob_users
			WHERE chain_id = $1
			UNION
			SELECT name, address
			FROM blob_attribution_claims
			WHERE chain_id = $1 AND LOWER(status) <> 'disputed'
		) AS entity_addresses
		WHERE LOWER(name) LIKE $2 ESCAPE E'\\'
		GROUP BY name
		ORDER BY name ASC
		LIMIT $3
	`

	// queryRecordTopStreaks reads the longest maintained runs of one kind for
	// /records. blob_block_streaks holds one row per maximal run (see
	// migration 000013), and idx_blob_block_streaks_chain_kind_length is in
	// this exact order, so the read is a top-N index scan with no sort.
	queryRecordTopStreaks = `
		SELECT start_block, end_block, length, start_timestamp, end_timestamp
		FROM blob_block_streaks
		WHERE chain_id = $1 AND kind = $2
		ORDER BY length DESC, end_block DESC
		LIMIT $3
	`

	// queryRecordCurrentStreak reads the run that ends at the network's last
	// indexed block, or nothing when the tip block does not qualify. Runs are
	// disjoint, so the run with the greatest start_block is also the one with
	// the greatest end_block; comparing to the tip in the same statement makes
	// "still running" a server-side check rather than two round trips.
	//
	// The tip is the highest block_metrics row, which the indexer can reach
	// before its predecessor lands (blocks commit concurrently). During that
	// window a run in progress can read as shorter than it is, or as absent;
	// the next block's commit corrects it.
	queryRecordCurrentStreak = `
		SELECT s.start_block, s.end_block, s.length, s.start_timestamp, s.end_timestamp
		FROM blob_block_streaks s
		WHERE s.chain_id = $1
			AND s.kind = $2
			AND s.start_block = (
				SELECT MAX(start_block) FROM blob_block_streaks
				WHERE chain_id = $1 AND kind = $2
			)
			AND s.end_block = (
				SELECT MAX(block_number) FROM block_metrics WHERE chain_id = $1
			)
	`

	// queryRecordBaseFeePeaks reads the blocks with the highest blob base fee,
	// one row per block. Served in order by
	// idx_block_metrics_chain_blob_base_fee, which also INCLUDEs the projected
	// columns, so this is an index-only top-N read.
	queryRecordBaseFeePeaks = `
		SELECT block_number, block_timestamp, blob_base_fee, blob_count
		FROM block_metrics
		WHERE chain_id = $1
		ORDER BY blob_base_fee DESC, block_number DESC
		LIMIT $2
	`

	// queryRecordBusiestHours ranks UTC hour buckets by blob count. The
	// ranking comes from the trigger-maintained hourly block_metrics_rollups
	// rows via idx_block_metrics_rollups_hourly_blob_count (a top-N index
	// scan), and only those few buckets then pay a keyed lookup into
	// blob_chart_rollups for the hour's blob cost. Buckets with no blobs are
	// excluded so an empty leaderboard stays empty rather than listing
	// arbitrary idle hours. Ties on blob count break by most recent bucket,
	// keeping the response deterministic.
	//
	// The two figures come from different tables: the count from block
	// headers, the cost from the blobs those headers describe. The indexer
	// writes both in one transaction, so they cannot drift; if they ever did,
	// the COALESCE would render an unknown cost as "0" rather than flagging
	// it. Neither table's hourly buckets are pruned (PruneFineChartRollups
	// only touches the 60s buckets), so retention cannot open a gap either.
	queryRecordBusiestHours = `
		SELECT
			r.bucket_start AS bucket_start,
			r.sum_blob_count AS blob_count,
			COALESCE((
				SELECT SUM(c.total_cost_wei)
				FROM blob_chart_rollups c
				WHERE c.chain_id = r.chain_id
					AND c.bucket_seconds = 3600
					AND c.bucket_start = r.bucket_start
			), 0)::text AS total_cost_wei
		FROM block_metrics_rollups r
		WHERE r.chain_id = $1
			AND r.bucket_seconds = 3600
			AND r.sum_blob_count > 0
		ORDER BY r.sum_blob_count DESC, r.bucket_start DESC
		LIMIT $2
	`

	// queryRecordBusiestDays is the same ranking over the daily rollup
	// buckets, which the same triggers maintain. A network accumulates roughly
	// 365 daily rows a year, so this one needs no index of its own: the
	// primary-key prefix scan already reads a trivial number of rows, which is
	// also why the bucket size is not factored out into a shared query with
	// the hourly form (that one depends on a partial index keyed to 3600).
	queryRecordBusiestDays = `
		SELECT
			r.bucket_start AS bucket_start,
			r.sum_blob_count AS blob_count,
			COALESCE((
				SELECT SUM(c.total_cost_wei)
				FROM blob_chart_rollups c
				WHERE c.chain_id = r.chain_id
					AND c.bucket_seconds = 86400
					AND c.bucket_start = r.bucket_start
			), 0)::text AS total_cost_wei
		FROM block_metrics_rollups r
		WHERE r.chain_id = $1
			AND r.bucket_seconds = 86400
			AND r.sum_blob_count > 0
		ORDER BY r.sum_blob_count DESC, r.bucket_start DESC
		LIMIT $2
	`

	// queryRecordHighestUtilizationDays ranks days by mean blob utilization.
	// block_metrics_rollups stores the utilization sum and the block count, so
	// the average is exact rather than an average of averages. Days with no
	// blocks are excluded, and the ratio is rendered as a percentage to two
	// decimal places to match utilization_percent on /blob/pricing.
	queryRecordHighestUtilizationDays = `
		SELECT
			r.bucket_start AS day_start,
			r.block_count,
			r.sum_blob_count AS blob_count,
			ROUND((r.sum_utilization / r.block_count) * 100, 2)::float8 AS average_utilization_percent,
			r.blocks_at_max,
			r.blocks_above_target
		FROM block_metrics_rollups r
		WHERE r.chain_id = $1
			AND r.bucket_seconds = 86400
			AND r.block_count > 0
			AND r.sum_utilization > 0
		ORDER BY (r.sum_utilization / r.block_count) DESC, r.bucket_start DESC
		LIMIT $2
	`

	// queryRecordMostExpensiveBlocks ranks blocks by total blob spend, which
	// is blob_base_fee * blob_count * 131072 (params.BlobTxBlobGasPerBlob).
	// The constant factor cannot change the ordering, so
	// idx_block_metrics_chain_blob_spend indexes the bare product and the
	// projection multiplies it out; the ORDER BY must stay written exactly as
	// the index expression for the ordered scan to be used.
	//
	// This is a genuinely different ranking from queryRecordBaseFeePeaks: a
	// full block at a moderate fee can outspend a near-empty block at a peak
	// fee. Zero-blob blocks are excluded so an empty leaderboard stays empty.
	queryRecordMostExpensiveBlocks = `
		SELECT
			block_number,
			block_timestamp,
			blob_count,
			blob_base_fee,
			(blob_base_fee * blob_count * 131072)::text AS total_cost_wei
		FROM block_metrics
		WHERE chain_id = $1 AND blob_count > 0
		ORDER BY (blob_base_fee * blob_count) DESC, block_number DESC
		LIMIT $2
	`

	// queryLatestBlobsByAddresses retrieves confirmed blobs for any of a set
	// of sender addresses — the union behind /blob/latest?entity=. Addresses
	// are matched in their stored form (resolveEntityAddresses returns
	// blob_user_stats.from_address, which the maintenance triggers copy
	// verbatim from blobs.from_address), so each LATERAL arm is its own
	// idx_blobs_chain_from_timestamp top-K scan and the outer sort merges at
	// most (limit+offset) rows per address instead of every address's full
	// history. The candidate rows carry only raw columns; the computed
	// projection (the versioned_hashes sibling-row probe and the confirmed
	// literal) is applied to the final page only, so the per-request probe
	// count stays bounded by limit rather than by
	// address_count x (limit+offset). Ordering matches
	// queryLatestBlobsByAddress.
	queryLatestBlobsByAddresses = `
		SELECT
			page.id,
			page.chain_id,
			page.block_number,
			page.blob_index,
			page.tx_hash,
			page.from_address,
			page.user_attribution,
			page.blob_size_bytes,
			page.base_fee_per_blob_gas,
			page.tip_per_blob_gas,
			page.total_cost_wei,
			page.timestamp,
			true AS confirmed,
			page.max_fee_per_blob_gas,
			page.blob_gas_used,
			page.versioned_hash,
			page.slot,
			page.max_priority_fee_per_gas,
			page.max_fee_per_gas,
			page.priority_fee_per_gas,
			ARRAY(
				SELECT b2.versioned_hash FROM blobs b2
				WHERE b2.chain_id = page.chain_id AND b2.tx_hash = page.tx_hash
					AND b2.versioned_hash IS NOT NULL
				ORDER BY b2.blob_index
			) AS versioned_hashes
		FROM (
			SELECT b.* FROM unnest($2::text[]) AS addr(from_address)
			CROSS JOIN LATERAL (
				SELECT
					id, chain_id, block_number, blob_index, tx_hash, from_address,
					user_attribution, blob_size_bytes, base_fee_per_blob_gas,
					tip_per_blob_gas, total_cost_wei, timestamp,
					max_fee_per_blob_gas, blob_gas_used, versioned_hash, slot,
					max_priority_fee_per_gas, max_fee_per_gas, priority_fee_per_gas
				FROM blobs
				WHERE chain_id = $1 AND from_address = addr.from_address
				ORDER BY timestamp DESC, blob_index ASC
				LIMIT $3::bigint + $4::bigint
			) b
			ORDER BY b.timestamp DESC, b.blob_index ASC
			LIMIT $3 OFFSET $4
		) page
		ORDER BY page.timestamp DESC, page.blob_index ASC
	`

	// queryMempoolBlobsByAddresses retrieves pending blobs for any of a set of
	// sender addresses — the union behind /blob/mempool?entity=. The pending
	// set is tiny, so the case-insensitive match needs no index; it covers
	// registry-listed addresses whose only rows are pending (no confirmed
	// activity yet means no stored-form stats row to resolve).
	queryMempoolBlobsByAddresses = `
		SELECT ` + mempoolBlobSelectColumns + ` FROM mempool_blobs
		WHERE chain_id = $1
			AND LOWER(from_address) IN (SELECT LOWER(u.a) FROM unnest($2::text[]) AS u(a))
		ORDER BY timestamp DESC
		LIMIT $3 OFFSET $4
	`

	// queryRecordTopSpenders ranks senders by all-history blob spend straight
	// off blob_user_stats, the same trigger-maintained table /users reads, via
	// idx_blob_user_stats_chain_spend. Attribution falls back to the known
	// blob_users name exactly as queryTopBlobUsersAllBase does, so a sender
	// carries the same label on both endpoints.
	//
	// from_address is the final sort key because the three preceding ones can
	// all tie, and blob_user_stats is keyed by (chain_id, from_address): with
	// no unique tail the row order among tied senders follows physical layout,
	// so an unrelated write that moves a row can silently reorder the
	// leaderboard and change who falls outside the limit.
	queryRecordTopSpenders = `
		SELECT
			s.from_address,
			COALESCE(NULLIF(BTRIM(s.user_attribution), ''), NULLIF(BTRIM(bu.name), ''), '') AS user_attribution,
			s.blob_count,
			s.total_cost_wei::text AS total_cost_wei
		FROM blob_user_stats s
		LEFT JOIN blob_users bu
			ON bu.chain_id = s.chain_id
			AND LOWER(bu.address) = LOWER(s.from_address)
		WHERE s.chain_id = $1 AND s.total_cost_wei > 0
		ORDER BY s.total_cost_wei DESC, s.blob_count DESC, s.last_timestamp DESC, s.from_address ASC
		LIMIT $2
	`
)

// entityKeySQL renders the canonical entity-key derivation for a display-name
// expression: lowercase, runs of non-alphanumerics collapsed to '_', leading
// and trailing '_' trimmed. Every entity-keyed surface — the
// /charts/attribution-usage summary shares, /users entity grouping, and
// /entities/{key} — must derive keys with this exact expression so the
// identifiers join across endpoints. slugifyEntityKey is the Go mirror.
func entityKeySQL(nameExpr string) string {
	return "TRIM(BOTH '_' FROM regexp_replace(lower(" + nameExpr + "), '[^a-z0-9]+', '_', 'g'))"
}

// entityAttributedAddressesSQL builds the attributed CTE body shared by the
// entity queries: one row per address carrying its resolved display name,
// combining all-history sender stats with the attribution registry. The FULL
// OUTER JOIN keeps registry addresses that have no indexed activity (bu-only
// rows) as well as historically attributed senders the registry no longer
// lists (s-only rows); the chain filter must COALESCE across both sides
// because a plain predicate on either would drop the other side's unmatched
// rows.
//
// Attribution is address-level, matching /users: an address belongs to
// exactly one entity — its current effective attribution — even though the
// blob-list claim model permits different entities across block ranges. An
// address that switched entities mid-history counts its whole history toward
// its current entity here, while the per-blob attribution chart splits it;
// this is the same divergence /users already has, accepted so entity totals
// stay the sum of the addresses' /users rows.
const entityAttributedAddressesSQL = `
		SELECT
			COALESCE(s.from_address, bu.address) AS address,
			COALESCE(NULLIF(BTRIM(s.user_attribution), ''), NULLIF(BTRIM(bu.name), ''), '') AS display_name,
			COALESCE(NULLIF(BTRIM(bu.category), ''), 'unknown') AS category,
			COALESCE(s.blob_count, 0)::bigint AS blob_count,
			COALESCE(s.total_cost_wei, 0) AS total_cost_wei,
			s.last_timestamp,
			(bu.address IS NOT NULL) AS in_registry
		FROM blob_user_stats s
		FULL OUTER JOIN blob_users bu
			ON bu.chain_id = s.chain_id
			AND LOWER(bu.address) = LOWER(s.from_address)
		WHERE COALESCE(s.chain_id, bu.chain_id) = $1
`

// entityDetailProjectionSQL is the shared projection of the entity detail
// queries: the per-address columns plus entity-level aggregates duplicated
// onto every row via window functions over the (small) address set. Entity
// name and category follow the attribution chart's conventions (MIN of the
// display names; first non-unknown category), and the share percentages use
// the same denominators as the corresponding /users variant.
const entityDetailProjectionSQL = `
		ea.address,
		ea.display_name,
		ea.category,
		ea.blob_count,
		ea.total_cost_wei::text AS total_cost_wei,
		ea.last_timestamp,
		ea.in_registry,
		MIN(ea.display_name) OVER () AS entity_name,
		COALESCE(NULLIF(MIN(NULLIF(ea.category, 'unknown')) OVER (), ''), 'unknown') AS entity_category,
		(SUM(ea.blob_count) OVER ())::bigint AS entity_blob_count,
		COALESCE(SUM(ea.total_cost_wei) OVER (), 0)::text AS entity_total_cost_wei,
		MAX(ea.last_timestamp) OVER () AS entity_last_timestamp,
		CASE
			WHEN totals.total_blobs > 0 THEN ROUND(((SUM(ea.blob_count) OVER ())::numeric / totals.total_blobs::numeric) * 100, 6)::float8
			ELSE 0
		END AS blob_share_percent,
		CASE
			WHEN totals.total_spend > 0 THEN ROUND((SUM(ea.total_cost_wei) OVER () / totals.total_spend) * 100, 6)::float8
			ELSE 0
		END AS spend_share_percent
`

// queryEntityAddresses resolves an entity key to its stored-form sender
// addresses for the entity-filtered blob listings. Membership matches the
// detail queries' all-history rule: an address belongs to the entity when its
// resolved display name (indexed attribution first, registry name as
// fallback) slugs to the requested key.
var queryEntityAddresses = `
	WITH attributed AS (
` + entityAttributedAddressesSQL + `
	)
	SELECT address FROM attributed
	WHERE display_name <> ''
		AND ` + entityKeySQL("display_name") + ` = $2
	ORDER BY address ASC
`

// queryEntityDetailAll returns one row per address of one attributed entity
// with all-history totals from blob_user_stats — busiest address first — or
// no rows when the key matches nothing (the handler's 404). Share
// denominators fold pending blobs into the maintained network summary exactly
// as queryTopBlobUsersAllBase does. $3 is the resolved window and must be
// 'all'; it is bound (rather than inlined) so both detail variants take the
// same argument tuple.
var queryEntityDetailAll = `
	WITH attributed AS (
` + entityAttributedAddressesSQL + `
			AND $3::text = 'all'
	),
	entity_addresses AS (
		SELECT * FROM attributed
		WHERE display_name <> ''
			AND ` + entityKeySQL("display_name") + ` = $2
	),
	pending AS (
		SELECT
			COUNT(*)::bigint AS total_pending_blobs,
			COALESCE(SUM(total_cost_wei::numeric), 0) AS pending_total_cost
		FROM mempool_blobs
		WHERE chain_id = $1
	),
	totals AS (
		SELECT
			(COALESCE(s.total_confirmed_blobs, 0) + p.total_pending_blobs) AS total_blobs,
			(COALESCE(s.sum_total_cost, 0) + p.pending_total_cost) AS total_spend
		FROM pending p
		LEFT JOIN network_blob_stats s ON s.chain_id = $1
	)
	SELECT ` + entityDetailProjectionSQL + `
	FROM entity_addresses ea
	CROSS JOIN totals
	ORDER BY ea.blob_count DESC, ea.total_cost_wei DESC, ea.address ASC
`

// queryEntityDetailWindowed is the windowed variant of queryEntityDetailAll
// ($3 is '1h', '24h', '7d', or '30d'), aggregating the same rollup-backed
// windows as queryTopBlobUsersWithOptions — identical bucket selection,
// aligned lower bound, and share denominators, so an entity's windowed totals
// are exactly the sum of its addresses' /users rows.
//
// The address universe (and therefore entity membership) is the same
// attributed base the all-history query filters, with the window totals
// joined on afterwards: every range of an existing entity answers 200 with
// the identical address list, and addresses with no activity inside the
// window — registry-listed or purely historical — appear with zero counts
// rather than making narrow ranges 404. last_timestamp stays the exact
// all-history last-seen from blob_user_stats, mirroring /users.
var queryEntityDetailWindowed = `
	WITH window_params AS (
		SELECT
			CASE WHEN $3 = '1h' THEN 60 ELSE 3600 END AS bucket_seconds,
			-- bucket_start is a naive UTC timestamp, so the bound must be
			-- computed in UTC wall time; bare NOW() would shift the window
			-- by the session TimeZone offset.
			CASE
				WHEN $3 = '1h' THEN date_trunc('minute', (NOW() AT TIME ZONE 'UTC') - INTERVAL '1 hour')
				WHEN $3 = '24h' THEN date_trunc('hour', (NOW() AT TIME ZONE 'UTC') - INTERVAL '24 hours')
				WHEN $3 = '30d' THEN date_trunc('hour', (NOW() AT TIME ZONE 'UTC') - INTERVAL '30 days')
				ELSE date_trunc('hour', (NOW() AT TIME ZONE 'UTC') - INTERVAL '7 days')
			END AS start_time
	),
	window_totals AS (
		SELECT
			r.from_address,
			COALESCE(SUM(r.blob_count), 0)::bigint AS blob_count,
			COALESCE(SUM(r.total_cost_wei), 0) AS total_cost_wei
		FROM blob_chart_rollups r
		CROSS JOIN window_params wp
		WHERE r.chain_id = $1
			AND r.bucket_seconds = wp.bucket_seconds
			AND r.bucket_start >= wp.start_time
		GROUP BY r.from_address
	),
	totals AS (
		SELECT
			COALESCE(SUM(blob_count), 0) AS total_blobs,
			COALESCE(SUM(total_cost_wei), 0) AS total_spend
		FROM window_totals
	),
	attributed AS (
` + entityAttributedAddressesSQL + `
	),
	entity_addresses AS (
		SELECT
			a.address,
			a.display_name,
			a.category,
			COALESCE(w.blob_count, 0)::bigint AS blob_count,
			COALESCE(w.total_cost_wei, 0) AS total_cost_wei,
			a.last_timestamp,
			a.in_registry
		FROM attributed a
		LEFT JOIN window_totals w ON LOWER(w.from_address) = LOWER(a.address)
		WHERE a.display_name <> ''
			AND ` + entityKeySQL("a.display_name") + ` = $2
	)
	SELECT ` + entityDetailProjectionSQL + `
	FROM entity_addresses ea
	CROSS JOIN totals
	ORDER BY ea.blob_count DESC, ea.total_cost_wei DESC, ea.address ASC
`
