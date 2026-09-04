package api

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

// BlobTipsChartResponse contains bucketed priority fees (tips) paid by blob
// transactions, overall and per attributed entity. Only blobs with a recorded
// priority fee (rows indexed since the column existed) enter the fee
// statistics; summary.total_blobs counts every blob in the range so clients
// can tell partial coverage from a quiet market.
type BlobTipsChartResponse struct {
	ChainID       int                      `json:"chain_id"`
	NetworkName   string                   `json:"network_name"`
	Range         string                   `json:"range"`
	Granularity   string                   `json:"granularity"`
	BucketSeconds int64                    `json:"bucket_seconds"`
	StartTime     time.Time                `json:"start_time"`
	EndTime       time.Time                `json:"end_time"`
	GeneratedAt   time.Time                `json:"generated_at"`
	Series        []AttributionUsageSeries `json:"series"`
	Points        []BlobTipsChartPoint     `json:"points"`
	Summary       BlobTipsChartSummary     `json:"summary"`
}

// BlobTipsChartPoint is one tip chart bucket. Fee fields are gwei per
// execution gas.
type BlobTipsChartPoint struct {
	Timestamp  time.Time `json:"timestamp"`
	StartBlock *int64    `json:"start_block,omitempty"`
	EndBlock   *int64    `json:"end_block,omitempty"`
	// BlobCount is the number of blobs in the bucket with a recorded priority fee.
	BlobCount              int                           `json:"blob_count"`
	AveragePriorityFeeGwei string                        `json:"average_priority_fee_gwei"`
	MedianPriorityFeeGwei  string                        `json:"median_priority_fee_gwei"`
	P95PriorityFeeGwei     string                        `json:"p95_priority_fee_gwei"`
	MaxPriorityFeeGwei     string                        `json:"max_priority_fee_gwei"`
	Values                 map[string]BlobTipsChartValue `json:"values"`
}

// BlobTipsChartValue is one series' tips within a bucket.
type BlobTipsChartValue struct {
	BlobCount              int    `json:"blob_count"`
	AveragePriorityFeeGwei string `json:"average_priority_fee_gwei"`
	MaxPriorityFeeGwei     string `json:"max_priority_fee_gwei"`
}

// BlobTipsChartSummary aggregates a tip chart range.
type BlobTipsChartSummary struct {
	// TotalBlobs counts every blob in the range, priced or not.
	TotalBlobs int `json:"total_blobs"`
	// PricedBlobs counts the blobs with a recorded priority fee, the
	// population behind every fee figure in the response.
	PricedBlobs            int                  `json:"priced_blobs"`
	AveragePriorityFeeGwei string               `json:"average_priority_fee_gwei"`
	MedianPriorityFeeGwei  string               `json:"median_priority_fee_gwei"`
	P95PriorityFeeGwei     string               `json:"p95_priority_fee_gwei"`
	MaxPriorityFeeGwei     string               `json:"max_priority_fee_gwei"`
	Shares                 []BlobTipsChartShare `json:"shares"`
}

// BlobTipsChartShare is one series' tips over the whole range.
type BlobTipsChartShare struct {
	Key                    string  `json:"key"`
	Name                   string  `json:"name"`
	Category               string  `json:"category"`
	BlobCount              int     `json:"blob_count"`
	BlobSharePercent       float64 `json:"blob_share_percent"`
	AveragePriorityFeeGwei string  `json:"average_priority_fee_gwei"`
	MaxPriorityFeeGwei     string  `json:"max_priority_fee_gwei"`
}

