package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/lib/pq"
	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

// Builder key prefixes the indexer's registry uses for its fallbacks: a
// printable extra-data label it does not recognize, or the bare fee
// recipient when there is no usable extra data. Everything else came from a
// registry match, which is what `known` reports. The raw fee_recipient and
// extra_data fields stay authoritative either way, so a later registry
// release can relabel the rows without the API changing shape.
const (
	builderKeyPrefixExtra = "extra:"
	builderKeyPrefixAddr  = "addr:"
)

// builderWindowAlignSeconds snaps the requested window's end down to the
// minute. Requests arriving within the same minute therefore share a window,
// so the short-TTL response cache, the ETag, and the edge all see one entry
// per URL instead of a window that drifts with every request.
const builderWindowAlignSeconds = 60

// defaultBuilderSeriesLimit is how many builders /charts/builder-share keeps
// as their own series before grouping the long tail into 'other'.
const defaultBuilderSeriesLimit = 8

// errBuilderNotFound signals that a builder key built no blocks in the
// requested window. Like the entity detail endpoint, the outcome is not
// cached: a builder that has just produced its first block should resolve
// immediately.
var errBuilderNotFound = errors.New("builder not found")

// builderRangeError names the accepted values so a client that sent `all`
// (which no rollup covers, since none carries the builder) learns what to
// send instead.
func builderRangeError() error {
	return fmt.Errorf("invalid range parameter; expected 1h, 24h, 7d, or 30d (all is not supported for builder endpoints)")
}

// builderKeyKnown reports whether a builder key came from the indexer's
// registry rather than one of its fallbacks.
func builderKeyKnown(key string) bool {
	return !strings.HasPrefix(key, builderKeyPrefixExtra) && !strings.HasPrefix(key, builderKeyPrefixAddr)
}

// parseBuilderRange resolves the aggregation window for the builder
// endpoints: one of the bounded chart ranges, defaulting to 24h.
func parseBuilderRange(r *http.Request, now time.Time) (label string, start, end time.Time, err error) {
	label = strings.ToLower(strings.TrimSpace(r.URL.Query().Get("range")))
	if label == "" {
		label = defaultChartRange
	}
	duration, ok := chartRangeDurations[label]
	if !ok {
		return "", time.Time{}, time.Time{}, builderRangeError()
	}
	end = alignChartEnd(now, builderWindowAlignSeconds)
	return label, end.Add(-duration), end, nil
}

// builderAggregateRow is one row of queryBuilderAggregates: a builder's
// totals over the window plus the window-wide totals (identical on every
// row) the share percentages divide by.
type builderAggregateRow struct {
	BuilderKey                string          `db:"builder_key"`
	BuilderName               string          `db:"builder_name"`
	FeeRecipients             pq.StringArray  `db:"fee_recipients"`
	Blocks                    int64           `db:"blocks"`
	BlobBlocks                int64           `db:"blob_blocks"`
	Blobs                     int64           `db:"blobs"`
	FullBlocks                int64           `db:"full_blocks"`
	MEVBoostBlocks            int64           `db:"mev_boost_blocks"`
	ProposerPaymentMedianWei  sql.NullString  `db:"proposer_payment_median_wei"`
	ProposerPaymentTotalWei   sql.NullString  `db:"proposer_payment_total_wei"`
	SnapshotBlocks            int64           `db:"snapshot_blocks"`
	BlocksWithEligibleSkipped int64           `db:"blocks_with_eligible_skipped"`
	EligibleSkippedTxs        int64           `db:"eligible_skipped_txs"`
	EligibleSkippedBlobs      int64           `db:"eligible_skipped_blobs"`
	EligibleSkippedMaxTipWei  sql.NullString  `db:"eligible_skipped_max_tip_wei"`
	TipTxCount                int64           `db:"tip_tx_count"`
	TipMinWei                 sql.NullString  `db:"tip_min_wei"`
	TipP10Wei                 sql.NullString  `db:"tip_p10_wei"`
	TipP50Wei                 sql.NullString  `db:"tip_p50_wei"`
	TipP90Wei                 sql.NullString  `db:"tip_p90_wei"`
	InclusionSampleCount      int64           `db:"inclusion_sample_count"`
	InclusionP50Ms            sql.NullFloat64 `db:"inclusion_p50_ms"`
	InclusionP90Ms            sql.NullFloat64 `db:"inclusion_p90_ms"`
	TotalBlocks               int64           `db:"total_blocks"`
	TotalBlobBlocks           int64           `db:"total_blob_blocks"`
	TotalBlobs                int64           `db:"total_blobs"`
}

