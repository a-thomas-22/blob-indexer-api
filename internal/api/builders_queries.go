package api

import "fmt"

// SQL behind the block-builder endpoints (/builders, /builders/{key},
// /charts/builder-share) and the builder fields the block payloads carry.
//
// Every range-scoped query is anchored on block_builders' (chain_id,
// block_timestamp DESC) index — or, for the per-builder reads, on (chain_id,
// builder_key, block_timestamp DESC) — and joins block_metrics and blobs by
// block number within that window, so the scan is bounded by the requested
// range. range=all is rejected by the handlers: no rollup carries the
// builder, so an unbounded request would scan the whole blobs table.
//
// Wei-denominated columns are NUMERIC. They are projected as ::text and
// scanned as strings so the exact integer survives to the wire; the handlers
// add the gwei/ETH display fields.
//
// Percentiles over per-transaction values use percentile_cont, which is
// defined on double precision, so the ordering expression is cast
// explicitly and the result cast back to numeric for a plain decimal
// rendering. Per-block proposer payments instead use percentile_disc: those
// values reach 1e18 wei, past the 2^53 boundary where a double detour would
// round, and a discrete median is an actually-observed payment.

// The requested window is spliced into every predicate as the bare parameter
// placeholders rather than read out of a one-row `bounds` CTE. A CTE
// referenced more than once is materialized by default (Postgres only
// considers inlining a CTE with a single reference), and a materialized CTE
// is an optimization fence: the planner never sees the timestamp values, so
// it falls back to its default range selectivity and estimated millions of
// matching blobs rows, which bought a sequential scan of the whole blobs
// table with the window applied as a join filter. With the placeholders in
// the predicates the planner has the actual bounds at plan time and reaches
// blobs through idx_blobs_chain_timestamp_chart_cover. NOT MATERIALIZED
// would also defeat the fence, but inline parameters are immune to the
// planning rules entirely — see HL-38.
//
// block_metrics is joined by block number but bounded by the window's
// timestamps as well. Its primary key (chain_id, block_number) and
// idx_block_metrics_chain_timestamp_cover (chain_id, block_timestamp DESC,
// INCLUDE blob_count among others) are its only general indexes: the
// (chain_id, block_number DESC) duplicate was dropped in migration 000004.
// A plain equality join would give the planner nothing to bound the metrics
// side with, and it hashes the whole chain's history against the window's
// builder rows (a sequential scan of block_metrics, measured at ~1M rows).
// The timestamp predicate is the cheapest bound available: block_builders
// and block_metrics carry the same header timestamp for a block, written in
// the same transaction, so the predicate is a no-op on the result, and the
// planner sees the literal window and reaches block_metrics through the
// timestamp index — an index-only scan where only blob_count is read
// (the builder-share charts), a heap-backed index scan where blob_params_max
// is also read (the leaderboard), which is not in the INCLUDE list.
//
// queryBuilderRecentBlocks is the exception: it is LIMIT-driven, so a
// nested loop of primary-key lookups from the few builder rows it returns
// is the right shape, and it joins by (chain_id, block_number) alone.
//
// This replaced an earlier scheme that derived MIN/MAX block_number from the
// builder rows in a CTE and fed them back as InitPlan scalars, which bounded
// the primary key instead. On the 1M-block fixture the timestamp bound scans
// block_metrics five to seven times faster for the charts, and unlike
// InitPlan parameters it is visible at plan time: the scalar bounds left the
// planner at its default range selectivity (a few thousand rows whatever the
// window), which at 30d underestimated the metrics side about forty-fold
// and made the hash join resize and spill to a second batch.
const builderMetricsWindowSQL = `
			AND bm.block_timestamp >= $2::timestamp
			AND bm.block_timestamp < $3::timestamp`

