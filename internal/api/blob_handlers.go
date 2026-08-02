package api

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/params"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/blobparams"
	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

const mempoolPressureSampleLimit = 10000

// BlobResponse is a response containing blob data
type BlobResponse struct {
	ChainID     int    `json:"chain_id"`
	NetworkName string `json:"network_name,omitempty"`
	// BlockNumber is the including block height, or null for pending (mempool)
	// blobs that have not yet been included. The confirmed flag is the source of
	// truth for inclusion; this field is null whenever the internal block_number
	// sentinel (models.PendingBlockNumber) is stored.
	BlockNumber           *int64 `json:"block_number" extensions:"x-nullable=true" example:"19000000"`
	BlobIndex             int    `json:"blob_index"`
	TxHash                string `json:"tx_hash"`
	TransactionURL        string `json:"transaction_url,omitempty"`
	FromAddress           string `json:"from_address"`
	FromAddressURL        string `json:"from_address_url,omitempty"`
	BlockURL              string `json:"block_url,omitempty"`
	UserAttribution       string `json:"user_attribution,omitempty"`
	BlobSizeBytes         int64  `json:"blob_size_bytes"`
	BaseFeePerBlobGas     string `json:"base_fee_per_blob_gas"`
	BaseFeePerBlobGasGwei string `json:"base_fee_per_blob_gas_gwei,omitempty"`
	TipPerBlobGas         string `json:"tip_per_blob_gas"`
	TipPerBlobGasGwei     string `json:"tip_per_blob_gas_gwei,omitempty"`
	// Realized blob base-fee cost in wei, serialized as a decimal string.
	TotalCostWei         string    `json:"total_cost_wei" example:"4718548746240"`
	Timestamp            time.Time `json:"timestamp"`
	Confirmed            bool      `json:"confirmed"`
	MaxFeePerBlobGas     *string   `json:"max_fee_per_blob_gas,omitempty"`
	MaxFeePerBlobGasGwei string    `json:"max_fee_per_blob_gas_gwei,omitempty"`
	BlobGasUsed          *int64    `json:"blob_gas_used,omitempty"`
	RealizedCostWei      *string   `json:"realized_cost_wei,omitempty"`
	MaxCostWei           *string   `json:"max_cost_wei,omitempty"`
	HeadroomWei          *string   `json:"fee_cap_headroom_wei,omitempty"`
	HeadroomPercent      *string   `json:"fee_cap_headroom_percent,omitempty"`
	// VersionedHash is this blob's own EIP-4844 versioned hash
	// (0x01-prefixed). blob_index cannot locate the blob within
	// versioned_hashes — for confirmed rows it is the block-wide ordinal —
	// so this field is what identifies the row's blob. Omitted for rows
	// indexed before versioned hashes were stored.
	VersionedHash *string `json:"versioned_hash,omitempty" example:"0x01a1f8730e4064f7dd90279b721b25e28c07fc3e16d5fd4a26e6d3d5e9e0dbeb"`
	// VersionedHashes is the carrying transaction's full ordered list of
	// EIP-4844 versioned blob hashes (0x01-prefixed). Omitted for rows
	// indexed before versioned hashes were stored.
	VersionedHashes []string `json:"versioned_hashes,omitempty" example:"0x01a1f8730e4064f7dd90279b721b25e28c07fc3e16d5fd4a26e6d3d5e9e0dbeb"`
	// Slot is the beacon slot of the including block — the key beacon-side
	// blob reads (e.g. BlobArchive's /eth/v1/beacon/blobs/{slot}) are indexed
	// by. Stored at index time; for rows indexed before the slot column
	// existed it is derived at read time from the block timestamp, which
	// post-merge consensus makes exact. Omitted for pending (mempool) blobs —
	// no slot exists until inclusion — and for networks whose beacon genesis
	// time is not configured.
	Slot *uint64 `json:"slot,omitempty" example:"11813607"`
}

// BlobReplacementResponse is one observed replacement event: a pending blob
// transaction evicted from the mempool view because its sender reused the
// nonce in a fee-bumped replacement. replacement_tx_hash is the transaction
// that superseded it — resolve it via /blob/{txHash} to see whether it is
// pending or confirmed.
type BlobReplacementResponse struct {
	ChainID           int       `json:"chain_id"`
	NetworkName       string    `json:"network_name,omitempty"`
	ReplacedTxHash    string    `json:"replaced_tx_hash"`
	ReplacementTxHash string    `json:"replacement_tx_hash"`
	FromAddress       string    `json:"from_address"`
	Nonce             int64     `json:"nonce"`
	ReplacedAt        time.Time `json:"replaced_at"`
}

// BlockPricingResponse represents block-level blob pricing data
type BlockPricingResponse struct {
	BlockNumber        int64   `json:"block_number"`
	BlockTimestamp     string  `json:"block_timestamp"`
	BlobCount          int     `json:"blob_count"`
	BlobGasUsed        int64   `json:"blob_gas_used"`
	BlobGasTarget      int64   `json:"blob_gas_target"`
	BlobGasLimit       int64   `json:"blob_gas_limit"`
	ExcessBlobGas      int64   `json:"excess_blob_gas"`
	BlobBaseFee        string  `json:"blob_base_fee"`
	BlobBaseFeeGwei    string  `json:"blob_base_fee_gwei,omitempty"`
	UtilizationRatio   string  `json:"utilization_ratio"`
	BlobParamsTarget   int     `json:"blob_params_target"`
	BlobParamsMax      int     `json:"blob_params_max"`
	TargetBlobs        int     `json:"target_blobs"`
	MaxBlobs           int     `json:"max_blobs"`
	AvailableBlobs     int     `json:"available_blobs"`
	UtilizationPercent float64 `json:"utilization_percent"`
	IsFull             bool    `json:"is_full"`
	IsAboveTarget      bool    `json:"is_above_target"`
	UpdateFraction     int64   `json:"update_fraction"`
}

