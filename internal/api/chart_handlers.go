package api

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/params"
	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

const (
	chartRange1h  = "1h"
	chartRange24h = "24h"
	chartRange7d  = "7d"
	chartRange30d = "30d"
	chartRangeAll = "all"

	chartGranularityAuto   = "auto"
	chartGranularityBlock  = "block"
	chartGranularityMinute = "minute"
	chartGranularityHour   = "hour"
	chartGranularityDay    = "day"

	defaultChartRange      = chartRange24h
	defaultChartPointLimit = 300
	maxChartPointLimit     = 1000
	approxBlockSeconds     = 12

	defaultAttributionSeriesLimit = 5
	maxAttributionSeriesLimit     = 25

	calldataGasPerByte = 16
)

const calldataCostModelDescription = "Approximation: calldata equivalent is modeled as 16 gas per blob byte and priced with the indexed blob base fee as the fee-rate proxy because execution base fee is not stored."

var chartRangeDurations = map[string]time.Duration{
	chartRange1h:  time.Hour,
	chartRange24h: 24 * time.Hour,
	chartRange7d:  7 * 24 * time.Hour,
	chartRange30d: 30 * 24 * time.Hour,
}

// BlobMarketChartResponse contains bucketed blob fee, utilization, and usage data.
type BlobMarketChartResponse struct {
	NetworkID     int                    `json:"network_id"`
	NetworkName   string                 `json:"network_name"`
	Range         string                 `json:"range"`
	Granularity   string                 `json:"granularity"`
	BucketSeconds int64                  `json:"bucket_seconds"`
	StartTime     time.Time              `json:"start_time"`
	EndTime       time.Time              `json:"end_time"`
	GeneratedAt   time.Time              `json:"generated_at"`
	Points        []BlobMarketChartPoint `json:"points"`
	Summary       BlobMarketChartSummary `json:"summary"`
}

// BlobMarketChartPoint is one market chart bucket.
type BlobMarketChartPoint struct {
	Timestamp              time.Time `json:"timestamp"`
	Label                  string    `json:"label,omitempty"`
	StartBlock             *int64    `json:"start_block,omitempty"`
	EndBlock               *int64    `json:"end_block,omitempty"`
	AverageBlobBaseFeeGwei string    `json:"average_blob_base_fee_gwei"`
	MedianBlobBaseFeeGwei  string    `json:"median_blob_base_fee_gwei"`
	P95BlobBaseFeeGwei     string    `json:"p95_blob_base_fee_gwei"`
	BlobCount              int       `json:"blob_count"`
	BlobGasUsed            int64     `json:"blob_gas_used"`
	BlobGasTarget          int64     `json:"blob_gas_target"`
	AverageUtilization     string    `json:"average_utilization"`
	TotalCostWei           string    `json:"total_cost_wei"`
	UniqueSenders          int       `json:"unique_senders"`
}

// BlobMarketChartSummary aggregates a blob market chart range.
type BlobMarketChartSummary struct {
	CurrentBaseFeeGwei     string `json:"current_base_fee_gwei"`
	AverageBlobBaseFeeGwei string `json:"average_blob_base_fee_gwei"`
	MedianBlobBaseFeeGwei  string `json:"median_blob_base_fee_gwei"`
	P95BlobBaseFeeGwei     string `json:"p95_blob_base_fee_gwei"`
	TotalBlobs             int    `json:"total_blobs"`
	TotalBlobGasUsed       int64  `json:"total_blob_gas_used"`
	AverageUtilization     string `json:"average_utilization"`
	TotalCostWei           string `json:"total_cost_wei"`
	UniqueSenders          int    `json:"unique_senders"`
}

// AttributionUsageChartResponse contains bucketed usage by attribution.
type AttributionUsageChartResponse struct {
	NetworkID     int                      `json:"network_id"`
	NetworkName   string                   `json:"network_name"`
	Range         string                   `json:"range"`
	Granularity   string                   `json:"granularity"`
	BucketSeconds int64                    `json:"bucket_seconds"`
	StartTime     time.Time                `json:"start_time"`
	EndTime       time.Time                `json:"end_time"`
	GeneratedAt   time.Time                `json:"generated_at"`
	Series        []AttributionUsageSeries `json:"series"`
	Points        []AttributionUsagePoint  `json:"points"`
	Summary       AttributionUsageSummary  `json:"summary"`
}

// AttributionUsageSeries identifies one attribution series.
type AttributionUsageSeries struct {
	Key      string `json:"key"`
	Name     string `json:"name"`
	Category string `json:"category"`
	Address  string `json:"address,omitempty"`
}

// AttributionUsagePoint is one attribution chart bucket.
type AttributionUsagePoint struct {
	Timestamp time.Time                        `json:"timestamp"`
	Values    map[string]AttributionUsageValue `json:"values"`
}

// AttributionUsageValue contains per-series usage for one bucket.
type AttributionUsageValue struct {
	BlobCount    int    `json:"blob_count"`
	TotalCostWei string `json:"total_cost_wei"`
	BlobGasUsed  int64  `json:"blob_gas_used"`
}

// AttributionUsageSummary aggregates attribution usage over the range.
type AttributionUsageSummary struct {
	TotalBlobs   int                     `json:"total_blobs"`
	TotalCostWei string                  `json:"total_cost_wei"`
	Shares       []AttributionUsageShare `json:"shares"`
}

// AttributionUsageShare contains one attribution's range share.
type AttributionUsageShare struct {
	Key               string  `json:"key"`
	Name              string  `json:"name"`
	Category          string  `json:"category"`
	BlobCount         int     `json:"blob_count"`
	TotalCostWei      string  `json:"total_cost_wei"`
	BlobSharePercent  float64 `json:"blob_share_percent"`
	SpendSharePercent float64 `json:"spend_share_percent"`
}

// CostComparisonChartResponse contains bucketed blob-vs-calldata costs.
type CostComparisonChartResponse struct {
	NetworkID     int                        `json:"network_id"`
	NetworkName   string                     `json:"network_name"`
	Range         string                     `json:"range"`
	Granularity   string                     `json:"granularity"`
	BucketSeconds int64                      `json:"bucket_seconds"`
	StartTime     time.Time                  `json:"start_time"`
	EndTime       time.Time                  `json:"end_time"`
	GeneratedAt   time.Time                  `json:"generated_at"`
	Model         CostComparisonModel        `json:"model"`
	Points        []CostComparisonChartPoint `json:"points"`
	Summary       CostComparisonSummary      `json:"summary"`
}

// CostComparisonModel documents the calldata-equivalent cost model.
type CostComparisonModel struct {
	CalldataGasPerByte int    `json:"calldata_gas_per_byte"`
	BlobSizeBytes      int    `json:"blob_size_bytes"`
	Description        string `json:"description"`
}

// CostComparisonChartPoint is one cost comparison bucket.
type CostComparisonChartPoint struct {
	Timestamp                  time.Time `json:"timestamp"`
	BlobCount                  int       `json:"blob_count"`
	BlobBytes                  int64     `json:"blob_bytes"`
	BlobCostWei                string    `json:"blob_cost_wei"`
	CalldataEquivalentCostWei  string    `json:"calldata_equivalent_cost_wei"`
	SavingsWei                 string    `json:"savings_wei"`
	SavingsPercent             float64   `json:"savings_percent"`
	AverageExecutionBaseFeeWei *string   `json:"average_execution_base_fee_wei,omitempty"`
}

// CostComparisonSummary aggregates cost comparison over the range.
type CostComparisonSummary struct {
	BlobCostWei               string  `json:"blob_cost_wei"`
	CalldataEquivalentCostWei string  `json:"calldata_equivalent_cost_wei"`
	SavingsWei                string  `json:"savings_wei"`
	SavingsPercent            float64 `json:"savings_percent"`
}