// builderTxSourceSQL collapses the range's confirmed blob rows to one row per
// transaction — the unit tips and inclusion latency are measured in — keyed
// by block so the callers can attach the builder. The columns are constant
// across a transaction's blob rows, so MIN() is just a group-by-compatible
// pick rather than an aggregate with meaning.
//
// Every column it and builderEntityKeyedTxsSQL read is in the INCLUDE list
// of idx_blobs_chain_timestamp_builder_cover (migration 000019), so the
// window is an index-only scan. Reading a column outside that list turns it
// back into a heap fetch per blob row — tens of thousands of scattered
// pages for a 7d window, which is what timed these endpoints out before the
// index existed; the EXPLAIN integration test pins the index-only path.
//
// The caller passes its own placeholders for the chain id and the window so
// the bounds land in the predicate as literals the planner can use.
func builderTxSourceSQL(chain, rangeStart, rangeEnd string) string {
	return fmt.Sprintf(`
	SELECT
		bl.block_number,
		bl.tx_hash,
		MIN(bl.priority_fee_per_gas) AS priority_fee,
		MIN(bl.first_seen_at) AS first_seen_at,
		COUNT(*)::bigint AS blob_count
	FROM blobs bl
	WHERE bl.chain_id = %[1]s
		AND bl.timestamp >= %[2]s::timestamp
		AND bl.timestamp < %[3]s::timestamp
	GROUP BY bl.block_number, bl.tx_hash
`, chain, rangeStart, rangeEnd)
}