// BlobParamsResponse holds the current fork's blob parameters
type BlobParamsResponse struct {
	Target         int    `json:"target"`
	Max            int    `json:"max"`
	UpdateFraction uint64 `json:"update_fraction"`
	TargetGas      uint64 `json:"target_gas"`
	MaxGas         uint64 `json:"max_gas"`
}

// FeeEstimateRangeResponse represents a low/high blob fee estimate range.
type FeeEstimateRangeResponse struct {
	Low  string `json:"low"`
	High string `json:"high"`
}

// MarketPressureResponse summarizes recent blob market pressure indicators.
type MarketPressureResponse struct {
	RecentBlocksAboveTarget  int                      `json:"recent_blocks_above_target"`
	ConsecutiveFullBlocks    int                      `json:"consecutive_full_blocks"`
	PercentRecentBlocksAtMax float64                  `json:"percent_recent_blocks_at_max_blobs"`
	PredictedDirection       string                   `json:"predicted_direction"`
	NextBlockFeeEstimate     FeeEstimateRangeResponse `json:"next_block_fee_estimate"`
}

// PricingResponse is the top-level pricing API response
type PricingResponse struct {
	ChainID              int                    `json:"chain_id"`
	NetworkName          string                 `json:"network_name"`
	CurrentBaseFee       string                 `json:"current_base_fee"`
	CurrentBaseFeeGwei   string                 `json:"current_base_fee_gwei,omitempty"`
	CurrentExcessGas     int64                  `json:"current_excess_gas"`
	CurrentUtilization   string                 `json:"current_utilization"`
	PredictedNextFee     string                 `json:"predicted_next_fee"`
	PredictedNextFeeGwei string                 `json:"predicted_next_fee_gwei,omitempty"`
	ForkStage            string                 `json:"fork_stage"`
	BlobParams           BlobParamsResponse     `json:"blob_params"`
	MarketPressure       MarketPressureResponse `json:"market_pressure"`
	RecentBlocks         []BlockPricingResponse `json:"recent_blocks"`
}

// MempoolFeeDistributionResponse represents pending max fee distribution.
type MempoolFeeDistributionResponse struct {
	Min    string `json:"min"`
	Avg    string `json:"avg"`
	Median string `json:"median"`
	P95    string `json:"p95"`
	Max    string `json:"max"`
}

// MempoolAgeStatsResponse represents pending transaction age statistics.
type MempoolAgeStatsResponse struct {
	OldestAgeSeconds  float64    `json:"oldest_age_seconds"`
	NewestAgeSeconds  float64    `json:"newest_age_seconds"`
	AverageAgeSeconds float64    `json:"average_age_seconds"`
	OldestTimestamp   *time.Time `json:"oldest_timestamp,omitempty"`
	NewestTimestamp   *time.Time `json:"newest_timestamp,omitempty"`
}

// MempoolIncludabilityResponse summarizes likely pending transaction includability.
type MempoolIncludabilityResponse struct {
	LatestBlobBaseFee     string `json:"latest_blob_base_fee"`
	PricingAvailable      bool   `json:"pricing_available"`
	LikelyIncludableCount int    `json:"likely_includable_count"`
	UnderpricedCount      int    `json:"underpriced_count"`
	UnknownPricingCount   int    `json:"unknown_pricing_count"`
}

// MempoolPressureResponse is the top-level mempool pressure API response.
type MempoolPressureResponse struct {
	ChainID              int                            `json:"chain_id"`
	NetworkName          string                         `json:"network_name"`
	PendingBlobCount     int                            `json:"pending_blob_count"`
	PendingBlobGas       int64                          `json:"pending_blob_gas"`
	PendingUniqueSenders int                            `json:"pending_unique_senders"`
	MaxFeePerBlobGas     MempoolFeeDistributionResponse `json:"max_fee_per_blob_gas"`
	PendingTxAge         MempoolAgeStatsResponse        `json:"pending_tx_age"`
	Includability        MempoolIncludabilityResponse   `json:"includability"`
	SampleLimit          int                            `json:"sample_limit"`
	SampleTruncated      bool                           `json:"sample_truncated"`
	GeneratedAt          time.Time                      `json:"generated_at"`
}

type mempoolPressureAggregate struct {
	PendingBlobCount     int          `db:"pending_blob_count"`
	PendingBlobGas       int64        `db:"pending_blob_gas"`
	PendingUniqueSenders int          `db:"pending_unique_senders"`
	MaxFeeMin            string       `db:"max_fee_min"`
	MaxFeeAvg            string       `db:"max_fee_avg"`
	MaxFeeMedian         string       `db:"max_fee_median"`
	MaxFeeP95            string       `db:"max_fee_p95"`
	MaxFeeMax            string       `db:"max_fee_max"`
	OldestAgeSeconds     float64      `db:"oldest_age_seconds"`
	NewestAgeSeconds     float64      `db:"newest_age_seconds"`
	AverageAgeSeconds    float64      `db:"average_age_seconds"`
	OldestTimestamp      sql.NullTime `db:"oldest_timestamp"`
	NewestTimestamp      sql.NullTime `db:"newest_timestamp"`
	LikelyIncludable     int          `db:"likely_includable_count"`
	Underpriced          int          `db:"underpriced_count"`
	UnknownPricing       int          `db:"unknown_pricing_count"`
	SampleTruncated      bool         `db:"sample_truncated"`
}

