package api

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

	// queryBlobByTxHash retrieves a single blob by transaction hash and network.
	queryBlobByTxHash = "SELECT " + blobSelectColumns + " FROM blobs WHERE tx_hash = $1 AND network_id = $2"

	// queryTopBlobUsers aggregates blob usage statistics grouped by sender address.
	queryTopBlobUsers = `
		SELECT
			from_address,
			user_attribution,
			COUNT(*) as blob_count,
			SUM(total_cost_eth::numeric) as total_cost_eth,
			MAX(timestamp) as last_timestamp
		FROM blobs
		WHERE network_id = $1
		GROUP BY from_address, user_attribution
		ORDER BY blob_count DESC
		LIMIT $2 OFFSET $3
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
		SELECT
			from_address,
			MAX(user_attribution) as user_attribution,
			COUNT(*) as blob_count,
			SUM(total_cost_eth::numeric) as total_cost_eth,
			MAX(timestamp) as last_timestamp
		FROM blobs
		WHERE network_id = $1 AND from_address = $2
		GROUP BY from_address
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
	queryLastIndexedBlock = "SELECT value FROM indexer_metadata WHERE network_id = $1 AND key = 'last_indexed_block'"

	// queryNewBlobsSinceBlock retrieves confirmed blobs after a given block number.
	queryNewBlobsSinceBlock = `
		SELECT ` + blobSelectColumns + ` FROM blobs
		WHERE confirmed = true AND network_id = $1 AND block_number > $2
		ORDER BY block_number ASC, blob_index ASC
		LIMIT $3
	`
)
