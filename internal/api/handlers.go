package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// MaxQueryLimit is the maximum number of results any endpoint will return
const MaxQueryLimit = 100

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
}

// UserResponse is a response containing user data
type UserResponse struct {
	NetworkID     int       `json:"network_id"`
	NetworkName   string    `json:"network_name,omitempty"`
	Address       string    `json:"address"`
	Name          string    `json:"name,omitempty"`
	BlobCount     int       `json:"blob_count"`
	TotalCostETH  string    `json:"total_cost_eth"`
	LastTimestamp time.Time `json:"last_timestamp"`
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

// ReindexRequest is a request to reindex blocks
type ReindexRequest struct {
	NetworkID  int    `json:"network_id"`
	StartBlock uint64 `json:"start_block"`
	EndBlock   uint64 `json:"end_block"`
}

// respondJSON responds with JSON
func (a *API) respondJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		logger.Error("Failed to encode JSON response", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
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

// GetLatestBlobs godoc
// @Summary Get latest confirmed blobs
// @Description Retrieve the latest confirmed blob transactions from the blockchain
// @Tags blobs
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Param limit query int false "Number of blobs to return (default: 10)"
// @Success 200 {object} Response{data=[]BlobResponse} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 500 {object} Response "Internal server error"
// @Router /blob/latest [get]
func (a *API) GetLatestBlobs(w http.ResponseWriter, r *http.Request) {
	// Get the network from the request
	idx, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Get network info
	network := idx.GetNetworkInfo()

	// Get query parameters
	limit := 10
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		var err error
		limit, err = strconv.Atoi(limitStr)
		if err != nil || limit <= 0 {
			a.respondError(w, http.StatusBadRequest, "Invalid limit parameter")
			return
		}
	}
	if limit > MaxQueryLimit {
		limit = MaxQueryLimit
	}

	logger.Debug("Getting latest blobs",
		zap.String("network", network.Name),
		zap.Int("limit", limit))

	// Get the latest blobs
	var blobs []models.Blob
	query := `
		SELECT * FROM blobs
		WHERE confirmed = true AND network_id = $1
		ORDER BY block_number DESC, blob_index ASC
		LIMIT $2
	`
	if err := a.db.SelectContext(r.Context(), &blobs, query, network.ChainID, limit); err != nil {
		logger.Error("Failed to get latest blobs",
			zap.String("network", network.Name),
			zap.Error(err))
		a.respondError(w, http.StatusInternalServerError, "Failed to get latest blobs")
		return
	}

	// Convert to response format
	var response []BlobResponse
	for _, blob := range blobs {
		response = append(response, BlobResponse{
			NetworkID:         blob.NetworkID,
			NetworkName:       network.Name,
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
		})
	}

	logger.Debug("Returning latest blobs",
		zap.String("network", network.Name),
		zap.Int("count", len(response)))
	a.respondJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    response,
	})
}

// GetMempoolBlobs godoc
// @Summary Get pending blobs from mempool
// @Description Retrieve pending blob transactions from the mempool
// @Tags blobs
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Param limit query int false "Number of blobs to return (default: 10)"
// @Success 200 {object} Response{data=[]BlobResponse} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 500 {object} Response "Internal server error"
// @Router /blob/mempool [get]
func (a *API) GetMempoolBlobs(w http.ResponseWriter, r *http.Request) {
	// Get the network from the request
	idx, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Get network info
	network := idx.GetNetworkInfo()

	// Get query parameters
	limit := 10
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		var err error
		limit, err = strconv.Atoi(limitStr)
		if err != nil || limit <= 0 {
			a.respondError(w, http.StatusBadRequest, "Invalid limit parameter")
			return
		}
	}
	if limit > MaxQueryLimit {
		limit = MaxQueryLimit
	}

	logger.Debug("Getting mempool blobs",
		zap.String("network", network.Name),
		zap.Int("limit", limit))

	// Get the pending blobs
	var blobs []models.Blob
	query := `
		SELECT * FROM blobs
		WHERE confirmed = false AND network_id = $1
		ORDER BY timestamp DESC
		LIMIT $2
	`
	if err := a.db.SelectContext(r.Context(), &blobs, query, network.ChainID, limit); err != nil {
		logger.Error("Failed to get pending blobs",
			zap.String("network", network.Name),
			zap.Error(err))
		a.respondError(w, http.StatusInternalServerError, "Failed to get pending blobs")
		return
	}

	// Convert to response format
	var response []BlobResponse
	for _, blob := range blobs {
		response = append(response, BlobResponse{
			NetworkID:         blob.NetworkID,
			NetworkName:       network.Name,
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
		})
	}

	logger.Debug("Returning mempool blobs",
		zap.String("network", network.Name),
		zap.Int("count", len(response)))
	a.respondJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    response,
	})
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
	// Get the network from the request
	idx, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Get network info
	network := idx.GetNetworkInfo()

	// Get the transaction hash from the URL
	txHash := chi.URLParam(r, "txHash")
	if txHash == "" {
		a.respondError(w, http.StatusBadRequest, "Transaction hash is required")
		return
	}

	logger.Debug("Getting blob by tx hash",
		zap.String("network", network.Name),
		zap.String("tx_hash", txHash))

	// Get the blob
	var blob models.Blob
	query := "SELECT * FROM blobs WHERE tx_hash = $1 AND network_id = $2"
	if err := a.db.GetContext(r.Context(), &blob, query, txHash, network.ChainID); err != nil {
		logger.Warn("Blob not found",
			zap.String("network", network.Name),
			zap.String("tx_hash", txHash),
			zap.Error(err))
		a.respondError(w, http.StatusNotFound, "Blob not found")
		return
	}

	// Convert to response format
	response := BlobResponse{
		NetworkID:         blob.NetworkID,
		NetworkName:       network.Name,
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
	}

	a.respondJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    response,
	})
}