type blobTipsChartRow struct {
	Timestamp                    time.Time      `db:"timestamp"`
	RangeStart                   time.Time      `db:"range_start"`
	RangeEnd                     time.Time      `db:"range_end"`
	BlockNumber                  sql.NullInt64  `db:"block_number"`
	BucketBlobCount              int            `db:"bucket_blob_count"`
	BucketAveragePriorityFeeWei  string         `db:"bucket_average_priority_fee_wei"`
	BucketMedianPriorityFeeWei   string         `db:"bucket_median_priority_fee_wei"`
	BucketP95PriorityFeeWei      string         `db:"bucket_p95_priority_fee_wei"`
	BucketMaxPriorityFeeWei      string         `db:"bucket_max_priority_fee_wei"`
	Key                          sql.NullString `db:"series_key"`
	Name                         sql.NullString `db:"series_name"`
	Category                     sql.NullString `db:"series_category"`
	Address                      sql.NullString `db:"series_address"`
	SeriesBlobCount              int            `db:"series_blob_count"`
	SeriesAveragePriorityFeeWei  string         `db:"series_average_priority_fee_wei"`
	SeriesMaxPriorityFeeWei      string         `db:"series_max_priority_fee_wei"`
	SummaryTotalBlobs            int            `db:"summary_total_blobs"`
	SummaryPricedBlobs           int            `db:"summary_priced_blobs"`
	SummaryAveragePriorityFeeWei string         `db:"summary_average_priority_fee_wei"`
	SummaryMedianPriorityFeeWei  string         `db:"summary_median_priority_fee_wei"`
	SummaryP95PriorityFeeWei     string         `db:"summary_p95_priority_fee_wei"`
	SummaryMaxPriorityFeeWei     string         `db:"summary_max_priority_fee_wei"`
}

// GetBlobTipsChart godoc
// @Summary Get blob tip chart data
// @Description Retrieve bucketed priority fees (tips per execution gas) paid by blob transactions, overall and grouped by attribution with long-tail grouping. Builders order competing blob transactions by this fee, so it shows which senders outbid the rest for a block's blob slots. Only blobs with a recorded priority fee enter the fee statistics; summary.total_blobs counts every blob in the range.
// @Tags charts
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Param range query string false "Range: 1h, 24h, 7d, or 30d (default: 24h; all is not supported)"
// @Param granularity query string false "Granularity: auto, block, minute, hour, or day (default: auto)"
// @Param limit query int false "Top attribution series before grouping long-tail into other (default: 5, max: 25)"
// @Success 200 {object} Response{data=BlobTipsChartResponse} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 500 {object} Response "Internal server error"
// @Failure 503 {object} Response "Database overloaded; retry later"
// @Router /charts/blob-tips [get]
func (a *API) GetBlobTipsChart(w http.ResponseWriter, r *http.Request) {
	network, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	chart, err := parseChartRequest(r, time.Now().UTC(), false)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Tips are read from the blobs table directly (no rollup carries them),
	// through the partial covering index on priced rows; the scan is bounded
	// by the range like blob-market's.
	if chart.Range == chartRangeAll {
		a.respondError(w, http.StatusBadRequest, "range=all is not supported for blob-tips; use range=30d or narrower")
		return
	}

	seriesLimit, err := parseAttributionSeriesLimit(r.URL.Query().Get("limit"))
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	logger.Debug("Getting blob tips chart",
		zap.String("network", network.Name),
		zap.String("range", chart.Range),
		zap.String("granularity", chart.Granularity),
		zap.Int("series_limit", seriesLimit))

	cacheKey := fmt.Sprintf("chart:blob-tips:%d:%s:%s:%d:%d", network.ChainID, chart.Range, chart.Granularity, chart.BucketSeconds, seriesLimit)
	value, err := a.cachedChartResponse(r, cacheKey, func(ctx context.Context) (interface{}, error) {
		queryCtx, cancel := context.WithTimeout(ctx, aggregateQueryTimeout)
		defer cancel()

		query := queryBlobTipsTimeChart
		args := append(chartBlobMarketTimeArgs(network.ChainID, chart), seriesLimit)
		if chart.Granularity == chartGranularityBlock {
			query = queryBlobTipsBlockChart
			args = append(chartBlockArgs(network.ChainID, chart), seriesLimit)
		}

		var rows []blobTipsChartRow
		if err := a.db.SelectContext(queryCtx, &rows, query, args...); err != nil {
			return nil, err
		}
		return buildBlobTipsChartResponse(network.ChainID, network.Name, chart, rows), nil
	})
	if err != nil {
		logger.Error("Failed to get blob tips chart",
			zap.String("network", network.Name),
			zap.Error(err))
		a.respondAggregateError(w, err, "Failed to get blob tips chart")
		return
	}

	setCacheControl(w, aggregateCacheTTL, aggregateEdgeTTL)
	a.respondSuccess(w, value)
}