// toBlobResponse converts a models.Blob to a BlobResponse.
func toBlobResponse(blob models.Blob, network config.NetworkConfig) BlobResponse {
	explorerURLs := explorerURLsForBlob(blob.ChainID, blob.TxHash, blob.FromAddress, blob.BlockNumber, blob.Confirmed)

	// Pending (mempool) rows carry the internal block_number sentinel
	// (models.PendingBlockNumber); serialize them as JSON null rather than a
	// negative number. The confirmed flag remains the source of truth for
	// inclusion.
	var blockNumber *int64
	if blob.BlockNumber >= 0 {
		bn := blob.BlockNumber
		blockNumber = &bn
	}

	response := BlobResponse{
		ChainID:               blob.ChainID,
		NetworkName:           network.Name,
		BlockNumber:           blockNumber,
		BlobIndex:             blob.BlobIndex,
		TxHash:                blob.TxHash,
		TransactionURL:        explorerURLs.Transaction,
		FromAddress:           blob.FromAddress,
		FromAddressURL:        explorerURLs.Address,
		BlockURL:              explorerURLs.Block,
		UserAttribution:       blob.UserAttribution,
		BlobSizeBytes:         blob.BlobSizeBytes,
		BaseFeePerBlobGas:     blob.BaseFeePerBlobGas,
		BaseFeePerBlobGasGwei: formatWeiAsGwei(blob.BaseFeePerBlobGas),
		TipPerBlobGas:         blob.TipPerBlobGas,
		TipPerBlobGasGwei:     formatWeiAsGwei(blob.TipPerBlobGas),
		TotalCostWei:          blob.TotalCostWei,
		Timestamp:             blob.Timestamp,
		Confirmed:             blob.Confirmed,
		MaxFeePerBlobGas:      blob.MaxFeePerBlobGas,
		MaxFeePerBlobGasGwei:  formatOptionalWeiAsGwei(blob.MaxFeePerBlobGas),
		BlobGasUsed:           blob.BlobGasUsed,
		VersionedHash:         blob.VersionedHash,
		VersionedHashes:       []string(blob.VersionedHashes),
		Slot:                  blobSlot(blob, network),
	}
	response.RealizedCostWei, response.MaxCostWei, response.HeadroomWei, response.HeadroomPercent = deriveBlobCostFields(blob)
	return response
}

// blobSlot resolves a blob's beacon slot: the stored index-time value when
// present, else — for confirmed rows indexed before the slot column existed —
// the same derivation the indexer applies (block timestamp against the
// network's beacon genesis time; exact for post-merge blocks, which every
// blob-carrying block is). Pending rows have no slot until inclusion, and
// networks without a known or configured beacon genesis time yield none.
func blobSlot(blob models.Blob, network config.NetworkConfig) *uint64 {
	if blob.Slot != nil && *blob.Slot >= 0 {
		s := uint64(*blob.Slot)
		return &s
	}
	if !blob.Confirmed {
		return nil
	}
	clock, ok := network.BeaconClock()
	if !ok {
		return nil
	}
	ts := blob.Timestamp.Unix()
	if ts < 0 {
		// A zero-value timestamp would wrap the uint64 conversion below.
		return nil
	}
	s, ok := clock.SlotAt(uint64(ts))
	if !ok {
		return nil
	}
	return &s
}

func deriveBlobCostFields(blob models.Blob) (realizedCostWei, maxCostWei, headroomWei, headroomPercent *string) {
	if blob.BlobGasUsed == nil || *blob.BlobGasUsed < 0 {
		return nil, nil, nil, nil
	}

	blobGasUsed := big.NewInt(*blob.BlobGasUsed)

	var realizedCost *big.Int
	if baseFeePerBlobGas, ok := parseNonNegativeDecimalInt(blob.BaseFeePerBlobGas); ok {
		realizedCost = new(big.Int).Mul(baseFeePerBlobGas, blobGasUsed)
		realizedCostStr := realizedCost.String()
		realizedCostWei = &realizedCostStr
	}

	if blob.MaxFeePerBlobGas == nil {
		return realizedCostWei, nil, nil, nil
	}
	maxFeePerBlobGas, ok := parseNonNegativeDecimalInt(*blob.MaxFeePerBlobGas)
	if !ok {
		return realizedCostWei, nil, nil, nil
	}

	maxCost := new(big.Int).Mul(maxFeePerBlobGas, blobGasUsed)
	maxCostStr := maxCost.String()
	maxCostWei = &maxCostStr

	if realizedCost == nil {
		return realizedCostWei, maxCostWei, nil, nil
	}

	headroom := new(big.Int).Sub(maxCost, realizedCost)
	headroomStr := headroom.String()
	headroomWei = &headroomStr

	if maxCost.Sign() == 0 {
		return realizedCostWei, maxCostWei, headroomWei, nil
	}
	percentNumerator := new(big.Int).Mul(headroom, big.NewInt(100))
	percent := new(big.Rat).SetFrac(percentNumerator, maxCost)
	percentStr := percent.FloatString(6)
	headroomPercent = &percentStr

	return realizedCostWei, maxCostWei, headroomWei, headroomPercent
}

