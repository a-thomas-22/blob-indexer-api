package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

// maxBuilderSeriesLimit caps the `limit` parameter of
// /charts/builder-share, matching the attribution chart's ceiling.
const maxBuilderSeriesLimit = 25

// BuilderShareSeries identifies one builder series of the share chart. The
// long tail collapses into a single series keyed 'other'.
type BuilderShareSeries struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	// Known mirrors /builders: true only for registry-recognized builders,
	// and always false for the aggregate 'other' series.
	Known bool `json:"known"`
}

// BuilderShareValue is one builder's blocks and blobs within one bucket.
type BuilderShareValue struct {
	Blocks int64 `json:"blocks"`
	Blobs  int64 `json:"blobs"`
}

// BuilderShareChartPoint is one bucket of the builder share chart. Every
// series present in the response carries a value in every bucket, zero-filled
// where the builder produced nothing, so clients can stack the series without
// aligning sparse arrays.
type BuilderShareChartPoint struct {
	Timestamp  time.Time `json:"timestamp"`
	StartBlock *int64    `json:"start_block,omitempty"`
	EndBlock   *int64    `json:"end_block,omitempty"`
	Blocks     int64     `json:"blocks"`
	Blobs      int64     `json:"blobs"`

	Values map[string]BuilderShareValue `json:"values"`
}

// BuilderShareChartShare is one series' share of the whole range.
type BuilderShareChartShare struct {
	Key               string  `json:"key"`
	Name              string  `json:"name"`
	Known             bool    `json:"known"`
	Blocks            int64   `json:"blocks"`
	Blobs             int64   `json:"blobs"`
	BlockSharePercent float64 `json:"block_share_percent"`
	BlobSharePercent  float64 `json:"blob_share_percent"`
}

// BuilderShareChartSummary aggregates the builder share chart over the range.
type BuilderShareChartSummary struct {
	TotalBlocks int64                    `json:"total_blocks"`
	TotalBlobs  int64                    `json:"total_blobs"`
	Shares      []BuilderShareChartShare `json:"shares"`
}

// BuilderShareChartResponse contains blocks and blobs per builder per bucket.
// Only blocks with a block_builders row are counted, so a network whose
// builder backfill has not reached the range start reports less than the
// blob-market chart does for the same window.
type BuilderShareChartResponse struct {
	ChainID       int                      `json:"chain_id"`
	NetworkName   string                   `json:"network_name"`
	Range         string                   `json:"range"`
	Granularity   string                   `json:"granularity"`
	BucketSeconds int64                    `json:"bucket_seconds"`
	StartTime     time.Time                `json:"start_time"`
	EndTime       time.Time                `json:"end_time"`
	GeneratedAt   time.Time                `json:"generated_at"`
	Series        []BuilderShareSeries     `json:"series"`
	Points        []BuilderShareChartPoint `json:"points"`
	Summary       BuilderShareChartSummary `json:"summary"`
}

type builderShareChartRow struct {
	Timestamp          time.Time      `db:"timestamp"`
	RangeStart         time.Time      `db:"range_start"`
	RangeEnd           time.Time      `db:"range_end"`
	BlockNumber        sql.NullInt64  `db:"block_number"`
	BucketBlobs        int64          `db:"bucket_blobs"`
	BucketBlocks       int64          `db:"bucket_blocks"`
	SeriesKey          sql.NullString `db:"series_key"`
	SeriesName         sql.NullString `db:"series_name"`
	SeriesBlobs        int64          `db:"series_blobs"`
	SeriesBlocks       int64          `db:"series_blocks"`
	SummaryTotalBlobs  int64          `db:"summary_total_blobs"`
	SummaryTotalBlocks int64          `db:"summary_total_blocks"`
}

// parseBuilderSeriesLimit resolves how many builders keep their own series
// before the long tail is grouped into 'other'.
func parseBuilderSeriesLimit(raw string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return defaultBuilderSeriesLimit, nil
	}
	limit, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || limit <= 0 {
		return 0, fmt.Errorf("invalid limit parameter")
	}
	if limit > maxBuilderSeriesLimit {
		limit = maxBuilderSeriesLimit
	}
	return limit, nil
}