// queryBuilderAggregates is the leaderboard body shared by /builders and the
// aggregate half of /builders/{key}. $4 is the builder key filter: the empty
// string aggregates every builder, a key restricts the per-builder CTEs to
// that builder while the range totals (and therefore the share percentages)
// still cover every block in the window.
//
// Args: $1 chain id, $2 range start, $3 range end, $4 builder key, empty string for every builder.
var queryBuilderAggregates = `
	WITH range_builder_blocks AS MATERIALIZED (
		SELECT
			bb.block_number,
			bb.block_timestamp,
			bb.builder_key,
			bb.builder_name,
			bb.fee_recipient,
			bb.proposer_payment_wei,
			bb.candidate_snapshot,
			bb.eligible_skipped_txs,
			bb.eligible_skipped_blobs,
			bb.eligible_skipped_max_tip
		FROM block_builders bb
		WHERE bb.chain_id = $1
			AND bb.block_timestamp >= $2::timestamp
			AND bb.block_timestamp < $3::timestamp
	),
	range_blocks AS MATERIALIZED (
		SELECT
			rb.*,
			COALESCE(bm.blob_count, 0) AS blob_count,
			bm.blob_params_max
		FROM range_builder_blocks rb
		LEFT JOIN block_metrics bm
			ON bm.chain_id = $1
			AND bm.block_number = rb.block_number
			` + builderMetricsWindowSQL + `
	),
	totals AS (
		SELECT
			COUNT(*)::bigint AS total_blocks,
			COUNT(*) FILTER (WHERE blob_count > 0)::bigint AS total_blob_blocks,
			COALESCE(SUM(blob_count), 0)::bigint AS total_blobs
		FROM range_blocks
	),
	selected_blocks AS (
		SELECT * FROM range_blocks WHERE $4 = '' OR builder_key = $4
	),
	block_stats AS (
		SELECT
			builder_key,
			(array_agg(builder_name ORDER BY block_number DESC))[1] AS builder_name,
			COUNT(*)::bigint AS blocks,
			COUNT(*) FILTER (WHERE blob_count > 0)::bigint AS blob_blocks,
			COALESCE(SUM(blob_count), 0)::bigint AS blobs,
			COUNT(*) FILTER (
				WHERE blob_params_max IS NOT NULL
					AND blob_params_max > 0
					AND blob_count = blob_params_max
			)::bigint AS full_blocks,
			COUNT(proposer_payment_wei)::bigint AS mev_boost_blocks,
			(percentile_disc(0.5) WITHIN GROUP (ORDER BY proposer_payment_wei))::text
				AS proposer_payment_median_wei,
			SUM(proposer_payment_wei)::text AS proposer_payment_total_wei,
			COUNT(*) FILTER (WHERE candidate_snapshot)::bigint AS snapshot_blocks,
			COUNT(*) FILTER (
				WHERE candidate_snapshot AND COALESCE(eligible_skipped_txs, 0) > 0
			)::bigint AS blocks_with_eligible_skipped,
			COALESCE(SUM(eligible_skipped_txs), 0)::bigint AS eligible_skipped_txs,
			COALESCE(SUM(eligible_skipped_blobs), 0)::bigint AS eligible_skipped_blobs,
			MAX(eligible_skipped_max_tip)::text AS eligible_skipped_max_tip_wei
		FROM selected_blocks
		GROUP BY builder_key
	),
	recipient_ranks AS (
		SELECT
			builder_key,
			fee_recipient,
			ROW_NUMBER() OVER (
				PARTITION BY builder_key
				ORDER BY COUNT(*) DESC, fee_recipient ASC
			) AS rn
		FROM selected_blocks
		GROUP BY builder_key, fee_recipient
	),
	recipients AS (
		SELECT builder_key, array_agg(fee_recipient ORDER BY rn) AS fee_recipients
		FROM recipient_ranks
		WHERE rn <= 3
		GROUP BY builder_key
	),
	range_txs AS MATERIALIZED (` + builderTxSourceSQL("$1", "$2", "$3") + `),
	builder_txs AS (
		SELECT
			sb.builder_key,
			t.priority_fee,
			CASE
				WHEN t.first_seen_at IS NOT NULL
				THEN (EXTRACT(EPOCH FROM (sb.block_timestamp - t.first_seen_at)) * 1000)::double precision
			END AS time_to_inclusion_ms
		FROM selected_blocks sb
		JOIN range_txs t ON t.block_number = sb.block_number
	),
	tx_stats AS (
		SELECT
			builder_key,
			COUNT(priority_fee)::bigint AS tip_tx_count,
			MIN(priority_fee)::text AS tip_min_wei,
			-- Cast to double for percentile_cont: an execution-layer tip is a
			-- few gwei, orders of magnitude below the 2^53 boundary where a
			-- double loses wei, unlike the proposer payments above.
			(percentile_cont(0.10) WITHIN GROUP (ORDER BY priority_fee::double precision))::numeric::text AS tip_p10_wei,
			(percentile_cont(0.50) WITHIN GROUP (ORDER BY priority_fee::double precision))::numeric::text AS tip_p50_wei,
			(percentile_cont(0.90) WITHIN GROUP (ORDER BY priority_fee::double precision))::numeric::text AS tip_p90_wei,
			COUNT(time_to_inclusion_ms)::bigint AS inclusion_sample_count,
			percentile_cont(0.50) WITHIN GROUP (ORDER BY time_to_inclusion_ms) AS inclusion_p50_ms,
			percentile_cont(0.90) WITHIN GROUP (ORDER BY time_to_inclusion_ms) AS inclusion_p90_ms
		FROM builder_txs
		GROUP BY builder_key
	)
	SELECT
		bs.builder_key,
		bs.builder_name,
		COALESCE(r.fee_recipients, ARRAY[]::text[]) AS fee_recipients,
		bs.blocks,
		bs.blob_blocks,
		bs.blobs,
		bs.full_blocks,
		bs.mev_boost_blocks,
		bs.proposer_payment_median_wei,
		bs.proposer_payment_total_wei,
		bs.snapshot_blocks,
		bs.blocks_with_eligible_skipped,
		bs.eligible_skipped_txs,
		bs.eligible_skipped_blobs,
		bs.eligible_skipped_max_tip_wei,
		COALESCE(ts.tip_tx_count, 0) AS tip_tx_count,
		ts.tip_min_wei,
		ts.tip_p10_wei,
		ts.tip_p50_wei,
		ts.tip_p90_wei,
		COALESCE(ts.inclusion_sample_count, 0) AS inclusion_sample_count,
		ts.inclusion_p50_ms,
		ts.inclusion_p90_ms,
		t.total_blocks,
		t.total_blob_blocks,
		t.total_blobs
	FROM block_stats bs
	LEFT JOIN recipients r ON r.builder_key = bs.builder_key
	LEFT JOIN tx_stats ts ON ts.builder_key = bs.builder_key
	CROSS JOIN totals t
	ORDER BY bs.blocks DESC, bs.builder_key ASC
`

