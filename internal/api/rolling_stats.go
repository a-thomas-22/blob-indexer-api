package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

const (
	maxRollingStatsWindows        = 8
	minRollingStatsWindowDuration = time.Minute
	maxRollingStatsWindowDuration = 30 * 24 * time.Hour

	// rollingStatsRawWindowCutoff is the longest window served below the
	// hourly-rollup tier. Longer windows read hourly chart rollups, because
	// scan cost grows with window width and blows the aggregate query timeout
	// on large deployments. Windows at or below the cutoff read fine (60s)
	// rollups when those cover the window, and fall back to raw blob and
	// block-metric scans while fine coverage is still being backfilled.
	rollingStatsRawWindowCutoff = 24 * time.Hour
)

var defaultRollingStatsWindows = []statsWindowSpec{
	{Label: "5m", Duration: 5 * time.Minute},
	{Label: "1h", Duration: time.Hour},
	{Label: apiWindow24h, Duration: 24 * time.Hour},
	{Label: "7d", Duration: 7 * 24 * time.Hour},
}

type statsWindowSpec struct {
	Label    string
	Duration time.Duration
}

// RollingStatsResponse is a response containing rolling-window blob market stats.
type RollingStatsResponse struct {
	ChainID     int                  `json:"chain_id,omitempty"`
	NetworkName string               `json:"network_name,omitempty"`
	GeneratedAt time.Time            `json:"generated_at"`
	Windows     []RollingWindowStats `json:"windows"`
}

// RollingWindowStats contains blob market statistics for one rolling time window.
type RollingWindowStats struct {
	Window          string    `json:"window"`
	DurationSeconds int64     `json:"duration_seconds"`
	StartTime       time.Time `json:"start_time"`
	EndTime         time.Time `json:"end_time"`
	// Average blob base fee in wei. Aggregate averages may include fractional decimal precision.
	AverageBlobBaseFeeWei string `json:"average_blob_base_fee_wei" example:"4841467206.84506683"`
	// Median blob base fee in wei.
	MedianBlobBaseFeeWei string `json:"median_blob_base_fee_wei" example:"4841467206"`
	// P95 blob base fee in wei.
	P95BlobBaseFeeWei string `json:"p95_blob_base_fee_wei" example:"9123456789"`
	// Deprecated alias: use average_blob_base_fee_wei.
	AverageBlobBaseFee string `json:"average_blob_base_fee" extensions:"x-deprecated,x-replacement=average_blob_base_fee_wei" example:"4841467206.84506683"`
	// Deprecated alias: use median_blob_base_fee_wei.
	MedianBlobBaseFee string `json:"median_blob_base_fee" extensions:"x-deprecated,x-replacement=median_blob_base_fee_wei" example:"4841467206"`
	// Deprecated alias: use p95_blob_base_fee_wei.
	P95BlobBaseFee     string `json:"p95_blob_base_fee" extensions:"x-deprecated,x-replacement=p95_blob_base_fee_wei" example:"9123456789"`
	TotalBlobs         int    `json:"total_blobs"`
	TotalBlobGasUsed   int64  `json:"total_blob_gas_used"`
	AverageUtilization string `json:"average_utilization"`
	// Total blocks indexed in the window (from block metrics).
	TotalBlocks int64 `json:"total_blocks"`
	// Blocks in the window whose blob gas used exceeded the block's blob gas target.
	BlocksAboveTarget int64 `json:"blocks_above_target"`
	// Blocks in the window whose blob gas used reached the block's blob gas limit.
	BlocksAtMax int64 `json:"blocks_at_max"`
	// Total realized blob base-fee cost in wei for the window (sum of per-blob integer wei costs).
	TotalCostWei  string `json:"total_cost_wei" example:"26494271031506069"`
	UniqueSenders int    `json:"unique_senders"`
}

type rollingStatsWindowRow struct {
	Window             string    `db:"stats_window"`
	DurationSeconds    int64     `db:"duration_seconds"`
	StartTime          time.Time `db:"start_time"`
	EndTime            time.Time `db:"end_time"`
	AverageBlobBaseFee string    `db:"average_blob_base_fee"`
	MedianBlobBaseFee  string    `db:"median_blob_base_fee"`
	P95BlobBaseFee     string    `db:"p95_blob_base_fee"`
	TotalBlobs         int       `db:"total_blobs"`
	TotalBlobGasUsed   int64     `db:"total_blob_gas_used"`
	AverageUtilization string    `db:"average_utilization"`
	TotalBlocks        int64     `db:"total_blocks"`
	BlocksAboveTarget  int64     `db:"blocks_above_target"`
	BlocksAtMax        int64     `db:"blocks_at_max"`
	TotalCostWei       string    `db:"total_cost_wei"`
	UniqueSenders      int       `db:"unique_senders"`
}

