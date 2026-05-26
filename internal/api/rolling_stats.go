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
	NetworkID   int                  `json:"network_id,omitempty"`
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
	// Total realized blob base-fee cost in wei for the window (sum of per-blob integer wei costs).
	TotalCostWei string `json:"total_cost_wei" example:"26494271031506069"`
	// Deprecated alias: use total_cost_wei. This legacy field contains wei, not ETH.
	TotalCostETH  string `json:"total_cost_eth" extensions:"x-deprecated,x-replacement=total_cost_wei" example:"26494271031506069"`
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
	TotalCostETH       string    `db:"total_cost_eth"`
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
	durationSeconds := make([]int64, 0, len(windows))
	for _, window := range windows {
		labels = append(labels, window.Label)
		durationSeconds = append(durationSeconds, int64(window.Duration/time.Second))
	}

	cacheKey := fmt.Sprintf("rolling:%d:%s", network.ChainID, strings.Join(labels, ","))
	a.cacheMu.RLock()
	if cached, ok := a.rollingCache[cacheKey]; ok && time.Now().Before(cached.expiresAt) {
		a.cacheMu.RUnlock()
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

		var rows []rollingStatsWindowRow
		if err := a.db.SelectContext(
			queryCtx,
			&rows,
			queryRollingStatsWindows,
			network.ChainID,
			pq.Array(labels),
			pq.Array(durationSeconds),
			generatedAt,
		); err != nil {
			return RollingStatsResponse{}, err
		}

		response := RollingStatsResponse{
			NetworkID:   network.ChainID,
			NetworkName: network.Name,
			GeneratedAt: generatedAt,
			Windows:     make([]RollingWindowStats, 0, len(rows)),
		}
		for _, row := range rows {
			response.Windows = append(response.Windows, toRollingWindowStats(row))
		}

		a.cacheMu.Lock()
		a.rollingCache[cacheKey] = rollingStatsCacheEntry{
			response:  response,
			expiresAt: time.Now().Add(aggregateCacheTTL),
		}
		a.cacheMu.Unlock()

		return response, nil
	})
	if err != nil {
		logger.Error("Failed to get rolling blob market statistics",
			zap.String("network", network.Name),
			zap.Error(err))
		a.respondError(w, http.StatusInternalServerError, "Failed to get rolling blob market statistics")
		return
	}

	// The singleflight closure above always returns RollingStatsResponse or an error,
	// so the assertion's ok value can never be false here.
	response, _ := value.(RollingStatsResponse)

	a.respondSuccess(w, response)
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
		TotalCostWei:          row.TotalCostETH,
		TotalCostETH:          row.TotalCostETH,
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
