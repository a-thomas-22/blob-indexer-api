package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

const MaxQueryLimit = 100

// DefaultPricingBlocks is the /blob/pricing window when no blocks param is
// given; out-of-range values below 1 also clamp here.
const DefaultPricingBlocks = 20

// MaxPricingBlocks caps the /blob/pricing blocks param. The frontend's 1h
// view requests 300 (a full mainnet hour at 12s slots — missed slots only
// shrink the block count, so 300 blocks always spans ≥ 1h of wall time); 512
// leaves headroom for shorter slot times. The query is a single backward
// range scan on the block_metrics primary key, so a larger cap is cheap.
const MaxPricingBlocks = 512

// MaxQueryOffset prevents very expensive deep pagination queries.
const MaxQueryOffset = 10000

const aggregateCacheTTL = 30 * time.Second
const aggregateQueryTimeout = 5 * time.Second
const apiWindow24h = "24h"

// statsCacheTTL keeps /stats and /stats/windows fresher than the chart and
// leaderboard caches: roughly one block of staleness.
const statsCacheTTL = 15 * time.Second

// mempoolPressureCacheTTL bounds mempool pressure staleness to under one block.
const mempoolPressureCacheTTL = 10 * time.Second

// pricingCacheTTL bounds the in-process cache and browser staleness for
// /blob/pricing, which is identical across users within a block.
const pricingCacheTTL = 5 * time.Second

// latestBlobsCacheTTL bounds the in-process cache and browser staleness of the
// hot /blob/latest list. Kept well under the ~12s block time, and clients also
// receive live updates over the WebSocket, so a few seconds of staleness on
// the polled list is fine while letting Cloudflare coalesce polling bursts.
const latestBlobsCacheTTL = 5 * time.Second

// mempoolBlobsCacheTTL bounds the in-process cache and browser staleness of
// the /blob/mempool list; the WebSocket feed carries the real-time diffs.
const mempoolBlobsCacheTTL = 3 * time.Second

// confirmedBlobCacheTTL caches a single confirmed blob lookup (/blob/{txHash}).
// A confirmed blob at a tx hash is effectively immutable; the moderate TTL still
// lets the entry self-heal after a (rare) reorg rather than being pinned.
const confirmedBlobCacheTTL = 60 * time.Second

// Edge (shared-cache) TTLs, sent as s-maxage so Cloudflare can serve the
// blob-flow polling herd from cache while browsers revalidate sooner.
// Staleness is additive across layers: the worst case is the SUM of
// in-process TTL + s-maxage + max-age, not the largest term. The hot
// block-cadence endpoints (latest/mempool/pricing) keep each term at or
// under one ~12s block, bounding their stacked worst case to a few blocks —
// and poller-driven invalidation plus ETag revalidation keep the typical
// case well below that. Slower-moving aggregates (stats/users/charts)
// deliberately trade more staleness for edge hit ratio.
const (
	latestBlobsEdgeTTL     = 5 * time.Second
	mempoolBlobsEdgeTTL    = 5 * time.Second
	pricingEdgeTTL         = 5 * time.Second
	mempoolPressureEdgeTTL = 10 * time.Second
	statsEdgeTTL           = 15 * time.Second
	aggregateEdgeTTL       = 30 * time.Second
	networkStatusEdgeTTL   = 10 * time.Second
	// confirmedBlobEdgeTTL matches the browser TTL: the 60s bound is what lets
	// a cached confirmed blob self-heal after a (rare) reorg, and that bound
	// has to hold at shared caches too or the edge pins the pre-reorg copy.
	confirmedBlobEdgeTTL = confirmedBlobCacheTTL
)

// networkStatusCacheTTL is the browser TTL for /networks and /status, which
// change at most once per block.
const networkStatusCacheTTL = 5 * time.Second

// setCacheControl marks a response as publicly cacheable so browsers (maxAge)
// and shared caches like Cloudflare (sMaxAge) can absorb the dashboard's
// identical concurrent requests. Cloudflare only honors these directives once
// a Cache Rule marks /api/v1/* GETs as eligible for cache.
//
// Deliberately NO stale-while-revalidate: blob-flow refetches the hot
// endpoints on block cadence (~12s), so any SWR window reaching past
// (poll interval - max-age) would make browsers serve every block-cadence
// refetch from the stale previous entry and only revalidate in the
// background — rendering the dashboard permanently one block behind.
func setCacheControl(w http.ResponseWriter, maxAge, sMaxAge time.Duration) {
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d, s-maxage=%d",
		int(maxAge.Seconds()), int(sMaxAge.Seconds())))
}