// GetRollingStatsWindows godoc
// @Summary Get rolling blob market statistics
// @Description Retrieve rolling time-window statistics for blob market dashboards
// @Tags stats
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Param windows query string false "Comma-separated rolling windows using m/h/d units (default: 5m,1h,24h,7d; max 8 windows, max 30d each)"
// @Success 200 {object} Response{data=RollingStatsResponse} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 500 {object} Response "Internal server error"
// @Failure 503 {object} Response "Database overloaded; retry later"
// @Router /stats/windows [get]
func (a *API) GetRollingStatsWindows(w http.ResponseWriter, r *http.Request) {
	network, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	windows, err := parseRollingStatsWindows(r.URL.Query().Get("windows"))
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	logger.Debug("Getting rolling blob market statistics",
		zap.String("network", network.Name),
		zap.Int("windows", len(windows)))

	labels := make([]string, 0, len(windows))
	for _, window := range windows {
		labels = append(labels, window.Label)
	}

	cacheKey := fmt.Sprintf("rolling:%d:%s", network.ChainID, strings.Join(labels, ","))
	a.cacheMu.RLock()
	if cached, ok := a.rollingCache[cacheKey]; ok && time.Now().Before(cached.expiresAt) {
		a.cacheMu.RUnlock()
		setCacheControl(w, statsCacheTTL)
		a.respondSuccess(w, cached.response)
		return
	}
	a.cacheMu.RUnlock()

	value, err, _ := a.aggregateGroup.Do(cacheKey, func() (interface{}, error) {
		a.cacheMu.RLock()
		if cached, ok := a.rollingCache[cacheKey]; ok && time.Now().Before(cached.expiresAt) {
			a.cacheMu.RUnlock()
			return cached.response, nil
		}
		a.cacheMu.RUnlock()

		generatedAt := time.Now().UTC()
		queryCtx, cancel := context.WithTimeout(aggregateWorkContext(r), aggregateQueryTimeout)
		defer cancel()

		fineCoverageStart, hasFineCoverage, err := a.fineRollupCoverageStart(queryCtx, network.ChainID)
		if err != nil {
			return RollingStatsResponse{}, err
		}

		fineWindows, rawWindows, rollupWindows := splitRollingStatsWindows(windows, generatedAt, fineCoverageStart, hasFineCoverage)
		rowsByLabel := make(map[string]rollingStatsWindowRow, len(windows))
		runWindowsQuery := func(query string, specs []statsWindowSpec) error {
			if len(specs) == 0 {
				return nil
			}
			queryLabels := make([]string, 0, len(specs))
			queryDurations := make([]int64, 0, len(specs))
			for _, spec := range specs {
				queryLabels = append(queryLabels, spec.Label)
				queryDurations = append(queryDurations, int64(spec.Duration/time.Second))
			}
			var rows []rollingStatsWindowRow
			if err := a.db.SelectContext(
				queryCtx,
				&rows,
				query,
				network.ChainID,
				pq.Array(queryLabels),
				pq.Array(queryDurations),
				generatedAt,
			); err != nil {
				return err
			}
			for _, row := range rows {
				rowsByLabel[row.Window] = row
			}
			return nil
		}
		if err := runWindowsQuery(queryRollingStatsWindowsFine, fineWindows); err != nil {
			return RollingStatsResponse{}, err
		}
		if err := runWindowsQuery(queryRollingStatsWindows, rawWindows); err != nil {
			return RollingStatsResponse{}, err
		}
		if err := runWindowsQuery(queryRollingStatsWindowsRollup, rollupWindows); err != nil {
			return RollingStatsResponse{}, err
		}

		response := RollingStatsResponse{
			ChainID:     network.ChainID,
			NetworkName: network.Name,
			GeneratedAt: generatedAt,
			Windows:     make([]RollingWindowStats, 0, len(windows)),
		}
		for _, window := range windows {
			if row, ok := rowsByLabel[window.Label]; ok {
				response.Windows = append(response.Windows, toRollingWindowStats(row))
			}
		}

		a.cacheMu.Lock()
		a.rollingCache[cacheKey] = rollingStatsCacheEntry{
			response:  response,
			expiresAt: time.Now().Add(statsCacheTTL),
		}
		a.cacheMu.Unlock()

		return response, nil
	})
	if err != nil {
		logger.Error("Failed to get rolling blob market statistics",
			zap.String("network", network.Name),
			zap.Error(err))
		a.respondAggregateError(w, err, "Failed to get rolling blob market statistics")
		return
	}

	// The singleflight closure above always returns RollingStatsResponse or an error,
	// so the assertion's ok value can never be false here.
	response, _ := value.(RollingStatsResponse)

	setCacheControl(w, statsCacheTTL)
	a.respondSuccess(w, response)
}