type chartRequest struct {
	Range         string
	Granularity   string
	BucketSeconds int64
	TruncateUnit  string
	StartTime     time.Time
	EndTime       time.Time
	GeneratedAt   time.Time
	PointLimit    int
}

type blobMarketChartRow struct {
	Timestamp                    time.Time     `db:"timestamp"`
	RangeStart                   time.Time     `db:"range_start"`
	RangeEnd                     time.Time     `db:"range_end"`
	StartBlock                   sql.NullInt64 `db:"start_block"`
	EndBlock                     sql.NullInt64 `db:"end_block"`
	AverageBlobBaseFeeWei        string        `db:"average_blob_base_fee_wei"`
	MedianBlobBaseFeeWei         string        `db:"median_blob_base_fee_wei"`
	P95BlobBaseFeeWei            string        `db:"p95_blob_base_fee_wei"`
	BlobCount                    int           `db:"blob_count"`
	BlobGasUsed                  int64         `db:"blob_gas_used"`
	BlobGasTarget                int64         `db:"blob_gas_target"`
	AverageUtilization           string        `db:"average_utilization"`
	TotalCostWei                 string        `db:"total_cost_wei"`
	UniqueSenders                int           `db:"unique_senders"`
	SummaryCurrentBaseFeeWei     string        `db:"summary_current_base_fee_wei"`
	SummaryAverageBlobBaseFeeWei string        `db:"summary_average_blob_base_fee_wei"`
	SummaryMedianBlobBaseFeeWei  string        `db:"summary_median_blob_base_fee_wei"`
	SummaryP95BlobBaseFeeWei     string        `db:"summary_p95_blob_base_fee_wei"`
	SummaryTotalBlobs            int           `db:"summary_total_blobs"`
	SummaryTotalBlobGasUsed      int64         `db:"summary_total_blob_gas_used"`
	SummaryAverageUtilization    string        `db:"summary_average_utilization"`
	SummaryTotalCostWei          string        `db:"summary_total_cost_wei"`
	SummaryUniqueSenders         int           `db:"summary_unique_senders"`
}

type attributionUsageChartRow struct {
	Timestamp    time.Time      `db:"timestamp"`
	RangeStart   time.Time      `db:"range_start"`
	RangeEnd     time.Time      `db:"range_end"`
	Key          sql.NullString `db:"series_key"`
	Name         sql.NullString `db:"series_name"`
	Category     sql.NullString `db:"series_category"`
	Address      sql.NullString `db:"series_address"`
	BlobCount    int            `db:"blob_count"`
	TotalCostWei string         `db:"total_cost_wei"`
	BlobGasUsed  int64          `db:"blob_gas_used"`
}

type costComparisonChartRow struct {
	Timestamp                        time.Time      `db:"timestamp"`
	RangeStart                       time.Time      `db:"range_start"`
	RangeEnd                         time.Time      `db:"range_end"`
	BlobCount                        int            `db:"blob_count"`
	BlobBytes                        int64          `db:"blob_bytes"`
	BlobCostWei                      string         `db:"blob_cost_wei"`
	CalldataEquivalentCostWei        string         `db:"calldata_equivalent_cost_wei"`
	SavingsWei                       string         `db:"savings_wei"`
	SavingsPercent                   float64        `db:"savings_percent"`
	AverageExecutionBaseFeeWei       sql.NullString `db:"average_execution_base_fee_wei"`
	SummaryBlobCostWei               string         `db:"summary_blob_cost_wei"`
	SummaryCalldataEquivalentCostWei string         `db:"summary_calldata_equivalent_cost_wei"`
	SummarySavingsWei                string         `db:"summary_savings_wei"`
	SummarySavingsPercent            float64        `db:"summary_savings_percent"`
}

// GetBlobMarketChart godoc
// @Summary Get blob market chart data
// @Description Retrieve bucketed blob base fee, utilization, cost, and usage data
// @Tags charts
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Param range query string false "Range: 1h, 24h, 7d, 30d, or all (default: 24h)"
// @Param granularity query string false "Granularity: auto, block, minute, hour, or day (default: auto)"
// @Param limit query int false "Maximum point count for explicit granularities (default: 300, max: 1000)"
// @Success 200 {object} Response{data=BlobMarketChartResponse} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 500 {object} Response "Internal server error"
// @Router /charts/blob-market [get]
func (a *API) GetBlobMarketChart(w http.ResponseWriter, r *http.Request) {
	network, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	chart, err := parseChartRequest(r, time.Now().UTC(), true)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	logger.Debug("Getting blob market chart",
		zap.String("network", network.Name),
		zap.String("range", chart.Range),
		zap.String("granularity", chart.Granularity))

	query := queryBlobMarketTimeChart
	args := chartTimeArgs(network.ChainID, chart)
	if chart.Granularity == chartGranularityBlock {
		query = queryBlobMarketBlockChart
		args = chartBlockArgs(network.ChainID, chart)
	}

	queryCtx, cancel := context.WithTimeout(r.Context(), aggregateQueryTimeout)
	defer cancel()

	var rows []blobMarketChartRow
	if err := a.db.SelectContext(queryCtx, &rows, query, args...); err != nil {
		logger.Error("Failed to get blob market chart",
			zap.String("network", network.Name),
			zap.Error(err))
		a.respondError(w, http.StatusInternalServerError, "Failed to get blob market chart")
		return
	}

	a.respondSuccess(w, buildBlobMarketChartResponse(network.ChainID, network.Name, chart, rows))
}

// GetAttributionUsageChart godoc
// @Summary Get attribution usage chart data
// @Description Retrieve bucketed blob usage grouped by attribution with long-tail grouping
// @Tags charts
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Param range query string false "Range: 1h, 24h, 7d, 30d, or all (default: 24h)"
// @Param granularity query string false "Granularity: auto, block, minute, hour, or day (default: auto)"
// @Param limit query int false "Top attribution series before grouping long-tail into other (default: 5, max: 25)"
// @Success 200 {object} Response{data=AttributionUsageChartResponse} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 500 {object} Response "Internal server error"
// @Router /charts/attribution-usage [get]
func (a *API) GetAttributionUsageChart(w http.ResponseWriter, r *http.Request) {
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

	seriesLimit, err := parseAttributionSeriesLimit(r.URL.Query().Get("limit"))
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	logger.Debug("Getting attribution usage chart",
		zap.String("network", network.Name),
		zap.String("range", chart.Range),
		zap.String("granularity", chart.Granularity),
		zap.Int("series_limit", seriesLimit))

	query := queryAttributionUsageTimeChart
	args := append(chartTimeArgs(network.ChainID, chart), seriesLimit)
	if chart.Granularity == chartGranularityBlock {
		query = queryAttributionUsageBlockChart
		args = append(chartBlockArgs(network.ChainID, chart), seriesLimit)
	}

	queryCtx, cancel := context.WithTimeout(r.Context(), aggregateQueryTimeout)
	defer cancel()

	var rows []attributionUsageChartRow
	if err := a.db.SelectContext(queryCtx, &rows, query, args...); err != nil {
		logger.Error("Failed to get attribution usage chart",
			zap.String("network", network.Name),
			zap.Error(err))
		a.respondError(w, http.StatusInternalServerError, "Failed to get attribution usage chart")
		return
	}

	a.respondSuccess(w, buildAttributionUsageChartResponse(network.ChainID, network.Name, chart, rows))
}

