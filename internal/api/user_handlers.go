package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

type userSortOption string

const (
	userSortCount userSortOption = "count"
	userSortSpend userSortOption = "spend"
)

type userWindowOption string

const (
	userWindow24h userWindowOption = apiWindow24h
	userWindow7d  userWindowOption = "7d"
	userWindowAll userWindowOption = "all"
)

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
	a.getTopBlobUsers(w, r, false)
}

// GetTopUnattributedBlobUsers godoc
// @Summary Get top unattributed blob users
// @Description Retrieve the top unattributed blob transaction senders by count or spend, optionally scoped to a recent window
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
// @Router /users/unattributed [get]
func (a *API) GetTopUnattributedBlobUsers(w http.ResponseWriter, r *http.Request) {
	a.getTopBlobUsers(w, r, true)
}

func (a *API) getTopBlobUsers(w http.ResponseWriter, r *http.Request, unattributedOnly bool) {
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

	logMessage := "Getting top blob users"
	errMessage := "Failed to get top blob users"
	returnMessage := "Returning top blob users"
	query := queryTopBlobUsersWithOptions
	cacheKey := fmt.Sprintf("%d:%d:%d:%s:%s", network.ChainID, limit, offset, sort, window)
	if unattributedOnly {
		logMessage = "Getting top unattributed blob users"
		errMessage = "Failed to get top unattributed blob users"
		returnMessage = "Returning top unattributed blob users"
		query = queryTopUnattributedBlobUsersWithOptions
		cacheKey = fmt.Sprintf("unattributed:%d:%d:%d:%s:%s", network.ChainID, limit, offset, sort, window)
	}

	logger.Debug(logMessage,
		zap.String("network", network.Name),
		zap.Int("limit", limit),
		zap.Int("offset", offset),
		zap.String("sort", string(sort)),
		zap.String("window", string(window)))

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
	if err := a.db.SelectContext(queryCtx, &users, query, network.ChainID, limit, offset, string(window), string(sort)); err != nil {
		logger.Error(errMessage,
			zap.String("network", network.Name),
			zap.String("sort", string(sort)),
			zap.String("window", string(window)),
			zap.Error(err))
		a.respondError(w, http.StatusInternalServerError, errMessage)
		return
	}

	response := make([]UserResponse, 0, len(users))
	for _, user := range users {
		response = append(response, toUserResponse(user, network.ChainID, network.Name))
	}

	logger.Debug(returnMessage,
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
