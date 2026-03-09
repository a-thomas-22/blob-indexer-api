package api

import (
	"context"
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
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

// API holds the API dependencies
type API struct {
	db             DBProvider
	networks       map[int]config.NetworkConfig
	config         *config.Config
	startTime      time.Time
	totalRequests  int64 // accessed via sync/atomic
	activeRequests int64 // accessed via sync/atomic
}

// NewRouter creates a new API router
func NewRouter(db DBProvider, cfg *config.Config) http.Handler {
	networks := make(map[int]config.NetworkConfig)
	for _, n := range cfg.GetEnabledNetworks() {
		networks[n.ChainID] = n
	}

	api := &API{
		db:        db,
		networks:  networks,
		config:    cfg,
		startTime: time.Now(),
	}

	r := chi.NewRouter()

	// Rate limiter: 100 requests/second per IP with burst of 200
	rateLimiter := NewRateLimiter(100, 200)

	// Middleware
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(SecurityHeadersMiddleware)
	r.Use(MaxBytesMiddleware)
	r.Use(RateLimitMiddleware(rateLimiter))
	r.Use(LoggerMiddleware)
	r.Use(ContentTypeJSON)
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

	// Versioned API routes under /api/v1
	r.Route("/api/v1", func(r chi.Router) {
		// Networks endpoint
		r.Route("/networks", func(r chi.Router) {
			r.Get("/", api.GetNetworks)
			r.Get("/{chainId}", api.GetNetworkStatus)
		})

		// Blob endpoints
		r.Route("/blob", func(r chi.Router) {
			r.Get("/latest", api.GetLatestBlobs)
			r.Get("/mempool", api.GetMempoolBlobs)
			r.Get("/pricing", api.GetBlobPricing)
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

		// Development endpoints — all gated behind dev mode
		r.Route("/dev", func(r chi.Router) {
			r.Use(DevModeMiddleware(cfg.Server.DevMode))

			r.Get("/metrics", api.DevMetrics)
			r.Get("/indexers", api.DevIndexers)
			r.Get("/database", api.DevDatabase)
			r.Get("/logs", api.DevLogs)
			r.Get("/queries", api.DevQueries)
			r.Get("/dashboard", api.DevDashboard)
		})
	})

	// Backward compatibility: redirect /api/* to /api/v1/* with 301 Moved Permanently
	r.HandleFunc("/api/*", func(w http.ResponseWriter, r *http.Request) {
		// Strip the "/api" prefix and prepend "/api/v1"
		newPath := "/api/v1" + r.URL.Path[len("/api"):]
		if r.URL.RawQuery != "" {
			newPath += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, newPath, http.StatusMovedPermanently)
	})

	// Also handle the exact /api path (without trailing slash or sub-paths)
	r.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		newPath := "/api/v1"
		if r.URL.RawQuery != "" {
			newPath += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, newPath, http.StatusMovedPermanently)
	})

	logger.Info("API routes initialized",
		zap.String("api_base", "/api/v1"),
		zap.String("swagger_ui", "/swagger/index.html"),
		zap.Bool("dev_mode", cfg.Server.DevMode))

	return r
}

// getNetworkFromRequest gets the network from the request query parameters.
// If no network is specified, it returns the first enabled network.
func (a *API) getNetworkFromRequest(r *http.Request) (config.NetworkConfig, error) {
	networkParam := r.URL.Query().Get("network")
	if networkParam != "" {
		// Try to parse as chain ID
		chainID, err := strconv.Atoi(networkParam)
		if err == nil {
			if n, ok := a.networks[chainID]; ok {
				return n, nil
			}
		}

		// Try to look up by name
		for _, n := range a.networks {
			if n.Name == networkParam {
				return n, nil
			}
		}

		return config.NetworkConfig{}, ErrNetworkNotFound
	}

	if len(a.networks) == 0 {
		return config.NetworkConfig{}, ErrNoNetworksAvailable
	}

	// Return the first network
	for _, n := range a.networks {
		return n, nil
	}

	return config.NetworkConfig{}, ErrNoNetworksAvailable
}

// GetNetworks returns the list of available networks
func (a *API) GetNetworks(w http.ResponseWriter, r *http.Request) {
	networks := make([]NetworkResponse, 0, len(a.networks))
	for _, n := range a.networks {
		lastBlock := a.getLastIndexedBlockFromDB(r.Context(), n.ChainID)
		networks = append(networks, NetworkResponse{
			ChainID:          n.ChainID,
			Name:             n.Name,
			LastIndexedBlock: lastBlock,
		})
	}

	a.respondSuccess(w, networks)
}

// GetNetworkStatus returns the status of a specific network
func (a *API) GetNetworkStatus(w http.ResponseWriter, r *http.Request) {
	chainIDStr := chi.URLParam(r, "chainId")
	chainID, err := strconv.Atoi(chainIDStr)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, "Invalid chain ID")
		return
	}

	n, ok := a.networks[chainID]
	if !ok {
		a.respondError(w, http.StatusNotFound, "Network not found")
		return
	}

	response := NetworkStatusResponse{
		ChainID:          n.ChainID,
		Name:             n.Name,
		LastIndexedBlock: a.getLastIndexedBlockFromDB(r.Context(), n.ChainID),
		IndexerVersion:   a.config.Indexer.Version,
	}

	a.respondSuccess(w, response)
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

// getLastIndexedBlockFromDB reads the last indexed block from the indexer_metadata table.
func (a *API) getLastIndexedBlockFromDB(ctx context.Context, networkID int) uint64 {
	var value string
	query := "SELECT value FROM indexer_metadata WHERE network_id = $1 AND key = 'last_indexed_block'"
	if err := a.db.GetContext(ctx, &value, query, networkID); err != nil {
		return 0
	}
	block, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0
	}
	return block
}
