package db

import (
	"context"
	"fmt"
	"time"
)

const (
	// FineChartRollupBucketSeconds is the fine chart-rollup bucket size.
	// Mirrors chart_rollup_fine_bucket_seconds() in migration 2.
	FineChartRollupBucketSeconds = 60

	// FineChartRollupRetention is how much fine-bucket history is kept.
	// Mirrors chart_rollup_fine_retention() in migration 2; the trigger paths
	// stop maintaining fine buckets for rows older than this, and the indexer
	// prunes expired fine buckets on a timer.
	FineChartRollupRetention = 48 * time.Hour
)

// FineChartRollupBucketDuration is FineChartRollupBucketSeconds as a Duration.
const FineChartRollupBucketDuration = FineChartRollupBucketSeconds * time.Second

// backfillFineBlobChartRollupsChunk recomputes fine blob rollup buckets from
// raw blobs for [start, end). The upsert fully replaces each bucket, which is
// only correct because callers pass bucket-aligned chunk boundaries — a bucket
// never spans two chunks, so each statement sees the bucket's complete raw
// rows. Mirrors blob_chart_rollups_insert_statement_trigger's aggregation.
const backfillFineBlobChartRollupsChunk = `
	INSERT INTO blob_chart_rollups (
		chain_id, bucket_seconds, bucket_start, from_address, user_attribution,
		blob_count, blob_bytes, blob_gas_used, total_cost_wei, sum_size_base_fee, updated_at
	)
	SELECT
		b.chain_id,
		$4::int,
		chart_rollup_bucket_start(b.timestamp, $4::int),
		b.from_address,
		COALESCE(NULLIF(MAX(BTRIM(b.user_attribution)), ''), ''),
		COUNT(*)::bigint,
		COALESCE(SUM(b.blob_size_bytes), 0)::bigint,
		COALESCE(SUM(COALESCE(b.blob_gas_used, 0)), 0)::bigint,
		COALESCE(SUM(b.total_cost_wei::numeric), 0),
		COALESCE(SUM(b.blob_size_bytes::numeric * b.base_fee_per_blob_gas::numeric), 0),
		NOW()
	FROM blobs b
	WHERE b.chain_id = $1
		AND b.confirmed = true
		AND b.timestamp >= $2
		AND b.timestamp < $3
	GROUP BY b.chain_id, chart_rollup_bucket_start(b.timestamp, $4::int), b.from_address
	ON CONFLICT (chain_id, bucket_seconds, bucket_start, from_address) DO UPDATE SET
		user_attribution = EXCLUDED.user_attribution,
		blob_count = EXCLUDED.blob_count,
		blob_bytes = EXCLUDED.blob_bytes,
		blob_gas_used = EXCLUDED.blob_gas_used,
		total_cost_wei = EXCLUDED.total_cost_wei,
		sum_size_base_fee = EXCLUDED.sum_size_base_fee,
		updated_at = NOW()
`