// BuilderWindow is the resolved aggregation window, echoed so a client can
// label a chart without re-deriving it from `range`.
type BuilderWindow struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// BuilderTotals are the window's totals across every builder — the
// denominators behind each builder's block_share_percent and
// blob_share_percent.
//
// They cover only blocks that have a builder row, so on a network whose
// builder backfill has not reached the range start they read lower than the
// blob-market chart does for the same window. BuilderUserRow's
// share_overall_percent deliberately uses a different denominator: every
// indexed blob in the window, builder-attributed or not.
type BuilderTotals struct {
	Blocks     int64 `json:"blocks"`
	BlobBlocks int64 `json:"blob_blocks"`
	Blobs      int64 `json:"blobs"`
}

// BuilderTipStats summarizes the execution-layer priority fees of the blob
// transactions a builder included, one sample per transaction (a multi-blob
// transaction counts once). Wei values are decimal strings with a gwei
// display companion. Percentiles are interpolated (percentile_cont).
type BuilderTipStats struct {
	// TxCount is the number of included blob transactions with a recorded
	// priority fee; rows indexed before the fee was stored are excluded.
	TxCount int64  `json:"tx_count"`
	MinWei  string `json:"min"`
	MinGwei string `json:"min_gwei"`
	P10Wei  string `json:"p10"`
	P10Gwei string `json:"p10_gwei"`
	P50Wei  string `json:"p50"`
	P50Gwei string `json:"p50_gwei"`
	P90Wei  string `json:"p90"`
	P90Gwei string `json:"p90_gwei"`
}

// BuilderInclusionStats summarizes how long the builder's included blob
// transactions waited: the block's timestamp minus when the indexer first
// saw the transaction pending, one sample per transaction.
//
// Values can be negative. A block's timestamp is its slot start, and a
// transaction that reached our node after that instant but still made the
// block yields a negative wait. That is real information — it means the
// builder took a transaction our node saw late — so it is reported rather
// than clamped.
type BuilderInclusionStats struct {
	// SampleCount counts the included transactions with a first-seen time;
	// transactions indexed from history or that arrived with their block
	// have none.
	SampleCount int64   `json:"sample_count"`
	P50         float64 `json:"p50"`
	P90         float64 `json:"p90"`
}

// BuilderProposerPayment is the conventional MEV-Boost proposer payment —
// the value of a block's last transaction when the fee recipient sent it —
// summarized over the builder's blocks.
type BuilderProposerPayment struct {
	// MedianWei is a discrete median: an actually-observed payment rather
	// than an interpolated one, so the exact wei integer survives.
	MedianWei string `json:"median"`
	MedianEth string `json:"median_eth"`
	TotalWei  string `json:"total"`
	TotalEth  string `json:"total_eth"`
}

// BuilderCandidateStats summarizes the pending blob transactions our node
// had seen when this builder's blocks arrived and that the blocks did not
// include, counting only the ones nothing else disqualified.
//
// Our node's mempool is not the builder's: private order flow and slow blob
// propagation mean a candidate we saw may never have reached the builder.
// Present these as "visible to our node and not included", not as proof the
// builder passed on them.
type BuilderCandidateStats struct {
	// SnapshotBlocks is how many of the builder's blocks arrived live enough
	// for the indexer to classify the pending pool against them; blocks
	// indexed from history contribute nothing.
	SnapshotBlocks            int64  `json:"snapshot_blocks"`
	BlocksWithEligibleSkipped int64  `json:"blocks_with_eligible_skipped"`
	EligibleSkippedTxs        int64  `json:"eligible_skipped_txs"`
	EligibleSkippedBlobs      int64  `json:"eligible_skipped_blobs"`
	EligibleSkippedMaxTipWei  string `json:"eligible_skipped_max_tip,omitempty"`
	EligibleSkippedMaxTipGwei string `json:"eligible_skipped_max_tip_gwei,omitempty"`
}

