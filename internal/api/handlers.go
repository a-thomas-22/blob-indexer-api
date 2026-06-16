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

// pricingCacheMaxAge is the client/CDN cache hint for /blob/pricing, which is
// cheap enough to serve uncached but identical across users within a block.
const pricingCacheMaxAge = 10 * time.Second

// setCacheControl marks a response as publicly cacheable for ttl so browsers
// and CDNs can absorb the dashboard's identical concurrent requests.
func setCacheControl(w http.ResponseWriter, ttl time.Duration) {
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(ttl.Seconds())))
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
	Error   string      `json:"error,omitempty"`
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
