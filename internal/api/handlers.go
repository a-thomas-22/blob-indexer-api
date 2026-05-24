package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

const MaxQueryLimit = 100

// MaxQueryOffset prevents very expensive deep pagination queries.
const MaxQueryOffset = 10000

const aggregateCacheTTL = 30 * time.Second
const aggregateQueryTimeout = 5 * time.Second
const apiWindow24h = "24h"

// Response is the standard API response wrapper
type Response struct {
	Success bool        `json:"success"`
	Data    interface{} `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
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