// BuilderStats is one builder's behavior over the requested window.
type BuilderStats struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	// Known is true when the indexer's registry recognized the builder;
	// false means the key was derived from the raw extra data or the fee
	// recipient.
	Known bool `json:"known"`
	// FeeRecipients are the builder's three most-used coinbase addresses in
	// the window, busiest first.
	FeeRecipients []string `json:"fee_recipients"`
	Blocks        int64    `json:"blocks"`
	BlobBlocks    int64    `json:"blob_blocks"`
	Blobs         int64    `json:"blobs"`
	// FullBlocks counts blocks that carried the fork's maximum blobs.
	FullBlocks           int64   `json:"full_blocks"`
	BlockSharePercent    float64 `json:"block_share_percent"`
	BlobSharePercent     float64 `json:"blob_share_percent"`
	AvgBlobsPerBlobBlock float64 `json:"avg_blobs_per_blob_block"`
	// MEVBoostBlocks counts blocks carrying a proposer payment, the
	// signature of a block bought through MEV-Boost.
	MEVBoostBlocks int64 `json:"mev_boost_blocks"`
	// The four blocks below are null when the window holds no sample:
	// no proposer payments, no priced blob transactions, no first-seen
	// times, and no live candidate snapshot respectively.
	ProposerPayment   *BuilderProposerPayment `json:"proposer_payment_wei"`
	Tip               *BuilderTipStats        `json:"tip"`
	TimeToInclusionMs *BuilderInclusionStats  `json:"time_to_inclusion_ms"`
	Candidates        *BuilderCandidateStats  `json:"candidates"`
}

// BuildersResponse is the /builders leaderboard: every builder that produced
// an indexed block in the window, most blocks first.
type BuildersResponse struct {
	ChainID     int           `json:"chain_id"`
	NetworkName string        `json:"network_name,omitempty"`
	Range       string        `json:"range"`
	Window      BuilderWindow `json:"window"`
	Totals      BuilderTotals `json:"totals"`
	// GeneratedAt is when the window was resolved.
	GeneratedAt time.Time      `json:"generated_at"`
	Builders    []BuilderStats `json:"builders"`
}

// BuilderUserRow is one attribution entity's usage of a single builder.
// Attributed senders collapse into their entity, unattributed senders stay
// keyed by address — the same grouping /users?group=entity applies.
type BuilderUserRow struct {
	Key  string `json:"key"`
	Name string `json:"name,omitempty"`
	// IsEntity distinguishes an attribution entity from a bare sender
	// address standing in for an unattributed sender.
	IsEntity bool  `json:"is_entity"`
	Blobs    int64 `json:"blobs"`
	TxCount  int64 `json:"tx_count"`
	// ShareWithinBuilderPercent is this entity's share of the builder's
	// blobs; ShareOverallPercent is its share of every indexed blob in the
	// window.
	ShareWithinBuilderPercent float64 `json:"share_within_builder_percent"`
	ShareOverallPercent       float64 `json:"share_overall_percent"`
	// InclusionIndex is within/overall: 1 means the builder carries this
	// sender exactly as often as the market does, above 1 means it favors
	// them. Null when the sender posted nothing in the window overall,
	// which leaves the ratio undefined.
	InclusionIndex    *float64               `json:"inclusion_index"`
	TipP50Wei         string                 `json:"tip_p50,omitempty"`
	TipP50Gwei        string                 `json:"tip_p50_gwei,omitempty"`
	TimeToInclusionMs *BuilderInclusionP50Ms `json:"time_to_inclusion_ms"`
}

// BuilderInclusionP50Ms is the median inclusion wait of one sender on one
// builder. Same sign convention as BuilderInclusionStats.
type BuilderInclusionP50Ms struct {
	SampleCount int64   `json:"sample_count"`
	P50         float64 `json:"p50"`
}

// BuilderSkippedRow is one attribution entity's transactions that were
// pending, eligible, and left out of this builder's blocks. See
// BuilderCandidateStats for why these are node-visibility counts rather than
// proof of a builder decision.
type BuilderSkippedRow struct {
	Key        string `json:"key"`
	Name       string `json:"name,omitempty"`
	IsEntity   bool   `json:"is_entity"`
	Txs        int64  `json:"txs"`
	Blobs      int64  `json:"blobs"`
	MaxTipWei  string `json:"max_tip,omitempty"`
	MaxTipGwei string `json:"max_tip_gwei,omitempty"`
	P50TipWei  string `json:"p50_tip,omitempty"`
	P50TipGwei string `json:"p50_tip_gwei,omitempty"`
}