// blobTipsSeriesTotal accumulates one series across buckets. The range
// average is the blob-weighted mean of bucket averages, which equals the
// mean over the underlying blobs exactly, so it is kept as a rational rather
// than re-rounded per bucket.
type blobTipsSeriesTotal struct {
	blobCount   int
	weightedSum *big.Rat
	maxWei      string
}

func buildBlobTipsChartResponse(networkID int, networkName string, chart chartRequest, rows []blobTipsChartRow) BlobTipsChartResponse {
	response := BlobTipsChartResponse{
		ChainID:       networkID,
		NetworkName:   networkName,
		Range:         chart.Range,
		Granularity:   chart.Granularity,
		BucketSeconds: chart.BucketSeconds,
		StartTime:     chart.StartTime,
		EndTime:       chart.EndTime,
		GeneratedAt:   chart.GeneratedAt,
		Series:        []AttributionUsageSeries{},
		Points:        make([]BlobTipsChartPoint, 0, len(rows)),
		Summary:       zeroBlobTipsChartSummary(),
	}
	if len(rows) > 0 {
		response.StartTime = rows[0].RangeStart
		response.EndTime = rows[0].RangeEnd
		first := rows[0]
		response.Summary = BlobTipsChartSummary{
			TotalBlobs:             first.SummaryTotalBlobs,
			PricedBlobs:            first.SummaryPricedBlobs,
			AveragePriorityFeeGwei: gweiOrZero(first.SummaryAveragePriorityFeeWei),
			MedianPriorityFeeGwei:  gweiOrZero(first.SummaryMedianPriorityFeeWei),
			P95PriorityFeeGwei:     gweiOrZero(first.SummaryP95PriorityFeeWei),
			MaxPriorityFeeGwei:     gweiOrZero(first.SummaryMaxPriorityFeeWei),
			Shares:                 []BlobTipsChartShare{},
		}
	}

	pointIndexByKey := make(map[attributionPointKey]int)
	seriesByKey := make(map[string]AttributionUsageSeries)
	totalsByKey := make(map[string]*blobTipsSeriesTotal)

	for _, row := range rows {
		pointKey := attributionPointKey{timestamp: row.Timestamp}
		if row.BlockNumber.Valid {
			pointKey.block = row.BlockNumber.Int64
			pointKey.hasBlock = true
		}
		pointIndex, ok := pointIndexByKey[pointKey]
		if !ok {
			point := BlobTipsChartPoint{
				Timestamp:              row.Timestamp,
				BlobCount:              row.BucketBlobCount,
				AveragePriorityFeeGwei: gweiOrZero(row.BucketAveragePriorityFeeWei),
				MedianPriorityFeeGwei:  gweiOrZero(row.BucketMedianPriorityFeeWei),
				P95PriorityFeeGwei:     gweiOrZero(row.BucketP95PriorityFeeWei),
				MaxPriorityFeeGwei:     gweiOrZero(row.BucketMaxPriorityFeeWei),
				Values:                 make(map[string]BlobTipsChartValue),
			}
			if row.BlockNumber.Valid {
				block := row.BlockNumber.Int64
				point.StartBlock = &block
				point.EndBlock = &block
			}
			response.Points = append(response.Points, point)
			pointIndex = len(response.Points) - 1
			pointIndexByKey[pointKey] = pointIndex
		}

		if !row.Key.Valid || strings.TrimSpace(row.Key.String) == "" || row.SeriesBlobCount <= 0 {
			continue
		}
		key := row.Key.String
		response.Points[pointIndex].Values[key] = BlobTipsChartValue{
			BlobCount:              row.SeriesBlobCount,
			AveragePriorityFeeGwei: gweiOrZero(row.SeriesAveragePriorityFeeWei),
			MaxPriorityFeeGwei:     gweiOrZero(row.SeriesMaxPriorityFeeWei),
		}

		if _, ok := seriesByKey[key]; !ok {
			seriesByKey[key] = AttributionUsageSeries{
				Key:      key,
				Name:     nullStringDefault(row.Name, key),
				Category: nullStringDefault(row.Category, "unknown"),
				Address:  nullStringDefault(row.Address, ""),
			}
		}
		total, ok := totalsByKey[key]
		if !ok {
			total = &blobTipsSeriesTotal{weightedSum: new(big.Rat), maxWei: "0"}
			totalsByKey[key] = total
		}
		total.blobCount += row.SeriesBlobCount
		if average, ok := decimalStringToRat(row.SeriesAveragePriorityFeeWei); ok {
			total.weightedSum.Add(total.weightedSum, average.Mul(average, big.NewRat(int64(row.SeriesBlobCount), 1)))
		}
		if compareDecimalStrings(row.SeriesMaxPriorityFeeWei, total.maxWei) > 0 {
			total.maxWei = row.SeriesMaxPriorityFeeWei
		}
	}

	response.Series = orderedBlobTipsSeries(seriesByKey, totalsByKey)
	zeroValue := BlobTipsChartValue{AveragePriorityFeeGwei: "0", MaxPriorityFeeGwei: "0"}
	for i := range response.Points {
		for _, series := range response.Series {
			if _, ok := response.Points[i].Values[series.Key]; !ok {
				response.Points[i].Values[series.Key] = zeroValue
			}
		}
	}

	for _, series := range response.Series {
		total := totalsByKey[series.Key]
		averageWei := "0"
		if total.blobCount > 0 {
			average := new(big.Rat).Quo(total.weightedSum, big.NewRat(int64(total.blobCount), 1))
			averageWei = formatRatDecimal(average, 18)
		}
		response.Summary.Shares = append(response.Summary.Shares, BlobTipsChartShare{
			Key:                    series.Key,
			Name:                   series.Name,
			Category:               series.Category,
			BlobCount:              total.blobCount,
			BlobSharePercent:       chartPercentage(float64(total.blobCount), float64(response.Summary.PricedBlobs)),
			AveragePriorityFeeGwei: gweiOrZero(averageWei),
			MaxPriorityFeeGwei:     gweiOrZero(total.maxWei),
		})
	}

	return response
}