// builderAddressAttributionSQL picks one attribution per sender address the
// way /users?group=entity does (queries.go, user_totals): every row of the
// address in the window is folded into a single name before any entity slug
// is derived, so an address whose stored attribution changed mid-window
// still collapses to exactly one row instead of splitting into one entity
// per name it carried. The stored per-row attribution wins over the
// blob_users registry name, matching the per-address COALESCE there.
//
// It expects a source CTE exposing from_address, row_attribution and
// known_name, and produces address_attribution.
func builderAddressAttributionSQL(source string) string {
	return `
	address_attribution AS (
		SELECT
			from_address,
			COALESCE(
				NULLIF(MAX(BTRIM(row_attribution)), ''),
				NULLIF(MAX(BTRIM(known_name)), ''),
				''
			) AS attribution
		FROM ` + source + `
		GROUP BY from_address
	)`
}

// builderEntityKeyedTxsSQL keys the range's blob transactions by attribution
// entity using the /users group=entity rule: the sender's window-wide
// attribution (builderAddressAttributionSQL) decides the key, an attributed
// sender collapses into its entity slug, and an unattributed one stays keyed
// by its own address. The caller passes the chain and window placeholders so
// the bounds reach the blobs predicate directly. It produces keyed_txs.
func builderEntityKeyedTxsSQL(chain, rangeStart, rangeEnd string) string {
	return fmt.Sprintf(`
	range_blob_rows AS MATERIALIZED (
		SELECT
			bl.block_number,
			bl.tx_hash,
			bl.from_address,
			bl.user_attribution AS row_attribution,
			ku.name AS known_name,
			bl.priority_fee_per_gas,
			bl.first_seen_at
		FROM blobs bl
		LEFT JOIN blob_users ku
			ON ku.chain_id = bl.chain_id
			AND LOWER(ku.address) = LOWER(bl.from_address)
		WHERE bl.chain_id = %[1]s
			AND bl.timestamp >= %[2]s::timestamp
			AND bl.timestamp < %[3]s::timestamp
	),
	`+builderAddressAttributionSQL("range_blob_rows")+`,
	range_txs AS (
		SELECT
			block_number,
			tx_hash,
			MIN(from_address) AS from_address,
			COUNT(*)::bigint AS blob_count,
			MIN(priority_fee_per_gas) AS priority_fee,
			MIN(first_seen_at) AS first_seen_at
		FROM range_blob_rows
		GROUP BY block_number, tx_hash
	),
	keyed_txs AS (
		SELECT
			CASE WHEN slug.entity_slug = '' THEN t.from_address ELSE slug.entity_slug END AS group_key,
			(slug.entity_slug <> '') AS is_entity,
			a.attribution,
			t.from_address,
			t.block_number,
			t.blob_count,
			t.priority_fee,
			t.first_seen_at
		FROM range_txs t
		JOIN address_attribution a ON a.from_address = t.from_address
		CROSS JOIN LATERAL (
			SELECT COALESCE(NULLIF(`+entityKeySQL("a.attribution")+`, ''), '') AS entity_slug
		) slug
	)
`, chain, rangeStart, rangeEnd)
}

