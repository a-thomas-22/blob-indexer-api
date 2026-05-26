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
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

const mempoolPressureSampleLimit = 10000

// BlobResponse is a response containing blob data
type BlobResponse struct {
	NetworkID             int       `json:"network_id"`
	NetworkName           string    `json:"network_name,omitempty"`
	BlockNumber           int64     `json:"block_number"`
	BlobIndex             int       `json:"blob_index"`
	TxHash                string    `json:"tx_hash"`
	TransactionURL        string    `json:"transaction_url,omitempty"`
	FromAddress           string    `json:"from_address"`
	FromAddressURL        string    `json:"from_address_url,omitempty"`
	BlockURL              string    `json:"block_url,omitempty"`
	UserAttribution       string    `json:"user_attribution,omitempty"`
	BlobSizeBytes         int64     `json:"blob_size_bytes"`
	BaseFeePerBlobGas     string    `json:"base_fee_per_blob_gas"`
	BaseFeePerBlobGasGwei string    `json:"base_fee_per_blob_gas_gwei,omitempty"`
	TipPerBlobGas         string    `json:"tip_per_blob_gas"`
	TipPerBlobGasGwei     string    `json:"tip_per_blob_gas_gwei,omitempty"`
	TotalCostETH          string    `json:"total_cost_eth"`
	Timestamp             time.Time `json:"timestamp"`
	Confirmed             bool      `json:"confirmed"`
	MaxFeePerBlobGas      *string   `json:"max_fee_per_blob_gas,omitempty"`
	MaxFeePerBlobGasGwei  string    `json:"max_fee_per_blob_gas_gwei,omitempty"`
	BlobGasUsed           *int64    `json:"blob_gas_used,omitempty"`
	RealizedCostWei       *string   `json:"realized_cost_wei,omitempty"`
	MaxCostWei            *string   `json:"max_cost_wei,omitempty"`
	HeadroomWei           *string   `json:"fee_cap_headroom_wei,omitempty"`
	HeadroomPercent       *string   `json:"fee_cap_headroom_percent,omitempty"`
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
	NetworkID            int                    `json:"network_id"`
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
	NetworkID            int                            `json:"network_id"`
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
func toBlobResponse(blob models.Blob, networkName string) BlobResponse {
	explorerURLs := explorerURLsForBlob(blob.NetworkID, blob.TxHash, blob.FromAddress, blob.BlockNumber, blob.Confirmed)

	response := BlobResponse{
		NetworkID:             blob.NetworkID,
		NetworkName:           networkName,
		BlockNumber:           blob.BlockNumber,
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
		TotalCostETH:          blob.TotalCostETH,
		Timestamp:             blob.Timestamp,
		Confirmed:             blob.Confirmed,
		MaxFeePerBlobGas:      blob.MaxFeePerBlobGas,
		MaxFeePerBlobGasGwei:  formatOptionalWeiAsGwei(blob.MaxFeePerBlobGas),
		BlobGasUsed:           blob.BlobGasUsed,
	}
	response.RealizedCostWei, response.MaxCostWei, response.HeadroomWei, response.HeadroomPercent = deriveBlobCostFields(blob)
	return response
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

	limit, offset, err := a.parsePagination(r, 10)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	fromAddress := r.URL.Query().Get("from")

	logger.Debug("Getting latest blobs",
		zap.String("network", network.Name),
		zap.Int("limit", limit),
		zap.Int("offset", offset))

	// Get the latest blobs, optionally filtered by sender address
	var blobs []models.Blob
	if fromAddress != "" {
		if !common.IsHexAddress(fromAddress) {
			a.respondError(w, http.StatusBadRequest, "Invalid address format")
			return
		}
		fromAddress = common.HexToAddress(fromAddress).Hex()
		if err := a.db.SelectContext(r.Context(), &blobs, queryLatestBlobsByAddress, network.ChainID, fromAddress, limit, offset); err != nil {
			logger.Error("Failed to get latest blobs by address",
				zap.String("network", network.Name),
				zap.String("from", fromAddress),
				zap.Error(err))
			a.respondError(w, http.StatusInternalServerError, "Failed to get latest blobs")
			return
		}
	} else if err := a.db.SelectContext(r.Context(), &blobs, queryLatestBlobs, network.ChainID, limit, offset); err != nil {
		logger.Error("Failed to get latest blobs",
			zap.String("network", network.Name),
			zap.Error(err))
		a.respondError(w, http.StatusInternalServerError, "Failed to get latest blobs")
		return
	}

	// Convert to response format
	response := make([]BlobResponse, 0, len(blobs))
	for _, blob := range blobs {
		response = append(response, toBlobResponse(blob, network.Name))
	}

	logger.Debug("Returning latest blobs",
		zap.String("network", network.Name),
		zap.Int("count", len(response)))
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

	limit, offset, err := a.parsePagination(r, 10)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	fromAddress := r.URL.Query().Get("from")

	logger.Debug("Getting mempool blobs",
		zap.String("network", network.Name),
		zap.Int("limit", limit),
		zap.Int("offset", offset))

	// Get the pending blobs, optionally filtered by sender address
	var blobs []models.Blob
	if fromAddress != "" {
		if !common.IsHexAddress(fromAddress) {
			a.respondError(w, http.StatusBadRequest, "Invalid address format")
			return
		}
		fromAddress = common.HexToAddress(fromAddress).Hex()
		if err := a.db.SelectContext(r.Context(), &blobs, queryMempoolBlobsByAddress, network.ChainID, fromAddress, limit, offset); err != nil {
			logger.Error("Failed to get pending blobs by address",
				zap.String("network", network.Name),
				zap.String("from", fromAddress),
				zap.Error(err))
			a.respondError(w, http.StatusInternalServerError, "Failed to get pending blobs")
			return
		}
	} else if err := a.db.SelectContext(r.Context(), &blobs, queryMempoolBlobs, network.ChainID, limit, offset); err != nil {
		logger.Error("Failed to get pending blobs",
			zap.String("network", network.Name),
			zap.Error(err))
		a.respondError(w, http.StatusInternalServerError, "Failed to get pending blobs")
		return
	}

	// Convert to response format
	response := make([]BlobResponse, 0, len(blobs))
	for _, blob := range blobs {
		response = append(response, toBlobResponse(blob, network.Name))
	}

	logger.Debug("Returning mempool blobs",
		zap.String("network", network.Name),
		zap.Int("count", len(response)))
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

		return a.queryMempoolPressure(r.Context(), network.ChainID, network.Name)
	})
	if err != nil {
		logger.Error("Failed to get mempool pressure",
			zap.String("network", network.Name),
			zap.Error(err))
		a.respondError(w, http.StatusInternalServerError, "Failed to get mempool pressure")
		return
	}

	// The singleflight closure above always returns MempoolPressureResponse or an error,
	// so the assertion's ok value can never be false here.
	response, _ := value.(MempoolPressureResponse)
	logger.Debug("Returning mempool pressure",
		zap.String("network", network.Name),
		zap.Int("pending_blob_count", response.PendingBlobCount),
		zap.Bool("pricing_available", response.Includability.PricingAvailable))
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
		NetworkID:            networkID,
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
		expiresAt: time.Now().Add(aggregateCacheTTL),
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

	// Convert to response format
	a.respondSuccess(w, toBlobResponse(blob, network.Name))
}

