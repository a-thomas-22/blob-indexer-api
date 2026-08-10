package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

// Entity keys are slugs of attribution display names, so two labels appearing
// on the attribution chart are grouping buckets rather than entities:
// 'unknown' aggregates unattributed senders and 'other' aggregates
// below-cutoff series. Neither resolves to an address set, so both are
// rejected as entity keys.
const (
	entityKeyUnknown = "unknown"
	entityKeyOther   = "other"
)

// errEntityNotFound signals that an entity key matched no attributed or
// registry-listed address. The detail endpoint deliberately does not cache
// this outcome — it sits behind the aggregate rate limit, so a brand-new
// entity can resolve as soon as its first attribution lands.
var errEntityNotFound = errors.New("entity not found")

var entityKeyNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// slugifyEntityKey is the Go mirror of entityKeySQL: it canonicalizes
// free-form input — an entity key or a display name, in any casing — to the
// entity key shared by /charts/attribution-usage, /users, and /entities.
// Returns "" when the input contains no alphanumerics.
func slugifyEntityKey(input string) string {
	slug := entityKeyNonAlnum.ReplaceAllString(strings.ToLower(input), "_")
	return strings.Trim(slug, "_")
}

// entityDetailRow is one per-address row of the entity detail queries. The
// entity_-prefixed columns carry the entity-level aggregates and are
// identical on every row (window functions over the address set).
type entityDetailRow struct {
	Address             string       `db:"address"`
	DisplayName         string       `db:"display_name"`
	Category            string       `db:"category"`
	BlobCount           int64        `db:"blob_count"`
	TotalCostWei        string       `db:"total_cost_wei"`
	LastTimestamp       sql.NullTime `db:"last_timestamp"`
	InRegistry          bool         `db:"in_registry"`
	EntityName          string       `db:"entity_name"`
	EntityCategory      string       `db:"entity_category"`
	EntityBlobCount     int64        `db:"entity_blob_count"`
	EntityTotalCostWei  string       `db:"entity_total_cost_wei"`
	EntityLastTimestamp sql.NullTime `db:"entity_last_timestamp"`
	BlobSharePercent    float64      `db:"blob_share_percent"`
	SpendSharePercent   float64      `db:"spend_share_percent"`
}

// EntityAddressResponse is one sender address of an attributed entity.
type EntityAddressResponse struct {
	Address   string `json:"address"`
	BlobCount int64  `json:"blob_count"`
	// Total realized blob base-fee cost in wei, serialized as a decimal string.
	TotalCostWei string `json:"total_cost_wei" example:"4718548746240"`
	TotalCostEth string `json:"total_cost_eth" example:"0.00000471854874624"`
	// LastTimestamp is the address's most recent confirmed blob; omitted for
	// registry addresses with no indexed activity.
	LastTimestamp *time.Time `json:"last_timestamp,omitempty"`
	// InRegistry reports whether the attribution registry currently maps this
	// address to the entity; false means the address is included through
	// indexed historical attribution only.
	InRegistry bool `json:"in_registry"`
}

// EntityResponse is the detail view of one attributed entity: its metadata,
// aggregates over the requested range, and the per-address breakdown
// (busiest first). The key joins against the /charts/attribution-usage
// summary shares and the /users entity grouping.
type EntityResponse struct {
	ChainID     int    `json:"chain_id"`
	NetworkName string `json:"network_name,omitempty"`
	Key         string `json:"key"`
	Name        string `json:"name"`
	Category    string `json:"category"`
	// Range is the resolved aggregation window (default: all).
	Range     string `json:"range"`
	BlobCount int64  `json:"blob_count"`
	// Total realized blob base-fee cost in wei, serialized as a decimal string.
	TotalCostWei string `json:"total_cost_wei" example:"4718548746240"`
	TotalCostEth string `json:"total_cost_eth" example:"0.00000471854874624"`
	// LastTimestamp is the most recent confirmed blob across the entity's
	// addresses; omitted when the entity has no indexed activity.
	LastTimestamp     *time.Time              `json:"last_timestamp,omitempty"`
	BlobSharePercent  float64                 `json:"blob_share_percent"`
	SpendSharePercent float64                 `json:"spend_share_percent"`
	Addresses         []EntityAddressResponse `json:"addresses"`
}

