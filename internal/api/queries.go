package api

import "github.com/a-thomas-22/blob-indexer-api/internal/db/models"

// blobSelectColumns projects blobs rows into the models.Blob shape. The
// blobs table holds confirmed rows only (pending rows live in mempool_blobs),
// so the wire-visible confirmed flag is the projected literal true rather
// than a stored column.
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
	blob_gas_used
`

// mempoolBlobSelectColumns projects mempool_blobs rows into the models.Blob
// shape. Pending rows carry the internal block-number sentinel
// (models.PendingBlockNumber) and are never confirmed, so downstream
// serialization (JSON null block_number, confirmed=false) is unchanged.
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
	blob_gas_used
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
)