// GetTopBlobUsers godoc
// @Summary Get top blob users
// @Description Retrieve the top users of blob transactions by count
// @Tags users
// @Accept json
// @Produce json
// @Param network query string false "Network name or chain ID (default: first enabled network)"
// @Param limit query int false "Number of users to return (default: 10)"
// @Success 200 {object} Response{data=[]UserResponse} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 500 {object} Response "Internal server error"
// @Router /users [get]
func (a *API) GetTopBlobUsers(w http.ResponseWriter, r *http.Request) {
	// Get the network from the request
	idx, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Get network info
	network := idx.GetNetworkInfo()

	// Get query parameters
	limit := 10
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		var err error
		limit, err = strconv.Atoi(limitStr)
		if err != nil || limit <= 0 {
			a.respondError(w, http.StatusBadRequest, "Invalid limit parameter")
			return
		}
	}
	if limit > MaxQueryLimit {
		limit = MaxQueryLimit
	}

	logger.Debug("Getting top blob users",
		zap.String("network", network.Name),
		zap.Int("limit", limit))

	// Get the top blob users
	users, err := idx.GetTopBlobUsers(r.Context(), limit)
	if err != nil {
		logger.Error("Failed to get top blob users",
			zap.String("network", network.Name),
			zap.Error(err))
		a.respondError(w, http.StatusInternalServerError, "Failed to get top blob users")
		return
	}

	// Convert to response format
	var response []UserResponse
	for _, user := range users {
		response = append(response, UserResponse{
			NetworkID:     network.ChainID,
			NetworkName:   network.Name,
			Address:       user.Address,
			Name:          user.Name,
			BlobCount:     user.BlobCount,
			TotalCostETH:  user.TotalCostETH,
			LastTimestamp: user.LastTimestamp,
		})
	}

	logger.Debug("Returning top blob users",
		zap.String("network", network.Name),
		zap.Int("count", len(response)))
	a.respondJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    response,
	})
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
	// Get the network from the request
	idx, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Get network info
	network := idx.GetNetworkInfo()

	logger.Debug("Getting blob statistics", zap.String("network", network.Name))

	// Get blob statistics
	var stats struct {
		TotalBlobs          int       `db:"total_blobs"`
		TotalConfirmedBlobs int       `db:"total_confirmed_blobs"`
		TotalPendingBlobs   int       `db:"total_pending_blobs"`
		AverageBaseFee      string    `db:"average_base_fee"`
		AverageTip          string    `db:"average_tip"`
		AverageTotalCost    string    `db:"average_total_cost"`
		LastIndexedTime     time.Time `db:"last_indexed_time"`
	}

	query := `
		SELECT
			COUNT(*) as total_blobs,
			SUM(CASE WHEN confirmed = true THEN 1 ELSE 0 END) as total_confirmed_blobs,
			SUM(CASE WHEN confirmed = false THEN 1 ELSE 0 END) as total_pending_blobs,
			AVG(base_fee_per_blob_gas::numeric) as average_base_fee,
			AVG(tip_per_blob_gas::numeric) as average_tip,
			AVG(total_cost_eth::numeric) as average_total_cost,
			MAX(timestamp) as last_indexed_time
		FROM blobs
		WHERE network_id = $1
	`
	if err := a.db.GetContext(r.Context(), &stats, query, network.ChainID); err != nil {
		logger.Error("Failed to get blob statistics",
			zap.String("network", network.Name),
			zap.Error(err))
		a.respondError(w, http.StatusInternalServerError, "Failed to get blob statistics")
		return
	}

	// Get the last indexed block
	lastIndexedBlock := idx.GetLastIndexedBlock()

	// Create the response
	response := StatsResponse{
		NetworkID:           network.ChainID,
		NetworkName:         network.Name,
		TotalBlobs:          stats.TotalBlobs,
		TotalConfirmedBlobs: stats.TotalConfirmedBlobs,
		TotalPendingBlobs:   stats.TotalPendingBlobs,
		AverageBaseFee:      stats.AverageBaseFee,
		AverageTip:          stats.AverageTip,
		AverageTotalCost:    stats.AverageTotalCost,
		LastIndexedBlock:    lastIndexedBlock,
		LastIndexedTime:     stats.LastIndexedTime,
	}

	a.respondJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    response,
	})
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
	// Get the network from the request
	idx, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Get network info
	network := idx.GetNetworkInfo()

	logger.Debug("Getting indexer status", zap.String("network", network.Name))

	// Get the last indexed block
	lastIndexedBlock := idx.GetLastIndexedBlock()

	// Get the indexer version
	indexerVersion := a.config.Indexer.Version

	// Get the last indexed time
	var lastIndexedTime time.Time
	query := "SELECT MAX(timestamp) FROM blobs WHERE confirmed = true AND network_id = $1"
	if err := a.db.GetContext(r.Context(), &lastIndexedTime, query, network.ChainID); err != nil {
		logger.Error("Failed to get last indexed time",
			zap.String("network", network.Name),
			zap.Error(err))
		a.respondError(w, http.StatusInternalServerError, "Failed to get last indexed time")
		return
	}

	// Calculate uptime (placeholder)
	uptime := "1h 30m"

	// Create the response
	response := StatusResponse{
		NetworkID:        network.ChainID,
		NetworkName:      network.Name,
		LastIndexedBlock: lastIndexedBlock,
		IndexerVersion:   indexerVersion,
		Uptime:           uptime,
		LastIndexedTime:  lastIndexedTime,
	}

	a.respondJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    response,
	})
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
	Status              string    `json:"status"`
	LastIndexedBlock    uint64    `json:"last_indexed_block"`
	CurrentBlock        uint64    `json:"current_block"`
	BlocksBehind        uint64    `json:"blocks_behind"`
	IndexingRate        float64   `json:"indexing_rate"`
	ErrorCount          int       `json:"error_count"`
	LastError           string    `json:"last_error,omitempty"`
	LastErrorTime       time.Time `json:"last_error_time,omitempty"`
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
	Query         string    `json:"query"`
	ExecutionTime float64   `json:"execution_time"`
	Calls         int       `json:"calls"`
	RowsReturned  int       `json:"rows_returned"`
	LastExecuted  time.Time `json:"last_executed"`
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

	a.respondJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    metrics,
	})
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

	var metrics []IndexerMetrics

	// Get metrics for each indexer
	for _, idx := range a.indexers {
		network := idx.GetNetworkInfo()
		lastIndexedBlock := idx.GetLastIndexedBlock()

		// Get the current block
		currentBlock, err := idx.GetCurrentBlock(r.Context())
		if err != nil {
			logger.Error("Failed to get current block",
				zap.String("network", network.Name),
				zap.Error(err))
			currentBlock = lastIndexedBlock // Fallback
		}

		// Calculate blocks behind
		var blocksBehind uint64
		if currentBlock > lastIndexedBlock {
			blocksBehind = currentBlock - lastIndexedBlock
		}

		// Get blob counts
		confirmedCount, pendingCount, err := idx.GetBlobCounts(r.Context())
		if err != nil {
			logger.Error("Failed to get blob counts",
				zap.String("network", network.Name),
				zap.Error(err))
		}

		// Get last indexed time
		var lastIndexedTime time.Time
		query := "SELECT MAX(timestamp) FROM blobs WHERE confirmed = true AND network_id = $1"
		if err := a.db.GetContext(r.Context(), &lastIndexedTime, query, network.ChainID); err != nil {
			logger.Error("Failed to get last indexed time",
				zap.String("network", network.Name),
				zap.Error(err))
			lastIndexedTime = time.Now() // Fallback
		}

		// Calculate indexing rate (blocks per minute)
		indexingRate := 0.0
		// This is a placeholder calculation
		// In a real implementation, you would track the indexing rate over time

		metrics = append(metrics, IndexerMetrics{
			NetworkID:           network.ChainID,
			NetworkName:         network.Name,
			Status:              "running", // Placeholder
			LastIndexedBlock:    lastIndexedBlock,
			CurrentBlock:        currentBlock,
			BlocksBehind:        blocksBehind,
			IndexingRate:        indexingRate,
			ErrorCount:          0, // Placeholder
			LastIndexedTime:     lastIndexedTime,
			TotalBlobsIndexed:   confirmedCount + pendingCount,
			PendingBlobsCount:   pendingCount,
			ConfirmedBlobsCount: confirmedCount,
		})
	}

	a.respondJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    metrics,
	})
}