// dbQueryCanceled is the Postgres query_canceled error code, raised when the
// server-side statement timeout aborts a query.
const dbQueryCanceled = "57014"

// isDBTimeout reports whether err means the database query was cut off by a
// deadline — the request-scoped query timeout or the server statement timeout.
func isDBTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && string(pqErr.Code) == dbQueryCanceled
}

// respondAggregateError maps database timeouts to 503 with Retry-After so
// clients can tell overload from bugs; other errors remain generic 500s.
func (a *API) respondAggregateError(w http.ResponseWriter, err error, message string) {
	if isDBTimeout(err) {
		w.Header().Set("Retry-After", "5")
		a.respondError(w, http.StatusServiceUnavailable, message)
		return
	}
	a.respondError(w, http.StatusInternalServerError, message)
}

// aggregateWorkContext keeps request-scoped values but decouples shared
// singleflight work from one caller's cancellation.
func aggregateWorkContext(r *http.Request) context.Context {
	return context.WithoutCancel(r.Context())
}

// Response is the standard API response wrapper
type Response struct {
	Success bool        `json:"success"`
	Data    interface{} `json:"data,omitempty"`
	// Meta carries optional request-scoped metadata about how data was
	// resolved (e.g. the aggregation range echoed by /users). Handlers must
	// only derive it from the request URL, never from per-request state, so
	// cached copies of the response stay valid for every requester.
	Meta  interface{} `json:"meta,omitempty"`
	Error string      `json:"error,omitempty"`
	// ErrorCode is a stable, machine-readable identifier for the error
	// (e.g. "not_found", "rate_limited"). It is derived from the HTTP status
	// and the human-readable message, and is omitted on success responses.
	ErrorCode string `json:"error_code,omitempty"`
}

// respondJSON responds with JSON
func (a *API) respondJSON(w http.ResponseWriter, status int, data interface{}) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(data); err != nil {
		logger.Error("Failed to encode JSON response", zap.Error(err))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		// Hand-written to match the standard error envelope (success, error,
		// error_code) since the encoder just failed.
		_, _ = w.Write([]byte(`{"success":false,"error":"internal server error","error_code":"` + errCodeInternal + `"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// Stable, machine-readable error codes returned in Response.ErrorCode. These
// are part of the API contract: clients branch on them, so existing values
// must not change.
const (
	errCodeInvalidRequest     = "invalid_request"
	errCodeUnauthorized       = "unauthorized"
	errCodeForbidden          = "forbidden"
	errCodeNotFound           = "not_found"
	errCodeRateLimited        = "rate_limited"
	errCodeServiceUnavailable = "service_unavailable"
	errCodeInternal           = "internal_error"
	errCodeNetworkNotFound    = "network_not_found"
	errCodeGeneric            = "error"
)

// errorCodeFor derives a stable, machine-readable error code from the HTTP
// status and the human-readable message. The status provides the baseline
// code; a few message-keyed overrides surface common specifics that clients
// branch on. The returned code is intended to be stable across releases so
// callers can rely on it rather than parsing the human message.
func errorCodeFor(status int, message string) string {
	// Message-keyed overrides take precedence so specific errors keep a
	// distinct code even when they share a status with the generic case.
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "network not found"):
		return errCodeNetworkNotFound
	case strings.Contains(lower, "rate limit"):
		return errCodeRateLimited
	}

	switch status {
	case http.StatusBadRequest:
		return errCodeInvalidRequest
	case http.StatusUnauthorized:
		return errCodeUnauthorized
	case http.StatusForbidden:
		return errCodeForbidden
	case http.StatusNotFound:
		return errCodeNotFound
	case http.StatusTooManyRequests:
		return errCodeRateLimited
	case http.StatusServiceUnavailable:
		return errCodeServiceUnavailable
	case http.StatusInternalServerError:
		return errCodeInternal
	default:
		return errCodeGeneric
	}
}

// respondError responds with an error
func (a *API) respondError(w http.ResponseWriter, status int, message string) {
	logger.Warn("API error response",
		zap.Int("status", status),
		zap.String("message", message))
	a.respondJSON(w, status, Response{
		Success:   false,
		Error:     message,
		ErrorCode: errorCodeFor(status, message),
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