// queryBuilderUsers breaks one builder's included blob transactions down by
// attribution entity and compares each entity's share within the builder
// against its share of the whole range. The ratio of the two (computed by the
// handler) is the inclusion index: above 1 means the builder carries more of
// that sender than the market average.
//
// Args: $1 chain id, $2 range start, $3 range end, $4 builder key.
var queryBuilderUsers = `
	WITH ` + builderEntityKeyedTxsSQL("$1", "$2", "$3") + `,
	range_totals AS (
		SELECT COALESCE(SUM(blob_count), 0)::bigint AS total_blobs FROM keyed_txs
	),
	overall AS (
		SELECT group_key, SUM(blob_count)::bigint AS blobs FROM keyed_txs GROUP BY group_key
	),
	builder_blocks AS (
		SELECT bb.block_number, bb.block_timestamp
		FROM block_builders bb
		WHERE bb.chain_id = $1
			AND bb.builder_key = $4
			AND bb.block_timestamp >= $2::timestamp
			AND bb.block_timestamp < $3::timestamp
	),
	builder_txs AS (
		SELECT k.*, bb.block_timestamp
		FROM keyed_txs k
		JOIN builder_blocks bb ON bb.block_number = k.block_number
	),
	builder_totals AS (
		SELECT COALESCE(SUM(blob_count), 0)::bigint AS total_blobs FROM builder_txs
	),
	builder_rows AS (
		SELECT
			group_key,
			(array_agg(attribution ORDER BY blob_count DESC, from_address ASC))[1] AS name,
			bool_or(is_entity) AS is_entity,
			SUM(blob_count)::bigint AS blobs,
			COUNT(*)::bigint AS tx_count,
			-- Cast to double for percentile_cont: an execution-layer tip is a
			-- few gwei, orders of magnitude below the 2^53 boundary where a
			-- double loses wei, unlike the proposer payments above.
			(percentile_cont(0.5) WITHIN GROUP (ORDER BY priority_fee::double precision))::numeric::text AS tip_p50_wei,
			COUNT(first_seen_at)::bigint AS inclusion_sample_count,
			percentile_cont(0.5) WITHIN GROUP (
				ORDER BY CASE
					WHEN first_seen_at IS NOT NULL
					THEN (EXTRACT(EPOCH FROM (block_timestamp - first_seen_at)) * 1000)::double precision
				END
			) AS inclusion_p50_ms
		FROM builder_txs
		GROUP BY group_key
	)
	SELECT
		br.group_key AS key,
		br.name,
		br.is_entity,
		br.blobs,
		br.tx_count,
		br.tip_p50_wei,
		br.inclusion_sample_count,
		br.inclusion_p50_ms,
		CASE
			WHEN bt.total_blobs > 0
			THEN ROUND((br.blobs::numeric / bt.total_blobs::numeric) * 100, 6)::float8
			ELSE 0
		END AS share_within_builder_percent,
		CASE
			WHEN rt.total_blobs > 0
			THEN ROUND((COALESCE(o.blobs, 0)::numeric / rt.total_blobs::numeric) * 100, 6)::float8
			ELSE 0
		END AS share_overall_percent
	FROM builder_rows br
	LEFT JOIN overall o ON o.group_key = br.group_key
	CROSS JOIN builder_totals bt
	CROSS JOIN range_totals rt
	ORDER BY br.blobs DESC, br.group_key ASC
`