// DevDatabase godoc
// @Summary Get database statistics
// @Description Retrieve statistics about the database
// @Tags dev
// @Accept json
// @Produce json
// @Success 200 {object} Response{data=DatabaseStats} "Success"
// @Failure 500 {object} Response "Internal server error"
// @Router /dev/database [get]
func (a *API) DevDatabase(w http.ResponseWriter, r *http.Request) {
	logger.Debug("Getting database statistics")

	// Get table statistics
	var tableStats []TableStat

	// Get statistics for each table
	tables := []string{"blobs", "blob_users", "networks", "indexer_metadata"}
	for _, table := range tables {
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
		query = `
			SELECT pg_total_relation_size($1)
		`
		if err := a.db.GetContext(r.Context(), &sizeBytes, query, table); err != nil {
			logger.Error("Failed to get table size",
				zap.String("table", table),
				zap.Error(err))
			sizeBytes = 0 // Fallback
		}

		// Get index count
		var indexCount int
		query = `
			SELECT COUNT(*)
			FROM pg_indexes
			WHERE tablename = $1
		`
		if err := a.db.GetContext(r.Context(), &indexCount, query, table); err != nil {
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
	query := `
		SELECT pg_database_size(current_database())
	`
	if err := a.db.GetContext(r.Context(), &totalSize, query); err != nil {
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

	a.respondJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    dbStats,
	})
}

// formatBytes formats bytes as a human-readable string
func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
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

	// Get query parameters
	limit := 100
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		var err error
		limit, err = strconv.Atoi(limitStr)
		if err != nil || limit <= 0 {
			a.respondError(w, http.StatusBadRequest, "Invalid limit parameter")
			return
		}
	}
	if limit > MaxQueryLimit {
		limit = MaxQueryLimit
	}

	level := r.URL.Query().Get("level")

	// This is a placeholder implementation
	// In a real implementation, you would retrieve logs from a log store
	logs := []LogEntry{
		{
			Timestamp: time.Now().Add(-5 * time.Minute),
			Level:     "info",
			Message:   "API server started",
			Fields: map[string]string{
				"port": "8080",
			},
		},
		{
			Timestamp: time.Now().Add(-4 * time.Minute),
			Level:     "info",
			Message:   "Connected to database",
			Fields: map[string]string{
				"db_name": "blobindexer",
			},
		},
		{
			Timestamp: time.Now().Add(-3 * time.Minute),
			Level:     "info",
			Message:   "Indexer started",
			Fields: map[string]string{
				"network": "mainnet",
			},
		},
		{
			Timestamp: time.Now().Add(-2 * time.Minute),
			Level:     "warn",
			Message:   "Slow query detected",
			Fields: map[string]string{
				"query":          "SELECT * FROM blobs WHERE...",
				"execution_time": "1.5s",
			},
		},
		{
			Timestamp: time.Now().Add(-1 * time.Minute),
			Level:     "error",
			Message:   "Failed to connect to Ethereum node",
			Fields: map[string]string{
				"network": "sepolia",
				"error":   "connection refused",
			},
		},
	}

	// Filter by level if specified
	if level != "" {
		var filtered []LogEntry
		for _, log := range logs {
			if log.Level == level {
				filtered = append(filtered, log)
			}
		}
		logs = filtered
	}

	// Limit the number of logs
	if len(logs) > limit {
		logs = logs[:limit]
	}

	a.respondJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    logs,
	})
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

	// Get query parameters
	limit := 20
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		var err error
		limit, err = strconv.Atoi(limitStr)
		if err != nil || limit <= 0 {
			a.respondError(w, http.StatusBadRequest, "Invalid limit parameter")
			return
		}
	}
	if limit > MaxQueryLimit {
		limit = MaxQueryLimit
	}

	// This is a placeholder implementation
	// In a real implementation, you would retrieve query statistics from pg_stat_statements
	queries := []QueryStat{
		{
			Query:         "SELECT * FROM blobs WHERE confirmed = true AND network_id = $1 ORDER BY block_number DESC, blob_index ASC LIMIT $2",
			ExecutionTime: 0.05,
			Calls:         1000,
			RowsReturned:  10,
			LastExecuted:  time.Now().Add(-5 * time.Minute),
		},
		{
			Query:         "SELECT * FROM blobs WHERE confirmed = false AND network_id = $1 ORDER BY timestamp DESC LIMIT $2",
			ExecutionTime: 0.03,
			Calls:         500,
			RowsReturned:  5,
			LastExecuted:  time.Now().Add(-10 * time.Minute),
		},
		{
			Query:         "SELECT * FROM blobs WHERE tx_hash = $1 AND network_id = $2",
			ExecutionTime: 0.01,
			Calls:         200,
			RowsReturned:  1,
			LastExecuted:  time.Now().Add(-15 * time.Minute),
		},
		{
			Query:         "SELECT COUNT(*) FROM blobs WHERE network_id = $1",
			ExecutionTime: 0.1,
			Calls:         100,
			RowsReturned:  1,
			LastExecuted:  time.Now().Add(-20 * time.Minute),
		},
		{
			Query:         "SELECT MAX(timestamp) FROM blobs WHERE confirmed = true AND network_id = $1",
			ExecutionTime: 0.02,
			Calls:         300,
			RowsReturned:  1,
			LastExecuted:  time.Now().Add(-25 * time.Minute),
		},
	}

	// Limit the number of queries
	if len(queries) > limit {
		queries = queries[:limit]
	}

	a.respondJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    queries,
	})
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

	// This is a placeholder for a development dashboard
	// In a real implementation, you would render an HTML page with charts and stats
	a.respondJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    "Development dashboard",
	})
}