// GetBuilderShareChart godoc
// @Summary Get builder share chart data
// @Description Retrieve blocks and blobs per block builder, bucketed over a range, with the long tail beyond the top builders grouped into a single 'other' series. Only blocks the indexer has a builder row for are counted. range=all is not supported: no rollup carries the builder, so the window is capped at 30d.
// @Tags charts
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Param range query string false "Range: 1h, 24h, 7d, or 30d (default: 24h; all is not supported)"
// @Param granularity query string false "Granularity: auto, block, minute, hour, or day (default: auto)"
// @Param limit query int false "Top builders before grouping the long tail into other (default: 8, max: 25)"
// @Success 200 {object} Response{data=BuilderShareChartResponse} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 500 {object} Response "Internal server error"
// @Failure 503 {object} Response "Database overloaded; retry later"
// @Router /charts/builder-share [get]
func (a *API) GetBuilderShareChart(w http.ResponseWriter, r *http.Request) {
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
	// block_builders carries no rollup, so an unbounded range would scan the
	// whole table; the window is capped like blob-market's and blob-tips'.
	if chart.Range == chartRangeAll {
		a.respondError(w, http.StatusBadRequest, builderRangeError().Error())
		return
	}

	seriesLimit, err := parseBuilderSeriesLimit(r.URL.Query().Get("limit"))
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	logger.Debug("Getting builder share chart",
		zap.String("network", network.Name),
		zap.String("range", chart.Range),
		zap.String("granularity", chart.Granularity),
		zap.Int("series_limit", seriesLimit))

	cacheKey := fmt.Sprintf("chart:builder-share:%d:%s:%s:%d:%d", network.ChainID, chart.Range, chart.Granularity, chart.BucketSeconds, seriesLimit)
	value, err := a.cachedChartResponse(r, cacheKey, func(ctx context.Context) (interface{}, error) {
		queryCtx, cancel := context.WithTimeout(ctx, aggregateQueryTimeout)
		defer cancel()

		query := queryBuilderShareTimeChart
		args := append(chartBlobMarketTimeArgs(network.ChainID, chart), seriesLimit)
		if chart.Granularity == chartGranularityBlock {
			query = queryBuilderShareBlockChart
			args = append(chartBlockArgs(network.ChainID, chart), seriesLimit)
		}

		var rows []builderShareChartRow
		if err := a.db.SelectContext(queryCtx, &rows, query, args...); err != nil {
			return nil, err
		}
		return buildBuilderShareChartResponse(network.ChainID, network.Name, chart, rows), nil
	})
	if err != nil {
		logger.Error("Failed to get builder share chart",
			zap.String("network", network.Name),
			zap.Error(err))
		a.respondAggregateError(w, err, "Failed to get builder share chart")
		return
	}

	setCacheControl(w, aggregateCacheTTL, aggregateEdgeTTL)
	a.respondSuccess(w, value)
}

// builderShareTotal accumulates one series across buckets.
type builderShareTotal struct {
	blocks int64
	blobs  int64
}

