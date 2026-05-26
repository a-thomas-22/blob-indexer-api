package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	httpSwagger "github.com/swaggo/http-swagger"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"

	apidocs "github.com/a-thomas-22/blob-indexer-api/docs"
	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
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
	cacheMu        sync.RWMutex
	statsCache     map[int]statsCacheEntry
	topUsersCache  map[string]topUsersCacheEntry
	aggregateGroup singleflight.Group
	breakdownCache map[string]userBreakdownCacheEntry
	rollingCache   map[string]rollingStatsCacheEntry
	mempoolCache   map[int]mempoolPressureCacheEntry
	hub            *Hub
	poller         *Poller
}

type statsCacheEntry struct {
	response  StatsResponse
	expiresAt time.Time
}

type topUsersCacheEntry struct {
	response  []UserResponse
	expiresAt time.Time
}

type userBreakdownCacheEntry struct {
	response  UserBreakdownResponse
	expiresAt time.Time
}

type rollingStatsCacheEntry struct {
	response  RollingStatsResponse
	expiresAt time.Time
}

type mempoolPressureCacheEntry struct {
	response  MempoolPressureResponse
	expiresAt time.Time
}

type freshnessMetadataRow struct {
	Key   string `db:"key"`
	Value string `db:"value"`
}

type networkFreshness struct {
	LastIndexedBlock uint64
	FreshnessResponse
	backfill backfillMetadata
}

type backfillMetadata struct {
	Active       bool
	activeSet    bool
	StartBlock   *uint64
	CurrentBlock *uint64
	TargetBlock  *uint64
	UpdatedAt    *time.Time
	CompletedAt  *time.Time
}

type routerOptions struct {
	name                string
	includePublicRoutes bool
	includeDevRoutes    bool
	includeSwagger      bool
}

// NewRouter creates the main API router. The provided context controls the
// lifetime of background goroutines (WebSocket hub and poller).
func NewRouter(ctx context.Context, db DBProvider, cfg *config.Config) http.Handler {
	router, _ := NewRouters(ctx, db, cfg)
	return router
}

// NewRouters creates the public API router and, when server.dev_port is set, a
// dedicated dev API router. Both routers share the same API state and
// background goroutines.
func NewRouters(ctx context.Context, db DBProvider, cfg *config.Config) (publicRouter, devRouter http.Handler) {
	api := newAPI(ctx, db, cfg)

	publicRouter = api.newRouter(routerOptions{
		name:                "api",
		includePublicRoutes: true,
		includeDevRoutes:    cfg.Server.DevPort == 0,
		includeSwagger:      true,
	})

	if cfg.Server.DevPort == 0 {
		return publicRouter, nil
	}

	devRouter = api.newRouter(routerOptions{
		name:             "dev-api",
		includeDevRoutes: true,
	})

	return publicRouter, devRouter
}

func newAPI(ctx context.Context, db DBProvider, cfg *config.Config) *API {
	networks := make(map[int]config.NetworkConfig)
	for _, n := range cfg.GetEnabledNetworks() {
		networks[n.ChainID] = n
	}

	hub := NewHub()
	go hub.Run()

	poller := NewPoller(db, hub, networks, cfg.WebSocket.PollInterval, cfg.WebSocket.UsersThrottleInterval)
	go poller.Run(ctx)

	// Stop hub when context is canceled.
	go func() {
		<-ctx.Done()
		hub.Stop()
	}()

	api := &API{
		db:             db,
		networks:       networks,
		config:         cfg,
		startTime:      time.Now(),
		statsCache:     make(map[int]statsCacheEntry),
		topUsersCache:  make(map[string]topUsersCacheEntry),
		breakdownCache: make(map[string]userBreakdownCacheEntry),
		rollingCache:   make(map[string]rollingStatsCacheEntry),
		mempoolCache:   make(map[int]mempoolPressureCacheEntry),
		hub:            hub,
		poller:         poller,
	}

	return api
}