// DevReindex godoc
// @Summary Trigger a reindex of blocks
// @Description Reindex a range of blocks (only available in dev mode)
// @Tags dev
// @Accept json
// @Produce json
// @Param request body ReindexRequest true "Reindex request"
// @Success 200 {object} Response{data=string} "Success"
// @Failure 400 {object} Response "Bad request"
// @Failure 500 {object} Response "Internal server error"
// @Router /dev/reindex [post]
func (a *API) DevReindex(w http.ResponseWriter, r *http.Request) {
	// Parse the request body
	var req ReindexRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if isMaxBytesError(err) {
			logger.Warn("Reindex request body too large", zap.Error(err))
			RespondMaxBytesError(w)
			return
		}
		logger.Warn("Invalid reindex request body", zap.Error(err))
		a.respondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	// Validate the request
	if req.StartBlock > req.EndBlock {
		a.respondError(w, http.StatusBadRequest, "Start block must be less than or equal to end block")
		return
	}

	// Get the indexer for this network
	idx, ok := a.indexers[req.NetworkID]
	if !ok {
		a.respondError(w, http.StatusBadRequest, "Invalid network ID")
		return
	}

	// Get network info
	network := idx.GetNetworkInfo()

	logger.Info("Triggering reindex",
		zap.String("network", network.Name),
		zap.Uint64("start_block", req.StartBlock),
		zap.Uint64("end_block", req.EndBlock))

	// Trigger the reindex
	if err := idx.Reindex(req.StartBlock, req.EndBlock); err != nil {
		logger.Error("Failed to reindex blocks",
			zap.String("network", network.Name),
			zap.Uint64("start_block", req.StartBlock),
			zap.Uint64("end_block", req.EndBlock),
			zap.Error(err))
		a.respondError(w, http.StatusInternalServerError, "Failed to reindex blocks")
		return
	}

	logger.Info("Reindex triggered successfully",
		zap.String("network", network.Name),
		zap.Uint64("start_block", req.StartBlock),
		zap.Uint64("end_block", req.EndBlock))

	a.respondJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    "Reindex triggered",
	})
}