// GetCostComparisonChart godoc
// @Summary Get blob cost comparison chart data
// @Description Retrieve bucketed blob cost versus calldata-equivalent cost approximation
// @Tags charts
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Param range query string false "Range: 1h, 24h, 7d, 30d, or all (default: 24h)"
// @Param granularity query string false "Granularity: auto, block, minute, hour, or day (default: auto)"
// @Param limit query int false "Maximum point count for explicit granularities (default: 300, max: 1000)"
// @Success 200 {object} Response{data=CostComparisonChartResponse} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 500 {object} Response "Internal server error"
// @Router /charts/cost-comparison [get]
func (a *API) GetCostComparisonChart(w http.ResponseWriter, r *http.Request) {
	network, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	chart, err := parseChartRequest(r, time.Now().UTC(), true)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	logger.Debug("Getting cost comparison chart",
		zap.String("network", network.Name),
		zap.String("range", chart.Range),
		zap.String("granularity", chart.Granularity))

	query := queryCostComparisonTimeChart
	args := append(chartTimeArgs(network.ChainID, chart), calldataGasPerByte)
	if chart.Granularity == chartGranularityBlock {
		query = queryCostComparisonBlockChart
		args = append(chartBlockArgs(network.ChainID, chart), calldataGasPerByte)
	}

	queryCtx, cancel := context.WithTimeout(r.Context(), aggregateQueryTimeout)
	defer cancel()

	var rows []costComparisonChartRow
	if err := a.db.SelectContext(queryCtx, &rows, query, args...); err != nil {
		logger.Error("Failed to get cost comparison chart",
			zap.String("network", network.Name),
			zap.Error(err))
		a.respondError(w, http.StatusInternalServerError, "Failed to get cost comparison chart")
		return
	}

	a.respondSuccess(w, buildCostComparisonChartResponse(network.ChainID, network.Name, chart, rows))
}

func parseChartRequest(r *http.Request, generatedAt time.Time, useLimitForPointCap bool) (chartRequest, error) {
	q := r.URL.Query()
	rangeLabel := strings.ToLower(strings.TrimSpace(q.Get("range")))
	if rangeLabel == "" {
		rangeLabel = defaultChartRange
	}
	duration, isAll, err := chartRangeDuration(rangeLabel)
	if err != nil {
		return chartRequest{}, err
	}

	pointLimit := maxChartPointLimit
	if useLimitForPointCap {
		parsedLimit, err := parseChartPointLimit(q.Get("limit"))
		if err != nil {
			return chartRequest{}, err
		}
		pointLimit = parsedLimit
	}

	rawGranularity := strings.ToLower(strings.TrimSpace(q.Get("granularity")))
	if rawGranularity == "" {
		rawGranularity = chartGranularityAuto
	}

	granularity, bucketSeconds, truncateUnit, err := resolveChartGranularity(rangeLabel, rawGranularity)
	if err != nil {
		return chartRequest{}, err
	}
	if granularity == chartGranularityBlock && isAll {
		return chartRequest{}, fmt.Errorf("block granularity is not supported with range=all; use auto or a time granularity")
	}
	if !isAll && granularity != chartGranularityBlock && time.Duration(bucketSeconds)*time.Second > duration {
		return chartRequest{}, fmt.Errorf("%s granularity is too coarse for range=%s; use auto or a finer granularity", granularity, rangeLabel)
	}

	generatedAt = generatedAt.UTC()
	endTime := generatedAt
	startTime := time.Time{}
	if !isAll {
		if granularity == chartGranularityBlock {
			startTime = endTime.Add(-duration)
			if estimatedBlockPoints(duration) > pointLimit {
				return chartRequest{}, fmt.Errorf("block granularity would return more than %d points; use auto or a coarser granularity", pointLimit)
			}
		} else {
			endTime = alignChartEnd(generatedAt, bucketSeconds)
			startTime = endTime.Add(-duration)
			if estimatedTimePoints(duration, bucketSeconds) > pointLimit {
				return chartRequest{}, fmt.Errorf("%s granularity would return more than %d points for range=%s; use auto or a coarser granularity", granularity, pointLimit, rangeLabel)
			}
		}
	} else if granularity != chartGranularityBlock {
		endTime = alignChartEnd(generatedAt, bucketSeconds)
	}

	return chartRequest{
		Range:         rangeLabel,
		Granularity:   granularity,
		BucketSeconds: bucketSeconds,
		TruncateUnit:  truncateUnit,
		StartTime:     startTime,
		EndTime:       endTime,
		GeneratedAt:   generatedAt,
		PointLimit:    pointLimit,
	}, nil
}

func chartRangeDuration(rangeLabel string) (time.Duration, bool, error) {
	if rangeLabel == chartRangeAll {
		return 0, true, nil
	}
	duration, ok := chartRangeDurations[rangeLabel]
	if !ok {
		return 0, false, fmt.Errorf("invalid range parameter; expected one of 1h, 24h, 7d, 30d, all")
	}
	return duration, false, nil
}

func resolveChartGranularity(rangeLabel, granularity string) (resolved string, bucketSeconds int64, truncateUnit string, err error) {
	if granularity == chartGranularityAuto {
		return autoChartGranularity(rangeLabel)
	}
	switch granularity {
	case chartGranularityBlock:
		return chartGranularityBlock, approxBlockSeconds, "", nil
	case chartGranularityMinute:
		return chartGranularityMinute, int64(time.Minute / time.Second), chartGranularityMinute, nil
	case chartGranularityHour:
		return chartGranularityHour, int64(time.Hour / time.Second), chartGranularityHour, nil
	case chartGranularityDay:
		return chartGranularityDay, int64((24 * time.Hour) / time.Second), chartGranularityDay, nil
	default:
		return "", 0, "", fmt.Errorf("invalid granularity parameter; expected auto, block, minute, hour, or day")
	}
}

func autoChartGranularity(rangeLabel string) (resolved string, bucketSeconds int64, truncateUnit string, err error) {
	switch rangeLabel {
	case chartRange1h:
		return chartGranularityMinute, int64(time.Minute / time.Second), chartGranularityMinute, nil
	case chartRange24h:
		return chartGranularityMinute, int64((5 * time.Minute) / time.Second), chartGranularityMinute, nil
	case chartRange7d:
		return chartGranularityHour, int64(time.Hour / time.Second), chartGranularityHour, nil
	case chartRange30d:
		return chartGranularityHour, int64((6 * time.Hour) / time.Second), chartGranularityHour, nil
	case chartRangeAll:
		return chartGranularityDay, int64((24 * time.Hour) / time.Second), chartGranularityDay, nil
	default:
		return "", 0, "", fmt.Errorf("invalid range parameter; expected one of 1h, 24h, 7d, 30d, all")
	}
}

func parseChartPointLimit(raw string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return defaultChartPointLimit, nil
	}
	limit, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || limit <= 0 {
		return 0, fmt.Errorf("invalid limit parameter")
	}
	if limit > maxChartPointLimit {
		limit = maxChartPointLimit
	}
	return limit, nil
}

func parseAttributionSeriesLimit(raw string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return defaultAttributionSeriesLimit, nil
	}
	limit, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || limit <= 0 {
		return 0, fmt.Errorf("invalid limit parameter")
	}
	if limit > maxAttributionSeriesLimit {
		limit = maxAttributionSeriesLimit
	}
	return limit, nil
}

func alignChartEnd(now time.Time, bucketSeconds int64) time.Time {
	if bucketSeconds <= 0 {
		return now.UTC()
	}
	bucket := time.Duration(bucketSeconds) * time.Second
	return now.UTC().Truncate(bucket).Add(bucket)
}

func estimatedTimePoints(duration time.Duration, bucketSeconds int64) int {
	if duration <= 0 || bucketSeconds <= 0 {
		return 0
	}
	return int(math.Ceil(duration.Seconds() / float64(bucketSeconds)))
}