func buildEntityResponse(chainID int, networkName, key, window string, rows []entityDetailRow) EntityResponse {
	head := rows[0]
	entityCost := nonEmptyDecimal(head.EntityTotalCostWei)
	response := EntityResponse{
		ChainID:           chainID,
		NetworkName:       networkName,
		Key:               key,
		Name:              head.EntityName,
		Category:          head.EntityCategory,
		Range:             window,
		BlobCount:         head.EntityBlobCount,
		TotalCostWei:      entityCost,
		TotalCostEth:      formatWeiAsETH(entityCost),
		LastTimestamp:     nullTimePtr(head.EntityLastTimestamp),
		BlobSharePercent:  head.BlobSharePercent,
		SpendSharePercent: head.SpendSharePercent,
		Addresses:         make([]EntityAddressResponse, 0, len(rows)),
	}
	for _, row := range rows {
		cost := nonEmptyDecimal(row.TotalCostWei)
		response.Addresses = append(response.Addresses, EntityAddressResponse{
			Address:       row.Address,
			BlobCount:     row.BlobCount,
			TotalCostWei:  cost,
			TotalCostEth:  formatWeiAsETH(cost),
			LastTimestamp: nullTimePtr(row.LastTimestamp),
			InRegistry:    row.InRegistry,
		})
	}
	return response
}

// GetEntityByKey godoc
// @Summary Get attributed entity detail
// @Description Retrieve one attributed entity (e.g. a rollup) by its stable entity key: metadata, aggregates over the requested range, and the per-address breakdown, busiest address first. The key is the same identifier used by the /charts/attribution-usage summary shares and the /users entity grouping; a display name (case-insensitive, e.g. "Arbitrum One") is also accepted and resolves to its key. Registry addresses with no indexed activity are included with zero counts; in_registry=false marks addresses attributed in indexed history but no longer listed by the registry.
// @Tags entities
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Param key path string true "Entity key (or display name) as used by /charts/attribution-usage summary shares"
// @Param range query string false "Time range to aggregate; echoed back as range (default: all)" Enums(1h, 24h, 7d, 30d, all)
// @Param window query string false "Deprecated alias for range" Enums(1h, 24h, 7d, 30d, all)
// @Success 200 {object} Response{data=EntityResponse} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 404 {object} Response "Entity not found"
// @Failure 500 {object} Response "Internal server error"
// @Failure 503 {object} Response "Database overloaded; retry later"
// @Router /entities/{key} [get]
func (a *API) GetEntityByKey(w http.ResponseWriter, r *http.Request) {
	network, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	rawKey := chi.URLParam(r, "key")
	if unescaped, unescapeErr := url.PathUnescape(rawKey); unescapeErr == nil {
		rawKey = unescaped
	}
	key := slugifyEntityKey(rawKey)
	if key == "" {
		a.respondError(w, http.StatusBadRequest, "Invalid entity key")
		return
	}
	if key == entityKeyUnknown || key == entityKeyOther {
		a.respondError(w, http.StatusNotFound, "Entity not found")
		return
	}

	window, _, err := parseUserRangeOption(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	logger.Debug("Getting entity detail",
		zap.String("network", network.Name),
		zap.String("key", key),
		zap.String("window", string(window)))

	cacheKey := fmt.Sprintf("entity:%d:%s:%s", network.ChainID, key, window)

	a.cacheMu.RLock()
	if cached, ok := a.entityCache[cacheKey]; ok && time.Now().Before(cached.expiresAt) {
		a.cacheMu.RUnlock()
		setCacheControl(w, aggregateCacheTTL, aggregateEdgeTTL)
		a.respondSuccess(w, cached.response)
		return
	}
	a.cacheMu.RUnlock()

	value, err, _ := a.aggregateGroup.Do(cacheKey, func() (interface{}, error) {
		a.cacheMu.RLock()
		if cached, ok := a.entityCache[cacheKey]; ok && time.Now().Before(cached.expiresAt) {
			a.cacheMu.RUnlock()
			return cached.response, nil
		}
		a.cacheMu.RUnlock()

		query := queryEntityDetailWindowed
		if window == userWindowAll {
			query = queryEntityDetailAll
		}

		queryCtx, cancel := context.WithTimeout(aggregateWorkContext(r), aggregateQueryTimeout)
		defer cancel()
		var rows []entityDetailRow
		if err := a.db.SelectContext(queryCtx, &rows, query, network.ChainID, key, string(window)); err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return nil, errEntityNotFound
		}

		response := buildEntityResponse(network.ChainID, network.Name, key, string(window), rows)

		a.cacheMu.Lock()
		a.entityCache[cacheKey] = entityCacheEntry{
			response:  response,
			expiresAt: time.Now().Add(aggregateCacheTTL),
		}
		a.cacheMu.Unlock()
		return response, nil
	})
	if err != nil {
		if errors.Is(err, errEntityNotFound) {
			a.respondError(w, http.StatusNotFound, "Entity not found")
			return
		}
		logger.Error("Failed to get entity detail",
			zap.String("network", network.Name),
			zap.String("key", key),
			zap.String("window", string(window)),
			zap.Error(err))
		a.respondAggregateError(w, err, "Failed to get entity")
		return
	}

	// The singleflight closure above always returns EntityResponse or an error,
	// so the assertion's ok value can never be false here.
	response, _ := value.(EntityResponse)
	setCacheControl(w, aggregateCacheTTL, aggregateEdgeTTL)
	a.respondSuccess(w, response)
}

