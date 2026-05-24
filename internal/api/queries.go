package api

import "github.com/a-thomas-22/blob-indexer-api/internal/db/models"

// SQL query constants used by API handlers.
const (
	// queryLatestBlobs retrieves confirmed blobs ordered by block number descending.
	queryLatestBlobs = `
		SELECT ` + blobSelectColumns + ` FROM blobs
		WHERE confirmed = true AND network_id = $1
		ORDER BY block_number DESC, blob_index ASC
		LIMIT $2 OFFSET $3
	`

	// queryMempoolBlobs retrieves unconfirmed (pending) blobs ordered by timestamp descending.
	queryMempoolBlobs = `
		SELECT ` + blobSelectColumns + ` FROM blobs
		WHERE confirmed = false AND network_id = $1
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
			FROM blobs
			WHERE confirmed = false AND network_id = $1
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
		WHERE network_id = $1
		ORDER BY block_number DESC
		LIMIT 1
	`

	// queryBlobByTxHash retrieves a single blob by transaction hash and network.
	queryBlobByTxHash = "SELECT " + blobSelectColumns + " FROM blobs WHERE tx_hash = $1 AND network_id = $2"

	// queryTopBlobUsersWithOptions aggregates sender usage with safe sort/window parameters.
	queryTopBlobUsersWithOptions = `
		WITH filtered_blobs AS (
			SELECT
				b.from_address,
				b.user_attribution,
				b.total_cost_eth,
				b.timestamp,
				bu.name AS known_name,
				bu.category AS known_category
			FROM blobs b
			LEFT JOIN blob_users bu
				ON bu.network_id = b.network_id
				AND LOWER(bu.address) = LOWER(b.from_address)
			WHERE b.network_id = $1
				AND (
					$4 = 'all'
					OR ($4 = '24h' AND b.timestamp >= NOW() - INTERVAL '24 hours')
					OR ($4 = '7d' AND b.timestamp >= NOW() - INTERVAL '7 days')
				)
		),
		user_totals AS (
			SELECT
				from_address,
				COALESCE(NULLIF(MAX(BTRIM(user_attribution)), ''), NULLIF(MAX(BTRIM(known_name)), ''), '') AS user_attribution,
				COALESCE(NULLIF(MAX(BTRIM(known_category)), ''), 'unknown') AS category,
				COUNT(*) AS blob_count,
				COALESCE(SUM(total_cost_eth::numeric), 0) AS total_cost_eth,
				MAX(timestamp) AS last_timestamp
			FROM filtered_blobs
			GROUP BY from_address
		),
		totals AS (
			SELECT
				COALESCE(SUM(blob_count), 0) AS total_blobs,
				COALESCE(SUM(total_cost_eth), 0) AS total_spend
			FROM user_totals
		)
		SELECT
			user_totals.from_address,
			user_totals.user_attribution,
			user_totals.category,
			user_totals.blob_count,
			user_totals.total_cost_eth::text AS total_cost_eth,
			user_totals.last_timestamp,
			CASE
				WHEN totals.total_blobs > 0 THEN ROUND((user_totals.blob_count::numeric / totals.total_blobs::numeric) * 100, 6)::float8
				ELSE 0
			END AS blob_share_percent,
			CASE
				WHEN totals.total_spend > 0 THEN ROUND((user_totals.total_cost_eth / totals.total_spend) * 100, 6)::float8
				ELSE 0
			END AS spend_share_percent
		FROM user_totals
		CROSS JOIN totals
		ORDER BY
			CASE WHEN $5 = 'count' THEN user_totals.blob_count END DESC,
			CASE WHEN $5 = 'spend' THEN user_totals.total_cost_eth END DESC,
			user_totals.blob_count DESC,
			user_totals.total_cost_eth DESC,
			user_totals.last_timestamp DESC
		LIMIT $2 OFFSET $3
	`

	// queryBlobUserCategoryBreakdown aggregates blob usage by known user category.
	queryBlobUserCategoryBreakdown = `
		WITH category_totals AS (
			SELECT
				COALESCE(NULLIF(BTRIM(bu.category), ''), 'unknown') AS category,
				COUNT(*) AS blob_count,
				COALESCE(SUM(b.total_cost_eth::numeric), 0) AS total_cost_eth
			FROM blobs b
			LEFT JOIN blob_users bu
				ON bu.network_id = b.network_id
				AND LOWER(bu.address) = LOWER(b.from_address)
			WHERE b.network_id = $1
				AND (
					$2 = 'all'
					OR ($2 = '24h' AND b.timestamp >= NOW() - INTERVAL '24 hours')
					OR ($2 = '7d' AND b.timestamp >= NOW() - INTERVAL '7 days')
				)
			GROUP BY COALESCE(NULLIF(BTRIM(bu.category), ''), 'unknown')
		),
		totals AS (
			SELECT
				COALESCE(SUM(blob_count), 0) AS total_blobs,
				COALESCE(SUM(total_cost_eth), 0) AS total_spend
			FROM category_totals
		)
		SELECT
			category_totals.category,
			category_totals.blob_count,
			category_totals.total_cost_eth::text AS total_cost_eth,
			CASE
				WHEN totals.total_blobs > 0 THEN ROUND((category_totals.blob_count::numeric / totals.total_blobs::numeric) * 100, 6)::float8
				ELSE 0
			END AS blob_share_percent,
			CASE
				WHEN totals.total_spend > 0 THEN ROUND((category_totals.total_cost_eth / totals.total_spend) * 100, 6)::float8
				ELSE 0
			END AS spend_share_percent
		FROM category_totals
		CROSS JOIN totals
		ORDER BY category_totals.blob_count DESC, category_totals.total_cost_eth DESC, category_totals.category ASC
	`

	// queryBlobStats computes aggregate statistics for all blobs on a network.
	queryBlobStats = `
		SELECT
			COUNT(*) as total_blobs,
			COALESCE(SUM(CASE WHEN confirmed = true THEN 1 ELSE 0 END), 0) as total_confirmed_blobs,
			COALESCE(SUM(CASE WHEN confirmed = false THEN 1 ELSE 0 END), 0) as total_pending_blobs,
			COALESCE(AVG(base_fee_per_blob_gas::numeric), '0'::numeric) as average_base_fee,
			COALESCE(AVG(tip_per_blob_gas::numeric), '0'::numeric) as average_tip,
			COALESCE(AVG(total_cost_eth::numeric), '0'::numeric) as average_total_cost,
			COALESCE(MAX(timestamp), '1970-01-01'::timestamp) as last_indexed_time
		FROM blobs
		WHERE network_id = $1
	`

	// queryRollingStatsWindows computes time-windowed blob market statistics.
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
			bs.total_cost_eth,
			bs.unique_senders,
			bms.average_utilization
		FROM window_bounds wb
		LEFT JOIN LATERAL (
			SELECT
				COUNT(*) AS total_blobs,
				COALESCE(SUM(COALESCE(b.blob_gas_used, 0)), 0) AS total_blob_gas_used,
				COALESCE(SUM(b.total_cost_eth::numeric), 0) AS total_cost_eth,
				COUNT(DISTINCT b.from_address) AS unique_senders,
				COALESCE(AVG(b.base_fee_per_blob_gas::numeric), 0) AS average_blob_base_fee,
				COALESCE(percentile_disc(0.5) WITHIN GROUP (ORDER BY b.base_fee_per_blob_gas::numeric), 0) AS median_blob_base_fee,
				COALESCE(percentile_disc(0.95) WITHIN GROUP (ORDER BY b.base_fee_per_blob_gas::numeric), 0) AS p95_blob_base_fee
			FROM blobs b
			WHERE b.network_id = $1
				AND b.confirmed = true
				AND b.timestamp >= wb.start_time
				AND b.timestamp < wb.end_time
		) bs ON true
		LEFT JOIN LATERAL (
			SELECT
				COALESCE(AVG(bm.utilization_ratio::numeric), 0) AS average_utilization
			FROM block_metrics bm
			WHERE bm.network_id = $1
				AND bm.block_timestamp >= wb.start_time
				AND bm.block_timestamp < wb.end_time
		) bms ON true
		ORDER BY wb.ord
	`

	// queryBlockMetrics retrieves recent block metrics for pricing data.
	queryBlockMetrics = `
		SELECT ` + blockMetricsSelectColumns + ` FROM block_metrics
		WHERE network_id = $1
		ORDER BY block_number DESC
		LIMIT $2
	`

	// queryLatestBlobsByAddress retrieves confirmed blobs for a specific sender address.
	queryLatestBlobsByAddress = `
		SELECT ` + blobSelectColumns + ` FROM blobs
		WHERE confirmed = true AND network_id = $1 AND from_address = $2
		ORDER BY block_number DESC, blob_index ASC
		LIMIT $3 OFFSET $4
	`

	// queryMempoolBlobsByAddress retrieves unconfirmed blobs for a specific sender address.
	queryMempoolBlobsByAddress = `
		SELECT ` + blobSelectColumns + ` FROM blobs
		WHERE confirmed = false AND network_id = $1 AND from_address = $2
		ORDER BY timestamp DESC
		LIMIT $3 OFFSET $4
	`

	// queryUserByAddress retrieves aggregated stats for a single sender address.
	queryUserByAddress = `
		WITH selected_user AS (
			SELECT
				b.from_address,
				COALESCE(NULLIF(MAX(BTRIM(b.user_attribution)), ''), NULLIF(MAX(BTRIM(bu.name)), ''), '') AS user_attribution,
				COALESCE(NULLIF(MAX(BTRIM(bu.category)), ''), 'unknown') AS category,
				COUNT(*) AS blob_count,
				COALESCE(SUM(b.total_cost_eth::numeric), 0) AS total_cost_eth,
				MAX(b.timestamp) AS last_timestamp
			FROM blobs b
			LEFT JOIN blob_users bu
				ON bu.network_id = b.network_id
				AND LOWER(bu.address) = LOWER(b.from_address)
			WHERE b.network_id = $1 AND b.from_address = $2
			GROUP BY b.from_address
		),
		totals AS (
			SELECT
				COUNT(*) AS total_blobs,
				COALESCE(SUM(total_cost_eth::numeric), 0) AS total_spend
			FROM blobs
			WHERE network_id = $1
		)
		SELECT
			selected_user.from_address,
			selected_user.user_attribution,
			selected_user.category,
			selected_user.blob_count,
			selected_user.total_cost_eth::text AS total_cost_eth,
			selected_user.last_timestamp,
			CASE
				WHEN totals.total_blobs > 0 THEN ROUND((selected_user.blob_count::numeric / totals.total_blobs::numeric) * 100, 6)::float8
				ELSE 0
			END AS blob_share_percent,
			CASE
				WHEN totals.total_spend > 0 THEN ROUND((selected_user.total_cost_eth / totals.total_spend) * 100, 6)::float8
				ELSE 0
			END AS spend_share_percent
		FROM selected_user
		CROSS JOIN totals
	`

	// queryLastIndexedTimeCoalesce retrieves the most recent confirmed blob timestamp,
	// defaulting to epoch if no blobs exist.
	queryLastIndexedTimeCoalesce = "SELECT COALESCE(MAX(timestamp), '1970-01-01'::timestamp) FROM blobs WHERE confirmed = true AND network_id = $1"

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
	queryLastIndexedBlock = "SELECT value FROM indexer_metadata WHERE network_id = $1 AND key = '" + models.MetadataLastIndexedBlock + "'"

	// queryNetworkFreshnessMetadata retrieves frontend freshness metadata for a network.
	queryNetworkFreshnessMetadata = `
		SELECT key, value
		FROM indexer_metadata
		WHERE network_id = $1
			AND key IN (
				'` + models.MetadataLastIndexedBlock + `',
				'` + models.MetadataCurrentChainHead + `',
				'` + models.MetadataChainHeadUpdatedAt + `',
				'` + models.MetadataLastIndexedAt + `',
				'` + models.MetadataWebSocketFreshnessAt + `'
			)
	`

	// queryNewBlobsSinceBlock retrieves confirmed blobs after a given block number.
	queryNewBlobsSinceBlock = `
		SELECT ` + blobSelectColumns + ` FROM blobs
		WHERE confirmed = true AND network_id = $1 AND block_number > $2
		ORDER BY block_number ASC, blob_index ASC
		LIMIT $3
	`
)
