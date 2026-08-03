package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

// Streak kinds stored in blob_block_streaks. These are part of the schema
// contract with migration 000013, not free-form strings.
const (
	streakKindFull        = "full"
	streakKindAboveTarget = "above_target"
)

// DefaultRecordsLimit is the size of each /records top list when no limit is
// given. It matches what blob-flow's records page requests.
const DefaultRecordsLimit = 10

// recordsCacheTTL bounds in-process and browser staleness for /records. The
// leaderboards move at most once per block, and the poller drops the cached
// entry the moment a new block lands, so this is the ceiling rather than the
// typical staleness.
const recordsCacheTTL = 15 * time.Second

// recordsEdgeTTL is the shared-cache (Cloudflare) TTL for /records.
const recordsEdgeTTL = 15 * time.Second

// RecordRunResponse is one maximal run of consecutive indexed blocks that all
// satisfied a streak predicate.
type RecordRunResponse struct {
	Length         int64     `json:"length" example:"14"`
	StartBlock     int64     `json:"start_block" example:"22811332"`
	EndBlock       int64     `json:"end_block" example:"22811345"`
	StartTimestamp time.Time `json:"start_timestamp"`
	EndTimestamp   time.Time `json:"end_timestamp"`
}

// RecordStreaksResponse is one streak leaderboard: the all-time longest runs
// plus the run in progress at the chain tip, if any.
type RecordStreaksResponse struct {
	// Current is the run ending at the last indexed block, or null when the
	// tip block does not satisfy the predicate. A current run that is also
	// among the longest appears in Top as well.
	Current *RecordRunResponse  `json:"current" extensions:"x-nullable=true"`
	Top     []RecordRunResponse `json:"top"`
}

// RecordBaseFeePeakResponse is one block among the highest observed blob base
// fees.
type RecordBaseFeePeakResponse struct {
	BlockNumber int64     `json:"block_number" example:"19426587"`
	Timestamp   time.Time `json:"timestamp"`
	// BlobBaseFee is the block's blob base fee in wei as a decimal integer
	// string.
	BlobBaseFee string `json:"blob_base_fee" example:"496587109376"`
	// BlobBaseFeeGwei is the same fee rendered in gwei as a decimal string.
	BlobBaseFeeGwei string `json:"blob_base_fee_gwei" example:"496.587109376"`
	BlobCount       int    `json:"blob_count" example:"6"`
}

// RecordBusiestHourResponse is one UTC hour bucket among those carrying the
// most blobs.
type RecordBusiestHourResponse struct {
	// HourStart is the start of the UTC hour bucket.
	HourStart time.Time `json:"hour_start"`
	BlobCount int64     `json:"blob_count" example:"4211"`
	// TotalCostWei is the summed realized blob cost over the hour, in wei, as
	// a decimal integer string.
	TotalCostWei string `json:"total_cost_wei" example:"18446744073709551616"`
}

// RecordsResponse is the /records payload: historical leaderboards over a
// network's whole indexed history.
type RecordsResponse struct {
	NetworkID   int       `json:"network_id" example:"1"`
	NetworkName string    `json:"network_name" example:"mainnet"`
	GeneratedAt time.Time `json:"generated_at"`
	// FullBlockStreaks ranks runs of blocks that used their maximum blob
	// count, the same predicate as is_full on /blob/pricing recent blocks.
	FullBlockStreaks RecordStreaksResponse `json:"full_block_streaks"`
	// AboveTargetStreaks ranks runs of blocks that used more than their blob
	// target, the same predicate as is_above_target on /blob/pricing.
	AboveTargetStreaks RecordStreaksResponse       `json:"above_target_streaks"`
	BaseFeePeaks       []RecordBaseFeePeakResponse `json:"base_fee_peaks"`
	BusiestHours       []RecordBusiestHourResponse `json:"busiest_hours"`
}

type recordsCacheEntry struct {
	response  RecordsResponse
	expiresAt time.Time
}

