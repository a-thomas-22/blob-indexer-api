package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/misc/eip4844"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/blobparams"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

// MaxQueryLimit is the maximum number of results any endpoint will return
const MaxQueryLimit = 100

// MaxQueryOffset is the maximum offset to prevent abuse of deep pagination
const MaxQueryOffset = 10000

const blobSelectColumns = `
	id,
	network_id,
	block_number,
	blob_index,
	tx_hash,
	from_address,
	user_attribution,
	blob_size_bytes,
	base_fee_per_blob_gas,
	tip_per_blob_gas,
	total_cost_eth,
	timestamp,
	confirmed,
	indexer_version,
	max_fee_per_blob_gas,
	blob_gas_used
`

const blockMetricsSelectColumns = `
	network_id,
	block_number,
	block_timestamp,
	blob_count,
	blob_gas_used,
	blob_gas_target,
	blob_gas_limit,
	excess_blob_gas,
	blob_base_fee,
	utilization_ratio,
	blob_params_target,
	blob_params_max,
	update_fraction
`

const aggregateCacheTTL = 30 * time.Second
const aggregateQueryTimeout = 5 * time.Second
const blobGasPerBlob = 131072

type userSortOption string

const (
	userSortCount userSortOption = "count"
	userSortSpend userSortOption = "spend"
)

type userWindowOption string

const (
	userWindow24h userWindowOption = "24h"
	userWindow7d  userWindowOption = "7d"
	userWindowAll userWindowOption = "all"
)

const (
	queryDevIndexerCounts = `
			SELECT
				COALESCE(SUM(CASE WHEN confirmed = true THEN 1 ELSE 0 END), 0) as confirmed_count,
				COALESCE(SUM(CASE WHEN confirmed = false THEN 1 ELSE 0 END), 0) as pending_count
			FROM blobs WHERE network_id = $1
		`
)