func buildBuilderShareChartResponse(networkID int, networkName string, chart chartRequest, rows []builderShareChartRow) BuilderShareChartResponse {
	response := BuilderShareChartResponse{
		ChainID:       networkID,
		NetworkName:   networkName,
		Range:         chart.Range,
		Granularity:   chart.Granularity,
		BucketSeconds: chart.BucketSeconds,
		StartTime:     chart.StartTime,
		EndTime:       chart.EndTime,
		GeneratedAt:   chart.GeneratedAt,
		Series:        []BuilderShareSeries{},
		Points:        make([]BuilderShareChartPoint, 0, len(rows)),
		Summary:       BuilderShareChartSummary{Shares: []BuilderShareChartShare{}},
	}
	if len(rows) > 0 {
		response.StartTime = rows[0].RangeStart
		response.EndTime = rows[0].RangeEnd
		response.Summary.TotalBlocks = rows[0].SummaryTotalBlocks
		response.Summary.TotalBlobs = rows[0].SummaryTotalBlobs
	}

	pointIndexByKey := make(map[attributionPointKey]int)
	seriesByKey := make(map[string]BuilderShareSeries)
	totalsByKey := make(map[string]*builderShareTotal)

	for _, row := range rows {
		pointKey := attributionPointKey{timestamp: row.Timestamp}
		if row.BlockNumber.Valid {
			pointKey.block = row.BlockNumber.Int64
			pointKey.hasBlock = true
		}
		pointIndex, ok := pointIndexByKey[pointKey]
		if !ok {
			point := BuilderShareChartPoint{
				Timestamp: row.Timestamp,
				Blocks:    row.BucketBlocks,
				Blobs:     row.BucketBlobs,
				Values:    make(map[string]BuilderShareValue),
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

		if !row.SeriesKey.Valid || strings.TrimSpace(row.SeriesKey.String) == "" || row.SeriesBlocks <= 0 {
			continue
		}
		key := row.SeriesKey.String
		response.Points[pointIndex].Values[key] = BuilderShareValue{
			Blocks: row.SeriesBlocks,
			Blobs:  row.SeriesBlobs,
		}

		if _, ok := seriesByKey[key]; !ok {
			seriesByKey[key] = BuilderShareSeries{
				Key:   key,
				Name:  nullStringDefault(row.SeriesName, key),
				Known: key != builderOtherSeriesKey && builderKeyKnown(key),
			}
		}
		total, ok := totalsByKey[key]
		if !ok {
			total = &builderShareTotal{}
			totalsByKey[key] = total
		}
		total.blocks += row.SeriesBlocks
		total.blobs += row.SeriesBlobs
	}

	response.Series = orderedBuilderShareSeries(seriesByKey, totalsByKey)
	for i := range response.Points {
		for _, series := range response.Series {
			if _, ok := response.Points[i].Values[series.Key]; !ok {
				response.Points[i].Values[series.Key] = BuilderShareValue{}
			}
		}
	}

	for _, series := range response.Series {
		total := totalsByKey[series.Key]
		response.Summary.Shares = append(response.Summary.Shares, BuilderShareChartShare{
			Key:               series.Key,
			Name:              series.Name,
			Known:             series.Known,
			Blocks:            total.blocks,
			Blobs:             total.blobs,
			BlockSharePercent: chartPercentage(float64(total.blocks), float64(response.Summary.TotalBlocks)),
			BlobSharePercent:  chartPercentage(float64(total.blobs), float64(response.Summary.TotalBlobs)),
		})
	}

	return response
}

// builderOtherSeriesKey is the aggregate series the long tail collapses into.
const builderOtherSeriesKey = "other"

// orderedBuilderShareSeries ranks series by blobs, then blocks, so the
// heaviest blob posters lead; the aggregate 'other' series always sorts last
// so it renders as the tail it represents.
func orderedBuilderShareSeries(seriesByKey map[string]BuilderShareSeries, totalsByKey map[string]*builderShareTotal) []BuilderShareSeries {
	series := make([]BuilderShareSeries, 0, len(seriesByKey))
	for _, item := range seriesByKey {
		series = append(series, item)
	}
	sort.Slice(series, func(i, j int) bool {
		if (series[i].Key == builderOtherSeriesKey) != (series[j].Key == builderOtherSeriesKey) {
			return series[j].Key == builderOtherSeriesKey
		}
		left := totalsByKey[series[i].Key]
		right := totalsByKey[series[j].Key]
		if left.blobs != right.blobs {
			return left.blobs > right.blobs
		}
		if left.blocks != right.blocks {
			return left.blocks > right.blocks
		}
		return series[i].Key < series[j].Key
	})
	return series
}