// backfillFineBlockMetricsRollupsChunk recomputes fine block-metric rollup
// buckets from raw block_metrics for [start, end). Same bucket-aligned chunk
// contract as the blob variant; mirrors block_metrics_rollups_refresh's
// aggregation, including the effective target/max fallbacks.
const backfillFineBlockMetricsRollupsChunk = `
	INSERT INTO block_metrics_rollups (
		chain_id, bucket_seconds, bucket_start, block_count, start_block, end_block,
		sum_blob_count, sum_blob_gas_used, sum_blob_gas_target, sum_blob_base_fee,
		sum_utilization, median_blob_base_fee, p95_blob_base_fee,
		blocks_above_target, blocks_at_max, updated_at
	)
	SELECT
		bm.chain_id,
		$4::int,
		chart_rollup_bucket_start(bm.block_timestamp, $4::int),
		COUNT(*)::bigint,
		COALESCE(MIN(bm.block_number), 0),
		COALESCE(MAX(bm.block_number), 0),
		COALESCE(SUM(bm.blob_count), 0)::bigint,
		COALESCE(SUM(bm.blob_gas_used::numeric), 0),
		COALESCE(SUM(bm.blob_gas_target::numeric), 0),
		COALESCE(SUM(bm.blob_base_fee::numeric), 0),
		COALESCE(SUM(bm.utilization_ratio::numeric), 0),
		COALESCE(percentile_disc(0.5) WITHIN GROUP (ORDER BY bm.blob_base_fee::numeric), 0),
		COALESCE(percentile_disc(0.95) WITHIN GROUP (ORDER BY bm.blob_base_fee::numeric), 0),
		COUNT(*) FILTER (
			WHERE bm.target_blob_gas > 0 AND bm.effective_blob_gas_used > bm.target_blob_gas
		)::bigint,
		COUNT(*) FILTER (
			WHERE bm.max_blob_gas > 0 AND bm.effective_blob_gas_used >= bm.max_blob_gas
		)::bigint,
		NOW()
	FROM (
		SELECT
			chain_id,
			block_timestamp,
			block_number,
			blob_count,
			blob_gas_used,
			blob_gas_target,
			blob_base_fee,
			utilization_ratio,
			GREATEST(blob_gas_used, 0)::bigint AS effective_blob_gas_used,
			CASE
				WHEN blob_gas_target > 0 THEN blob_gas_target
				WHEN blob_params_target > 0 THEN blob_params_target::bigint * 131072
				ELSE 0
			END::bigint AS target_blob_gas,
			CASE
				WHEN blob_gas_limit > 0 THEN blob_gas_limit
				WHEN blob_params_max > 0 THEN blob_params_max::bigint * 131072
				ELSE 0
			END::bigint AS max_blob_gas
		FROM block_metrics
		WHERE chain_id = $1
			AND block_timestamp >= $2
			AND block_timestamp < $3
	) bm
	GROUP BY bm.chain_id, chart_rollup_bucket_start(bm.block_timestamp, $4::int)
	ON CONFLICT (chain_id, bucket_seconds, bucket_start) DO UPDATE SET
		block_count = EXCLUDED.block_count,
		start_block = EXCLUDED.start_block,
		end_block = EXCLUDED.end_block,
		sum_blob_count = EXCLUDED.sum_blob_count,
		sum_blob_gas_used = EXCLUDED.sum_blob_gas_used,
		sum_blob_gas_target = EXCLUDED.sum_blob_gas_target,
		sum_blob_base_fee = EXCLUDED.sum_blob_base_fee,
		sum_utilization = EXCLUDED.sum_utilization,
		median_blob_base_fee = EXCLUDED.median_blob_base_fee,
		p95_blob_base_fee = EXCLUDED.p95_blob_base_fee,
		blocks_above_target = EXCLUDED.blocks_above_target,
		blocks_at_max = EXCLUDED.blocks_at_max,
		updated_at = NOW()
`

// BackfillFineChartRollupsChunk recomputes the fine (60s) chart rollup buckets
// covering [start, end) from raw blobs and block_metrics. Both bounds must be
// aligned to the fine bucket size: each bucket is fully replaced, so a bucket
// spanning a chunk boundary would lose the rows outside the chunk.
func (db *DB) BackfillFineChartRollupsChunk(ctx context.Context, networkID int, start, end time.Time) error {
	if !start.Truncate(FineChartRollupBucketDuration).Equal(start) || !end.Truncate(FineChartRollupBucketDuration).Equal(end) {
		return fmt.Errorf("fine rollup backfill chunk bounds must be aligned to %ds buckets, got [%s, %s)",
			FineChartRollupBucketSeconds, start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano))
	}
	if _, err := db.ExecContext(ctx, backfillFineBlobChartRollupsChunk, networkID, start, end, FineChartRollupBucketSeconds); err != nil {
		return fmt.Errorf("failed to backfill fine blob chart rollups for network %d [%s, %s): %w",
			networkID, start.Format(time.RFC3339), end.Format(time.RFC3339), err)
	}
	if _, err := db.ExecContext(ctx, backfillFineBlockMetricsRollupsChunk, networkID, start, end, FineChartRollupBucketSeconds); err != nil {
		return fmt.Errorf("failed to backfill fine block metrics rollups for network %d [%s, %s): %w",
			networkID, start.Format(time.RFC3339), end.Format(time.RFC3339), err)
	}
	return nil
}

// PruneFineChartRollups deletes fine (60s) rollup buckets starting before the
// cutoff and returns the total rows removed. Deleting rollup rows directly
// fires no triggers, and pruned buckets are disjoint from the recent buckets
// live writes touch, so this needs no write serialization.
func (db *DB) PruneFineChartRollups(ctx context.Context, networkID int, cutoff time.Time) (int64, error) {
	var total int64
	for _, query := range []string{
		"DELETE FROM blob_chart_rollups WHERE chain_id = $1 AND bucket_seconds = $2 AND bucket_start < $3",
		"DELETE FROM block_metrics_rollups WHERE chain_id = $1 AND bucket_seconds = $2 AND bucket_start < $3",
	} {
		res, err := db.ExecContext(ctx, query, networkID, FineChartRollupBucketSeconds, cutoff)
		if err != nil {
			return total, fmt.Errorf("failed to prune fine chart rollups for network %d: %w", networkID, err)
		}
		deleted, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("failed to count pruned fine chart rollups for network %d: %w", networkID, err)
		}
		total += deleted
	}
	return total, nil
}