func estimatedBlockPoints(duration time.Duration) int {
	if duration <= 0 {
		return 0
	}
	return int(math.Ceil(duration.Seconds() / approxBlockSeconds))
}

func chartTimeArgs(networkID int, chart chartRequest) []interface{} {
	return []interface{}{
		networkID,
		chart.Range,
		chart.StartTime,
		chart.EndTime,
		chart.BucketSeconds,
		chart.TruncateUnit,
	}
}

func chartBlockArgs(networkID int, chart chartRequest) []interface{} {
	return []interface{}{
		networkID,
		chart.StartTime,
		chart.EndTime,
	}
}

func buildBlobMarketChartResponse(networkID int, networkName string, chart chartRequest, rows []blobMarketChartRow) BlobMarketChartResponse {
	response := BlobMarketChartResponse{
		NetworkID:     networkID,
		NetworkName:   networkName,
		Range:         chart.Range,
		Granularity:   chart.Granularity,
		BucketSeconds: chart.BucketSeconds,
		StartTime:     chart.StartTime,
		EndTime:       chart.EndTime,
		GeneratedAt:   chart.GeneratedAt,
		Points:        make([]BlobMarketChartPoint, 0, len(rows)),
		Summary:       zeroBlobMarketChartSummary(),
	}
	if chart.Range == chartRangeAll && len(rows) == 0 {
		response.StartTime = chart.GeneratedAt
		response.EndTime = chart.GeneratedAt
	}
	if len(rows) > 0 {
		response.StartTime = rows[0].RangeStart
		response.EndTime = rows[0].RangeEnd
		response.Summary = blobMarketSummaryFromRow(rows[0])
	}
	for _, row := range rows {
		response.Points = append(response.Points, BlobMarketChartPoint{
			Timestamp:              row.Timestamp,
			StartBlock:             nullInt64Ptr(row.StartBlock),
			EndBlock:               nullInt64Ptr(row.EndBlock),
			AverageBlobBaseFeeGwei: formatDecimalWeiAsGwei(row.AverageBlobBaseFeeWei),
			MedianBlobBaseFeeGwei:  formatDecimalWeiAsGwei(row.MedianBlobBaseFeeWei),
			P95BlobBaseFeeGwei:     formatDecimalWeiAsGwei(row.P95BlobBaseFeeWei),
			BlobCount:              row.BlobCount,
			BlobGasUsed:            row.BlobGasUsed,
			BlobGasTarget:          row.BlobGasTarget,
			AverageUtilization:     nonEmptyDecimal(row.AverageUtilization),
			TotalCostWei:           nonEmptyDecimal(row.TotalCostWei),
			UniqueSenders:          row.UniqueSenders,
		})
	}
	return response
}

func blobMarketSummaryFromRow(row blobMarketChartRow) BlobMarketChartSummary {
	return BlobMarketChartSummary{
		CurrentBaseFeeGwei:     formatDecimalWeiAsGwei(row.SummaryCurrentBaseFeeWei),
		AverageBlobBaseFeeGwei: formatDecimalWeiAsGwei(row.SummaryAverageBlobBaseFeeWei),
		MedianBlobBaseFeeGwei:  formatDecimalWeiAsGwei(row.SummaryMedianBlobBaseFeeWei),
		P95BlobBaseFeeGwei:     formatDecimalWeiAsGwei(row.SummaryP95BlobBaseFeeWei),
		TotalBlobs:             row.SummaryTotalBlobs,
		TotalBlobGasUsed:       row.SummaryTotalBlobGasUsed,
		AverageUtilization:     nonEmptyDecimal(row.SummaryAverageUtilization),
		TotalCostWei:           nonEmptyDecimal(row.SummaryTotalCostWei),
		UniqueSenders:          row.SummaryUniqueSenders,
	}
}

func zeroBlobMarketChartSummary() BlobMarketChartSummary {
	return BlobMarketChartSummary{
		CurrentBaseFeeGwei:     "0",
		AverageBlobBaseFeeGwei: "0",
		MedianBlobBaseFeeGwei:  "0",
		P95BlobBaseFeeGwei:     "0",
		AverageUtilization:     "0",
		TotalCostWei:           "0",
	}
}

func buildAttributionUsageChartResponse(networkID int, networkName string, chart chartRequest, rows []attributionUsageChartRow) AttributionUsageChartResponse {
	response := AttributionUsageChartResponse{
		NetworkID:     networkID,
		NetworkName:   networkName,
		Range:         chart.Range,
		Granularity:   chart.Granularity,
		BucketSeconds: chart.BucketSeconds,
		StartTime:     chart.StartTime,
		EndTime:       chart.EndTime,
		GeneratedAt:   chart.GeneratedAt,
		Points:        make([]AttributionUsagePoint, 0, len(rows)),
	}
	if chart.Range == chartRangeAll && len(rows) == 0 {
		response.StartTime = chart.GeneratedAt
		response.EndTime = chart.GeneratedAt
	}
	if len(rows) > 0 {
		response.StartTime = rows[0].RangeStart
		response.EndTime = rows[0].RangeEnd
	}

	pointByTimestamp := make(map[time.Time]*AttributionUsagePoint)
	seriesByKey := make(map[string]AttributionUsageSeries)
	totalsByKey := make(map[string]AttributionUsageValue)

	for _, row := range rows {
		point, ok := pointByTimestamp[row.Timestamp]
		if !ok {
			response.Points = append(response.Points, AttributionUsagePoint{
				Timestamp: row.Timestamp,
				Values:    make(map[string]AttributionUsageValue),
			})
			point = &response.Points[len(response.Points)-1]
			pointByTimestamp[row.Timestamp] = point
		}

		if !row.Key.Valid || strings.TrimSpace(row.Key.String) == "" {
			continue
		}
		key := row.Key.String
		value := AttributionUsageValue{
			BlobCount:    row.BlobCount,
			TotalCostWei: nonEmptyDecimal(row.TotalCostWei),
			BlobGasUsed:  row.BlobGasUsed,
		}
		point.Values[key] = value

		if _, ok := seriesByKey[key]; !ok {
			seriesByKey[key] = AttributionUsageSeries{
				Key:      key,
				Name:     nullStringDefault(row.Name, key),
				Category: nullStringDefault(row.Category, "unknown"),
				Address:  nullStringDefault(row.Address, ""),
			}
		}
		total := totalsByKey[key]
		total.BlobCount += value.BlobCount
		total.TotalCostWei = addDecimalStrings(total.TotalCostWei, value.TotalCostWei)
		total.BlobGasUsed += value.BlobGasUsed
		totalsByKey[key] = total
	}

	response.Series = orderedAttributionSeries(seriesByKey, totalsByKey)
	response.Summary = buildAttributionUsageSummary(response.Series, totalsByKey)
	zeroValue := AttributionUsageValue{TotalCostWei: "0"}
	for i := range response.Points {
		for _, series := range response.Series {
			if _, ok := response.Points[i].Values[series.Key]; !ok {
				response.Points[i].Values[series.Key] = zeroValue
			}
		}
	}

	return response
}

func orderedAttributionSeries(seriesByKey map[string]AttributionUsageSeries, totalsByKey map[string]AttributionUsageValue) []AttributionUsageSeries {
	series := make([]AttributionUsageSeries, 0, len(seriesByKey))
	for _, item := range seriesByKey {
		series = append(series, item)
	}
	sort.Slice(series, func(i, j int) bool {
		left := totalsByKey[series[i].Key]
		right := totalsByKey[series[j].Key]
		if left.BlobCount != right.BlobCount {
			return left.BlobCount > right.BlobCount
		}
		if cmp := compareDecimalStrings(left.TotalCostWei, right.TotalCostWei); cmp != 0 {
			return cmp > 0
		}
		return series[i].Key < series[j].Key
	})
	return series
}