func parseNonNegativeDecimalInt(value string) (*big.Int, bool) {
	rat, ok := new(big.Rat).SetString(strings.TrimSpace(value))
	if !ok || rat.Sign() < 0 || rat.Denom().Cmp(big.NewInt(1)) != 0 {
		return nil, false
	}
	return new(big.Int).Set(rat.Num()), true
}

func toBlockPricingResponse(m models.BlockMetrics) BlockPricingResponse {
	targetBlobs := blobSpaceLimit(m.BlobParamsTarget, m.BlobGasTarget)
	maxBlobs := blobSpaceLimit(m.BlobParamsMax, m.BlobGasLimit)
	availableBlobs := maxBlobs - m.BlobCount
	if availableBlobs < 0 {
		availableBlobs = 0
	}

	usedBlobs := m.BlobCount
	if usedBlobs < 0 {
		usedBlobs = 0
	}
	if maxBlobs > 0 && usedBlobs > maxBlobs {
		usedBlobs = maxBlobs
	}

	var utilizationPercent float64
	if maxBlobs > 0 {
		utilizationPercent = math.Round((float64(usedBlobs)/float64(maxBlobs))*10000) / 100
	}

	return BlockPricingResponse{
		BlockNumber:        m.BlockNumber,
		BlockTimestamp:     m.BlockTimestamp.UTC().Format(time.RFC3339),
		BlobCount:          m.BlobCount,
		BlobGasUsed:        m.BlobGasUsed,
		BlobGasTarget:      m.BlobGasTarget,
		BlobGasLimit:       m.BlobGasLimit,
		ExcessBlobGas:      m.ExcessBlobGas,
		BlobBaseFee:        m.BlobBaseFee,
		BlobBaseFeeGwei:    formatWeiAsGwei(m.BlobBaseFee),
		UtilizationRatio:   m.UtilizationRatio,
		BlobParamsTarget:   m.BlobParamsTarget,
		BlobParamsMax:      m.BlobParamsMax,
		TargetBlobs:        targetBlobs,
		MaxBlobs:           maxBlobs,
		AvailableBlobs:     availableBlobs,
		UtilizationPercent: utilizationPercent,
		IsFull:             maxBlobs > 0 && m.BlobCount >= maxBlobs,
		IsAboveTarget:      targetBlobs > 0 && m.BlobCount > targetBlobs,
		UpdateFraction:     m.UpdateFraction,
	}
}

func blobSpaceLimit(paramsBlobs int, gasLimit int64) int {
	if paramsBlobs > 0 {
		return paramsBlobs
	}
	if gasLimit <= 0 {
		return 0
	}
	return int(gasLimit / params.BlobTxBlobGasPerBlob)
}

func nullTimePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}

// cachedBlobList serves a blob-list query through an in-process TTL cache,
// collapsing concurrent identical requests via singleflight. Only the
// offset=0, unfiltered request shape is cached — the only shape blob-flow
// issues — which also keeps the key space bounded by limit (<= MaxQueryLimit)
// per network.
func (a *API) cachedBlobList(
	r *http.Request,
	cache map[string]blobListCacheEntry,
	cachePrefix string,
	ttl time.Duration,
	network config.NetworkConfig,
	limit int,
	query string,
) ([]BlobResponse, error) {
	key := cachePrefix + ":" + strconv.Itoa(network.ChainID) + ":" + strconv.Itoa(limit)

	a.cacheMu.RLock()
	if cached, ok := cache[key]; ok && time.Now().Before(cached.expiresAt) {
		a.cacheMu.RUnlock()
		return cached.response, nil
	}
	a.cacheMu.RUnlock()

	value, err, _ := a.aggregateGroup.Do(key, func() (interface{}, error) {
		a.cacheMu.RLock()
		if cached, ok := cache[key]; ok && time.Now().Before(cached.expiresAt) {
			a.cacheMu.RUnlock()
			return cached.response, nil
		}
		a.cacheMu.RUnlock()

		queryCtx, cancel := context.WithTimeout(aggregateWorkContext(r), aggregateQueryTimeout)
		defer cancel()

		var blobs []models.Blob
		if err := a.db.SelectContext(queryCtx, &blobs, query, network.ChainID, limit, 0); err != nil {
			return nil, err
		}

		response := make([]BlobResponse, 0, len(blobs))
		for _, blob := range blobs {
			response = append(response, toBlobResponse(blob, network))
		}

		a.cacheMu.Lock()
		cache[key] = blobListCacheEntry{response: response, expiresAt: time.Now().Add(ttl)}
		a.cacheMu.Unlock()
		return response, nil
	})
	if err != nil {
		return nil, err
	}

	// The singleflight closure above always returns []BlobResponse or an error,
	// so the assertion's ok value can never be false here.
	response, _ := value.([]BlobResponse)
	return response, nil
}