// BuilderRecentBlock is one of the builder's newest blocks in the window.
type BuilderRecentBlock struct {
	BlockNumber              int64     `json:"number"`
	Timestamp                time.Time `json:"timestamp"`
	BlobCount                int       `json:"blob_count"`
	BlobParamsMax            *int      `json:"blob_params_max,omitempty"`
	ProposerPaymentWei       *string   `json:"proposer_payment_wei,omitempty"`
	CandidateSnapshot        bool      `json:"candidate_snapshot"`
	EligibleSkippedTxs       *int      `json:"eligible_skipped_txs,omitempty"`
	EligibleSkippedMaxTipWei *string   `json:"eligible_skipped_max_tip,omitempty"`
}

// BuilderDetailResponse is /builders/{key}: the same aggregate object
// /builders carries for the builder, plus who it included, who it left
// pending, and its most recent blocks.
type BuilderDetailResponse struct {
	ChainID      int                  `json:"chain_id"`
	NetworkName  string               `json:"network_name,omitempty"`
	Range        string               `json:"range"`
	Window       BuilderWindow        `json:"window"`
	Totals       BuilderTotals        `json:"totals"`
	GeneratedAt  time.Time            `json:"generated_at"`
	Builder      BuilderStats         `json:"builder"`
	Users        []BuilderUserRow     `json:"users"`
	Skipped      []BuilderSkippedRow  `json:"skipped"`
	RecentBlocks []BuilderRecentBlock `json:"recent_blocks"`
}

type builderUserRow struct {
	Key                       string          `db:"key"`
	Name                      sql.NullString  `db:"name"`
	IsEntity                  bool            `db:"is_entity"`
	Blobs                     int64           `db:"blobs"`
	TxCount                   int64           `db:"tx_count"`
	TipP50Wei                 sql.NullString  `db:"tip_p50_wei"`
	InclusionSampleCount      int64           `db:"inclusion_sample_count"`
	InclusionP50Ms            sql.NullFloat64 `db:"inclusion_p50_ms"`
	ShareWithinBuilderPercent float64         `db:"share_within_builder_percent"`
	ShareOverallPercent       float64         `db:"share_overall_percent"`
}

type builderSkippedRow struct {
	Key       string         `db:"key"`
	Name      sql.NullString `db:"name"`
	IsEntity  bool           `db:"is_entity"`
	Txs       int64          `db:"txs"`
	Blobs     int64          `db:"blobs"`
	MaxTipWei sql.NullString `db:"max_tip_wei"`
	P50TipWei sql.NullString `db:"p50_tip_wei"`
}

type builderRecentBlockRow struct {
	BlockNumber              int64          `db:"block_number"`
	BlockTimestamp           time.Time      `db:"block_timestamp"`
	BlobCount                int            `db:"blob_count"`
	BlobParamsMax            *int           `db:"blob_params_max"`
	ProposerPaymentWei       sql.NullString `db:"proposer_payment_wei"`
	CandidateSnapshot        bool           `db:"candidate_snapshot"`
	EligibleSkippedTxs       *int           `db:"eligible_skipped_txs"`
	EligibleSkippedMaxTipWei sql.NullString `db:"eligible_skipped_max_tip_wei"`
}

// nullDecimal returns a non-empty decimal string, or "" when the column was
// NULL or blank.
func nullDecimal(v sql.NullString) string {
	if !v.Valid {
		return ""
	}
	return strings.TrimSpace(v.String)
}

func nullDecimalPtr(v sql.NullString) *string {
	value := nullDecimal(v)
	if value == "" {
		return nil
	}
	return &value
}

// builderRatio rounds a plain ratio the way chartPercentage rounds a
// percentage, so repeated responses are byte-identical.
func builderRatio(part, total float64) float64 {
	if total == 0 {
		return 0
	}
	return roundPercent(part / total)
}

