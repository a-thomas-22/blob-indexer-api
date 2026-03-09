package api

import (
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	httpSwagger "github.com/swaggo/http-swagger"
	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db"
	"github.com/a-thomas-22/blob-indexer-api/internal/indexer"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

// API holds the API dependencies
type API struct {
	db             *db.DB
	indexers       map[int]*indexer.Indexer
	config         *config.Config
	startTime      time.Time
	totalRequests  int64 // accessed via sync/atomic
	activeRequests int64 // accessed via sync/atomic
}

// NewRouter creates a new API router
func NewRouter(db *db.DB, indexers map[int]*indexer.Indexer, cfg *config.Config) http.Handler {
	api := &API{
		db:        db,
		indexers:  indexers,
		config:    cfg,
		startTime: time.Now(),
	}

	r := chi.NewRouter()

	// Rate limiter: 100 requests/second per IP with burst of 200
	rateLimiter := NewRateLimiter(100, 200)

	// Middleware
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(RateLimitMiddleware(rateLimiter))
	r.Use(LoggerMiddleware)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(60 * time.Second))
	r.Use(api.requestCounterMiddleware)

	// CORS — AllowCredentials is false since this is a public read API.
	// Using wildcard origins with credentials enabled is a security risk.
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Content-Type"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: false,
		MaxAge:           3600,
	}))

	// Swagger UI
	r.Get("/swagger/*", httpSwagger.Handler(
		httpSwagger.URL("/swagger/doc.json"), // The URL pointing to API definition
	))

	// Routes
	r.Route("/api", func(r chi.Router) {
		// Networks endpoint
		r.Route("/networks", func(r chi.Router) {
			r.Get("/", api.GetNetworks)
			r.Get("/{chainId}", api.GetNetworkStatus)
		})

		// Blob endpoints
		r.Route("/blob", func(r chi.Router) {
			r.Get("/latest", api.GetLatestBlobs)
			r.Get("/mempool", api.GetMempoolBlobs)
			r.Get("/{txHash}", api.GetBlobByTxHash)
		})

		// User endpoints
		r.Route("/users", func(r chi.Router) {
			r.Get("/", api.GetTopBlobUsers)
		})

		// Stats endpoints
		r.Route("/stats", func(r chi.Router) {
			r.Get("/", api.GetBlobStats)
		})

		// Status endpoint
		r.Get("/status", api.GetIndexerStatus)

		// Development endpoints
		r.Route("/dev", func(r chi.Router) {
			// These endpoints are always available
			r.Get("/metrics", api.DevMetrics)
			r.Get("/indexers", api.DevIndexers)
			r.Get("/database", api.DevDatabase)
			r.Get("/logs", api.DevLogs)
			r.Get("/queries", api.DevQueries)
			r.Get("/dashboard", api.DevDashboard)

			// Privileged endpoints that require dev mode
			if cfg.Server.DevMode {
				r.Post("/reindex", api.DevReindex)
			}
		})
	})

	logger.Info("API routes initialized",
		zap.String("swagger_ui", "/swagger/index.html"),
		zap.Bool("dev_mode", cfg.Server.DevMode))

	return r
}

// getNetworkFromRequest gets the network from the request query parameters
// If no network is specified, it returns the first enabled network
func (a *API) getNetworkFromRequest(r *http.Request) (*indexer.Indexer, error) {
	// Check if network is specified in query parameters
	networkParam := r.URL.Query().Get("network")
	if networkParam != "" {
		// Try to parse as chain ID
		chainID, err := strconv.Atoi(networkParam)
		if err == nil {
			// Look up by chain ID
			if idx, ok := a.indexers[chainID]; ok {
				return idx, nil
			}
		}

		// Try to look up by name
		for _, idx := range a.indexers {
			if idx.GetNetworkInfo().Name == networkParam {
				return idx, nil
			}
		}

		// Network not found
		return nil, ErrNetworkNotFound
	}

	// No network specified, return the first one
	if len(a.indexers) == 0 {
		return nil, ErrNoNetworksAvailable
	}

	// Return the first indexer (usually mainnet)
	for _, idx := range a.indexers {
		return idx, nil
	}

	return nil, ErrNoNetworksAvailable
}

// GetNetworks returns the list of available networks
func (a *API) GetNetworks(w http.ResponseWriter, r *http.Request) {
	networks := make([]NetworkResponse, 0, len(a.indexers))
	for _, idx := range a.indexers {
		network := idx.GetNetworkInfo()
		networks = append(networks, NetworkResponse{
			ChainID:          network.ChainID,
			Name:             network.Name,
			LastIndexedBlock: idx.GetLastIndexedBlock(),
		})
	}

	a.respondJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    networks,
	})
}

// GetNetworkStatus returns the status of a specific network
func (a *API) GetNetworkStatus(w http.ResponseWriter, r *http.Request) {
	// Get the chain ID from the URL
	chainIDStr := chi.URLParam(r, "chainId")
	chainID, err := strconv.Atoi(chainIDStr)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, "Invalid chain ID")
		return
	}

	// Get the indexer for this network
	idx, ok := a.indexers[chainID]
	if !ok {
		a.respondError(w, http.StatusNotFound, "Network not found")
		return
	}

	// Get the network info
	network := idx.GetNetworkInfo()

	// Create the response
	response := NetworkStatusResponse{
		ChainID:          network.ChainID,
		Name:             network.Name,
		LastIndexedBlock: idx.GetLastIndexedBlock(),
		IndexerVersion:   a.config.Indexer.Version,
	}

	a.respondJSON(w, http.StatusOK, Response{
		Success: true,
		Data:    response,
	})
}

// NetworkResponse is a response containing network information
type NetworkResponse struct {
	ChainID          int    `json:"chain_id"`
	Name             string `json:"name"`
	LastIndexedBlock uint64 `json:"last_indexed_block"`
}

// NetworkStatusResponse is a response containing detailed network status
type NetworkStatusResponse struct {
	ChainID          int    `json:"chain_id"`
	Name             string `json:"name"`
	LastIndexedBlock uint64 `json:"last_indexed_block"`
	IndexerVersion   string `json:"indexer_version"`
}

// Common errors
var (
	ErrNetworkNotFound     = NewAPIError("Network not found", http.StatusNotFound)
	ErrNoNetworksAvailable = NewAPIError("No networks available", http.StatusInternalServerError)
)

// APIError represents an API error
type APIError struct {
	Message string
	Status  int
}

// Error returns the error message
func (e APIError) Error() string {
	return e.Message
}

// NewAPIError creates a new API error
func NewAPIError(message string, status int) APIError {
	return APIError{
		Message: message,
		Status:  status,
	}
}

// requestCounterMiddleware tracks total and active request counts using atomic counters.
func (a *API) requestCounterMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&a.totalRequests, 1)
		atomic.AddInt64(&a.activeRequests, 1)
		defer atomic.AddInt64(&a.activeRequests, -1)
		next.ServeHTTP(w, r)
	})
}