func (a *API) newRouter(opts routerOptions) http.Handler {
	cfg := a.config
	r := chi.NewRouter()

	clientIPs := newClientIPResolver(cfg.Server.TrustedIPHeaders)
	rateLimiter := NewRateLimiter(cfg.Server.RateLimitRPS, cfg.Server.RateLimitBurst)
	aggregateRateLimiter := NewRateLimiter(cfg.Server.AggregateRateLimitRPS, cfg.Server.AggregateRateLimitBurst)
	aggregateLimit := RateLimitMiddlewareWithResolver(aggregateRateLimiter, clientIPs)

	// Middleware — applied to all routes (including WebSocket upgrade).
	r.Use(middleware.RequestID)
	r.Use(middleware.ClientIPFromRemoteAddr)
	r.Use(SecurityHeadersMiddleware)
	r.Use(CORSMiddleware(cfg.CORS))
	r.Use(MaxBytesMiddleware)
	r.Use(RateLimitMiddlewareWithResolver(rateLimiter, clientIPs))
	r.Use(LoggerMiddlewareWithResolver(clientIPs))
	r.Use(ContentTypeJSON)
	r.Use(middleware.Recoverer)
	r.Use(a.requestCounterMiddleware)

	if opts.includeSwagger {
		// Swagger UI
		r.Get("/swagger/*", httpSwagger.Handler(
			httpSwagger.URL("/swagger/doc.json"), // The URL pointing to API definition
		))
		r.Get("/asyncapi.yaml", serveAsyncAPI)
	}

	// Versioned API routes under /api/v1
	r.Route("/api/v1", func(r chi.Router) {
		if opts.includePublicRoutes {
			a.mountPublicRoutes(r, aggregateLimit)
		}

		if opts.includeDevRoutes {
			r.Group(func(r chi.Router) {
				r.Use(middleware.Timeout(60 * time.Second))
				a.mountDevRoutes(r)
			})
		}
	})

	mountLegacyAPIRedirects(r)

	logger.Info("API routes initialized",
		zap.String("router", opts.name),
		zap.String("api_base", "/api/v1"),
		zap.String("swagger_ui", "/swagger/index.html"),
		zap.Bool("public_routes", opts.includePublicRoutes),
		zap.Bool("dev_routes", opts.includeDevRoutes),
		zap.Bool("dev_mode", cfg.Server.DevMode))

	return r
}

func serveAsyncAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(apidocs.AsyncAPIYAML)
}

func (a *API) mountPublicRoutes(r chi.Router, aggregateLimit func(http.Handler) http.Handler) {
	// WebSocket endpoint — no request timeout so connections persist.
	r.Get("/ws", a.HandleWebSocket)

	// REST endpoints — with request timeout.
	r.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(60 * time.Second))

		// Networks endpoint
		r.Route("/networks", func(r chi.Router) {
			r.Get("/", a.GetNetworks)
			r.Get("/{chainId}", a.GetNetworkStatus)
		})

		// Blob endpoints
		r.Route("/blob", func(r chi.Router) {
			r.Get("/latest", a.GetLatestBlobs)
			r.Get("/mempool", a.GetMempoolBlobs)
			r.With(aggregateLimit).Get("/mempool/pressure", a.GetMempoolPressure)
			r.Get("/pricing", a.GetBlobPricing)
			r.Get("/{txHash}", a.GetBlobByTxHash)
		})

		// User endpoints
		r.Route("/users", func(r chi.Router) {
			r.With(aggregateLimit).Get("/", a.GetTopBlobUsers)
			r.With(aggregateLimit).Get("/unattributed", a.GetTopUnattributedBlobUsers)
			r.With(aggregateLimit).Get("/breakdown", a.GetUserBreakdown)
			r.With(aggregateLimit).Get("/{address}", a.GetUserByAddress)
		})

		// Stats endpoints
		r.Route("/stats", func(r chi.Router) {
			r.With(aggregateLimit).Get("/", a.GetBlobStats)
			r.With(aggregateLimit).Get("/windows", a.GetRollingStatsWindows)
		})

		// Chart endpoints
		r.Route("/charts", func(r chi.Router) {
			r.Get("/blob-market", a.GetBlobMarketChart)
			r.Get("/attribution-usage", a.GetAttributionUsageChart)
			r.Get("/cost-comparison", a.GetCostComparisonChart)
			r.Get("/rolling-stats", a.GetRollingStatsWindows)
		})

		// Status endpoint
		r.Get("/status", a.GetIndexerStatus)
	})
}

func (a *API) mountDevRoutes(r chi.Router) {
	// Development endpoints — all gated behind dev mode.
	r.Route("/dev", func(r chi.Router) {
		r.Use(DevModeMiddleware(a.config.Server.DevMode))
		r.Use(DevAPIKeyMiddleware(a.config.Server.DevAPIKey))

		r.Get("/metrics", a.DevMetrics)
		r.Get("/indexers", a.DevIndexers)
		r.Get("/database", a.DevDatabase)
		r.Get("/logs", a.DevLogs)
		r.Get("/queries", a.DevQueries)
		r.Get("/dashboard", a.DevDashboard)
	})
}