// queryBuilderSkipped aggregates, per attribution entity, the pending blob
// transactions our node had seen and classified as 'eligible' against one
// builder's blocks — the transactions the builder could have taken and did
// not. Other reasons are excluded: they explain why inclusion was impossible
// regardless of the builder.
//
// Args: $1 chain id, $2 range start, $3 range end, $4 builder key.
var queryBuilderSkipped = `
	WITH builder_blocks AS (
		SELECT bb.block_number
		FROM block_builders bb
		WHERE bb.chain_id = $1
			AND bb.builder_key = $4
			AND bb.block_timestamp >= $2::timestamp
			AND bb.block_timestamp < $3::timestamp
	),
	candidates AS MATERIALIZED (
		SELECT
			c.tx_hash,
			c.from_address,
			c.user_attribution AS row_attribution,
			ku.name AS known_name,
			c.blob_count,
			c.max_priority_fee_per_gas
		FROM blob_inclusion_candidates c
		JOIN builder_blocks bb ON bb.block_number = c.block_number
		LEFT JOIN blob_users ku
			ON ku.chain_id = $1
			AND LOWER(ku.address) = LOWER(c.from_address)
		WHERE c.chain_id = $1
			AND c.block_timestamp >= $2::timestamp
			AND c.block_timestamp < $3::timestamp
			AND c.reason = 'eligible'
	),
	` + builderAddressAttributionSQL("candidates") + `,
	keyed AS (
		SELECT
			CASE WHEN slug.entity_slug = '' THEN c.from_address ELSE slug.entity_slug END AS group_key,
			(slug.entity_slug <> '') AS is_entity,
			a.attribution,
			c.from_address,
			c.blob_count,
			c.max_priority_fee_per_gas
		FROM candidates c
		JOIN address_attribution a ON a.from_address = c.from_address
		CROSS JOIN LATERAL (
			SELECT COALESCE(NULLIF(` + entityKeySQL("a.attribution") + `, ''), '') AS entity_slug
		) slug
	)
	SELECT
		group_key AS key,
		(array_agg(attribution ORDER BY blob_count DESC, from_address ASC))[1] AS name,
		bool_or(is_entity) AS is_entity,
		COUNT(*)::bigint AS txs,
		COALESCE(SUM(blob_count), 0)::bigint AS blobs,
		MAX(max_priority_fee_per_gas)::text AS max_tip_wei,
		-- Same lossless-in-practice double cast as the leaderboard tips.
		(percentile_cont(0.5) WITHIN GROUP (ORDER BY max_priority_fee_per_gas::double precision))::numeric::text AS p50_tip_wei
	FROM keyed
	GROUP BY group_key
	ORDER BY blobs DESC, key ASC
`

// queryBuilderSkippedDetailFrom is the earliest candidate row still held for
// the chain inside the requested window. block_builders' skipped aggregates
// are permanent, but the blob_inclusion_candidates rows the per-entity
// `skipped` list is rebuilt from are pruned on a retention window, so a 30d
// response can carry a month of aggregate counts over a week of detail.
// Reporting the boundary lets a client say which part of the window the
// detail actually covers instead of silently under-reporting it.
//
// It is chain-wide rather than builder-scoped on purpose: pruning is a
// chain-wide clock, so the oldest surviving row bounds every builder's
// detail. The (chain_id, block_timestamp) index turns the MIN into a single
// ordered index probe.
//
// Args: $1 chain id, $2 range start, $3 range end.
var queryBuilderSkippedDetailFrom = `
	SELECT MIN(block_timestamp)
	FROM blob_inclusion_candidates
	WHERE chain_id = $1
		AND block_timestamp >= $2
		AND block_timestamp < $3
`

// builderRecentBlockLimit is how many of a builder's most recent blocks the
// detail endpoint returns.
const builderRecentBlockLimit = 20

// queryBuilderRecentBlocks lists a builder's newest blocks in the range,
// served by idx_block_builders_chain_key_timestamp.
//
// Args: $1 chain id, $2 range start, $3 range end, $4 builder key, $5 limit.
var queryBuilderRecentBlocks = `
	SELECT
		bb.block_number,
		bb.block_timestamp,
		COALESCE(bm.blob_count, 0) AS blob_count,
		bm.blob_params_max,
		bb.proposer_payment_wei::text AS proposer_payment_wei,
		bb.candidate_snapshot,
		bb.eligible_skipped_txs,
		bb.eligible_skipped_max_tip::text AS eligible_skipped_max_tip_wei
	FROM block_builders bb
	LEFT JOIN block_metrics bm
		ON bm.chain_id = $1 AND bm.block_number = bb.block_number
	WHERE bb.chain_id = $1
		AND bb.builder_key = $4
		AND bb.block_timestamp >= $2
		AND bb.block_timestamp < $3
	ORDER BY bb.block_timestamp DESC, bb.block_number DESC
	LIMIT $5
`

// blockBuilderSelectColumns projects block_builders rows into the
// models.BlockBuilder shape.
const blockBuilderSelectColumns = `
	chain_id,
	block_number,
	block_timestamp,
	fee_recipient,
	extra_data,
	builder_key,
	builder_name,
	tx_count,
	proposer_payment_wei::text AS proposer_payment_wei,
	candidate_snapshot,
	proposer_payment_to,
	pending_candidate_txs,
	eligible_skipped_txs,
	eligible_skipped_blobs,
	eligible_skipped_max_tip::text AS eligible_skipped_max_tip
`

