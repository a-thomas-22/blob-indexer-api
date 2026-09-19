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

// builderTxSourceSQL collapses the range's confirmed blob rows to one row per
// transaction — the unit tips and inclusion latency are measured in — keyed
// by block so the callers can attach the builder. The columns are constant
// across a transaction's blob rows, so MIN() is just a group-by-compatible
// pick rather than an aggregate with meaning.
const builderTxSourceSQL = `
	SELECT
		bl.block_number,
		bl.tx_hash,
		MIN(bl.priority_fee_per_gas) AS priority_fee,
		MIN(bl.first_seen_at) AS first_seen_at,
		COUNT(*)::bigint AS blob_count
	FROM bounds b
	JOIN blobs bl
		ON bl.chain_id = $1
		AND bl.timestamp >= b.range_start
		AND bl.timestamp < b.range_end
	GROUP BY bl.block_number, bl.tx_hash
`

// queryBuilderAggregates is the leaderboard body shared by /builders and the
// aggregate half of /builders/{key}. $4 is the builder key filter: the empty
// string aggregates every builder, a key restricts the per-builder CTEs to
// that builder while the range totals (and therefore the share percentages)
// still cover every block in the window.
//
// Args: $1 chain id, $2 range start, $3 range end, $4 builder key, empty string for every builder.
var queryBuilderAggregates = `
	WITH bounds AS (
		SELECT $2::timestamp AS range_start, $3::timestamp AS range_end
	),
	range_blocks AS MATERIALIZED (
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
			bb.eligible_skipped_max_tip,
			COALESCE(bm.blob_count, 0) AS blob_count,
			bm.blob_params_max
		FROM bounds b
		JOIN block_builders bb
			ON bb.chain_id = $1
			AND bb.block_timestamp >= b.range_start
			AND bb.block_timestamp < b.range_end
		LEFT JOIN block_metrics bm
			ON bm.chain_id = $1
			AND bm.block_number = bb.block_number
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
	range_txs AS MATERIALIZED (` + builderTxSourceSQL + `),
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

// builderEntityKeyedTxsSQL keys the range's blob transactions by attribution
// entity using the /users group=entity rule: an attributed sender collapses
// into its entity slug, an unattributed one stays keyed by its own address.
// It expects a bounds CTE and produces keyed_txs.
var builderEntityKeyedTxsSQL = `
	range_blob_rows AS MATERIALIZED (
		SELECT
			bl.block_number,
			bl.tx_hash,
			bl.from_address,
			COALESCE(NULLIF(BTRIM(bl.user_attribution), ''), NULLIF(BTRIM(ku.name), ''), '') AS attribution,
			bl.priority_fee_per_gas,
			bl.first_seen_at
		FROM bounds b
		JOIN blobs bl
			ON bl.chain_id = $1
			AND bl.timestamp >= b.range_start
			AND bl.timestamp < b.range_end
		LEFT JOIN blob_users ku
			ON ku.chain_id = bl.chain_id
			AND LOWER(ku.address) = LOWER(bl.from_address)
	),
	range_txs AS (
		SELECT
			block_number,
			tx_hash,
			MIN(from_address) AS from_address,
			MIN(attribution) AS attribution,
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
			t.attribution,
			t.from_address,
			t.block_number,
			t.blob_count,
			t.priority_fee,
			t.first_seen_at
		FROM range_txs t
		CROSS JOIN LATERAL (
			SELECT COALESCE(NULLIF(` + entityKeySQL("t.attribution") + `, ''), '') AS entity_slug
		) slug
	)