func buildBuilderStats(row builderAggregateRow) BuilderStats {
	stats := BuilderStats{
		Key:                  row.BuilderKey,
		Name:                 row.BuilderName,
		Known:                builderKeyKnown(row.BuilderKey),
		FeeRecipients:        []string(row.FeeRecipients),
		Blocks:               row.Blocks,
		BlobBlocks:           row.BlobBlocks,
		Blobs:                row.Blobs,
		FullBlocks:           row.FullBlocks,
		BlockSharePercent:    chartPercentage(float64(row.Blocks), float64(row.TotalBlocks)),
		BlobSharePercent:     chartPercentage(float64(row.Blobs), float64(row.TotalBlobs)),
		AvgBlobsPerBlobBlock: builderRatio(float64(row.Blobs), float64(row.BlobBlocks)),
		MEVBoostBlocks:       row.MEVBoostBlocks,
	}
	if stats.FeeRecipients == nil {
		stats.FeeRecipients = []string{}
	}

	if median := nullDecimal(row.ProposerPaymentMedianWei); median != "" {
		total := nonEmptyDecimal(nullDecimal(row.ProposerPaymentTotalWei))
		stats.ProposerPayment = &BuilderProposerPayment{
			MedianWei: median,
			MedianEth: formatWeiAsETH(median),
			TotalWei:  total,
			TotalEth:  formatWeiAsETH(total),
		}
	}

	if row.TipTxCount > 0 {
		minWei := nonEmptyDecimal(nullDecimal(row.TipMinWei))
		p10 := nonEmptyDecimal(nullDecimal(row.TipP10Wei))
		p50 := nonEmptyDecimal(nullDecimal(row.TipP50Wei))
		p90 := nonEmptyDecimal(nullDecimal(row.TipP90Wei))
		stats.Tip = &BuilderTipStats{
			TxCount: row.TipTxCount,
			MinWei:  minWei,
			MinGwei: gweiOrZero(minWei),
			P10Wei:  p10,
			P10Gwei: gweiOrZero(p10),
			P50Wei:  p50,
			P50Gwei: gweiOrZero(p50),
			P90Wei:  p90,
			P90Gwei: gweiOrZero(p90),
		}
	}

	if row.InclusionSampleCount > 0 {
		stats.TimeToInclusionMs = &BuilderInclusionStats{
			SampleCount: row.InclusionSampleCount,
			P50:         roundPercent(row.InclusionP50Ms.Float64),
			P90:         roundPercent(row.InclusionP90Ms.Float64),
		}
	}

	if row.SnapshotBlocks > 0 {
		maxTip := nullDecimal(row.EligibleSkippedMaxTipWei)
		candidates := &BuilderCandidateStats{
			SnapshotBlocks:            row.SnapshotBlocks,
			BlocksWithEligibleSkipped: row.BlocksWithEligibleSkipped,
			EligibleSkippedTxs:        row.EligibleSkippedTxs,
			EligibleSkippedBlobs:      row.EligibleSkippedBlobs,
		}
		if maxTip != "" {
			candidates.EligibleSkippedMaxTipWei = maxTip
			candidates.EligibleSkippedMaxTipGwei = gweiOrZero(maxTip)
		}
		stats.Candidates = candidates
	}

	return stats
}

func buildBuildersResponse(chainID int, networkName, rangeLabel string, start, end, generatedAt time.Time, rows []builderAggregateRow) BuildersResponse {
	response := BuildersResponse{
		ChainID:     chainID,
		NetworkName: networkName,
		Range:       rangeLabel,
		Window:      BuilderWindow{From: start, To: end},
		GeneratedAt: generatedAt,
		Builders:    make([]BuilderStats, 0, len(rows)),
	}
	if len(rows) > 0 {
		response.Totals = BuilderTotals{
			Blocks:     rows[0].TotalBlocks,
			BlobBlocks: rows[0].TotalBlobBlocks,
			Blobs:      rows[0].TotalBlobs,
		}
	}
	for _, row := range rows {
		response.Builders = append(response.Builders, buildBuilderStats(row))
	}
	return response
}