// blobList answers a latest/mempool list request: the hot cacheable shape goes
// through cachedBlobList, everything else (address filter, pagination) queries
// the database directly.
func (a *API) blobList(
	w http.ResponseWriter,
	r *http.Request,
	network config.NetworkConfig,
	cache map[string]blobListCacheEntry,
	cachePrefix string,
	ttl time.Duration,
	listQuery, byAddressQuery, errorMessage string,
) ([]BlobResponse, bool) {
	limit, offset, err := a.parsePagination(r, 10)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return nil, false
	}

	fromAddress := r.URL.Query().Get("from")

	if fromAddress == "" && offset == 0 {
		response, err := a.cachedBlobList(r, cache, cachePrefix, ttl, network, limit, listQuery)
		if err != nil {
			logger.Error(errorMessage,
				zap.String("network", network.Name),
				zap.Error(err))
			a.respondAggregateError(w, err, errorMessage)
			return nil, false
		}
		return response, true
	}

	var blobs []models.Blob
	if fromAddress != "" {
		if !common.IsHexAddress(fromAddress) {
			a.respondError(w, http.StatusBadRequest, "Invalid address format")
			return nil, false
		}
		fromAddress = common.HexToAddress(fromAddress).Hex()
		err = a.db.SelectContext(r.Context(), &blobs, byAddressQuery, network.ChainID, fromAddress, limit, offset)
	} else {
		err = a.db.SelectContext(r.Context(), &blobs, listQuery, network.ChainID, limit, offset)
	}
	if err != nil {
		logger.Error(errorMessage,
			zap.String("network", network.Name),
			zap.String("from", fromAddress),
			zap.Error(err))
		a.respondError(w, http.StatusInternalServerError, errorMessage)
		return nil, false
	}

	response := make([]BlobResponse, 0, len(blobs))
	for _, blob := range blobs {
		response = append(response, toBlobResponse(blob, network))
	}
	return response, true
}

// GetLatestBlobs godoc
// @Summary Get latest confirmed blobs
// @Description Retrieve the latest confirmed blob transactions from the blockchain
// @Tags blobs
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Param limit query int false "Number of blobs to return (default: 10, max: 100)"
// @Param offset query int false "Number of blobs to skip for pagination (default: 0, max: 10000)"
// @Param from query string false "Filter by sender address"
// @Success 200 {object} Response{data=[]BlobResponse} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 500 {object} Response "Internal server error"
// @Router /blob/latest [get]
func (a *API) GetLatestBlobs(w http.ResponseWriter, r *http.Request) {
	network, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	logger.Debug("Getting latest blobs", zap.String("network", network.Name))

	response, ok := a.blobList(w, r, network,
		a.latestBlobsCache, "latest_blobs", latestBlobsCacheTTL,
		queryLatestBlobs, queryLatestBlobsByAddress, "Failed to get latest blobs")
	if !ok {
		return
	}

	logger.Debug("Returning latest blobs",
		zap.String("network", network.Name),
		zap.Int("count", len(response)))
	setCacheControl(w, latestBlobsCacheTTL, latestBlobsEdgeTTL)
	a.respondSuccess(w, response)
}

// GetMempoolBlobs godoc
// @Summary Get pending blobs from mempool
// @Description Retrieve pending blob transactions from the mempool
// @Tags blobs
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Param limit query int false "Number of blobs to return (default: 10, max: 100)"
// @Param offset query int false "Number of blobs to skip for pagination (default: 0, max: 10000)"
// @Param from query string false "Filter by sender address"
// @Success 200 {object} Response{data=[]BlobResponse} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 500 {object} Response "Internal server error"
// @Router /blob/mempool [get]
func (a *API) GetMempoolBlobs(w http.ResponseWriter, r *http.Request) {
	network, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	logger.Debug("Getting mempool blobs", zap.String("network", network.Name))

	response, ok := a.blobList(w, r, network,
		a.mempoolBlobsCache, "mempool_blobs", mempoolBlobsCacheTTL,
		queryMempoolBlobs, queryMempoolBlobsByAddress, "Failed to get pending blobs")
	if !ok {
		return
	}

	logger.Debug("Returning mempool blobs",
		zap.String("network", network.Name),
		zap.Int("count", len(response)))
	setCacheControl(w, mempoolBlobsCacheTTL, mempoolBlobsEdgeTTL)
	a.respondSuccess(w, response)
}

// GetBlobReplacements godoc
// @Summary List replaced blob transactions
// @Description Recent pending blob transactions evicted from the mempool view because the sender reused their nonce in a fee-bumped replacement, newest first. Events are recorded at eviction time — when the replacement was seen pending or when it confirmed — and retained for roughly a week. Pass tx_hash to resolve the events touching one transaction, matched on either side of the replacement.
// @Tags blobs
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Param tx_hash query string false "Filter to events where this hash was replaced or was the replacement"
// @Param limit query int false "Number of events to return (default: 25, max: 100)"
// @Param offset query int false "Number of events to skip for pagination (default: 0, max: 10000)"
// @Success 200 {object} Response{data=[]BlobReplacementResponse} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 500 {object} Response "Internal server error"
// @Router /blob/replacements [get]
func (a *API) GetBlobReplacements(w http.ResponseWriter, r *http.Request) {
	network, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	limit, offset, err := a.parsePagination(r, 25)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	query := queryBlobReplacements
	args := []interface{}{network.ChainID, limit, offset}
	if txHash := r.URL.Query().Get("tx_hash"); txHash != "" {
		if !strings.HasPrefix(txHash, "0x") || !common.IsHexHash(txHash) {
			a.respondError(w, http.StatusBadRequest, "Invalid transaction hash format")
			return
		}
		query = queryBlobReplacementsByTxHash
		args = []interface{}{network.ChainID, txHash, limit, offset}
	}

	var events []models.BlobReplacement
	if err := a.db.SelectContext(r.Context(), &events, query, args...); err != nil {
		logger.Error("Failed to get blob replacements",
			zap.String("network", network.Name),
			zap.Error(err))
		a.respondError(w, http.StatusInternalServerError, "Failed to get blob replacements")
		return
	}

	response := make([]BlobReplacementResponse, 0, len(events))
	for _, e := range events {
		response = append(response, BlobReplacementResponse{
			ChainID:           network.ChainID,
			NetworkName:       network.Name,
			ReplacedTxHash:    e.ReplacedTxHash,
			ReplacementTxHash: e.ReplacementTxHash,
			FromAddress:       e.FromAddress,
			Nonce:             e.Nonce,
			ReplacedAt:        e.ReplacedAt,
		})
	}
	a.respondSuccess(w, response)
}