// queryBlockBuilderForBlock reads one block's builder row for /block/{number}.
// The indexer writes it in the same transaction as block_metrics, so it is
// either present alongside the block or absent because no backfill has
// reached that height yet.
var queryBlockBuilderForBlock = `
	SELECT ` + blockBuilderSelectColumns + ` FROM block_builders
	WHERE chain_id = $1 AND block_number = $2
`

// queryBlockBuildersByBlockNumbers reads builder rows for a set of blocks —
// the WebSocket new_block broadcast and the reconnect snapshot.
var queryBlockBuildersByBlockNumbers = `
	SELECT ` + blockBuilderSelectColumns + ` FROM block_builders
	WHERE chain_id = $1 AND block_number = ANY($2::bigint[])
`

// queryBlobInclusionCandidatesForBlock lists the pending blob transactions
// our node had seen when one block arrived and that the block did not
// include. Empty when the block was indexed from history (no snapshot) or
// when the retention window has pruned the detail rows.
var queryBlobInclusionCandidatesForBlock = `
	SELECT
		chain_id,
		block_number,
		block_timestamp,
		tx_hash,
		from_address,
		COALESCE(user_attribution, '') AS user_attribution,
		nonce,
		blob_count,
		max_priority_fee_per_gas::text AS max_priority_fee_per_gas,
		max_fee_per_gas::text AS max_fee_per_gas,
		max_fee_per_blob_gas::text AS max_fee_per_blob_gas,
		first_seen_at,
		reason
	FROM blob_inclusion_candidates
	WHERE chain_id = $1 AND block_number = $2
	ORDER BY max_priority_fee_per_gas DESC NULLS LAST, tx_hash ASC
`

// builderShareSeriesSQL is the tail shared by the builder-share chart
// queries. It expects a builder_rows CTE of (bucket_start, builder_key,
// builder_name, blob_count) — one row per block in range — and groups the
// long tail beyond the top series into a single 'other' bucket, the same
// shape /charts/attribution-usage uses.
func builderShareSeriesSQL(limitPlaceholder string) string {
	return `
	builder_totals AS (
		SELECT
			builder_key,
			(array_agg(builder_name))[1] AS builder_name,
			COALESCE(SUM(blob_count), 0)::bigint AS blobs,
			COUNT(*)::bigint AS blocks
		FROM builder_rows
		GROUP BY builder_key
	),
	top_builders AS (
		SELECT *
		FROM builder_totals
		ORDER BY blobs DESC, blocks DESC, builder_key ASC
		LIMIT ` + limitPlaceholder + `
	),
	series_rows AS (
		SELECT
			br.bucket_start,
			br.blob_count,
			CASE WHEN tb.builder_key IS NOT NULL THEN br.builder_key ELSE 'other' END AS series_key,
			CASE WHEN tb.builder_key IS NOT NULL THEN tb.builder_name ELSE 'Other' END AS series_name
		FROM builder_rows br
		LEFT JOIN top_builders tb ON tb.builder_key = br.builder_key
	),
	bucketed_series AS (
		SELECT
			bucket_start,
			series_key,
			series_name,
			COALESCE(SUM(blob_count), 0)::bigint AS blobs,
			COUNT(*)::bigint AS blocks
		FROM series_rows
		GROUP BY bucket_start, series_key, series_name
	),
	bucket_stats AS (
		SELECT
			bucket_start,
			COALESCE(SUM(blob_count), 0)::bigint AS blobs,
			COUNT(*)::bigint AS blocks
		FROM builder_rows
		GROUP BY bucket_start
	),
	summary AS (
		SELECT
			COALESCE(SUM(blob_count), 0)::bigint AS total_blobs,
			COUNT(*)::bigint AS total_blocks
		FROM builder_rows
	)
`
}