// GetBuilders godoc
// @Summary List block builders
// @Description Rank the builders that produced indexed blocks over a window, most blocks first. Each row carries the builder's block and blob volume, its share of both, how often it filled a block, how many blocks carried an MEV-Boost proposer payment, the distribution of priority fees the blob transactions it included paid, how long those transactions waited, and — for blocks the indexer saw live — how many eligible pending blob transactions it left out. Wei values are decimal strings with gwei companions. range=all is not supported: no rollup carries the builder, so the window is capped at 30d.
// @Tags builders
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Param range query string false "Time range to aggregate (default: 24h; all is not supported)" Enums(1h, 24h, 7d, 30d)
// @Success 200 {object} Response{data=BuildersResponse} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 500 {object} Response "Internal server error"
// @Failure 503 {object} Response "Database overloaded; retry later"
// @Router /builders [get]
func (a *API) GetBuilders(w http.ResponseWriter, r *http.Request) {
	network, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	generatedAt := time.Now().UTC()
	rangeLabel, start, end, err := parseBuilderRange(r, generatedAt)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	logger.Debug("Getting builders",
		zap.String("network", network.Name),
		zap.String("range", rangeLabel))

	cacheKey := fmt.Sprintf("builders:%d:%s", network.ChainID, rangeLabel)
	value, err := a.cachedChartResponse(r, cacheKey, func(ctx context.Context) (interface{}, error) {
		queryCtx, cancel := context.WithTimeout(ctx, aggregateQueryTimeout)
		defer cancel()

		var rows []builderAggregateRow
		if err := a.db.SelectContext(queryCtx, &rows, queryBuilderAggregates, network.ChainID, start, end, ""); err != nil {
			return nil, err
		}
		return buildBuildersResponse(network.ChainID, network.Name, rangeLabel, start, end, generatedAt, rows), nil
	})
	if err != nil {
		logger.Error("Failed to get builders",
			zap.String("network", network.Name),
			zap.String("range", rangeLabel),
			zap.Error(err))
		a.respondAggregateError(w, err, "Failed to get builders")
		return
	}

	setCacheControl(w, aggregateCacheTTL, aggregateEdgeTTL)
	a.respondSuccess(w, value)
}

// GetBuilderByKey godoc
// @Summary Get one block builder's detail
// @Description Retrieve one builder by the key /builders returns: the same aggregate object, plus who it included (per attribution entity, with an inclusion index comparing the entity's share on this builder against its share of the whole window), who it left pending while eligible, and its most recent blocks. 404 when the builder produced no indexed block in the window. range=all is not supported.
// @Tags builders
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Param key path string true "Builder key as returned by /builders"
// @Param range query string false "Time range to aggregate (default: 24h; all is not supported)" Enums(1h, 24h, 7d, 30d)
// @Success 200 {object} Response{data=BuilderDetailResponse} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 404 {object} Response "Builder not found"
// @Failure 500 {object} Response "Internal server error"
// @Failure 503 {object} Response "Database overloaded; retry later"
// @Router /builders/{key} [get]
func (a *API) GetBuilderByKey(w http.ResponseWriter, r *http.Request) {
	network, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	// chi hands back the still-escaped segment when the path carried
	// percent-escapes, and builder keys legitimately contain ':' and '-'.
	key, err := url.PathUnescape(chi.URLParam(r, "key"))
	if err != nil {
		a.respondError(w, http.StatusBadRequest, "Invalid builder key")
		return
	}
	key = strings.TrimSpace(key)
	if key == "" {
		a.respondError(w, http.StatusBadRequest, "Invalid builder key")
		return
	}

	generatedAt := time.Now().UTC()
	rangeLabel, start, end, err := parseBuilderRange(r, generatedAt)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	logger.Debug("Getting builder detail",
		zap.String("network", network.Name),
		zap.String("key", key),
		zap.String("range", rangeLabel))

	cacheKey := fmt.Sprintf("builder:%d:%s:%s", network.ChainID, key, rangeLabel)
	value, err := a.cachedChartResponse(r, cacheKey, func(ctx context.Context) (interface{}, error) {
		queryCtx, cancel := context.WithTimeout(ctx, aggregateQueryTimeout)
		defer cancel()
		return a.builderDetail(queryCtx, network.ChainID, network.Name, key, rangeLabel, start, end, generatedAt)
	})
	if err != nil {
		if errors.Is(err, errBuilderNotFound) {
			a.respondError(w, http.StatusNotFound, "Builder not found")
			return
		}
		logger.Error("Failed to get builder detail",
			zap.String("network", network.Name),
			zap.String("key", key),
			zap.String("range", rangeLabel),
			zap.Error(err))
		a.respondAggregateError(w, err, "Failed to get builder")
		return
	}

	setCacheControl(w, aggregateCacheTTL, aggregateEdgeTTL)
	a.respondSuccess(w, value)
}