// GetMempoolPressure godoc
// @Summary Get blob mempool pressure
// @Description Retrieve bounded aggregate pressure metrics for pending blob transactions
// @Tags blobs
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Success 200 {object} Response{data=MempoolPressureResponse} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 500 {object} Response "Internal server error"
// @Failure 503 {object} Response "Database overloaded; retry later"
// @Router /blob/mempool/pressure [get]
func (a *API) GetMempoolPressure(w http.ResponseWriter, r *http.Request) {
	network, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	logger.Debug("Getting mempool pressure", zap.String("network", network.Name))

	a.cacheMu.RLock()
	if cached, ok := a.mempoolCache[network.ChainID]; ok && time.Now().Before(cached.expiresAt) {
		a.cacheMu.RUnlock()
		setCacheControl(w, mempoolPressureCacheTTL, mempoolPressureEdgeTTL)
		a.respondSuccess(w, cached.response)
		return
	}
	a.cacheMu.RUnlock()

	cacheKey := "mempool_pressure:" + strconv.Itoa(network.ChainID)
	value, err, _ := a.aggregateGroup.Do(cacheKey, func() (interface{}, error) {
		a.cacheMu.RLock()
		if cached, ok := a.mempoolCache[network.ChainID]; ok && time.Now().Before(cached.expiresAt) {
			a.cacheMu.RUnlock()
			return cached.response, nil
		}
		a.cacheMu.RUnlock()

		return a.queryMempoolPressure(aggregateWorkContext(r), network.ChainID, network.Name)
	})
	if err != nil {
		logger.Error("Failed to get mempool pressure",
			zap.String("network", network.Name),
			zap.Error(err))
		a.respondAggregateError(w, err, "Failed to get mempool pressure")
		return
	}

	// The singleflight closure above always returns MempoolPressureResponse or an error,
	// so the assertion's ok value can never be false here.
	response, _ := value.(MempoolPressureResponse)
	logger.Debug("Returning mempool pressure",
		zap.String("network", network.Name),
		zap.Int("pending_blob_count", response.PendingBlobCount),
		zap.Bool("pricing_available", response.Includability.PricingAvailable))
	setCacheControl(w, mempoolPressureCacheTTL, mempoolPressureEdgeTTL)
	a.respondSuccess(w, response)
}

func (a *API) queryMempoolPressure(ctx context.Context, networkID int, networkName string) (MempoolPressureResponse, error) {
	queryCtx, cancel := context.WithTimeout(ctx, aggregateQueryTimeout)
	defer cancel()

	latestBaseFee := "0"
	pricingAvailable := false
	if err := a.db.GetContext(queryCtx, &latestBaseFee, queryLatestBlobBaseFee, networkID); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return MempoolPressureResponse{}, err
		}
	} else {
		pricingAvailable = true
	}

	var latestBaseFeeArg interface{}
	if pricingAvailable {
		latestBaseFeeArg = latestBaseFee
	}

	var pressure mempoolPressureAggregate
	if err := a.db.GetContext(
		queryCtx,
		&pressure,
		queryMempoolPressure,
		networkID,
		mempoolPressureSampleLimit+1,
		mempoolPressureSampleLimit,
		latestBaseFeeArg,
	); err != nil {
		return MempoolPressureResponse{}, err
	}

	response := MempoolPressureResponse{
		ChainID:              networkID,
		NetworkName:          networkName,
		PendingBlobCount:     pressure.PendingBlobCount,
		PendingBlobGas:       pressure.PendingBlobGas,
		PendingUniqueSenders: pressure.PendingUniqueSenders,
		MaxFeePerBlobGas: MempoolFeeDistributionResponse{
			Min:    pressure.MaxFeeMin,
			Avg:    pressure.MaxFeeAvg,
			Median: pressure.MaxFeeMedian,
			P95:    pressure.MaxFeeP95,
			Max:    pressure.MaxFeeMax,
		},
		PendingTxAge: MempoolAgeStatsResponse{
			OldestAgeSeconds:  pressure.OldestAgeSeconds,
			NewestAgeSeconds:  pressure.NewestAgeSeconds,
			AverageAgeSeconds: pressure.AverageAgeSeconds,
			OldestTimestamp:   nullTimePtr(pressure.OldestTimestamp),
			NewestTimestamp:   nullTimePtr(pressure.NewestTimestamp),
		},
		Includability: MempoolIncludabilityResponse{
			LatestBlobBaseFee:     latestBaseFee,
			PricingAvailable:      pricingAvailable,
			LikelyIncludableCount: pressure.LikelyIncludable,
			UnderpricedCount:      pressure.Underpriced,
			UnknownPricingCount:   pressure.UnknownPricing,
		},
		SampleLimit:     mempoolPressureSampleLimit,
		SampleTruncated: pressure.SampleTruncated,
		GeneratedAt:     time.Now().UTC(),
	}

	a.cacheMu.Lock()
	a.mempoolCache[networkID] = mempoolPressureCacheEntry{
		response:  response,
		expiresAt: time.Now().Add(mempoolPressureCacheTTL),
	}
	a.cacheMu.Unlock()
	return response, nil
}