func buildAttributionUsageSummary(series []AttributionUsageSeries, totalsByKey map[string]AttributionUsageValue) AttributionUsageSummary {
	summary := AttributionUsageSummary{
		TotalCostWei: "0",
		Shares:       make([]AttributionUsageShare, 0, len(series)),
	}
	for _, total := range totalsByKey {
		summary.TotalBlobs += total.BlobCount
		summary.TotalCostWei = addDecimalStrings(summary.TotalCostWei, total.TotalCostWei)
	}
	for _, item := range series {
		total := totalsByKey[item.Key]
		share := AttributionUsageShare{
			Key:               item.Key,
			Name:              item.Name,
			Category:          item.Category,
			BlobCount:         total.BlobCount,
			TotalCostWei:      nonEmptyDecimal(total.TotalCostWei),
			BlobSharePercent:  chartPercentage(float64(total.BlobCount), float64(summary.TotalBlobs)),
			SpendSharePercent: decimalSharePercent(total.TotalCostWei, summary.TotalCostWei),
		}
		summary.Shares = append(summary.Shares, share)
	}
	return summary
}

func buildCostComparisonChartResponse(networkID int, networkName string, chart chartRequest, rows []costComparisonChartRow) CostComparisonChartResponse {
	response := CostComparisonChartResponse{
		NetworkID:     networkID,
		NetworkName:   networkName,
		Range:         chart.Range,
		Granularity:   chart.Granularity,
		BucketSeconds: chart.BucketSeconds,
		StartTime:     chart.StartTime,
		EndTime:       chart.EndTime,
		GeneratedAt:   chart.GeneratedAt,
		Model: CostComparisonModel{
			CalldataGasPerByte: calldataGasPerByte,
			BlobSizeBytes:      int(params.BlobTxBlobGasPerBlob),
			Description:        calldataCostModelDescription,
		},
		Points:  make([]CostComparisonChartPoint, 0, len(rows)),
		Summary: zeroCostComparisonSummary(),
	}
	if chart.Range == chartRangeAll && len(rows) == 0 {
		response.StartTime = chart.GeneratedAt
		response.EndTime = chart.GeneratedAt
	}
	if len(rows) > 0 {
		response.StartTime = rows[0].RangeStart
		response.EndTime = rows[0].RangeEnd
		response.Summary = CostComparisonSummary{
			BlobCostWei:               nonEmptyDecimal(rows[0].SummaryBlobCostWei),
			CalldataEquivalentCostWei: nonEmptyDecimal(rows[0].SummaryCalldataEquivalentCostWei),
			SavingsWei:                nonEmptyDecimal(rows[0].SummarySavingsWei),
			SavingsPercent:            rows[0].SummarySavingsPercent,
		}
	}
	for _, row := range rows {
		response.Points = append(response.Points, CostComparisonChartPoint{
			Timestamp:                  row.Timestamp,
			BlobCount:                  row.BlobCount,
			BlobBytes:                  row.BlobBytes,
			BlobCostWei:                nonEmptyDecimal(row.BlobCostWei),
			CalldataEquivalentCostWei:  nonEmptyDecimal(row.CalldataEquivalentCostWei),
			SavingsWei:                 nonEmptyDecimal(row.SavingsWei),
			SavingsPercent:             row.SavingsPercent,
			AverageExecutionBaseFeeWei: nullStringPtr(row.AverageExecutionBaseFeeWei),
		})
	}
	return response
}

func zeroCostComparisonSummary() CostComparisonSummary {
	return CostComparisonSummary{
		BlobCostWei:               "0",
		CalldataEquivalentCostWei: "0",
		SavingsWei:                "0",
	}
}

func nullInt64Ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	value := v.Int64
	return &value
}

func nullStringPtr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	value := v.String
	return &value
}

func nullStringDefault(v sql.NullString, fallback string) string {
	if !v.Valid || strings.TrimSpace(v.String) == "" {
		return fallback
	}
	return v.String
}

func nonEmptyDecimal(value string) string {
	if strings.TrimSpace(value) == "" {
		return "0"
	}
	return value
}

func formatDecimalWeiAsGwei(wei string) string {
	value, ok := decimalStringToRat(wei)
	if !ok {
		return ""
	}
	value.Quo(value, big.NewRat(1_000_000_000, 1))
	return formatRatDecimal(value, 18)
}

func addDecimalStrings(left, right string) string {
	l, ok := decimalStringToRat(left)
	if !ok {
		l = new(big.Rat)
	}
	r, ok := decimalStringToRat(right)
	if !ok {
		r = new(big.Rat)
	}
	l.Add(l, r)
	return formatRatDecimal(l, 18)
}

func compareDecimalStrings(left, right string) int {
	l, ok := decimalStringToRat(left)
	if !ok {
		l = new(big.Rat)
	}
	r, ok := decimalStringToRat(right)
	if !ok {
		r = new(big.Rat)
	}
	return l.Cmp(r)
}

func decimalSharePercent(part, total string) float64 {
	p, ok := decimalStringToRat(part)
	if !ok {
		return 0
	}
	t, ok := decimalStringToRat(total)
	if !ok || t.Sign() == 0 {
		return 0
	}
	p.Quo(p, t)
	p.Mul(p, big.NewRat(100, 1))
	value, _ := p.Float64()
	return roundPercent(value)
}

func chartPercentage(part, total float64) float64 {
	if total == 0 {
		return 0
	}
	return roundPercent((part / total) * 100)
}

func roundPercent(value float64) float64 {
	return math.Round(value*1_000_000) / 1_000_000
}

func decimalStringToRat(value string) (*big.Rat, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return new(big.Rat), true
	}
	rat, ok := new(big.Rat).SetString(value)
	if !ok {
		return nil, false
	}
	return rat, true
}

func formatRatDecimal(value *big.Rat, precision int) string {
	if value == nil {
		return "0"
	}
	if value.Sign() == 0 {
		return "0"
	}
	text := value.FloatString(precision)
	text = strings.TrimRight(text, "0")
	text = strings.TrimRight(text, ".")
	if text == "-0" || text == "" {
		return "0"
	}
	return text
}