const builderShareSelectSQL = `
	SELECT
		b.bucket_start AS timestamp,
		b.range_start,
		b.range_end,
		%s AS block_number,
		COALESCE(bs.blobs, 0) AS bucket_blobs,
		COALESCE(bs.blocks, 0) AS bucket_blocks,
		s.series_key,
		s.series_name,
		COALESCE(s.blobs, 0) AS series_blobs,
		COALESCE(s.blocks, 0) AS series_blocks,
		sm.total_blobs AS summary_total_blobs,
		sm.total_blocks AS summary_total_blocks
	FROM buckets b
	LEFT JOIN bucket_stats bs ON bs.bucket_start = b.bucket_start
	LEFT JOIN bucketed_series s ON s.bucket_start = b.bucket_start
	CROSS JOIN summary sm
	ORDER BY %s, s.blobs DESC NULLS LAST, s.series_key ASC
`

// queryBuilderShareTimeChart buckets blocks and blobs per builder by
// wall-clock interval.
//
// Args: $1 chain id, $2 range start, $3 range end, $4 bucket seconds,
// $5 series limit.
var queryBuilderShareTimeChart = `
	WITH buckets AS (
		SELECT
			g.bucket_start,
			$2::timestamp AS range_start,
			$3::timestamp AS range_end
		FROM generate_series(
			$2::timestamp,
			$3::timestamp - ($4::bigint * INTERVAL '1 second'),
			$4::bigint * INTERVAL '1 second'
		) AS g(bucket_start)
		WHERE $3::timestamp > $2::timestamp
	),
	range_builder_blocks AS MATERIALIZED (
		SELECT
			TIMESTAMP 'epoch' + (
				FLOOR(EXTRACT(EPOCH FROM bb.block_timestamp) / $4::numeric)::bigint
				* $4::bigint
				* INTERVAL '1 second'
			) AS bucket_start,
			bb.block_number,
			bb.builder_key,
			bb.builder_name
		FROM block_builders bb
		WHERE bb.chain_id = $1
			AND bb.block_timestamp >= $2::timestamp
			AND bb.block_timestamp < $3::timestamp
	),
	builder_rows AS MATERIALIZED (
		SELECT
			rb.bucket_start,
			rb.builder_key,
			rb.builder_name,
			COALESCE(bm.blob_count, 0) AS blob_count
		FROM range_builder_blocks rb
		LEFT JOIN block_metrics bm
			ON bm.chain_id = $1
			AND bm.block_number = rb.block_number
			` + builderMetricsWindowSQL + `
	),
` + builderShareSeriesSQL("$5") + fmt.Sprintf(builderShareSelectSQL, "NULL::bigint", "b.bucket_start ASC")

// queryBuilderShareBlockChart emits one bucket per indexed block.
//
// Args: $1 chain id, $2 range start, $3 range end, $4 series limit.
var queryBuilderShareBlockChart = `
	WITH range_builder_blocks AS MATERIALIZED (
		SELECT
			bb.block_number,
			bb.block_timestamp AS bucket_start,
			bb.builder_key,
			bb.builder_name,
			$2::timestamp AS range_start,
			$3::timestamp AS range_end
		FROM block_builders bb
		WHERE bb.chain_id = $1
			AND bb.block_timestamp >= $2::timestamp
			AND bb.block_timestamp < $3::timestamp
	),
	-- One bucket per block, and each block's builder already came out of the
	-- scan above: re-joining block_builders by block number here would only
	-- buy the planner an excuse to scan the whole table again.
	buckets AS (
		SELECT block_number, bucket_start, range_start, range_end
		FROM range_builder_blocks
	),
	builder_rows AS MATERIALIZED (
		SELECT
			rb.bucket_start,
			rb.builder_key,
			rb.builder_name,
			COALESCE(bm.blob_count, 0) AS blob_count
		FROM range_builder_blocks rb
		LEFT JOIN block_metrics bm
			ON bm.chain_id = $1
			AND bm.block_number = rb.block_number
			` + builderMetricsWindowSQL + `
	),
` + builderShareSeriesSQL("$4") + fmt.Sprintf(builderShareSelectSQL, "b.block_number", "b.block_number ASC")