type recordStreakRow struct {
	StartBlock     int64     `db:"start_block"`
	EndBlock       int64     `db:"end_block"`
	Length         int64     `db:"length"`
	StartTimestamp time.Time `db:"start_timestamp"`
	EndTimestamp   time.Time `db:"end_timestamp"`
}

type recordBaseFeePeakRow struct {
	BlockNumber    int64     `db:"block_number"`
	BlockTimestamp time.Time `db:"block_timestamp"`
	BlobBaseFee    string    `db:"blob_base_fee"`
	BlobCount      int       `db:"blob_count"`
}

type recordBusiestHourRow struct {
	HourStart    time.Time `db:"hour_start"`
	BlobCount    int64     `db:"blob_count"`
	TotalCostWei string    `db:"total_cost_wei"`
}

// GetBlobRecords godoc
// @Summary Get historical blob records
// @Description Retrieve all-time leaderboards for a network: longest runs of full and above-target blocks (with the run in progress, if any), the highest blob base fees, and the busiest UTC hours. Streak runs are maintained incrementally at index time, so the response cost does not grow with indexed history. A run is broken by any block that fails the predicate and by any gap in indexed history, including blocks removed by a reorg.
// @Tags records
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Param limit query int false "Entries per top list, clamped to 1-100" default(10)
// @Success 200 {object} Response{data=RecordsResponse} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 500 {object} Response "Internal server error"
// @Failure 503 {object} Response "Database overloaded; retry later"
// @Router /records [get]
func (a *API) GetBlobRecords(w http.ResponseWriter, r *http.Request) {
	network, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	limit, err := parseRecordsLimit(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	logger.Debug("Getting blob records",
		zap.String("network", network.Name),
		zap.Int("limit", limit))

	key := fmt.Sprintf("records:%d:%d", network.ChainID, limit)

	a.cacheMu.RLock()
	if cached, ok := a.recordsCache[key]; ok && time.Now().Before(cached.expiresAt) {
		a.cacheMu.RUnlock()
		setCacheControl(w, recordsCacheTTL, recordsEdgeTTL)
		a.respondSuccess(w, cached.response)
		return
	}
	a.cacheMu.RUnlock()

	value, err, _ := a.aggregateGroup.Do(key, func() (interface{}, error) {
		a.cacheMu.RLock()
		if cached, ok := a.recordsCache[key]; ok && time.Now().Before(cached.expiresAt) {
			a.cacheMu.RUnlock()
			return cached.response, nil
		}
		a.cacheMu.RUnlock()

		response, err := a.queryBlobRecords(aggregateWorkContext(r), network, limit)
		if err != nil {
			return nil, err
		}

		a.cacheMu.Lock()
		a.recordsCache[key] = recordsCacheEntry{
			response:  response,
			expiresAt: time.Now().Add(recordsCacheTTL),
		}
		a.cacheMu.Unlock()
		return response, nil
	})
	if err != nil {
		logger.Error("Failed to get blob records",
			zap.String("network", network.Name),
			zap.Error(err))
		a.respondAggregateError(w, err, "Failed to get blob records")
		return
	}

	// The singleflight closure returns RecordsResponse or an error, so the
	// assertion's ok value can never be false here.
	response, _ := value.(RecordsResponse)

	setCacheControl(w, recordsCacheTTL, recordsEdgeTTL)
	a.respondSuccess(w, response)
}

// parseRecordsLimit reads the limit query parameter, which sizes every top
// list in the response. Out-of-range values clamp into 1..MaxQueryLimit;
// values that are not numbers at all are a client bug and get a 400, matching
// the other list endpoints.
func parseRecordsLimit(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return DefaultRecordsLimit, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errors.New("invalid limit parameter")
	}
	if limit < 1 {
		return 1, nil
	}
	if limit > MaxQueryLimit {
		return MaxQueryLimit, nil
	}
	return limit, nil
}