// orderedBlobTipsSeries ranks series by blob count, then by the highest tip
// they paid, so the top posters lead and, among equals, the top bidders.
func orderedBlobTipsSeries(seriesByKey map[string]AttributionUsageSeries, totalsByKey map[string]*blobTipsSeriesTotal) []AttributionUsageSeries {
	series := make([]AttributionUsageSeries, 0, len(seriesByKey))
	for _, item := range seriesByKey {
		series = append(series, item)
	}
	sort.Slice(series, func(i, j int) bool {
		left := totalsByKey[series[i].Key]
		right := totalsByKey[series[j].Key]
		if left.blobCount != right.blobCount {
			return left.blobCount > right.blobCount
		}
		if cmp := compareDecimalStrings(left.maxWei, right.maxWei); cmp != 0 {
			return cmp > 0
		}
		return series[i].Key < series[j].Key
	})
	return series
}

func zeroBlobTipsChartSummary() BlobTipsChartSummary {
	return BlobTipsChartSummary{
		AveragePriorityFeeGwei: "0",
		MedianPriorityFeeGwei:  "0",
		P95PriorityFeeGwei:     "0",
		MaxPriorityFeeGwei:     "0",
		Shares:                 []BlobTipsChartShare{},
	}
}

// gweiOrZero converts a decimal wei string to gwei, mapping an empty or
// malformed value to "0" so a missing column never surfaces as "".
func gweiOrZero(wei string) string {
	if formatted := formatDecimalWeiAsGwei(wei); formatted != "" {
		return formatted
	}
	return "0"
}