const queryBlobMarketTimeChart = `
	WITH bounds AS (
		SELECT
			CASE
				WHEN $2::text = 'all' THEN date_trunc($6::text, COALESCE(MIN(block_timestamp), $4::timestamp))
				ELSE $3::timestamp
			END AS range_start,
			$4::timestamp AS range_end
		FROM block_metrics
		WHERE network_id = $1
	),
	buckets AS (
		SELECT
			g.bucket_start,
			b.range_start,
			b.range_end
		FROM bounds b
		CROSS JOIN LATERAL generate_series(
			b.range_start,
			b.range_end - ($5::bigint * INTERVAL '1 second'),
			$5::bigint * INTERVAL '1 second'
		) AS g(bucket_start)
		WHERE b.range_end > b.range_start
	),
	bucket_metrics AS (
		SELECT
			b.bucket_start,
			MIN(bm.block_number) AS start_block,
			MAX(bm.block_number) AS end_block,
			COALESCE(AVG(bm.blob_base_fee::numeric), 0)::text AS average_blob_base_fee_wei,
			COALESCE(percentile_disc(0.5) WITHIN GROUP (ORDER BY bm.blob_base_fee::numeric), 0)::text AS median_blob_base_fee_wei,
			COALESCE(percentile_disc(0.95) WITHIN GROUP (ORDER BY bm.blob_base_fee::numeric), 0)::text AS p95_blob_base_fee_wei,
			COALESCE(SUM(bm.blob_count), 0)::int AS blob_count,
			COALESCE(SUM(bm.blob_gas_used), 0)::bigint AS blob_gas_used,
			COALESCE(SUM(bm.blob_gas_target), 0)::bigint AS blob_gas_target,
			COALESCE(AVG(bm.utilization_ratio::numeric), 0)::text AS average_utilization
		FROM buckets b
		LEFT JOIN block_metrics bm
			ON bm.network_id = $1
			AND bm.block_timestamp >= b.bucket_start
			AND bm.block_timestamp < b.bucket_start + ($5::bigint * INTERVAL '1 second')
		GROUP BY b.bucket_start
	),
	bucket_blobs AS (
		SELECT
			b.bucket_start,
			COALESCE(SUM(bl.total_cost_eth::numeric), 0)::text AS total_cost_wei,
			COUNT(DISTINCT bl.from_address)::int AS unique_senders
		FROM buckets b
		LEFT JOIN blobs bl
			ON bl.network_id = $1
			AND bl.confirmed = true
			AND bl.timestamp >= b.bucket_start
			AND bl.timestamp < b.bucket_start + ($5::bigint * INTERVAL '1 second')
		GROUP BY b.bucket_start
	),
	summary_metrics AS (
		SELECT
			COALESCE(AVG(bm.blob_base_fee::numeric), 0)::text AS average_blob_base_fee_wei,
			COALESCE(percentile_disc(0.5) WITHIN GROUP (ORDER BY bm.blob_base_fee::numeric), 0)::text AS median_blob_base_fee_wei,
			COALESCE(percentile_disc(0.95) WITHIN GROUP (ORDER BY bm.blob_base_fee::numeric), 0)::text AS p95_blob_base_fee_wei,
			COALESCE(SUM(bm.blob_count), 0)::int AS total_blobs,
			COALESCE(SUM(bm.blob_gas_used), 0)::bigint AS total_blob_gas_used,
			COALESCE(AVG(bm.utilization_ratio::numeric), 0)::text AS average_utilization
		FROM bounds b
		LEFT JOIN block_metrics bm
			ON bm.network_id = $1
			AND bm.block_timestamp >= b.range_start
			AND bm.block_timestamp < b.range_end
	),
	summary_blobs AS (
		SELECT
			COALESCE(SUM(bl.total_cost_eth::numeric), 0)::text AS total_cost_wei,
			COUNT(DISTINCT bl.from_address)::int AS unique_senders
		FROM bounds b
		LEFT JOIN blobs bl
			ON bl.network_id = $1
			AND bl.confirmed = true
			AND bl.timestamp >= b.range_start
			AND bl.timestamp < b.range_end
	),
	latest_metric AS (
		SELECT COALESCE((
			SELECT bm.blob_base_fee::text
			FROM block_metrics bm
			WHERE bm.network_id = $1
			ORDER BY bm.block_number DESC
			LIMIT 1
		), '0') AS current_base_fee_wei
	)
	SELECT
		b.bucket_start AS timestamp,
		b.range_start,
		b.range_end,
		bm.start_block,
		bm.end_block,
		bm.average_blob_base_fee_wei,
		bm.median_blob_base_fee_wei,
		bm.p95_blob_base_fee_wei,
		bm.blob_count,
		bm.blob_gas_used,
		bm.blob_gas_target,
		bm.average_utilization,
		bb.total_cost_wei,
		bb.unique_senders,
		lm.current_base_fee_wei AS summary_current_base_fee_wei,
		sm.average_blob_base_fee_wei AS summary_average_blob_base_fee_wei,
		sm.median_blob_base_fee_wei AS summary_median_blob_base_fee_wei,
		sm.p95_blob_base_fee_wei AS summary_p95_blob_base_fee_wei,
		sm.total_blobs AS summary_total_blobs,
		sm.total_blob_gas_used AS summary_total_blob_gas_used,
		sm.average_utilization AS summary_average_utilization,
		sb.total_cost_wei AS summary_total_cost_wei,
		sb.unique_senders AS summary_unique_senders
	FROM buckets b
	JOIN bucket_metrics bm ON bm.bucket_start = b.bucket_start
	JOIN bucket_blobs bb ON bb.bucket_start = b.bucket_start
	CROSS JOIN summary_metrics sm
	CROSS JOIN summary_blobs sb
	CROSS JOIN latest_metric lm
	ORDER BY b.bucket_start ASC
`

const queryBlobMarketBlockChart = `
	WITH bounds AS (
		SELECT $2::timestamp AS range_start, $3::timestamp AS range_end
	),
	selected_blocks AS (
		SELECT bm.*
		FROM block_metrics bm
		CROSS JOIN bounds b
		WHERE bm.network_id = $1
			AND bm.block_timestamp >= b.range_start
			AND bm.block_timestamp < b.range_end
	),
	block_blobs AS (
		SELECT
			sb.block_number,
			COALESCE(SUM(bl.total_cost_eth::numeric), 0)::text AS total_cost_wei,
			COUNT(DISTINCT bl.from_address)::int AS unique_senders
		FROM selected_blocks sb
		LEFT JOIN blobs bl
			ON bl.network_id = $1
			AND bl.confirmed = true
			AND bl.block_number = sb.block_number
		GROUP BY sb.block_number
	),
	summary_metrics AS (
		SELECT
			COALESCE(AVG(blob_base_fee::numeric), 0)::text AS average_blob_base_fee_wei,
			COALESCE(percentile_disc(0.5) WITHIN GROUP (ORDER BY blob_base_fee::numeric), 0)::text AS median_blob_base_fee_wei,
			COALESCE(percentile_disc(0.95) WITHIN GROUP (ORDER BY blob_base_fee::numeric), 0)::text AS p95_blob_base_fee_wei,
			COALESCE(SUM(blob_count), 0)::int AS total_blobs,
			COALESCE(SUM(blob_gas_used), 0)::bigint AS total_blob_gas_used,
			COALESCE(AVG(utilization_ratio::numeric), 0)::text AS average_utilization
		FROM selected_blocks
	),
	summary_blobs AS (
		SELECT
			COALESCE(SUM(bl.total_cost_eth::numeric), 0)::text AS total_cost_wei,
			COUNT(DISTINCT bl.from_address)::int AS unique_senders
		FROM bounds b
		LEFT JOIN blobs bl
			ON bl.network_id = $1
			AND bl.confirmed = true
			AND bl.timestamp >= b.range_start
			AND bl.timestamp < b.range_end
	),
	latest_metric AS (
		SELECT COALESCE((
			SELECT bm.blob_base_fee::text
			FROM block_metrics bm
			WHERE bm.network_id = $1
			ORDER BY bm.block_number DESC
			LIMIT 1
		), '0') AS current_base_fee_wei
	)
	SELECT
		sb.block_timestamp AS timestamp,
		b.range_start,
		b.range_end,
		sb.block_number AS start_block,
		sb.block_number AS end_block,
		sb.blob_base_fee::text AS average_blob_base_fee_wei,
		sb.blob_base_fee::text AS median_blob_base_fee_wei,
		sb.blob_base_fee::text AS p95_blob_base_fee_wei,
		sb.blob_count::int AS blob_count,
		sb.blob_gas_used::bigint AS blob_gas_used,
		sb.blob_gas_target::bigint AS blob_gas_target,
		sb.utilization_ratio::text AS average_utilization,
		bb.total_cost_wei,
		bb.unique_senders,
		lm.current_base_fee_wei AS summary_current_base_fee_wei,
		sm.average_blob_base_fee_wei AS summary_average_blob_base_fee_wei,
		sm.median_blob_base_fee_wei AS summary_median_blob_base_fee_wei,
		sm.p95_blob_base_fee_wei AS summary_p95_blob_base_fee_wei,
		sm.total_blobs AS summary_total_blobs,
		sm.total_blob_gas_used AS summary_total_blob_gas_used,
		sm.average_utilization AS summary_average_utilization,
		sbl.total_cost_wei AS summary_total_cost_wei,
		sbl.unique_senders AS summary_unique_senders
	FROM selected_blocks sb
	CROSS JOIN bounds b
	JOIN block_blobs bb ON bb.block_number = sb.block_number
	CROSS JOIN summary_metrics sm
	CROSS JOIN summary_blobs sbl
	CROSS JOIN latest_metric lm
	ORDER BY sb.block_number ASC
`