// GetBlobPricing godoc
// @Summary Get blob pricing data
// @Description Retrieve current and historical blob pricing with utilization metrics and fork parameters
// @Tags blobs
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Param blocks query int false "Number of recent blocks to include (default: 20, max: 100)"
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

	// Parse blocks parameter
	blocks := 20
	if blocksStr := r.URL.Query().Get("blocks"); blocksStr != "" {
		b, err := strconv.Atoi(blocksStr)
		if err != nil || b < 1 {
			a.respondError(w, http.StatusBadRequest, "Invalid blocks parameter")
			return
		}
		if b > MaxQueryLimit {
			b = MaxQueryLimit
		}
		blocks = b
	}

	// Query recent block metrics
	var metrics []models.BlockMetrics
	if err := a.db.SelectContext(r.Context(), &metrics, queryBlockMetrics, network.ChainID, blocks); err != nil {
		logger.Error("Failed to get block metrics",
			zap.String("network", network.Name),
			zap.Error(err))
		a.respondError(w, http.StatusInternalServerError, "Failed to get pricing data")
		return
	}

	// Build response
	recentBlocks := make([]BlockPricingResponse, 0, len(metrics))
	for _, m := range metrics {
		recentBlocks = append(recentBlocks, toBlockPricingResponse(m))
	}

	cfg := blobparams.ChainConfigForID(network.ChainID)

	// Use the most recent block for current state
	resp := PricingResponse{
		NetworkID:      network.ChainID,
		NetworkName:    network.Name,
		MarketPressure: buildMarketPressure(metrics, cfg),
		RecentBlocks:   recentBlocks,
	}

	if len(metrics) > 0 {
		latest := metrics[0]
		latestTime := uint64(latest.BlockTimestamp.Unix())
		bp := blobparams.GetBlobParams(cfg, latestTime)

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
		resp.PredictedNextFee = predictNextBlockBlobFee(cfg, latest, effectiveBlobTargetGas(latest, bp)).String()
		resp.PredictedNextFeeGwei = formatWeiAsGwei(resp.PredictedNextFee)
	}

	a.respondSuccess(w, resp)
}

// calcNextExcessBlobGas estimates the next block's excess blob gas using the EIP-4844 formula.
func calcNextExcessBlobGas(excessBlobGas, blobGasUsed, targetGas uint64) uint64 {
	total := excessBlobGas + blobGasUsed
	if total < targetGas {
		return 0
	}
	return total - targetGas
}