// blobTipsSeriesSQL builds the CTE chain shared by the tip chart queries. It
// expects a tip_source CTE with (bucket_start, from_address, priority_fee,
// raw_name, raw_category) rows, one per priced blob in range (rows indexed
// before the fee was stored never enter it, which is what lets the scan use
// the partial covering index), and a range_blocks CTE whose total_blobs
// counts every blob in the range from block_metrics so callers can report
// coverage without touching the unpriced blob rows.
func blobTipsSeriesSQL(limitPlaceholder string) string {
	return `
	entity_rows AS MATERIALIZED (
		SELECT
			src.bucket_start,
			src.from_address,
			src.priority_fee,
			CASE
				WHEN src.raw_name = '' THEN 'unknown'
				ELSE COALESCE(NULLIF(` + entityKeySQL("src.raw_name") + `, ''), 'unknown')
			END AS entity_key,
			CASE WHEN src.raw_name = '' THEN 'Unknown' ELSE src.raw_name END AS entity_name,
			CASE
				WHEN src.raw_name = '' THEN 'unknown'
				ELSE COALESCE(NULLIF(src.raw_category, ''), 'unknown')
			END AS entity_category
		FROM tip_source src
	),
	entity_totals AS (
		SELECT
			entity_key,
			CASE WHEN entity_key = 'unknown' THEN 'Unknown' ELSE MIN(entity_name) END AS entity_name,
			CASE WHEN entity_key = 'unknown' THEN 'unknown' ELSE COALESCE(NULLIF(MIN(NULLIF(entity_category, 'unknown')), ''), 'unknown') END AS entity_category,
			CASE WHEN COUNT(DISTINCT from_address) = 1 THEN MIN(from_address) ELSE NULL END AS entity_address,
			COUNT(*)::int AS blob_count,
			MAX(priority_fee) AS max_priority_fee
		FROM entity_rows
		GROUP BY entity_key
	),
	top_entities AS (
		SELECT *
		FROM entity_totals
		WHERE entity_key <> 'unknown'
		ORDER BY blob_count DESC, max_priority_fee DESC, entity_name ASC
		LIMIT ` + limitPlaceholder + `
	),
	series_rows AS (
		SELECT
			er.bucket_start,
			er.priority_fee,
			CASE
				WHEN er.entity_key = 'unknown' THEN 'unknown'
				WHEN te.entity_key IS NOT NULL THEN er.entity_key
				ELSE 'other'
			END AS series_key,
			CASE
				WHEN er.entity_key = 'unknown' THEN 'Unknown'
				WHEN te.entity_key IS NOT NULL THEN te.entity_name
				ELSE 'Other'
			END AS series_name,
			CASE
				WHEN er.entity_key = 'unknown' THEN 'unknown'
				WHEN te.entity_key IS NOT NULL THEN te.entity_category
				ELSE 'other'
			END AS series_category,
			CASE
				WHEN er.entity_key <> 'unknown' AND te.entity_key IS NOT NULL THEN te.entity_address
				ELSE NULL
			END AS series_address
		FROM entity_rows er
		LEFT JOIN top_entities te ON te.entity_key = er.entity_key
	),
	bucketed_series AS (
		SELECT
			bucket_start,
			series_key,
			series_name,
			series_category,
			series_address,
			COUNT(*)::int AS blob_count,
			AVG(priority_fee)::text AS average_priority_fee_wei,
			MAX(priority_fee)::text AS max_priority_fee_wei
		FROM series_rows
		GROUP BY bucket_start, series_key, series_name, series_category, series_address
	),
	bucket_stats AS (
		SELECT
			bucket_start,
			COUNT(*)::int AS blob_count,
			COALESCE(AVG(priority_fee), 0)::text AS average_priority_fee_wei,
			COALESCE(percentile_disc(0.5) WITHIN GROUP (ORDER BY priority_fee), 0)::text AS median_priority_fee_wei,
			COALESCE(percentile_disc(0.95) WITHIN GROUP (ORDER BY priority_fee), 0)::text AS p95_priority_fee_wei,
			COALESCE(MAX(priority_fee), 0)::text AS max_priority_fee_wei
		FROM tip_source
		GROUP BY bucket_start
	),
	summary AS (
		SELECT
			rb.total_blobs,
			COUNT(ts.priority_fee)::int AS priced_blobs,
			COALESCE(AVG(ts.priority_fee), 0)::text AS average_priority_fee_wei,
			COALESCE(percentile_disc(0.5) WITHIN GROUP (ORDER BY ts.priority_fee), 0)::text AS median_priority_fee_wei,
			COALESCE(percentile_disc(0.95) WITHIN GROUP (ORDER BY ts.priority_fee), 0)::text AS p95_priority_fee_wei,
			COALESCE(MAX(ts.priority_fee), 0)::text AS max_priority_fee_wei
		FROM range_blocks rb
		LEFT JOIN tip_source ts ON TRUE
		GROUP BY rb.total_blobs
	)
`
}