const queryCostComparisonTimeChart = `
	WITH bounds AS (
		SELECT
			CASE
				WHEN $2::text = 'all' THEN date_trunc($6::text, COALESCE(MIN(timestamp), $4::timestamp))
				ELSE $3::timestamp
			END AS range_start,
			$4::timestamp AS range_end
		FROM blobs
		WHERE network_id = $1 AND confirmed = true
	),
	buckets AS (
		SELECT
			g.bucket_start,
			b.range_start,
			b.range_end
		FROM bounds b
		CROSS JOIN LATERAL generate_series(
			b.range_start,
			b.range_end - ($5::bigint * INTERVAL '1 second'),
			$5::bigint * INTERVAL '1 second'
		) AS g(bucket_start)
		WHERE b.range_end > b.range_start
	),
	bucket_costs AS (
		SELECT
			b.bucket_start,
			COUNT(bl.id)::int AS blob_count,
			COALESCE(SUM(bl.blob_size_bytes), 0)::bigint AS blob_bytes,
			COALESCE(SUM(bl.total_cost_eth::numeric), 0)::text AS blob_cost_wei,
			COALESCE(SUM(bl.blob_size_bytes::numeric * $7::numeric * bl.base_fee_per_blob_gas::numeric), 0)::text AS calldata_equivalent_cost_wei
		FROM buckets b
		LEFT JOIN blobs bl
			ON bl.network_id = $1
			AND bl.confirmed = true
			AND bl.timestamp >= b.bucket_start
			AND bl.timestamp < b.bucket_start + ($5::bigint * INTERVAL '1 second')
		GROUP BY b.bucket_start
	),
	summary_costs AS (
		SELECT
			COALESCE(SUM(bl.total_cost_eth::numeric), 0) AS blob_cost_wei,
			COALESCE(SUM(bl.blob_size_bytes::numeric * $7::numeric * bl.base_fee_per_blob_gas::numeric), 0) AS calldata_equivalent_cost_wei
		FROM bounds b
		LEFT JOIN blobs bl
			ON bl.network_id = $1
			AND bl.confirmed = true
			AND bl.timestamp >= b.range_start
			AND bl.timestamp < b.range_end
	)
	SELECT
		b.bucket_start AS timestamp,
		b.range_start,
		b.range_end,
		bc.blob_count,
		bc.blob_bytes,
		bc.blob_cost_wei,
		bc.calldata_equivalent_cost_wei,
		(bc.calldata_equivalent_cost_wei::numeric - bc.blob_cost_wei::numeric)::text AS savings_wei,
		CASE
			WHEN bc.calldata_equivalent_cost_wei::numeric > 0
				THEN ROUND(((bc.calldata_equivalent_cost_wei::numeric - bc.blob_cost_wei::numeric) / bc.calldata_equivalent_cost_wei::numeric) * 100, 6)::float8
			ELSE 0
		END AS savings_percent,
		NULL::text AS average_execution_base_fee_wei,
		sc.blob_cost_wei::text AS summary_blob_cost_wei,
		sc.calldata_equivalent_cost_wei::text AS summary_calldata_equivalent_cost_wei,
		(sc.calldata_equivalent_cost_wei - sc.blob_cost_wei)::text AS summary_savings_wei,
		CASE
			WHEN sc.calldata_equivalent_cost_wei > 0
				THEN ROUND(((sc.calldata_equivalent_cost_wei - sc.blob_cost_wei) / sc.calldata_equivalent_cost_wei) * 100, 6)::float8
			ELSE 0
		END AS summary_savings_percent
	FROM buckets b
	JOIN bucket_costs bc ON bc.bucket_start = b.bucket_start
	CROSS JOIN summary_costs sc
	ORDER BY b.bucket_start ASC
`

const queryCostComparisonBlockChart = `
	WITH bounds AS (
		SELECT $2::timestamp AS range_start, $3::timestamp AS range_end
	),
	selected_blocks AS (
		SELECT bm.block_number, bm.block_timestamp
		FROM block_metrics bm
		CROSS JOIN bounds b
		WHERE bm.network_id = $1
			AND bm.block_timestamp >= b.range_start
			AND bm.block_timestamp < b.range_end
	),
	block_costs AS (
		SELECT
			sb.block_number,
			COUNT(bl.id)::int AS blob_count,
			COALESCE(SUM(bl.blob_size_bytes), 0)::bigint AS blob_bytes,
			COALESCE(SUM(bl.total_cost_eth::numeric), 0)::text AS blob_cost_wei,
			COALESCE(SUM(bl.blob_size_bytes::numeric * $4::numeric * bl.base_fee_per_blob_gas::numeric), 0)::text AS calldata_equivalent_cost_wei
		FROM selected_blocks sb
		LEFT JOIN blobs bl
			ON bl.network_id = $1
			AND bl.confirmed = true
			AND bl.block_number = sb.block_number
		GROUP BY sb.block_number
	),
	summary_costs AS (
		SELECT
			COALESCE(SUM(bl.total_cost_eth::numeric), 0) AS blob_cost_wei,
			COALESCE(SUM(bl.blob_size_bytes::numeric * $4::numeric * bl.base_fee_per_blob_gas::numeric), 0) AS calldata_equivalent_cost_wei
		FROM bounds b
		LEFT JOIN blobs bl
			ON bl.network_id = $1
			AND bl.confirmed = true
			AND bl.timestamp >= b.range_start
			AND bl.timestamp < b.range_end
	)
	SELECT
		sb.block_timestamp AS timestamp,
		b.range_start,
		b.range_end,
		bc.blob_count,
		bc.blob_bytes,
		bc.blob_cost_wei,
		bc.calldata_equivalent_cost_wei,
		(bc.calldata_equivalent_cost_wei::numeric - bc.blob_cost_wei::numeric)::text AS savings_wei,
		CASE
			WHEN bc.calldata_equivalent_cost_wei::numeric > 0
				THEN ROUND(((bc.calldata_equivalent_cost_wei::numeric - bc.blob_cost_wei::numeric) / bc.calldata_equivalent_cost_wei::numeric) * 100, 6)::float8
			ELSE 0
		END AS savings_percent,
		NULL::text AS average_execution_base_fee_wei,
		sc.blob_cost_wei::text AS summary_blob_cost_wei,
		sc.calldata_equivalent_cost_wei::text AS summary_calldata_equivalent_cost_wei,
		(sc.calldata_equivalent_cost_wei - sc.blob_cost_wei)::text AS summary_savings_wei,
		CASE
			WHEN sc.calldata_equivalent_cost_wei > 0
				THEN ROUND(((sc.calldata_equivalent_cost_wei - sc.blob_cost_wei) / sc.calldata_equivalent_cost_wei) * 100, 6)::float8
			ELSE 0
		END AS summary_savings_percent
	FROM selected_blocks sb
	CROSS JOIN bounds b
	JOIN block_costs bc ON bc.block_number = sb.block_number
	CROSS JOIN summary_costs sc
	ORDER BY sb.block_number ASC
`