// GetRollingStatsChart godoc
// @Summary Get chart rolling blob market statistics
// @Description Retrieve rolling time-window statistics from the charts namespace
// @Tags charts
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Param windows query string false "Comma-separated rolling windows using m/h/d units (default: 5m,1h,24h,7d; max 8 windows, max 30d each)"
// @Success 200 {object} Response{data=RollingStatsResponse} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 500 {object} Response "Internal server error"
// @Failure 503 {object} Response "Database overloaded; retry later"
// @Router /charts/rolling-stats [get]
func (a *API) GetRollingStatsChart(w http.ResponseWriter, r *http.Request) {
	a.GetRollingStatsWindows(w, r)
}

// splitRollingStatsWindows partitions requested windows by serving tier:
// windows longer than the cutoff read hourly rollups, windows at or below it
// read fine (60s) rollups when coverage reaches the window start, and the
// remainder falls back to raw scans (fine coverage is only missing until the
// indexer's retention-window backfill completes after migration 4).
func splitRollingStatsWindows(windows []statsWindowSpec, generatedAt, fineCoverageStart time.Time, hasFineCoverage bool) (fine, raw, rollup []statsWindowSpec) {
	// The fine query aligns window ends down to the last completed minute, so
	// coverage is judged against the aligned window start.
	fineEnd := generatedAt.UTC().Truncate(time.Minute)
	for _, window := range windows {
		switch {
		case window.Duration > rollingStatsRawWindowCutoff:
			rollup = append(rollup, window)
		case hasFineCoverage && !fineCoverageStart.After(fineEnd.Add(-window.Duration)):
			fine = append(fine, window)
		default:
			raw = append(raw, window)
		}
	}
	return fine, raw, rollup
}

func toRollingWindowStats(row rollingStatsWindowRow) RollingWindowStats {
	return RollingWindowStats{
		Window:                row.Window,
		DurationSeconds:       row.DurationSeconds,
		StartTime:             row.StartTime,
		EndTime:               row.EndTime,
		AverageBlobBaseFeeWei: row.AverageBlobBaseFee,
		MedianBlobBaseFeeWei:  row.MedianBlobBaseFee,
		P95BlobBaseFeeWei:     row.P95BlobBaseFee,
		AverageBlobBaseFee:    row.AverageBlobBaseFee,
		MedianBlobBaseFee:     row.MedianBlobBaseFee,
		P95BlobBaseFee:        row.P95BlobBaseFee,
		TotalBlobs:            row.TotalBlobs,
		TotalBlobGasUsed:      row.TotalBlobGasUsed,
		AverageUtilization:    row.AverageUtilization,
		TotalBlocks:           row.TotalBlocks,
		BlocksAboveTarget:     row.BlocksAboveTarget,
		BlocksAtMax:           row.BlocksAtMax,
		TotalCostWei:          row.TotalCostWei,
		UniqueSenders:         row.UniqueSenders,
	}
}

func parseRollingStatsWindows(raw string) ([]statsWindowSpec, error) {
	if strings.TrimSpace(raw) == "" {
		windows := make([]statsWindowSpec, len(defaultRollingStatsWindows))
		copy(windows, defaultRollingStatsWindows)
		return windows, nil
	}

	parts := strings.Split(raw, ",")
	if len(parts) > maxRollingStatsWindows {
		return nil, fmt.Errorf("windows parameter supports at most %d windows", maxRollingStatsWindows)
	}

	windows := make([]statsWindowSpec, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		window, err := parseRollingStatsWindow(part)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[window.Label]; ok {
			return nil, fmt.Errorf("duplicate window %q", window.Label)
		}
		seen[window.Label] = struct{}{}
		windows = append(windows, window)
	}

	return windows, nil
}

func parseRollingStatsWindow(raw string) (statsWindowSpec, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	if len(value) < 2 {
		return statsWindowSpec{}, fmt.Errorf("invalid window %q; use durations like 5m, 1h, 24h, or 7d", raw)
	}

	unit := value[len(value)-1]
	amountText := value[:len(value)-1]
	for _, r := range amountText {
		if r < '0' || r > '9' {
			return statsWindowSpec{}, fmt.Errorf("invalid window %q; use durations like 5m, 1h, 24h, or 7d", raw)
		}
	}

	amount, err := strconv.Atoi(amountText)
	if err != nil || amount <= 0 {
		return statsWindowSpec{}, fmt.Errorf("invalid window %q; amount must be positive", raw)
	}

	var duration time.Duration
	switch unit {
	case 'm':
		duration = time.Duration(amount) * time.Minute
	case 'h':
		duration = time.Duration(amount) * time.Hour
	case 'd':
		duration = time.Duration(amount) * 24 * time.Hour
	default:
		return statsWindowSpec{}, fmt.Errorf("invalid window %q; supported units are m, h, and d", raw)
	}

	if duration < minRollingStatsWindowDuration || duration > maxRollingStatsWindowDuration {
		return statsWindowSpec{}, fmt.Errorf("invalid window %q; duration must be between 1m and 30d", raw)
	}

	return statsWindowSpec{
		Label:    fmt.Sprintf("%d%c", amount, unit),
		Duration: duration,
	}, nil
}