// Response is a generic API response
type Response struct {
	Success bool        `json:"success"`
	Data    interface{} `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
}

// BlobResponse is a response containing blob data
type BlobResponse struct {
	NetworkID         int       `json:"network_id"`
	NetworkName       string    `json:"network_name,omitempty"`
	BlockNumber       int64     `json:"block_number"`
	BlobIndex         int       `json:"blob_index"`
	TxHash            string    `json:"tx_hash"`
	FromAddress       string    `json:"from_address"`
	UserAttribution   string    `json:"user_attribution,omitempty"`
	BlobSizeBytes     int64     `json:"blob_size_bytes"`
	BaseFeePerBlobGas string    `json:"base_fee_per_blob_gas"`
	TipPerBlobGas     string    `json:"tip_per_blob_gas"`
	TotalCostETH      string    `json:"total_cost_eth"`
	Timestamp         time.Time `json:"timestamp"`
	Confirmed         bool      `json:"confirmed"`
	MaxFeePerBlobGas  *string   `json:"max_fee_per_blob_gas,omitempty"`
	BlobGasUsed       *int64    `json:"blob_gas_used,omitempty"`
	RealizedCostWei   *string   `json:"realized_cost_wei,omitempty"`
	MaxCostWei        *string   `json:"max_cost_wei,omitempty"`
	HeadroomWei       *string   `json:"fee_cap_headroom_wei,omitempty"`
	HeadroomPercent   *string   `json:"fee_cap_headroom_percent,omitempty"`
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

// PricingResponse is the top-level pricing API response
type PricingResponse struct {
	NetworkID          int                    `json:"network_id"`
	NetworkName        string                 `json:"network_name"`
	CurrentBaseFee     string                 `json:"current_base_fee"`
	CurrentExcessGas   int64                  `json:"current_excess_gas"`
	CurrentUtilization string                 `json:"current_utilization"`
	PredictedNextFee   string                 `json:"predicted_next_fee"`
	ForkStage          string                 `json:"fork_stage"`
	BlobParams         BlobParamsResponse     `json:"blob_params"`
	RecentBlocks       []BlockPricingResponse `json:"recent_blocks"`
}

// UserResponse is a response containing user data
type UserResponse struct {
	NetworkID         int       `json:"network_id"`
	NetworkName       string    `json:"network_name,omitempty"`
	Address           string    `json:"address"`
	Name              string    `json:"name,omitempty"`
	Category          string    `json:"category,omitempty"`
	BlobCount         int       `json:"blob_count"`
	TotalCostETH      string    `json:"total_cost_eth"`
	LastTimestamp     time.Time `json:"last_timestamp"`
	BlobSharePercent  float64   `json:"blob_share_percent,omitempty"`
	SpendSharePercent float64   `json:"spend_share_percent,omitempty"`
}

// CategoryShareResponse is a category-level market share bucket.
type CategoryShareResponse struct {
	Category          string  `json:"category"`
	BlobCount         int     `json:"blob_count"`
	TotalCostETH      string  `json:"total_cost_eth"`
	BlobSharePercent  float64 `json:"blob_share_percent"`
	SpendSharePercent float64 `json:"spend_share_percent"`
}

// UserBreakdownResponse is a response containing category market share data.
type UserBreakdownResponse struct {
	NetworkID      int                     `json:"network_id"`
	NetworkName    string                  `json:"network_name,omitempty"`
	Window         string                  `json:"window"`
	CategoryShares []CategoryShareResponse `json:"category_shares"`
}

// StatsResponse is a response containing blob statistics
type StatsResponse struct {
	NetworkID           int       `json:"network_id,omitempty"`
	NetworkName         string    `json:"network_name,omitempty"`
	TotalBlobs          int       `json:"total_blobs"`
	TotalConfirmedBlobs int       `json:"total_confirmed_blobs"`
	TotalPendingBlobs   int       `json:"total_pending_blobs"`
	AverageBaseFee      string    `json:"average_base_fee"`
	AverageTip          string    `json:"average_tip"`
	AverageTotalCost    string    `json:"average_total_cost"`
	LastIndexedBlock    uint64    `json:"last_indexed_block"`
	LastIndexedTime     time.Time `json:"last_indexed_time"`
}

// StatusResponse is a response containing indexer status
type StatusResponse struct {
	NetworkID        int       `json:"network_id,omitempty"`
	NetworkName      string    `json:"network_name,omitempty"`
	LastIndexedBlock uint64    `json:"last_indexed_block"`
	IndexerVersion   string    `json:"indexer_version"`
	Uptime           string    `json:"uptime"`
	LastIndexedTime  time.Time `json:"last_indexed_time"`
}

// toBlobResponse converts a models.Blob to a BlobResponse.
func toBlobResponse(blob models.Blob, networkName string) BlobResponse {
	response := BlobResponse{
		NetworkID:         blob.NetworkID,
		NetworkName:       networkName,
		BlockNumber:       blob.BlockNumber,
		BlobIndex:         blob.BlobIndex,
		TxHash:            blob.TxHash,
		FromAddress:       blob.FromAddress,
		UserAttribution:   blob.UserAttribution,
		BlobSizeBytes:     blob.BlobSizeBytes,
		BaseFeePerBlobGas: blob.BaseFeePerBlobGas,
		TipPerBlobGas:     blob.TipPerBlobGas,
		TotalCostETH:      blob.TotalCostETH,
		Timestamp:         blob.Timestamp,
		Confirmed:         blob.Confirmed,
		MaxFeePerBlobGas:  blob.MaxFeePerBlobGas,
		BlobGasUsed:       blob.BlobGasUsed,
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
	return int(gasLimit / blobGasPerBlob)
}

func toUserResponse(user models.BlobUserStats, networkID int, networkName string) UserResponse {
	return UserResponse{
		NetworkID:         networkID,
		NetworkName:       networkName,
		Address:           user.Address,
		Name:              user.Name,
		Category:          user.Category,
		BlobCount:         user.BlobCount,
		TotalCostETH:      user.TotalCostETH,
		LastTimestamp:     user.LastTimestamp,
		BlobSharePercent:  user.BlobSharePercent,
		SpendSharePercent: user.SpendSharePercent,
	}
}

func parseUserSortOption(r *http.Request) (userSortOption, error) {
	sort := strings.ToLower(r.URL.Query().Get("sort"))
	if sort == "" {
		return userSortCount, nil
	}
	switch userSortOption(sort) {
	case userSortCount, userSortSpend:
		return userSortOption(sort), nil
	default:
		return "", fmt.Errorf("invalid sort parameter")
	}
}

func parseUserWindowOption(r *http.Request) (userWindowOption, error) {
	window := strings.ToLower(r.URL.Query().Get("window"))
	if window == "" {
		return userWindowAll, nil
	}
	switch userWindowOption(window) {
	case userWindow24h, userWindow7d, userWindowAll:
		return userWindowOption(window), nil
	default:
		return "", fmt.Errorf("invalid window parameter")
	}
}

// respondJSON responds with JSON
func (a *API) respondJSON(w http.ResponseWriter, status int, data interface{}) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(data); err != nil {
		logger.Error("Failed to encode JSON response", zap.Error(err))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal server error"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// respondError responds with an error
func (a *API) respondError(w http.ResponseWriter, status int, message string) {
	logger.Warn("API error response",
		zap.Int("status", status),
		zap.String("message", message))
	a.respondJSON(w, status, Response{
		Success: false,
		Error:   message,
	})
}

// parsePagination parses limit/offset query params with clamping.
func (a *API) parsePagination(r *http.Request, defaultLimit int) (limit, offset int, err error) {
	limit = defaultLimit
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		parsed, parseErr := strconv.Atoi(limitStr)
		if parseErr != nil || parsed <= 0 {
			return 0, 0, fmt.Errorf("invalid limit parameter")
		}
		limit = parsed
	}
	if limit > MaxQueryLimit {
		limit = MaxQueryLimit
	}

	offset = 0
	if offsetStr := r.URL.Query().Get("offset"); offsetStr != "" {
		parsed, parseErr := strconv.Atoi(offsetStr)
		if parseErr != nil || parsed < 0 {
			return 0, 0, fmt.Errorf("invalid offset parameter")
		}
		offset = parsed
	}
	if offset > MaxQueryOffset {
		offset = MaxQueryOffset
	}

	return limit, offset, nil
}

// respondSuccess writes a successful JSON response with status 200.
func (a *API) respondSuccess(w http.ResponseWriter, data interface{}) {
	a.respondJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    data,
	})
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
		logger.Warn("Blob not found",
			zap.String("network", network.Name),
			zap.String("tx_hash", txHash),
			zap.Error(err))
		a.respondError(w, http.StatusNotFound, "Blob not found")
		return
	}

	// Convert to response format
	a.respondSuccess(w, toBlobResponse(blob, network.Name))
}

// GetTopBlobUsers godoc
// @Summary Get top blob users
// @Description Retrieve the top users of blob transactions by count or spend, optionally scoped to a recent window
// @Tags users
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Param limit query int false "Number of users to return (default: 10, max: 100)"
// @Param offset query int false "Number of users to skip for pagination (default: 0, max: 10000)"
// @Param sort query string false "Sort users by count or spend (default: count)" Enums(count, spend)
// @Param window query string false "Time window to aggregate (default: all)" Enums(24h, 7d, all)
// @Success 200 {object} Response{data=[]UserResponse} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 500 {object} Response "Internal server error"
// @Router /users [get]
func (a *API) GetTopBlobUsers(w http.ResponseWriter, r *http.Request) {
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

	sort, err := parseUserSortOption(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	window, err := parseUserWindowOption(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	logger.Debug("Getting top blob users",
		zap.String("network", network.Name),
		zap.Int("limit", limit),
		zap.Int("offset", offset),
		zap.String("sort", string(sort)),
		zap.String("window", string(window)))

	cacheKey := fmt.Sprintf("%d:%d:%d:%s:%s", network.ChainID, limit, offset, sort, window)
	a.cacheMu.RLock()
	if cached, ok := a.topUsersCache[cacheKey]; ok && time.Now().Before(cached.expiresAt) {
		a.cacheMu.RUnlock()
		a.respondJSON(w, http.StatusOK, Response{
			Success: true,
			Data:    cached.response,
		})
		return
	}
	a.cacheMu.RUnlock()

	var users []models.BlobUserStats
	queryCtx, cancel := context.WithTimeout(r.Context(), aggregateQueryTimeout)
	defer cancel()
	if err := a.db.SelectContext(queryCtx, &users, queryTopBlobUsersWithOptions, network.ChainID, limit, offset, string(window), string(sort)); err != nil {
		logger.Error("Failed to get top blob users",
			zap.String("network", network.Name),
			zap.String("sort", string(sort)),
			zap.String("window", string(window)),
			zap.Error(err))
		a.respondError(w, http.StatusInternalServerError, "Failed to get top blob users")
		return
	}

	response := make([]UserResponse, 0, len(users))
	for _, user := range users {
		response = append(response, toUserResponse(user, network.ChainID, network.Name))
	}

	logger.Debug("Returning top blob users",
		zap.String("network", network.Name),
		zap.String("sort", string(sort)),
		zap.String("window", string(window)),
		zap.Int("count", len(response)))
	a.cacheMu.Lock()
	a.topUsersCache[cacheKey] = topUsersCacheEntry{
		response:  response,
		expiresAt: time.Now().Add(aggregateCacheTTL),
	}
	a.cacheMu.Unlock()
	a.respondSuccess(w, response)
}

// GetUserBreakdown godoc
// @Summary Get blob user market breakdowns
// @Description Retrieve category market share for blob senders, optionally scoped to a recent window
// @Tags users
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Param window query string false "Time window to aggregate (default: all)" Enums(24h, 7d, all)
// @Success 200 {object} Response{data=UserBreakdownResponse} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 500 {object} Response "Internal server error"
// @Router /users/breakdown [get]
func (a *API) GetUserBreakdown(w http.ResponseWriter, r *http.Request) {
	network, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	window, err := parseUserWindowOption(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	logger.Debug("Getting blob user breakdown",
		zap.String("network", network.Name),
		zap.String("window", string(window)))

	queryCtx, cancel := context.WithTimeout(r.Context(), aggregateQueryTimeout)
	defer cancel()

	var categories []models.BlobUserCategoryShare
	if err := a.db.SelectContext(queryCtx, &categories, queryBlobUserCategoryBreakdown, network.ChainID, string(window)); err != nil {
		logger.Error("Failed to get blob user breakdown",
			zap.String("network", network.Name),
			zap.String("window", string(window)),
			zap.Error(err))
		a.respondError(w, http.StatusInternalServerError, "Failed to get user breakdown")
		return
	}

	categoryShares := make([]CategoryShareResponse, 0, len(categories))
	for _, category := range categories {
		categoryShares = append(categoryShares, CategoryShareResponse{
			Category:          category.Category,
			BlobCount:         category.BlobCount,
			TotalCostETH:      category.TotalCostETH,
			BlobSharePercent:  category.BlobSharePercent,
			SpendSharePercent: category.SpendSharePercent,
		})
	}

	a.respondSuccess(w, UserBreakdownResponse{
		NetworkID:      network.ChainID,
		NetworkName:    network.Name,
		Window:         string(window),
		CategoryShares: categoryShares,
	})
}

// GetUserByAddress godoc
// @Summary Get user by address
// @Description Retrieve aggregated blob statistics for a specific sender address
// @Tags users
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Param address path string true "Ethereum address"
// @Success 200 {object} Response{data=UserResponse} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 404 {object} Response "User not found"
// @Failure 500 {object} Response "Internal server error"
// @Router /users/{address} [get]
func (a *API) GetUserByAddress(w http.ResponseWriter, r *http.Request) {
	network, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	address := chi.URLParam(r, "address")
	if address == "" || !common.IsHexAddress(address) {
		a.respondError(w, http.StatusBadRequest, "Invalid address")
		return
	}
	address = common.HexToAddress(address).Hex()

	queryCtx, cancel := context.WithTimeout(r.Context(), aggregateQueryTimeout)
	defer cancel()

	var user models.BlobUserStats
	if err := a.db.GetContext(queryCtx, &user, queryUserByAddress, network.ChainID, address); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			a.respondError(w, http.StatusNotFound, "User not found")
			return
		}
		logger.Error("Failed to get user by address",
			zap.String("network", network.Name),
			zap.String("address", address),
			zap.Error(err))
		a.respondError(w, http.StatusInternalServerError, "Failed to get user")
		return
	}

	a.respondSuccess(w, toUserResponse(user, network.ChainID, network.Name))
}

// GetBlobStats godoc
// @Summary Get blob statistics
// @Description Retrieve statistics about blob transactions
// @Tags stats
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Success 200 {object} Response{data=StatsResponse} "Success"
// @Failure 500 {object} Response "Internal server error"
// @Router /stats [get]
func (a *API) GetBlobStats(w http.ResponseWriter, r *http.Request) {
	network, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	logger.Debug("Getting blob statistics", zap.String("network", network.Name))

	a.cacheMu.RLock()
	if cached, ok := a.statsCache[network.ChainID]; ok && time.Now().Before(cached.expiresAt) {
		a.cacheMu.RUnlock()
		a.respondJSON(w, http.StatusOK, Response{
			Success: true,
			Data:    cached.response,
		})
		return
	}
	a.cacheMu.RUnlock()

	var stats struct {
		TotalBlobs          int       `db:"total_blobs"`
		TotalConfirmedBlobs int       `db:"total_confirmed_blobs"`
		TotalPendingBlobs   int       `db:"total_pending_blobs"`
		AverageBaseFee      string    `db:"average_base_fee"`
		AverageTip          string    `db:"average_tip"`
		AverageTotalCost    string    `db:"average_total_cost"`
		LastIndexedTime     time.Time `db:"last_indexed_time"`
	}

	queryCtx, cancel := context.WithTimeout(r.Context(), aggregateQueryTimeout)
	defer cancel()
	if err := a.db.GetContext(queryCtx, &stats, queryBlobStats, network.ChainID); err != nil {
		logger.Error("Failed to get blob statistics",
			zap.String("network", network.Name),
			zap.Error(err))
		a.respondError(w, http.StatusInternalServerError, "Failed to get blob statistics")
		return
	}

	response := StatsResponse{
		NetworkID:           network.ChainID,
		NetworkName:         network.Name,
		TotalBlobs:          stats.TotalBlobs,
		TotalConfirmedBlobs: stats.TotalConfirmedBlobs,
		TotalPendingBlobs:   stats.TotalPendingBlobs,
		AverageBaseFee:      stats.AverageBaseFee,
		AverageTip:          stats.AverageTip,
		AverageTotalCost:    stats.AverageTotalCost,
		LastIndexedBlock:    a.getLastIndexedBlockFromDB(r.Context(), network.ChainID),
		LastIndexedTime:     stats.LastIndexedTime,
	}

	a.cacheMu.Lock()
	a.statsCache[network.ChainID] = statsCacheEntry{
		response:  response,
		expiresAt: time.Now().Add(aggregateCacheTTL),
	}
	a.cacheMu.Unlock()
	a.respondSuccess(w, response)
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

	// Use the most recent block for current state
	resp := PricingResponse{
		NetworkID:    network.ChainID,
		NetworkName:  network.Name,
		RecentBlocks: recentBlocks,
	}

	if len(metrics) > 0 {
		latest := metrics[0]
		resp.CurrentBaseFee = latest.BlobBaseFee
		resp.CurrentExcessGas = latest.ExcessBlobGas
		resp.CurrentUtilization = latest.UtilizationRatio
		resp.ForkStage = blobparams.ForkName(
			blobparams.ChainConfigForID(network.ChainID),
			uint64(latest.BlockTimestamp.Unix()),
		)
		resp.BlobParams = BlobParamsResponse{
			Target:         latest.BlobParamsTarget,
			Max:            latest.BlobParamsMax,
			UpdateFraction: uint64(latest.UpdateFraction),
			TargetGas:      uint64(latest.BlobParamsTarget) * blobGasPerBlob,
			MaxGas:         uint64(latest.BlobParamsMax) * blobGasPerBlob,
		}

		// Predict next base fee
		cfg := blobparams.ChainConfigForID(network.ChainID)
		bp := blobparams.GetBlobParams(cfg, uint64(latest.BlockTimestamp.Unix()))
		nextExcess := calcNextExcessBlobGas(uint64(latest.ExcessBlobGas), uint64(latest.BlobGasUsed), bp.TargetGas)
		nextHeader := &types.Header{
			Time:          uint64(latest.BlockTimestamp.Unix()) + 12,
			ExcessBlobGas: &nextExcess,
		}
		resp.PredictedNextFee = eip4844.CalcBlobFee(cfg, nextHeader).String()
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

// GetIndexerStatus godoc
// @Summary Get indexer status
// @Description Retrieve the current status of the indexer
// @Tags status
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Success 200 {object} Response{data=StatusResponse} "Success"
// @Failure 500 {object} Response "Internal server error"
// @Router /status [get]
func (a *API) GetIndexerStatus(w http.ResponseWriter, r *http.Request) {
	network, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	logger.Debug("Getting indexer status", zap.String("network", network.Name))

	var lastIndexedTime *time.Time
	query := "SELECT MAX(timestamp) FROM blobs WHERE confirmed = true AND network_id = $1"
	if err := a.db.GetContext(r.Context(), &lastIndexedTime, query, network.ChainID); err != nil {
		logger.Error("Failed to get last indexed time",
			zap.String("network", network.Name),
			zap.Error(err))
		a.respondError(w, http.StatusInternalServerError, "Failed to get last indexed time")
		return
	}

	uptime := time.Since(a.startTime).Truncate(time.Second).String()

	var indexedTime time.Time
	if lastIndexedTime != nil {
		indexedTime = *lastIndexedTime
	}

	response := StatusResponse{
		NetworkID:        network.ChainID,
		NetworkName:      network.Name,
		LastIndexedBlock: a.getLastIndexedBlockFromDB(r.Context(), network.ChainID),
		IndexerVersion:   a.config.Indexer.Version,
		Uptime:           uptime,
		LastIndexedTime:  indexedTime,
	}

	a.respondSuccess(w, response)
}

// SystemMetrics represents system-wide metrics
type SystemMetrics struct {
	Uptime          string    `json:"uptime"`
	GoVersion       string    `json:"go_version"`
	NumGoroutine    int       `json:"num_goroutine"`
	MemoryUsage     string    `json:"memory_usage"`
	TotalRequests   int64     `json:"total_requests"`
	ActiveRequests  int       `json:"active_requests"`
	StartTime       time.Time `json:"start_time"`
	CurrentTime     time.Time `json:"current_time"`
	NumCPU          int       `json:"num_cpu"`
	OperatingSystem string    `json:"operating_system"`
	Architecture    string    `json:"architecture"`
}

// IndexerMetrics represents metrics for a single indexer
type IndexerMetrics struct {
	NetworkID           int       `json:"network_id"`
	NetworkName         string    `json:"network_name"`
	LastIndexedBlock    uint64    `json:"last_indexed_block"`
	LastIndexedTime     time.Time `json:"last_indexed_time"`
	TotalBlobsIndexed   int       `json:"total_blobs_indexed"`
	PendingBlobsCount   int       `json:"pending_blobs_count"`
	ConfirmedBlobsCount int       `json:"confirmed_blobs_count"`
}

// DatabaseStats represents database statistics
type DatabaseStats struct {
	TotalTables        int         `json:"total_tables"`
	TotalSize          string      `json:"total_size"`
	TableStats         []TableStat `json:"table_stats"`
	ConnectionCount    int         `json:"connection_count"`
	IdleConnections    int         `json:"idle_connections"`
	InUseConnections   int         `json:"in_use_connections"`
	MaxOpenConnections int         `json:"max_open_connections"`
	LastMigrationTime  time.Time   `json:"last_migration_time"`
}

// TableStat represents statistics for a single database table
type TableStat struct {
	TableName    string    `json:"table_name"`
	RowCount     int       `json:"row_count"`
	SizeBytes    int64     `json:"size_bytes"`
	IndexCount   int       `json:"index_count"`
	LastVacuumed time.Time `json:"last_vacuumed"`
}

// LogEntry represents a single log entry
type LogEntry struct {
	Timestamp time.Time         `json:"timestamp"`
	Level     string            `json:"level"`
	Message   string            `json:"message"`
	Fields    map[string]string `json:"fields"`
}

// QueryStat represents statistics for a database query
type QueryStat struct {
	Query         string    `db:"query" json:"query"`
	ExecutionTime float64   `db:"execution_time" json:"execution_time"`
	Calls         int       `db:"calls" json:"calls"`
	RowsReturned  int       `db:"rows_returned" json:"rows_returned"`
	LastExecuted  time.Time `db:"last_executed" json:"last_executed"`
}

type devDashboardResponse struct {
	CurrentTime     time.Time `json:"current_time"`
	EnabledNetworks int       `json:"enabled_networks"`
	TotalRequests   int64     `json:"total_requests"`
	ActiveRequests  int64     `json:"active_requests"`
	Uptime          string    `json:"uptime"`
}

// DevMetrics godoc
// @Summary Get system metrics
// @Description Retrieve system-wide metrics including memory usage, goroutine count, etc.
// @Tags dev
// @Accept json
// @Produce json
// @Success 200 {object} Response{data=SystemMetrics} "Success"
// @Failure 500 {object} Response "Internal server error"
// @Router /dev/metrics [get]
func (a *API) DevMetrics(w http.ResponseWriter, r *http.Request) {
	logger.Debug("Getting system metrics")

	// Get current time
	currentTime := time.Now()

	// Calculate uptime
	startTime := currentTime.Add(-1 * time.Hour) // Placeholder, should be actual start time
	uptime := currentTime.Sub(startTime).String()

	// Create metrics response
	metrics := SystemMetrics{
		Uptime:          uptime,
		GoVersion:       runtime.Version(),
		NumGoroutine:    runtime.NumGoroutine(),
		MemoryUsage:     getMemoryUsage(),
		TotalRequests:   1000, // Placeholder
		ActiveRequests:  10,   // Placeholder
		StartTime:       startTime,
		CurrentTime:     currentTime,
		NumCPU:          runtime.NumCPU(),
		OperatingSystem: runtime.GOOS,
		Architecture:    runtime.GOARCH,
	}

	a.respondSuccess(w, metrics)
}

// getMemoryUsage returns the current memory usage as a string
func getMemoryUsage() string {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return fmt.Sprintf("%.2f MB", float64(m.Alloc)/1024/1024)
}

// DevIndexers godoc
// @Summary Get indexer metrics
// @Description Retrieve detailed metrics for all indexers
// @Tags dev
// @Accept json
// @Produce json
// @Success 200 {object} Response{data=[]IndexerMetrics} "Success"
// @Failure 500 {object} Response "Internal server error"
// @Router /dev/indexers [get]
func (a *API) DevIndexers(w http.ResponseWriter, r *http.Request) {
	logger.Debug("Getting indexer metrics")

	metrics := make([]IndexerMetrics, 0, len(a.networks))

	for _, network := range a.networks {
		lastIndexedBlock := a.getLastIndexedBlockFromDB(r.Context(), network.ChainID)

		var counts models.BlobCountTotals
		if err := a.db.GetContext(r.Context(), &counts, queryDevIndexerCounts, network.ChainID); err != nil {
			logger.Error("Failed to get blob counts",
				zap.String("network", network.Name),
				zap.Error(err))
		}

		var lastIndexedTime time.Time
		if err := a.db.GetContext(r.Context(), &lastIndexedTime, queryLastIndexedTimeCoalesce, network.ChainID); err != nil {
			logger.Error("Failed to get last indexed time",
				zap.String("network", network.Name),
				zap.Error(err))
		}

		metrics = append(metrics, IndexerMetrics{
			NetworkID:           network.ChainID,
			NetworkName:         network.Name,
			LastIndexedBlock:    lastIndexedBlock,
			LastIndexedTime:     lastIndexedTime,
			TotalBlobsIndexed:   counts.Confirmed + counts.Pending,
			PendingBlobsCount:   counts.Pending,
			ConfirmedBlobsCount: counts.Confirmed,
		})
	}

	a.respondSuccess(w, metrics)
}

// allowedTables is a whitelist of table names that can be queried in the DevDatabase handler.
// This prevents SQL injection by ensuring only known table names are used in queries.
var allowedTables = map[string]bool{
	"networks":         true,
	"blobs":            true,
	"blob_users":       true,
	"indexer_metadata": true,
	"indexed_blocks":   true,
}

// isAllowedTable checks whether a table name is in the whitelist of allowed tables.
func isAllowedTable(table string) bool {
	return allowedTables[table]
}

// DevDatabase godoc
// @Summary Get database statistics
// @Description Retrieve statistics about the database
// @Tags dev
// @Accept json
// @Produce json
// @Success 200 {object} Response{data=DatabaseStats} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 500 {object} Response "Internal server error"
// @Router /dev/database [get]
func (a *API) DevDatabase(w http.ResponseWriter, r *http.Request) {
	logger.Debug("Getting database statistics")

	// Get table statistics
	tables := []string{"blobs", "blob_users", "networks", "indexer_metadata"}
	tableStats := make([]TableStat, 0, len(tables))
	for _, table := range tables {
		// Validate the table name against the whitelist to prevent SQL injection
		if !isAllowedTable(table) {
			a.respondError(w, http.StatusBadRequest, fmt.Sprintf("Invalid table name: %s", table))
			return
		}

		// Get row count
		var rowCount int
		query := fmt.Sprintf("SELECT COUNT(*) FROM %s", table)
		if err := a.db.GetContext(r.Context(), &rowCount, query); err != nil {
			logger.Error("Failed to get row count",
				zap.String("table", table),
				zap.Error(err))
			continue
		}

		// Get table size
		var sizeBytes int64
		if err := a.db.GetContext(r.Context(), &sizeBytes, queryTableSize, table); err != nil {
			logger.Error("Failed to get table size",
				zap.String("table", table),
				zap.Error(err))
			sizeBytes = 0 // Fallback
		}

		// Get index count
		var indexCount int
		if err := a.db.GetContext(r.Context(), &indexCount, queryIndexCount, table); err != nil {
			logger.Error("Failed to get index count",
				zap.String("table", table),
				zap.Error(err))
			indexCount = 0 // Fallback
		}

		tableStats = append(tableStats, TableStat{
			TableName:    table,
			RowCount:     rowCount,
			SizeBytes:    sizeBytes,
			IndexCount:   indexCount,
			LastVacuumed: time.Now().Add(-24 * time.Hour), // Placeholder
		})
	}

	// Get total database size
	var totalSize int64
	if err := a.db.GetContext(r.Context(), &totalSize, queryDatabaseSize); err != nil {
		logger.Error("Failed to get database size", zap.Error(err))
		totalSize = 0 // Fallback
	}

	// Get connection statistics
	stats := a.db.Stats()

	// Create database stats response
	dbStats := DatabaseStats{
		TotalTables:        len(tableStats),
		TotalSize:          formatBytes(totalSize),
		TableStats:         tableStats,
		ConnectionCount:    stats.OpenConnections,
		IdleConnections:    stats.Idle,
		InUseConnections:   stats.InUse,
		MaxOpenConnections: stats.MaxOpenConnections,
		LastMigrationTime:  time.Now().Add(-7 * 24 * time.Hour), // Placeholder
	}

	a.respondSuccess(w, dbStats)
}

// formatBytes formats a byte count as a human-readable string
func formatBytes(numBytes int64) string {
	const unit = 1024
	if numBytes < unit {
		return fmt.Sprintf("%d B", numBytes)
	}
	div, exp := int64(unit), 0
	for n := numBytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(numBytes)/float64(div), "KMGTPE"[exp])
}

// DevLogs godoc
// @Summary Get recent logs
// @Description Retrieve recent application logs
// @Tags dev
// @Accept json
// @Produce json
// @Param limit query int false "Number of logs to return (default: 100)"
// @Param level query string false "Filter by log level (info, warn, error, debug)"
// @Success 200 {object} Response{data=[]LogEntry} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 500 {object} Response "Internal server error"
// @Router /dev/logs [get]
func (a *API) DevLogs(w http.ResponseWriter, r *http.Request) {
	logger.Debug("Getting recent logs")

	limit, _, err := a.parsePagination(r, 100)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	level := r.URL.Query().Get("level")
	if level != "" {
		switch level {
		case "debug", "info", "warn", "error":
		default:
			a.respondError(w, http.StatusBadRequest, "Invalid level parameter")
			return
		}
	}

	// Log ingestion is not wired to a persistent store yet; return an explicit empty set.
	logs := make([]LogEntry, 0, limit)
	if level != "" {
		logger.Debug("Dev log level filter requested without backing log store",
			zap.String("level", level))
	}
	a.respondSuccess(w, logs)
}

// DevQueries godoc
// @Summary Get database query statistics
// @Description Retrieve statistics about database queries
// @Tags dev
// @Accept json
// @Produce json
// @Param limit query int false "Number of queries to return (default: 20)"
// @Success 200 {object} Response{data=[]QueryStat} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 500 {object} Response "Internal server error"
// @Router /dev/queries [get]
func (a *API) DevQueries(w http.ResponseWriter, r *http.Request) {
	logger.Debug("Getting database query statistics")

	limit, _, err := a.parsePagination(r, 20)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	queries := make([]QueryStat, 0, limit)
	query := `
		SELECT
			query,
			mean_exec_time AS execution_time,
			calls,
			rows::int AS rows_returned,
			COALESCE(last_exec_time, NOW()) AS last_executed
		FROM pg_stat_statements
		ORDER BY mean_exec_time DESC
		LIMIT $1
	`
	if err := a.db.SelectContext(r.Context(), &queries, query, limit); err != nil {
		// pg_stat_statements may be unavailable in development/test DBs.
		logger.Warn("Failed to load pg_stat_statements data, returning empty query stats",
			zap.Error(err))
	}
	// Limit the number of queries
	if len(queries) > limit {
		queries = queries[:limit]
	}

	a.respondSuccess(w, queries)
}

// DevDashboard godoc
// @Summary Development dashboard
// @Description Access the development dashboard
// @Tags dev
// @Accept json
// @Produce json
// @Success 200 {object} Response{data=string} "Success"
// @Router /dev/dashboard [get]
func (a *API) DevDashboard(w http.ResponseWriter, r *http.Request) {
	logger.Debug("Accessing development dashboard")

	uptime := time.Since(a.startTime).Truncate(time.Second).String()
	resp := devDashboardResponse{
		CurrentTime:     time.Now(),
		EnabledNetworks: len(a.networks),
		TotalRequests:   atomic.LoadInt64(&a.totalRequests),
		ActiveRequests:  atomic.LoadInt64(&a.activeRequests),
		Uptime:          uptime,
	}
	a.respondSuccess(w, resp)
}