// builderDetail assembles the four reads behind /builders/{key}. The
// aggregate read runs first and short-circuits to errBuilderNotFound, so an
// unknown key costs one query rather than four.
func (a *API) builderDetail(ctx context.Context, chainID int, networkName, key, rangeLabel string, start, end, generatedAt time.Time) (BuilderDetailResponse, error) {
	var aggregates []builderAggregateRow
	if err := a.db.SelectContext(ctx, &aggregates, queryBuilderAggregates, chainID, start, end, key); err != nil {
		return BuilderDetailResponse{}, err
	}
	if len(aggregates) == 0 {
		return BuilderDetailResponse{}, errBuilderNotFound
	}

	response := BuilderDetailResponse{
		ChainID:     chainID,
		NetworkName: networkName,
		Range:       rangeLabel,
		Window:      BuilderWindow{From: start, To: end},
		Totals: BuilderTotals{
			Blocks:     aggregates[0].TotalBlocks,
			BlobBlocks: aggregates[0].TotalBlobBlocks,
			Blobs:      aggregates[0].TotalBlobs,
		},
		GeneratedAt:  generatedAt,
		Builder:      buildBuilderStats(aggregates[0]),
		Users:        []BuilderUserRow{},
		Skipped:      []BuilderSkippedRow{},
		RecentBlocks: []BuilderRecentBlock{},
	}

	var userRows []builderUserRow
	if err := a.db.SelectContext(ctx, &userRows, queryBuilderUsers, chainID, start, end, key); err != nil {
		return BuilderDetailResponse{}, err
	}
	for _, row := range userRows {
		user := BuilderUserRow{
			Key:                       row.Key,
			Name:                      nullStringDefault(row.Name, ""),
			IsEntity:                  row.IsEntity,
			Blobs:                     row.Blobs,
			TxCount:                   row.TxCount,
			ShareWithinBuilderPercent: row.ShareWithinBuilderPercent,
			ShareOverallPercent:       row.ShareOverallPercent,
		}
		if row.ShareOverallPercent != 0 {
			index := roundPercent(row.ShareWithinBuilderPercent / row.ShareOverallPercent)
			user.InclusionIndex = &index
		}
		if tip := nullDecimal(row.TipP50Wei); tip != "" {
			user.TipP50Wei = tip
			user.TipP50Gwei = gweiOrZero(tip)
		}
		if row.InclusionSampleCount > 0 {
			user.TimeToInclusionMs = &BuilderInclusionP50Ms{
				SampleCount: row.InclusionSampleCount,
				P50:         roundPercent(row.InclusionP50Ms.Float64),
			}
		}
		response.Users = append(response.Users, user)
	}

	var skippedRows []builderSkippedRow
	if err := a.db.SelectContext(ctx, &skippedRows, queryBuilderSkipped, chainID, start, end, key); err != nil {
		return BuilderDetailResponse{}, err
	}
	for _, row := range skippedRows {
		skipped := BuilderSkippedRow{
			Key:      row.Key,
			Name:     nullStringDefault(row.Name, ""),
			IsEntity: row.IsEntity,
			Txs:      row.Txs,
			Blobs:    row.Blobs,
		}
		if maxTip := nullDecimal(row.MaxTipWei); maxTip != "" {
			skipped.MaxTipWei = maxTip
			skipped.MaxTipGwei = gweiOrZero(maxTip)
		}
		if p50 := nullDecimal(row.P50TipWei); p50 != "" {
			skipped.P50TipWei = p50
			skipped.P50TipGwei = gweiOrZero(p50)
		}
		response.Skipped = append(response.Skipped, skipped)
	}

	var blockRows []builderRecentBlockRow
	if err := a.db.SelectContext(ctx, &blockRows, queryBuilderRecentBlocks, chainID, start, end, key, builderRecentBlockLimit); err != nil {
		return BuilderDetailResponse{}, err
	}
	for _, row := range blockRows {
		response.RecentBlocks = append(response.RecentBlocks, BuilderRecentBlock{
			BlockNumber:              row.BlockNumber,
			Timestamp:                row.BlockTimestamp,
			BlobCount:                row.BlobCount,
			BlobParamsMax:            row.BlobParamsMax,
			ProposerPaymentWei:       nullDecimalPtr(row.ProposerPaymentWei),
			CandidateSnapshot:        row.CandidateSnapshot,
			EligibleSkippedTxs:       row.EligibleSkippedTxs,
			EligibleSkippedMaxTipWei: nullDecimalPtr(row.EligibleSkippedMaxTipWei),
		})
	}

	return response, nil
}