func attributionEntityBaseSQL(limitPlaceholder string) string {
	return `
	entity_base AS (
		SELECT
			src.bucket_start,
			src.timestamp,
			src.from_address,
			src.total_cost_wei,
			src.blob_gas_used,
			CASE
				WHEN src.raw_name = '' THEN 'unknown'
				ELSE COALESCE(NULLIF(TRIM(BOTH '_' FROM regexp_replace(lower(src.raw_name), '[^a-z0-9]+', '_', 'g')), ''), 'unknown')
			END AS entity_key,
			CASE WHEN src.raw_name = '' THEN 'Unknown' ELSE src.raw_name END AS entity_name,
			CASE
				WHEN src.raw_name = '' THEN 'unknown'
				ELSE COALESCE(NULLIF(src.raw_category, ''), 'unknown')
			END AS entity_category
		FROM attribution_source src
	),
	entity_totals AS (
		SELECT
			entity_key,
			CASE WHEN entity_key = 'unknown' THEN 'Unknown' ELSE MIN(entity_name) END AS entity_name,
			CASE WHEN entity_key = 'unknown' THEN 'unknown' ELSE COALESCE(NULLIF(MIN(NULLIF(entity_category, 'unknown')), ''), 'unknown') END AS entity_category,
			CASE WHEN COUNT(DISTINCT from_address) = 1 THEN MIN(from_address) ELSE NULL END AS entity_address,
			COUNT(*)::int AS blob_count,
			COALESCE(SUM(total_cost_wei), 0) AS total_cost_wei
		FROM entity_base
		GROUP BY entity_key
	),
	top_entities AS (
		SELECT *
		FROM entity_totals
		WHERE entity_key <> 'unknown'
		ORDER BY blob_count DESC, total_cost_wei DESC, entity_name ASC
		LIMIT ` + limitPlaceholder + `
	),
	bucketed_usage AS (
		SELECT
			eb.bucket_start,
			CASE
				WHEN eb.entity_key = 'unknown' THEN 'unknown'
				WHEN te.entity_key IS NOT NULL THEN eb.entity_key
				ELSE 'other'
			END AS series_key,
			CASE
				WHEN eb.entity_key = 'unknown' THEN 'Unknown'
				WHEN te.entity_key IS NOT NULL THEN te.entity_name
				ELSE 'Other'
			END AS series_name,
			CASE
				WHEN eb.entity_key = 'unknown' THEN 'unknown'
				WHEN te.entity_key IS NOT NULL THEN te.entity_category
				ELSE 'other'
			END AS series_category,
			CASE
				WHEN eb.entity_key = 'unknown' THEN NULL
				WHEN te.entity_key IS NOT NULL THEN te.entity_address
				ELSE NULL
			END AS series_address,
			COUNT(*)::int AS blob_count,
			COALESCE(SUM(eb.total_cost_wei), 0)::text AS total_cost_wei,
			COALESCE(SUM(eb.blob_gas_used), 0)::bigint AS blob_gas_used
		FROM entity_base eb
		LEFT JOIN top_entities te ON te.entity_key = eb.entity_key
		GROUP BY
			eb.bucket_start,
			CASE
				WHEN eb.entity_key = 'unknown' THEN 'unknown'
				WHEN te.entity_key IS NOT NULL THEN eb.entity_key
				ELSE 'other'
			END,
			CASE
				WHEN eb.entity_key = 'unknown' THEN 'Unknown'
				WHEN te.entity_key IS NOT NULL THEN te.entity_name
				ELSE 'Other'
			END,
			CASE
				WHEN eb.entity_key = 'unknown' THEN 'unknown'
				WHEN te.entity_key IS NOT NULL THEN te.entity_category
				ELSE 'other'
			END,
			CASE
				WHEN eb.entity_key = 'unknown' THEN NULL
				WHEN te.entity_key IS NOT NULL THEN te.entity_address
				ELSE NULL
			END
	)
`
}

var queryAttributionUsageTimeChart = `
	WITH bounds AS (
		SELECT
			CASE
				WHEN $2::text = 'all' THEN date_trunc($6::text, COALESCE(MIN(timestamp), $4::timestamp))
				ELSE $3::timestamp
			END AS range_start,
			$4::timestamp AS range_end
		FROM blobs
		WHERE network_id = $1 AND confirmed = true
	),
	buckets AS (
		SELECT
			g.bucket_start,
			b.range_start,
			b.range_end
		FROM bounds b
		CROSS JOIN LATERAL generate_series(
			b.range_start,
			b.range_end - ($5::bigint * INTERVAL '1 second'),
			$5::bigint * INTERVAL '1 second'
		) AS g(bucket_start)
		WHERE b.range_end > b.range_start
	),
	attribution_source AS (
		SELECT
			bu.bucket_start,
			bl.timestamp,
			bl.from_address,
			bl.total_cost_eth::numeric AS total_cost_wei,
			COALESCE(bl.blob_gas_used, 0)::bigint AS blob_gas_used,
			COALESCE(NULLIF(BTRIM(bl.user_attribution), ''), NULLIF(BTRIM(known.name), ''), '') AS raw_name,
			COALESCE(NULLIF(BTRIM(known.category), ''), '') AS raw_category
		FROM buckets bu
		JOIN blobs bl
			ON bl.network_id = $1
			AND bl.confirmed = true
			AND bl.timestamp >= bu.bucket_start
			AND bl.timestamp < bu.bucket_start + ($5::bigint * INTERVAL '1 second')
		LEFT JOIN blob_users known
			ON known.network_id = bl.network_id
			AND LOWER(known.address) = LOWER(bl.from_address)
	),
` + attributionEntityBaseSQL("$7") + `
	SELECT
		b.bucket_start AS timestamp,
		b.range_start,
		b.range_end,
		u.series_key,
		u.series_name,
		u.series_category,
		u.series_address,
		COALESCE(u.blob_count, 0)::int AS blob_count,
		COALESCE(u.total_cost_wei, '0') AS total_cost_wei,
		COALESCE(u.blob_gas_used, 0)::bigint AS blob_gas_used
	FROM buckets b
	LEFT JOIN bucketed_usage u ON u.bucket_start = b.bucket_start
	ORDER BY b.bucket_start ASC, u.blob_count DESC NULLS LAST, u.series_key ASC
`

var queryAttributionUsageBlockChart = `
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
		WHERE bm.network_id = $1
			AND bm.block_timestamp >= b.range_start
			AND bm.block_timestamp < b.range_end
	),
	attribution_source AS (
		SELECT
			bu.bucket_start,
			bl.timestamp,
			bl.from_address,
			bl.total_cost_eth::numeric AS total_cost_wei,
			COALESCE(bl.blob_gas_used, 0)::bigint AS blob_gas_used,
			COALESCE(NULLIF(BTRIM(bl.user_attribution), ''), NULLIF(BTRIM(known.name), ''), '') AS raw_name,
			COALESCE(NULLIF(BTRIM(known.category), ''), '') AS raw_category
		FROM buckets bu
		JOIN blobs bl
			ON bl.network_id = $1
			AND bl.confirmed = true
			AND bl.block_number = bu.block_number
		LEFT JOIN blob_users known
			ON known.network_id = bl.network_id
			AND LOWER(known.address) = LOWER(bl.from_address)
	),
` + attributionEntityBaseSQL("$4") + `
	SELECT
		b.bucket_start AS timestamp,
		b.range_start,
		b.range_end,
		u.series_key,
		u.series_name,
		u.series_category,
		u.series_address,
		COALESCE(u.blob_count, 0)::int AS blob_count,
		COALESCE(u.total_cost_wei, '0') AS total_cost_wei,
		COALESCE(u.blob_gas_used, 0)::bigint AS blob_gas_used
	FROM buckets b
	LEFT JOIN bucketed_usage u ON u.bucket_start = b.bucket_start
	ORDER BY b.bucket_start ASC, u.blob_count DESC NULLS LAST, u.series_key ASC
`
