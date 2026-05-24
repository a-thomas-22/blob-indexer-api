package api

import (
	"fmt"
	"net/http"
	"runtime"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

const (
	queryDevIndexerCounts = `
			SELECT
				COALESCE(SUM(CASE WHEN confirmed = true THEN 1 ELSE 0 END), 0) as confirmed_count,
				COALESCE(SUM(CASE WHEN confirmed = false THEN 1 ELSE 0 END), 0) as pending_count
			FROM blobs WHERE network_id = $1
		`
)

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