func mountLegacyAPIRedirects(r chi.Router) {
	// Backward compatibility: redirect /api/* to /api/v1/* with 301 Moved Permanently
	r.HandleFunc("/api/*", func(w http.ResponseWriter, r *http.Request) {
		// Strip the "/api" prefix and prepend "/api/v1"
		location := (&url.URL{Path: "/api/v1" + r.URL.Path[len("/api"):], RawQuery: r.URL.RawQuery}).String()
		w.Header().Set("Location", location)
		w.WriteHeader(http.StatusMovedPermanently)
	})

	// Also handle the exact /api path (without trailing slash or sub-paths)
	r.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		location := (&url.URL{Path: "/api/v1", RawQuery: r.URL.RawQuery}).String()
		w.Header().Set("Location", location)
		w.WriteHeader(http.StatusMovedPermanently)
	})
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
		freshness := a.getNetworkFreshnessFromDB(r.Context(), n.ChainID)
		networks = append(networks, NetworkResponse{
			ChainID:           n.ChainID,
			Name:              n.Name,
			LastIndexedBlock:  freshness.LastIndexedBlock,
			FreshnessResponse: freshness.FreshnessResponse,
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

	freshness := a.getNetworkFreshnessFromDB(r.Context(), n.ChainID)
	response := NetworkStatusResponse{
		ChainID:           n.ChainID,
		Name:              n.Name,
		LastIndexedBlock:  freshness.LastIndexedBlock,
		IndexerVersion:    a.config.Indexer.Version,
		FreshnessResponse: freshness.FreshnessResponse,
	}

	a.respondSuccess(w, response)
}

// FreshnessResponse contains network trust and freshness metadata.
type FreshnessResponse struct {
	CurrentChainHead     *uint64    `json:"current_chain_head,omitempty"`
	IndexerLagBlocks     *uint64    `json:"indexer_lag_blocks,omitempty"`
	LastIndexedAt        *time.Time `json:"last_indexed_at,omitempty"`
	ChainHeadUpdatedAt   *time.Time `json:"chain_head_updated_at,omitempty"`
	WebSocketFreshnessAt *time.Time `json:"websocket_freshness_at,omitempty"`
}

// BackfillResponse contains the current historical catch-up status for a network.
type BackfillResponse struct {
	Active          bool       `json:"active"`
	StartBlock      *uint64    `json:"start_block,omitempty"`
	CurrentBlock    uint64     `json:"current_block"`
	TargetBlock     *uint64    `json:"target_block,omitempty"`
	RemainingBlocks *uint64    `json:"remaining_blocks,omitempty"`
	ProgressPercent *float64   `json:"progress_percent,omitempty"`
	UpdatedAt       *time.Time `json:"updated_at,omitempty"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
}

// NetworkResponse is a response containing network information
type NetworkResponse struct {
	ChainID          int    `json:"chain_id"`
	Name             string `json:"name"`
	LastIndexedBlock uint64 `json:"last_indexed_block"`
	FreshnessResponse
}

// NetworkStatusResponse is a response containing detailed network status
type NetworkStatusResponse struct {
	ChainID          int    `json:"chain_id"`
	Name             string `json:"name"`
	LastIndexedBlock uint64 `json:"last_indexed_block"`
	IndexerVersion   string `json:"indexer_version"`
	FreshnessResponse
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
	if err := a.db.GetContext(ctx, &value, queryLastIndexedBlock, networkID); err != nil {
		return 0
	}
	block, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0
	}
	return block
}

func (a *API) getNetworkFreshnessFromDB(ctx context.Context, networkID int) networkFreshness {
	var rows []freshnessMetadataRow
	if err := a.db.SelectContext(ctx, &rows, queryNetworkFreshnessMetadata, networkID); err != nil {
		return networkFreshness{}
	}

	var freshness networkFreshness
	for _, row := range rows {
		switch row.Key {
		case models.MetadataLastIndexedBlock:
			block, ok := parseMetadataUint(row.Value)
			if ok {
				freshness.LastIndexedBlock = block
			}
		case models.MetadataCurrentChainHead:
			head, ok := parseMetadataUint(row.Value)
			if ok {
				freshness.CurrentChainHead = &head
			}
		case models.MetadataChainHeadUpdatedAt:
			timestamp, ok := parseMetadataTimestamp(row.Value)
			if ok {
				freshness.ChainHeadUpdatedAt = &timestamp
			}
		case models.MetadataLastIndexedAt:
			timestamp, ok := parseMetadataTimestamp(row.Value)
			if ok {
				freshness.LastIndexedAt = &timestamp
			}
		case models.MetadataWebSocketFreshnessAt:
			timestamp, ok := parseMetadataTimestamp(row.Value)
			if ok {
				freshness.WebSocketFreshnessAt = &timestamp
			}
		case models.MetadataBackfillActive:
			active, ok := parseMetadataBool(row.Value)
			if ok {
				freshness.backfill.Active = active
				freshness.backfill.activeSet = true
			}
		case models.MetadataBackfillStartBlock:
			block, ok := parseMetadataUint(row.Value)
			if ok {
				freshness.backfill.StartBlock = &block
			}
		case models.MetadataBackfillCurrentBlock:
			block, ok := parseMetadataUint(row.Value)
			if ok {
				freshness.backfill.CurrentBlock = &block
			}
		case models.MetadataBackfillTargetBlock:
			block, ok := parseMetadataUint(row.Value)
			if ok {
				freshness.backfill.TargetBlock = &block
			}
		case models.MetadataBackfillUpdatedAt:
			timestamp, ok := parseMetadataTimestamp(row.Value)
			if ok {
				freshness.backfill.UpdatedAt = &timestamp
			}
		case models.MetadataBackfillCompletedAt:
			timestamp, ok := parseMetadataTimestamp(row.Value)
			if ok {
				freshness.backfill.CompletedAt = &timestamp
			}
		}
	}

	if freshness.CurrentChainHead != nil {
		lag := uint64(0)
		if *freshness.CurrentChainHead > freshness.LastIndexedBlock {
			lag = *freshness.CurrentChainHead - freshness.LastIndexedBlock
		}
		freshness.IndexerLagBlocks = &lag
	}

	return freshness
}

func parseMetadataUint(value string) (uint64, bool) {
	parsed, err := strconv.ParseUint(value, 10, 64)
	return parsed, err == nil
}

func parseMetadataBool(value string) (parsed, ok bool) {
	parsed, err := strconv.ParseBool(value)
	return parsed, err == nil
}

func parseMetadataTimestamp(value string) (time.Time, bool) {
	parsed, err := models.ParseMetadataTimestamp(value)
	return parsed, err == nil
}

func (f networkFreshness) backfillResponse() BackfillResponse {
	targetBlock := maxUint64Ptr(f.backfill.TargetBlock, f.CurrentChainHead)
	currentBlock := f.LastIndexedBlock
	if f.backfill.CurrentBlock != nil {
		currentBlock = *f.backfill.CurrentBlock
	}
	response := BackfillResponse{
		Active:       f.backfill.Active,
		StartBlock:   f.backfill.StartBlock,
		CurrentBlock: currentBlock,
		TargetBlock:  targetBlock,
		UpdatedAt:    f.backfill.UpdatedAt,
		CompletedAt:  f.backfill.CompletedAt,
	}

	if targetBlock != nil && *targetBlock <= currentBlock {
		response.Active = false
	} else if !f.backfill.activeSet && targetBlock != nil {
		response.Active = true
	}

	if targetBlock != nil {
		remaining := uint64(0)
		if *targetBlock > currentBlock {
			remaining = *targetBlock - currentBlock
		}
		response.RemainingBlocks = &remaining
	}

	if f.backfill.StartBlock != nil && targetBlock != nil {
		progress := backfillProgressPercent(*f.backfill.StartBlock, currentBlock, *targetBlock)
		response.ProgressPercent = &progress
	}

	return response
}

func maxUint64Ptr(a, b *uint64) *uint64 {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case *a >= *b:
		return a
	default:
		return b
	}
}

func backfillProgressPercent(startBlock, currentBlock, targetBlock uint64) float64 {
	if targetBlock <= startBlock {
		if currentBlock >= targetBlock {
			return 100
		}
		return 0
	}
	if currentBlock <= startBlock {
		return 0
	}
	if currentBlock >= targetBlock {
		return 100
	}
	return float64(currentBlock-startBlock) / float64(targetBlock-startBlock) * 100
}