// GetBlobByTxHash godoc
// @Summary Get blob by transaction hash
// @Description Retrieve a specific blob transaction by its hash
// @Tags blobs
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Param txHash path string true "Transaction hash"
// @Success 200 {object} Response{data=BlobResponse} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 404 {object} Response "Blob not found"
// @Failure 500 {object} Response "Internal server error"
// @Router /blob/{txHash} [get]
func (a *API) GetBlobByTxHash(w http.ResponseWriter, r *http.Request) {
	network, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Get the transaction hash from the URL
	txHash := chi.URLParam(r, "txHash")
	if txHash == "" {
		a.respondError(w, http.StatusBadRequest, "Transaction hash is required")
		return
	}
	if !strings.HasPrefix(txHash, "0x") || !common.IsHexHash(txHash) {
		a.respondError(w, http.StatusBadRequest, "Invalid transaction hash format")
		return
	}

	logger.Debug("Getting blob by tx hash",
		zap.String("network", network.Name),
		zap.String("tx_hash", txHash))

	// Get the blob
	var blob models.Blob
	query := queryBlobByTxHash
	if err := a.db.GetContext(r.Context(), &blob, query, txHash, network.ChainID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			logger.Warn("Blob not found",
				zap.String("network", network.Name),
				zap.String("tx_hash", txHash),
				zap.Error(err))
			a.respondError(w, http.StatusNotFound, "Blob not found")
			return
		}
		logger.Error("Failed to get blob by tx hash",
			zap.String("network", network.Name),
			zap.String("tx_hash", txHash),
			zap.Error(err))
		a.respondError(w, http.StatusInternalServerError, "Failed to get blob")
		return
	}

	// A confirmed blob at a tx hash is immutable, so it is safely cacheable.
	// Pending rows stay uncached since they change as the tx confirms/drops.
	if blob.Confirmed {
		setCacheControl(w, confirmedBlobCacheTTL, confirmedBlobEdgeTTL)
	}
	a.respondSuccess(w, toBlobResponse(blob, network))
}

// GetBlobByVersionedHash godoc
// @Summary Get blob by EIP-4844 versioned hash
// @Description Retrieve the blob transaction carrying the given versioned blob hash (0x01-prefixed, 32 bytes). The returned blob is the matching blob row itself: versioned_hash echoes the matched hash and versioned_hashes lists the carrying transaction's full hash list (blob_index keeps its usual semantics — block-wide ordinal for confirmed rows, transaction-local for pending ones). Confirmed inclusions win over pending ones; when identical blob content was posted more than once (same content means the same versioned hash), the newest inclusion as of evaluation is returned — confirmed responses are briefly cacheable, so a repost may be reflected only after the cache TTL, and any cached answer remains a valid carrying transaction.
// @Tags blobs
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Param versionedHash path string true "EIP-4844 versioned blob hash (0x01-prefixed, 32 bytes)"
// @Success 200 {object} Response{data=BlobResponse} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 404 {object} Response "Blob not found"
// @Failure 500 {object} Response "Internal server error"
// @Router /blob/by-hash/{versionedHash} [get]
func (a *API) GetBlobByVersionedHash(w http.ResponseWriter, r *http.Request) {
	network, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	versionedHash := chi.URLParam(r, "versionedHash")
	if versionedHash == "" {
		a.respondError(w, http.StatusBadRequest, "Versioned hash is required")
		return
	}
	if !strings.HasPrefix(versionedHash, "0x") || !common.IsHexHash(versionedHash) {
		a.respondError(w, http.StatusBadRequest, "Invalid versioned hash format")
		return
	}
	// The indexer stores hashes in go-ethereum's lowercase hex encoding, so
	// normalize before the version check and the (case-sensitive) equality
	// match on versioned_hash.
	versionedHash = strings.ToLower(versionedHash)
	// A versioned blob hash's first byte is the version — 0x01 is EIP-4844's
	// VERSIONED_HASH_VERSION_KZG, the only version that exists.
	if !strings.HasPrefix(versionedHash, "0x01") {
		a.respondError(w, http.StatusBadRequest, "Invalid versioned hash: expected 0x01 version prefix")
		return
	}

	logger.Debug("Getting blob by versioned hash",
		zap.String("network", network.Name),
		zap.String("versioned_hash", versionedHash))

	var blob models.Blob
	if err := a.db.GetContext(r.Context(), &blob, queryBlobByVersionedHash, versionedHash, network.ChainID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			logger.Warn("Blob not found by versioned hash",
				zap.String("network", network.Name),
				zap.String("versioned_hash", versionedHash),
				zap.Error(err))
			a.respondError(w, http.StatusNotFound, "Blob not found")
			return
		}
		logger.Error("Failed to get blob by versioned hash",
			zap.String("network", network.Name),
			zap.String("versioned_hash", versionedHash),
			zap.Error(err))
		a.respondError(w, http.StatusInternalServerError, "Failed to get blob")
		return
	}

	// Same policy as the tx-hash lookup: cache confirmed results only. A rare
	// duplicate posting of the same blob content can later change which
	// transaction is newest, but any cached answer remains a valid carrying
	// transaction, so the short TTL is acceptable.
	if blob.Confirmed {
		setCacheControl(w, confirmedBlobCacheTTL, confirmedBlobEdgeTTL)
	}
	a.respondSuccess(w, toBlobResponse(blob, network))
}