const blobTipsSelectSQL = `
	SELECT
		b.bucket_start AS timestamp,
		b.range_start,
		b.range_end,
		%s AS block_number,
		COALESCE(bs.blob_count, 0) AS bucket_blob_count,
		COALESCE(bs.average_priority_fee_wei, '0') AS bucket_average_priority_fee_wei,
		COALESCE(bs.median_priority_fee_wei, '0') AS bucket_median_priority_fee_wei,
		COALESCE(bs.p95_priority_fee_wei, '0') AS bucket_p95_priority_fee_wei,
		COALESCE(bs.max_priority_fee_wei, '0') AS bucket_max_priority_fee_wei,
		s.series_key,
		s.series_name,
		s.series_category,
		s.series_address,
		COALESCE(s.blob_count, 0) AS series_blob_count,
		COALESCE(s.average_priority_fee_wei, '0') AS series_average_priority_fee_wei,
		COALESCE(s.max_priority_fee_wei, '0') AS series_max_priority_fee_wei,
		sm.total_blobs AS summary_total_blobs,
		sm.priced_blobs AS summary_priced_blobs,
		sm.average_priority_fee_wei AS summary_average_priority_fee_wei,
		sm.median_priority_fee_wei AS summary_median_priority_fee_wei,
		sm.p95_priority_fee_wei AS summary_p95_priority_fee_wei,
		sm.max_priority_fee_wei AS summary_max_priority_fee_wei
	FROM buckets b
	LEFT JOIN bucket_stats bs ON bs.bucket_start = b.bucket_start
	LEFT JOIN bucketed_series s ON s.bucket_start = b.bucket_start
	CROSS JOIN summary sm
	ORDER BY %s, s.blob_count DESC NULLS LAST, s.series_key ASC
`

