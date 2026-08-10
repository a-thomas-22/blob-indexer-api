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
	userWindow1h  userWindowOption = "1h"
	userWindow24h userWindowOption = apiWindow24h
	userWindow7d  userWindowOption = "7d"
	userWindow30d userWindowOption = "30d"
	userWindowAll userWindowOption = "all"
)

type userGroupOption string

const (
	// userGroupAddress is the historical one-row-per-sender-address mode.
	userGroupAddress userGroupOption = "address"
	// userGroupEntity collapses attributed addresses into one row per
	// attribution entity; unattributed addresses stay individual rows.
	userGroupEntity userGroupOption = "entity"
)

// UserResponse is a response containing user data
type UserResponse struct {
	ChainID     int    `json:"chain_id"`
	NetworkName string `json:"network_name,omitempty"`
	// Address is the sender address; in entity-grouped mode (group=entity)
	// it is the group's primary (busiest) member address.
	Address  string `json:"address"`
	Name     string `json:"name,omitempty"`
	Category string `json:"category,omitempty"`
	// Key identifies the row in entity-grouped mode: the entity slug for
	// attributed rows (matching /charts/attribution-usage series keys) or the
	// sender address for unattributed rows. Omitted in per-address mode.
	Key string `json:"key,omitempty" example:"scroll"`
	// Addresses lists the group's member addresses, busiest first, in
	// entity-grouped mode; Address is its first element. Omitted in
	// per-address mode.
	Addresses []string `json:"addresses,omitempty"`
	BlobCount int      `json:"blob_count"`
	// Total realized blob base-fee cost in wei, serialized as a decimal string.
	TotalCostWei      string    `json:"total_cost_wei" example:"4718548746240"`
	LastTimestamp     time.Time `json:"last_timestamp"`
	BlobSharePercent  float64   `json:"blob_share_percent,omitempty"`
	SpendSharePercent float64   `json:"spend_share_percent,omitempty"`
}

// CategoryShareResponse is a category-level market share bucket.
type CategoryShareResponse struct {
	Category  string `json:"category"`
	BlobCount int    `json:"blob_count"`
	// Total realized blob base-fee cost in wei, serialized as a decimal string.
	TotalCostWei      string  `json:"total_cost_wei" example:"4718548746240"`
	BlobSharePercent  float64 `json:"blob_share_percent"`
	SpendSharePercent float64 `json:"spend_share_percent"`
}

// UserBreakdownResponse is a response containing category market share data.
type UserBreakdownResponse struct {
	ChainID        int                     `json:"chain_id"`
	NetworkName    string                  `json:"network_name,omitempty"`
	Window         string                  `json:"window"`
	CategoryShares []CategoryShareResponse `json:"category_shares"`
}

func toUserResponse(user models.BlobUserStats, networkID int, networkName string) UserResponse {
	return UserResponse{
		ChainID:           networkID,
		NetworkName:       networkName,
		Address:           user.Address,
		Name:              user.Name,
		Category:          user.Category,
		BlobCount:         user.BlobCount,
		TotalCostWei:      user.TotalCostWei,
		LastTimestamp:     user.LastTimestamp,
		BlobSharePercent:  user.BlobSharePercent,
		SpendSharePercent: user.SpendSharePercent,
	}
}