// GetBlobPricing godoc
// @Summary Get blob pricing data
// @Description Retrieve current and historical blob pricing with utilization metrics and fork parameters. recent_blocks holds the N most recently indexed blocks, newest first, zero-blob blocks included (they still carry a blob base fee); gaps appear only for missed slots or blocks not yet indexed. market_pressure is computed over the same requested window.
// @Tags blobs
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Param blocks query int false "Number of recent blocks to include (default: 20, max: 512; out-of-range values are clamped, not rejected)"
// @Success 200 {object} Response{data=PricingResponse}
// @Failure 400 {object} Response "Bad request"
// @Failure 500 {object} Response "Internal server error"
// @Router /blob/pricing [get]
func (a *API) GetBlobPricing(w http.ResponseWriter, r *http.Request) {
	network, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Out-of-range blocks values clamp rather than 400 so older clients keep
	// working when the limits move; the frontend feature-detects the cap by
	// comparing recent_blocks length to what it asked for.
	blocks := DefaultPricingBlocks
	if blocksStr := r.URL.Query().Get("blocks"); blocksStr != "" {
		// Values overflowing int are still numeric out-of-range: Atoi reports
		// ErrRange but returns the nearest representable int, which the clamp
		// below folds into the valid window. Only non-numeric input is a 400.
		b, err := strconv.Atoi(blocksStr)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			a.respondError(w, http.StatusBadRequest, "Invalid blocks parameter")
			return
		}
		switch {
		case b < 1:
			b = DefaultPricingBlocks
		case b > MaxPricingBlocks:
			b = MaxPricingBlocks
		}
		blocks = b
	}

	// blob-flow refetches pricing on every WebSocket new_block broadcast, so
	// all connected clients arrive nearly simultaneously; the cache plus
	// singleflight collapses that herd to at most one query per TTL per
	// (network, blocks) key. The key space is bounded: blocks <= MaxPricingBlocks,
	// and invalidateBlockCaches clears the network's entries on every new block.
	key := "pricing:" + strconv.Itoa(network.ChainID) + ":" + strconv.Itoa(blocks)

	a.cacheMu.RLock()
	if cached, ok := a.pricingCache[key]; ok && time.Now().Before(cached.expiresAt) {
		a.cacheMu.RUnlock()
		setCacheControl(w, pricingCacheTTL, pricingEdgeTTL)
		a.respondSuccess(w, cached.response)
		return
	}
	a.cacheMu.RUnlock()

	value, err, _ := a.aggregateGroup.Do(key, func() (interface{}, error) {
		a.cacheMu.RLock()
		if cached, ok := a.pricingCache[key]; ok && time.Now().Before(cached.expiresAt) {
			a.cacheMu.RUnlock()
			return cached.response, nil
		}
		a.cacheMu.RUnlock()

		resp, err := a.queryBlobPricing(aggregateWorkContext(r), network, blocks)
		if err != nil {
			return PricingResponse{}, err
		}

		a.cacheMu.Lock()
		a.pricingCache[key] = pricingCacheEntry{response: resp, expiresAt: time.Now().Add(pricingCacheTTL)}
		a.cacheMu.Unlock()
		return resp, nil
	})
	if err != nil {
		logger.Error("Failed to get block metrics",
			zap.String("network", network.Name),
			zap.Error(err))
		a.respondAggregateError(w, err, "Failed to get pricing data")
		return
	}

	// The singleflight closure above always returns PricingResponse or an error,
	// so the assertion's ok value can never be false here.
	resp, _ := value.(PricingResponse)
	setCacheControl(w, pricingCacheTTL, pricingEdgeTTL)
	a.respondSuccess(w, resp)
}

func (a *API) queryBlobPricing(ctx context.Context, network config.NetworkConfig, blocks int) (PricingResponse, error) {
	queryCtx, cancel := context.WithTimeout(ctx, aggregateQueryTimeout)
	defer cancel()

	var metrics []models.BlockMetrics
	if err := a.db.SelectContext(queryCtx, &metrics, queryBlockMetrics, network.ChainID, blocks); err != nil {
		return PricingResponse{}, err
	}

	// Build response
	recentBlocks := make([]BlockPricingResponse, 0, len(metrics))
	for _, m := range metrics {
		recentBlocks = append(recentBlocks, toBlockPricingResponse(m))
	}

	cfg := a.chainConfigForNetwork(queryCtx, network.ChainID)

	// Use the most recent block for current state
	resp := PricingResponse{
		ChainID:        network.ChainID,
		NetworkName:    network.Name,
		MarketPressure: buildMarketPressure(metrics, cfg),
		RecentBlocks:   recentBlocks,
	}

	if len(metrics) > 0 {
		latest := metrics[0]
		latestTime := uint64(latest.BlockTimestamp.Unix())

		resp.CurrentBaseFee = latest.BlobBaseFee
		resp.CurrentBaseFeeGwei = formatWeiAsGwei(latest.BlobBaseFee)
		resp.CurrentExcessGas = latest.ExcessBlobGas
		resp.CurrentUtilization = latest.UtilizationRatio
		resp.ForkStage = blobparams.ForkName(
			cfg,
			latestTime,
		)
		resp.BlobParams = BlobParamsResponse{
			Target:         latest.BlobParamsTarget,
			Max:            latest.BlobParamsMax,
			UpdateFraction: uint64(latest.UpdateFraction),
			TargetGas:      uint64(latest.BlobParamsTarget) * params.BlobTxBlobGasPerBlob,
			MaxGas:         uint64(latest.BlobParamsMax) * params.BlobTxBlobGasPerBlob,
		}

		// Predict next base fee
		resp.PredictedNextFee = predictNextBlockBlobFee(cfg, latest).String()
		resp.PredictedNextFeeGwei = formatWeiAsGwei(resp.PredictedNextFee)
	}

	return resp, nil
}