// queryBlobTipsTimeChart buckets blob priority fees by wall-clock interval.
// Args: chain id, range start, range end, bucket seconds, series limit.
//
// The blob scan is restricted to priced rows so it can be served by the
// partial covering index (idx_blobs_chain_timestamp_priced_cover); the
// unpriced remainder is only ever counted, and that count comes from
// block_metrics, one row per block rather than one per blob.
var queryBlobTipsTimeChart = `
	WITH bounds AS (
		SELECT
			$2::timestamp AS range_start,
			$3::timestamp AS range_end
	),
	buckets AS (
		SELECT
			g.bucket_start,
			b.range_start,
			b.range_end
		FROM bounds b
		CROSS JOIN LATERAL generate_series(
			b.range_start,
			b.range_end - ($4::bigint * INTERVAL '1 second'),
			$4::bigint * INTERVAL '1 second'
		) AS g(bucket_start)
		WHERE b.range_end > b.range_start
	),
	range_blocks AS (
		SELECT COALESCE(SUM(bm.blob_count), 0)::int AS total_blobs
		FROM bounds b
		LEFT JOIN block_metrics bm
			ON bm.chain_id = $1
			AND bm.block_timestamp >= b.range_start
			AND bm.block_timestamp < b.range_end
	),
	tip_source AS MATERIALIZED (
		SELECT
			TIMESTAMP 'epoch' + (
				FLOOR(EXTRACT(EPOCH FROM bl.timestamp) / $4::numeric)::bigint
				* $4::bigint
				* INTERVAL '1 second'
			) AS bucket_start,
			bl.from_address,
			bl.priority_fee_per_gas::numeric AS priority_fee,
			COALESCE(NULLIF(BTRIM(bl.user_attribution), ''), NULLIF(BTRIM(known.name), ''), '') AS raw_name,
			COALESCE(NULLIF(BTRIM(known.category), ''), '') AS raw_category
		FROM bounds b
		JOIN blobs bl
			ON bl.chain_id = $1
			AND bl.timestamp >= b.range_start
			AND bl.timestamp < b.range_end
			AND bl.priority_fee_per_gas IS NOT NULL
		LEFT JOIN blob_users known
			ON known.chain_id = bl.chain_id
			AND LOWER(known.address) = LOWER(bl.from_address)
	),
` + blobTipsSeriesSQL("$5") + fmt.Sprintf(blobTipsSelectSQL, "NULL::bigint", "b.bucket_start ASC")

// queryBlobTipsBlockChart buckets blob priority fees per indexed block.
// Args: chain id, range start, range end, series limit.
var queryBlobTipsBlockChart = `
	WITH bounds AS (
		SELECT $2::timestamp AS range_start, $3::timestamp AS range_end
	),
	buckets AS (
		SELECT
			bm.block_number,
			bm.block_timestamp AS bucket_start,
			b.range_start,
			b.range_end
		FROM block_metrics bm
		CROSS JOIN bounds b
		WHERE bm.chain_id = $1
			AND bm.block_timestamp >= b.range_start
			AND bm.block_timestamp < b.range_end
	),
	range_blocks AS (
		SELECT COALESCE(SUM(bm.blob_count), 0)::int AS total_blobs
		FROM buckets bu
		JOIN block_metrics bm ON bm.chain_id = $1 AND bm.block_number = bu.block_number
	),
	tip_source AS MATERIALIZED (
		SELECT
			bu.bucket_start,
			bl.from_address,
			bl.priority_fee_per_gas::numeric AS priority_fee,
			COALESCE(NULLIF(BTRIM(bl.user_attribution), ''), NULLIF(BTRIM(known.name), ''), '') AS raw_name,
			COALESCE(NULLIF(BTRIM(known.category), ''), '') AS raw_category
		FROM buckets bu
		JOIN blobs bl
			ON bl.chain_id = $1
			AND bl.block_number = bu.block_number
			AND bl.priority_fee_per_gas IS NOT NULL
		LEFT JOIN blob_users known
			ON known.chain_id = bl.chain_id
			AND LOWER(known.address) = LOWER(bl.from_address)
	),
` + blobTipsSeriesSQL("$4") + fmt.Sprintf(blobTipsSelectSQL, "b.block_number", "b.block_number ASC")