// toGroupedUserResponse maps one entity-grouped leaderboard row. Address is
// the group's primary (busiest) member — the grouped queries order the
// addresses array busiest first — so frontends can keep linking rows to
// per-address user pages.
func toGroupedUserResponse(group models.BlobUserGroupStats, networkID int, networkName string) UserResponse {
	primary := ""
	if len(group.Addresses) > 0 {
		primary = group.Addresses[0]
	}
	return UserResponse{
		ChainID:           networkID,
		NetworkName:       networkName,
		Address:           primary,
		Name:              group.Name,
		Category:          group.Category,
		Key:               group.Key,
		Addresses:         []string(group.Addresses),
		BlobCount:         group.BlobCount,
		TotalCostWei:      group.TotalCostWei,
		LastTimestamp:     group.LastTimestamp,
		BlobSharePercent:  group.BlobSharePercent,
		SpendSharePercent: group.SpendSharePercent,
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

// usersMeta echoes request-resolved response semantics: the aggregation
// window on requests that named one (so clients can confirm which window the
// returned rows describe) and the grouping mode on entity-grouped requests
// (whose rows carry the grouped shape). Both fields derive only from the URL,
// so cached copies stay valid for every requester.
type usersMeta struct {
	Range string `json:"range,omitempty"`
	Group string `json:"group,omitempty"`
}

// parseUserGroupOption resolves the row-grouping mode from the `group` query
// param. Omitted (or an explicit `address`) keeps the historical per-address
// rows; `entity` selects the entity-grouped leaderboard. explicit follows the
// parseUserRangeOption contract: only requests that named a mode get it
// echoed under meta, so omitted-group responses stay byte-identical to the
// historical shape.
func parseUserGroupOption(r *http.Request) (group userGroupOption, explicit bool, err error) {
	value := strings.ToLower(r.URL.Query().Get("group"))
	if value == "" {
		return userGroupAddress, false, nil
	}
	switch userGroupOption(value) {
	case userGroupAddress, userGroupEntity:
		return userGroupOption(value), true, nil
	default:
		return "", false, fmt.Errorf("invalid group parameter")
	}
}

// parseUserRangeOption resolves the aggregation window from the canonical
// `range` query param or its legacy `window` alias. explicit reports whether
// the client named a window at all: omitted requests keep the historical
// default (all-history) with a byte-identical response shape, while explicit
// requests additionally get the resolved window echoed under meta. Conflicting
// values across the two params are rejected rather than silently picking one,
// so a client never renders data labeled with the wrong window.
func parseUserRangeOption(r *http.Request) (window userWindowOption, explicit bool, err error) {
	rangeValue := strings.ToLower(r.URL.Query().Get("range"))
	windowValue := strings.ToLower(r.URL.Query().Get("window"))
	if rangeValue != "" && windowValue != "" && rangeValue != windowValue {
		return "", false, fmt.Errorf("conflicting range and window parameters")
	}
	param, value := "range", rangeValue
	if value == "" {
		param, value = "window", windowValue
	}
	if value == "" {
		return userWindowAll, false, nil
	}
	switch userWindowOption(value) {
	case userWindow1h, userWindow24h, userWindow7d, userWindow30d, userWindowAll:
		return userWindowOption(value), true, nil
	default:
		return "", false, fmt.Errorf("invalid %s parameter", param)
	}
}

// GetTopBlobUsers godoc
// @Summary Get top blob users
// @Description Retrieve the top users of blob transactions by count or spend, optionally scoped to a recent window. With group=entity, attributed addresses are merged into one row per attribution entity (unattributed addresses stay individual rows): totals and shares are summed per group, last_timestamp is the group maximum, rows gain key and addresses (busiest first), address is the busiest member, and ordering plus limit/offset apply after grouping.
// @Tags users
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Param limit query int false "Number of rows to return (default: 10, max: 100)"
// @Param offset query int false "Number of rows to skip for pagination (default: 0, max: 10000)"
// @Param sort query string false "Sort rows by count or spend (default: count)" Enums(count, spend)
// @Param range query string false "Time range to aggregate; echoed back in meta.range (default: all)" Enums(1h, 24h, 7d, 30d, all)
// @Param window query string false "Deprecated alias for range" Enums(1h, 24h, 7d, 30d, all)
// @Param group query string false "Row grouping; entity merges attributed addresses into one row per attribution entity and is echoed back in meta.group (default: address)" Enums(address, entity)
// @Success 200 {object} Response{data=[]UserResponse} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 500 {object} Response "Internal server error"
// @Failure 503 {object} Response "Database overloaded; retry later"
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
// @Param range query string false "Time range to aggregate; echoed back in meta.range (default: all)" Enums(1h, 24h, 7d, 30d, all)
// @Param window query string false "Deprecated alias for range" Enums(1h, 24h, 7d, 30d, all)
// @Success 200 {object} Response{data=[]UserResponse} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 500 {object} Response "Internal server error"
// @Failure 503 {object} Response "Database overloaded; retry later"
// @Router /users/unattributed [get]
func (a *API) GetTopUnattributedBlobUsers(w http.ResponseWriter, r *http.Request) {
	a.getTopBlobUsers(w, r, true)
}

// topBlobUsersAllQuery selects the all-history top-user query variant whose
// static ORDER BY matches the requested sort.
func topBlobUsersAllQuery(sort userSortOption, unattributedOnly bool) string {
	if unattributedOnly {
		if sort == userSortSpend {
			return queryTopUnattributedBlobUsersAllBySpend
		}
		return queryTopUnattributedBlobUsersAllByCount
	}
	if sort == userSortSpend {
		return queryTopBlobUsersAllBySpend
	}
	return queryTopBlobUsersAllByCount
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

	window, explicitWindow, err := parseUserRangeOption(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	group, explicitGroup, err := parseUserGroupOption(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	// /users/unattributed rows are unattributed by definition, so entity
	// grouping cannot merge anything there; reject rather than silently
	// serving per-address rows under a grouped label.
	if unattributedOnly && group == userGroupEntity {
		a.respondError(w, http.StatusBadRequest, "group=entity is not supported for unattributed users")
		return
	}
	grouped := group == userGroupEntity

	// Echo the resolved window and grouping mode only when the client named
	// them: omitted-param responses stay byte-identical to the historical
	// shape, and the meta is constant per URL so every cache layer stays
	// coherent.
	var meta interface{}
	if explicitWindow || explicitGroup {
		m := usersMeta{}
		if explicitWindow {
			m.Range = string(window)
		}
		if explicitGroup {
			m.Group = string(group)
		}
		meta = m
	}

	logMessage := "Getting top blob users"
	errMessage := "Failed to get top blob users"
	returnMessage := "Returning top blob users"
	query := queryTopBlobUsersWithOptions
	if window == userWindowAll {
		query = topBlobUsersAllQuery(sort, false)
	}
	cacheKey := fmt.Sprintf("%d:%d:%d:%s:%s", network.ChainID, limit, offset, sort, window)
	if unattributedOnly {
		logMessage = "Getting top unattributed blob users"
		errMessage = "Failed to get top unattributed blob users"
		returnMessage = "Returning top unattributed blob users"
		query = queryTopUnattributedBlobUsersWithOptions
		if window == userWindowAll {
			query = topBlobUsersAllQuery(sort, true)
		}
		cacheKey = fmt.Sprintf("unattributed:%d:%d:%d:%s:%s", network.ChainID, limit, offset, sort, window)
	}
	if grouped {
		logMessage = "Getting top blob user groups"
		errMessage = "Failed to get top blob user groups"
		returnMessage = "Returning top blob user groups"
		query = queryTopBlobUserGroupsWithOptions
		if window == userWindowAll {
			query = queryTopBlobUserGroupsAll
		}
		cacheKey = fmt.Sprintf("entity:%d:%d:%d:%s:%s", network.ChainID, limit, offset, sort, window)
	}

	// The windowed and grouped queries take the sort as a parameter; the
	// all-history per-address variants encode it statically in the query text
	// (see queries.go) and so take one fewer argument.
	queryArgs := []interface{}{network.ChainID, limit, offset, string(window), string(sort)}
	if window == userWindowAll && !grouped {
		queryArgs = []interface{}{network.ChainID, limit, offset, string(window)}
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
		setCacheControl(w, aggregateCacheTTL, aggregateEdgeTTL)
		a.respondJSON(w, http.StatusOK, Response{
			Success: true,
			Data:    cached.response,
			Meta:    meta,
		})
		return
	}
	a.cacheMu.RUnlock()

	value, err, _ := a.aggregateGroup.Do("top_users:"+cacheKey, func() (interface{}, error) {
		a.cacheMu.RLock()
		if cached, ok := a.topUsersCache[cacheKey]; ok && time.Now().Before(cached.expiresAt) {
			a.cacheMu.RUnlock()
			return cached.response, nil
		}
		a.cacheMu.RUnlock()

		queryCtx, cancel := context.WithTimeout(aggregateWorkContext(r), aggregateQueryTimeout)
		defer cancel()

		var response []UserResponse
		if grouped {
			var groups []models.BlobUserGroupStats
			if err := a.db.SelectContext(queryCtx, &groups, query, queryArgs...); err != nil {
				return nil, err
			}
			response = make([]UserResponse, 0, len(groups))
			for _, userGroup := range groups {
				response = append(response, toGroupedUserResponse(userGroup, network.ChainID, network.Name))
			}
		} else {
			var users []models.BlobUserStats
			if err := a.db.SelectContext(queryCtx, &users, query, queryArgs...); err != nil {
				return nil, err
			}
			response = make([]UserResponse, 0, len(users))
			for _, user := range users {
				response = append(response, toUserResponse(user, network.ChainID, network.Name))
			}
		}

		a.cacheMu.Lock()
		a.topUsersCache[cacheKey] = topUsersCacheEntry{
			response:  response,
			expiresAt: time.Now().Add(aggregateCacheTTL),
		}
		a.cacheMu.Unlock()

		return response, nil
	})
	if err != nil {
		logger.Error(errMessage,
			zap.String("network", network.Name),
			zap.String("sort", string(sort)),
			zap.String("window", string(window)),
			zap.Error(err))
		a.respondAggregateError(w, err, errMessage)
		return
	}

	// The singleflight closure above always returns []UserResponse or an error,
	// so the assertion's ok value can never be false here.
	response, _ := value.([]UserResponse)

	setCacheControl(w, aggregateCacheTTL, aggregateEdgeTTL)
	logger.Debug(returnMessage,
		zap.String("network", network.Name),
		zap.String("sort", string(sort)),
		zap.String("window", string(window)),
		zap.Int("count", len(response)))
	a.respondJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    response,
		Meta:    meta,
	})
}

// GetUserBreakdown godoc
// @Summary Get blob user market breakdowns
// @Description Retrieve category market share for blob senders, optionally scoped to a recent window
// @Tags users
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Param range query string false "Time range to aggregate; echoed back in meta.range (default: all)" Enums(1h, 24h, 7d, 30d, all)
// @Param window query string false "Deprecated alias for range" Enums(1h, 24h, 7d, 30d, all)
// @Success 200 {object} Response{data=UserBreakdownResponse} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 500 {object} Response "Internal server error"
// @Failure 503 {object} Response "Database overloaded; retry later"
// @Router /users/breakdown [get]
func (a *API) GetUserBreakdown(w http.ResponseWriter, r *http.Request) {
	network, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	window, explicitWindow, err := parseUserRangeOption(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Same explicit-only meta echo as getTopBlobUsers, so the range contract
	// is uniform across the /users endpoints.
	var meta interface{}
	if explicitWindow {
		meta = usersMeta{Range: string(window)}
	}

	logger.Debug("Getting blob user breakdown",
		zap.String("network", network.Name),
		zap.String("window", string(window)))

	cacheKey := fmt.Sprintf("breakdown:%d:%s", network.ChainID, window)
	a.cacheMu.RLock()
	if cached, ok := a.breakdownCache[cacheKey]; ok && time.Now().Before(cached.expiresAt) {
		a.cacheMu.RUnlock()
		setCacheControl(w, aggregateCacheTTL, aggregateEdgeTTL)
		a.respondJSON(w, http.StatusOK, Response{
			Success: true,
			Data:    cached.response,
			Meta:    meta,
		})
		return
	}
	a.cacheMu.RUnlock()

	value, err, _ := a.aggregateGroup.Do(cacheKey, func() (interface{}, error) {
		a.cacheMu.RLock()
		if cached, ok := a.breakdownCache[cacheKey]; ok && time.Now().Before(cached.expiresAt) {
			a.cacheMu.RUnlock()
			return cached.response, nil
		}
		a.cacheMu.RUnlock()

		queryCtx, cancel := context.WithTimeout(aggregateWorkContext(r), aggregateQueryTimeout)
		defer cancel()

		var categories []models.BlobUserCategoryShare
		query := queryBlobUserCategoryBreakdown
		if window == userWindowAll {
			query = queryBlobUserCategoryBreakdownAll
		}
		if err := a.db.SelectContext(queryCtx, &categories, query, network.ChainID, string(window)); err != nil {
			return UserBreakdownResponse{}, err
		}

		categoryShares := make([]CategoryShareResponse, 0, len(categories))
		for _, category := range categories {
			categoryShares = append(categoryShares, CategoryShareResponse{
				Category:          category.Category,
				BlobCount:         category.BlobCount,
				TotalCostWei:      category.TotalCostWei,
				BlobSharePercent:  category.BlobSharePercent,
				SpendSharePercent: category.SpendSharePercent,
			})
		}

		response := UserBreakdownResponse{
			ChainID:        network.ChainID,
			NetworkName:    network.Name,
			Window:         string(window),
			CategoryShares: categoryShares,
		}

		a.cacheMu.Lock()
		a.breakdownCache[cacheKey] = userBreakdownCacheEntry{
			response:  response,
			expiresAt: time.Now().Add(aggregateCacheTTL),
		}
		a.cacheMu.Unlock()

		return response, nil
	})
	if err != nil {
		logger.Error("Failed to get blob user breakdown",
			zap.String("network", network.Name),
			zap.String("window", string(window)),
			zap.Error(err))
		a.respondAggregateError(w, err, "Failed to get user breakdown")
		return
	}

	// The singleflight closure above always returns UserBreakdownResponse or an error,
	// so the assertion's ok value can never be false here.
	response, _ := value.(UserBreakdownResponse)

	setCacheControl(w, aggregateCacheTTL, aggregateEdgeTTL)
	a.respondJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    response,
		Meta:    meta,
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
// @Failure 503 {object} Response "Database overloaded; retry later"
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
		a.respondAggregateError(w, err, "Failed to get user")
		return
	}

	setCacheControl(w, aggregateCacheTTL, aggregateEdgeTTL)
	a.respondSuccess(w, toUserResponse(user, network.ChainID, network.Name))
}