`

// queryBuilderUsers breaks one builder's included blob transactions down by
// attribution entity and compares each entity's share within the builder
// against its share of the whole range. The ratio of the two (computed by the
// handler) is the inclusion index: above 1 means the builder carries more of
// that sender than the market average.
//
// Args: $1 chain id, $2 range start, $3 range end, $4 builder key.
var queryBuilderUsers = `
	WITH bounds AS (
		SELECT $2::timestamp AS range_start, $3::timestamp AS range_end
	),
	` + builderEntityKeyedTxsSQL + `,
	range_totals AS (
		SELECT COALESCE(SUM(blob_count), 0)::bigint AS total_blobs FROM keyed_txs
	),
	overall AS (
		SELECT group_key, SUM(blob_count)::bigint AS blobs FROM keyed_txs GROUP BY group_key
	),
	builder_blocks AS (
		SELECT bb.block_number, bb.block_timestamp
		FROM bounds b
		JOIN block_builders bb
			ON bb.chain_id = $1
			AND bb.builder_key = $4
			AND bb.block_timestamp >= b.range_start
			AND bb.block_timestamp < b.range_end
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
	WITH bounds AS (
		SELECT $2::timestamp AS range_start, $3::timestamp AS range_end
	),
	builder_blocks AS (
		SELECT bb.block_number
		FROM bounds b
		JOIN block_builders bb
			ON bb.chain_id = $1
			AND bb.builder_key = $4
			AND bb.block_timestamp >= b.range_start
			AND bb.block_timestamp < b.range_end
	),
	candidates AS (
		SELECT
			c.tx_hash,
			c.from_address,
			COALESCE(NULLIF(BTRIM(c.user_attribution), ''), NULLIF(BTRIM(ku.name), ''), '') AS attribution,
			c.blob_count,
			c.max_priority_fee_per_gas
		FROM bounds b
		JOIN blob_inclusion_candidates c
			ON c.chain_id = $1
			AND c.block_timestamp >= b.range_start
			AND c.block_timestamp < b.range_end
			AND c.reason = 'eligible'
		JOIN builder_blocks bb ON bb.block_number = c.block_number
		LEFT JOIN blob_users ku
			ON ku.chain_id = $1
			AND LOWER(ku.address) = LOWER(c.from_address)
	),
	keyed AS (
		SELECT
			CASE WHEN slug.entity_slug = '' THEN c.from_address ELSE slug.entity_slug END AS group_key,
			(slug.entity_slug <> '') AS is_entity,
			c.attribution,
			c.from_address,
			c.blob_count,
			c.max_priority_fee_per_gas
		FROM candidates c
		CROSS JOIN LATERAL (
			SELECT COALESCE(NULLIF(` + entityKeySQL("c.attribution") + `, ''), '') AS entity_slug
		) slug
	)
	SELECT
		group_key AS key,
		(array_agg(attribution ORDER BY blob_count DESC, from_address ASC))[1] AS name,
		bool_or(is_entity) AS is_entity,
		COUNT(*)::bigint AS txs,
		COALESCE(SUM(blob_count), 0)::bigint AS blobs,
		MAX(max_priority_fee_per_gas)::text AS max_tip_wei,
		(percentile_cont(0.5) WITHIN GROUP (ORDER BY max_priority_fee_per_gas::double precision))::numeric::text AS p50_tip_wei
	FROM keyed
	GROUP BY group_key
	ORDER BY blobs DESC, key ASC
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
	WITH bounds AS (
		SELECT $2::timestamp AS range_start, $3::timestamp AS range_end
	),
	buckets AS (
		SELECT g.bucket_start, b.range_start, b.range_end
		FROM bounds b
		CROSS JOIN LATERAL generate_series(
			b.range_start,
			b.range_end - ($4::bigint * INTERVAL '1 second'),
			$4::bigint * INTERVAL '1 second'
		) AS g(bucket_start)
		WHERE b.range_end > b.range_start
	),
	builder_rows AS MATERIALIZED (
		SELECT
			TIMESTAMP 'epoch' + (
				FLOOR(EXTRACT(EPOCH FROM bb.block_timestamp) / $4::numeric)::bigint
				* $4::bigint
				* INTERVAL '1 second'
			) AS bucket_start,
			bb.builder_key,
			bb.builder_name,
			COALESCE(bm.blob_count, 0) AS blob_count
		FROM bounds b
		JOIN block_builders bb
			ON bb.chain_id = $1
			AND bb.block_timestamp >= b.range_start
			AND bb.block_timestamp < b.range_end
		LEFT JOIN block_metrics bm
			ON bm.chain_id = $1
			AND bm.block_number = bb.block_number
	),
` + builderShareSeriesSQL("$5") + fmt.Sprintf(builderShareSelectSQL, "NULL::bigint", "b.bucket_start ASC")

// queryBuilderShareBlockChart emits one bucket per indexed block.
//
// Args: $1 chain id, $2 range start, $3 range end, $4 series limit.
var queryBuilderShareBlockChart = `
	WITH bounds AS (
		SELECT $2::timestamp AS range_start, $3::timestamp AS range_end
	),
	buckets AS (
		SELECT
			bb.block_number,
			bb.block_timestamp AS bucket_start,
			b.range_start,
			b.range_end
		FROM bounds b
		JOIN block_builders bb
			ON bb.chain_id = $1
			AND bb.block_timestamp >= b.range_start
			AND bb.block_timestamp < b.range_end
	),
	builder_rows AS MATERIALIZED (
		SELECT
			bu.bucket_start,
			bb.builder_key,
			bb.builder_name,
			COALESCE(bm.blob_count, 0) AS blob_count
		FROM buckets bu
		JOIN block_builders bb
			ON bb.chain_id = $1
			AND bb.block_number = bu.block_number
		LEFT JOIN block_metrics bm
			ON bm.chain_id = $1
			AND bm.block_number = bu.block_number
	),
` + builderShareSeriesSQL("$4") + fmt.Sprintf(builderShareSelectSQL, "b.block_number", "b.block_number ASC")