func (a *API) queryBlobRecords(ctx context.Context, network config.NetworkConfig, limit int) (RecordsResponse, error) {
	queryCtx, cancel := context.WithTimeout(ctx, aggregateQueryTimeout)
	defer cancel()

	response := RecordsResponse{
		NetworkID:   network.ChainID,
		NetworkName: network.Name,
		GeneratedAt: time.Now().UTC(),
	}

	var err error
	if response.FullBlockStreaks, err = a.queryRecordStreaks(queryCtx, network.ChainID, streakKindFull, limit); err != nil {
		return RecordsResponse{}, err
	}
	if response.AboveTargetStreaks, err = a.queryRecordStreaks(queryCtx, network.ChainID, streakKindAboveTarget, limit); err != nil {
		return RecordsResponse{}, err
	}
	if response.BaseFeePeaks, err = a.queryRecordBaseFeePeaks(queryCtx, network.ChainID, limit); err != nil {
		return RecordsResponse{}, err
	}
	if response.BusiestHours, err = a.queryRecordBusiestHours(queryCtx, network.ChainID, limit); err != nil {
		return RecordsResponse{}, err
	}

	return response, nil
}

func (a *API) queryRecordStreaks(ctx context.Context, networkID int, kind string, limit int) (RecordStreaksResponse, error) {
	var top []recordStreakRow
	if err := a.db.SelectContext(ctx, &top, queryRecordTopStreaks, networkID, kind, limit); err != nil {
		return RecordStreaksResponse{}, err
	}

	response := RecordStreaksResponse{Top: make([]RecordRunResponse, 0, len(top))}
	for _, row := range top {
		response.Top = append(response.Top, toRecordRunResponse(row))
	}

	var current recordStreakRow
	// No row means the tip block does not satisfy the predicate, which the
	// contract reports as a null current run rather than an error.
	if err := a.db.GetContext(ctx, &current, queryRecordCurrentStreak, networkID, kind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return response, nil
		}
		return RecordStreaksResponse{}, err
	}
	run := toRecordRunResponse(current)
	response.Current = &run

	return response, nil
}

func (a *API) queryRecordBaseFeePeaks(ctx context.Context, networkID, limit int) ([]RecordBaseFeePeakResponse, error) {
	var rows []recordBaseFeePeakRow
	if err := a.db.SelectContext(ctx, &rows, queryRecordBaseFeePeaks, networkID, limit); err != nil {
		return nil, err
	}

	peaks := make([]RecordBaseFeePeakResponse, 0, len(rows))
	for _, row := range rows {
		peaks = append(peaks, RecordBaseFeePeakResponse{
			BlockNumber:     row.BlockNumber,
			Timestamp:       row.BlockTimestamp.UTC(),
			BlobBaseFee:     row.BlobBaseFee,
			BlobBaseFeeGwei: formatWeiAsGwei(row.BlobBaseFee),
			BlobCount:       row.BlobCount,
		})
	}
	return peaks, nil
}

func (a *API) queryRecordBusiestHours(ctx context.Context, networkID, limit int) ([]RecordBusiestHourResponse, error) {
	var rows []recordBusiestHourRow
	if err := a.db.SelectContext(ctx, &rows, queryRecordBusiestHours, networkID, limit); err != nil {
		return nil, err
	}

	hours := make([]RecordBusiestHourResponse, 0, len(rows))
	for _, row := range rows {
		hours = append(hours, RecordBusiestHourResponse{
			HourStart:    row.HourStart.UTC(),
			BlobCount:    row.BlobCount,
			TotalCostWei: row.TotalCostWei,
		})
	}
	return hours, nil
}

func toRecordRunResponse(row recordStreakRow) RecordRunResponse {
	return RecordRunResponse{
		Length:         row.Length,
		StartBlock:     row.StartBlock,
		EndBlock:       row.EndBlock,
		StartTimestamp: row.StartTimestamp.UTC(),
		EndTimestamp:   row.EndTimestamp.UTC(),
	}
}