// resolveEntityAddresses resolves an entity key to its stored-form sender
// addresses for the entity-filtered blob listings, through a short in-process
// TTL cache plus singleflight (the listings poll on block cadence). Unlike
// the detail endpoint's 404s, empty results ARE cached: the listings take
// only the standard per-IP rate limit, so repeated unknown-key requests must
// not re-run the resolution scan each time. The trade is that a brand-new
// entity's listings can 404 for up to one TTL after its first attribution.
//
// Known staleness edge: a registry-listed address with no confirmed activity
// resolves in its lowercase registry form, which the confirmed-blob union
// matches case-sensitively against the stored EIP-55 form. Its first
// confirmed blobs therefore appear in entity listings only once the cached
// entry expires and resolution returns the stats-stored form (≤ one TTL).
func (a *API) resolveEntityAddresses(r *http.Request, chainID int, key string) ([]string, error) {
	cacheKey := fmt.Sprintf("entity_addrs:%d:%s", chainID, key)

	a.cacheMu.RLock()
	if cached, ok := a.entityAddrCache[cacheKey]; ok && time.Now().Before(cached.expiresAt) {
		a.cacheMu.RUnlock()
		return cached.addresses, nil
	}
	a.cacheMu.RUnlock()

	value, err, _ := a.aggregateGroup.Do(cacheKey, func() (interface{}, error) {
		a.cacheMu.RLock()
		if cached, ok := a.entityAddrCache[cacheKey]; ok && time.Now().Before(cached.expiresAt) {
			a.cacheMu.RUnlock()
			return cached.addresses, nil
		}
		a.cacheMu.RUnlock()

		queryCtx, cancel := context.WithTimeout(aggregateWorkContext(r), aggregateQueryTimeout)
		defer cancel()
		var addresses []string
		if err := a.db.SelectContext(queryCtx, &addresses, queryEntityAddresses, chainID, key); err != nil {
			return nil, err
		}
		a.cacheMu.Lock()
		a.entityAddrCache[cacheKey] = entityAddrCacheEntry{
			addresses: addresses,
			expiresAt: time.Now().Add(aggregateCacheTTL),
		}
		a.cacheMu.Unlock()
		return addresses, nil
	})
	if err != nil {
		return nil, err
	}

	// The singleflight closure above always returns []string or an error, so
	// the assertion's ok value can never be false here.
	addresses, _ := value.([]string)
	return addresses, nil
}
