package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/viper"
	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

const (
	networkMainnet                 = "mainnet"
	networkSepolia                 = "sepolia"
	maskedValue                    = "****"
	defaultCORSOriginLocalhost3000 = "http://localhost:3000"
	defaultCORSOriginLocalhost3001 = "http://localhost:3001"
	defaultCORSOriginLoopback3000  = "http://127.0.0.1:3000"
	defaultCORSOriginLoopback3001  = "http://127.0.0.1:3001"
	corsMethodGET                  = "GET"
	corsMethodOptions              = "OPTIONS"
	corsHeaderAccept               = "Accept"
	corsHeaderContentType          = "Content-Type"
	corsHeaderAuthorization        = "Authorization"
	corsHeaderContentLength        = "Content-Length"
	corsHeaderETag                 = "ETag"
)

// NetworkConfig holds the configuration for a single Ethereum network
type NetworkConfig struct {
	Name       string `mapstructure:"name" yaml:"name"`
	ChainID    int    `mapstructure:"chain_id" yaml:"chain_id"`
	RpcURL     string `mapstructure:"rpc_url" yaml:"rpc_url"`         // RPC URL is only stored in configuration, not in the database
	StartBlock string `mapstructure:"start_block" yaml:"start_block"` // "LATEST", "LATEST-1000", or specific number
	Enabled    bool   `mapstructure:"enabled" yaml:"enabled"`
}

// DatabaseConfig holds the database configuration
type DatabaseConfig struct {
	URL                string        `mapstructure:"url" yaml:"url"`
	RunMigrations      bool          `mapstructure:"run_migrations" yaml:"run_migrations"`
	SchemaWaitTimeout  time.Duration `mapstructure:"schema_wait_timeout" yaml:"schema_wait_timeout"`
	SchemaPollInterval time.Duration `mapstructure:"schema_poll_interval" yaml:"schema_poll_interval"`
	MaxOpenConns       int           `mapstructure:"max_open_conns" yaml:"max_open_conns"`
	MaxIdleConns       int           `mapstructure:"max_idle_conns" yaml:"max_idle_conns"`
	ConnMaxLifetime    time.Duration `mapstructure:"conn_max_lifetime" yaml:"conn_max_lifetime"`
	ConnMaxIdleTime    time.Duration `mapstructure:"conn_max_idle_time" yaml:"conn_max_idle_time"`
}

// ServerConfig holds the server configuration
type ServerConfig struct {
	Port            int           `mapstructure:"port" yaml:"port"`
	DevPort         int           `mapstructure:"dev_port" yaml:"dev_port"`
	DevMode         bool          `mapstructure:"dev_mode" yaml:"dev_mode"`
	ReadTimeout     time.Duration `mapstructure:"read_timeout" yaml:"read_timeout"`
	WriteTimeout    time.Duration `mapstructure:"write_timeout" yaml:"write_timeout"`
	IdleTimeout     time.Duration `mapstructure:"idle_timeout" yaml:"idle_timeout"`
	DevAPIKey       string        `mapstructure:"dev_api_key" yaml:"dev_api_key"`
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout" yaml:"shutdown_timeout"`
}

// CORSConfig holds browser cross-origin request settings for the API.
type CORSConfig struct {
	Enabled               bool     `mapstructure:"enabled" yaml:"enabled"`
	AllowedOrigins        []string `mapstructure:"allowed_origins" yaml:"allowed_origins"`
	AllowedOriginPatterns []string `mapstructure:"allowed_origin_patterns" yaml:"allowed_origin_patterns"`
	AllowAllOrigins       bool     `mapstructure:"allow_all_origins" yaml:"allow_all_origins"`
	AllowedMethods        []string `mapstructure:"allowed_methods" yaml:"allowed_methods"`
	AllowedHeaders        []string `mapstructure:"allowed_headers" yaml:"allowed_headers"`
	ExposedHeaders        []string `mapstructure:"exposed_headers" yaml:"exposed_headers"`
	AllowCredentials      bool     `mapstructure:"allow_credentials" yaml:"allow_credentials"`
	MaxAgeSeconds         int      `mapstructure:"max_age_seconds" yaml:"max_age_seconds"`
}

// LoggingConfig holds the logging configuration
type LoggingConfig struct {
	Level  string `mapstructure:"level" yaml:"level"`
	Format string `mapstructure:"format" yaml:"format"`
}

// IndexerConfig holds the indexer configuration
type IndexerConfig struct {
	Version                string        `mapstructure:"version" yaml:"version"`
	BatchSize              int           `mapstructure:"batch_size" yaml:"batch_size"`
	PollingInterval        time.Duration `mapstructure:"polling_interval" yaml:"polling_interval"`
	MempoolPollingInterval time.Duration `mapstructure:"mempool_polling_interval" yaml:"mempool_polling_interval"`
	MempoolTTL             time.Duration `mapstructure:"mempool_ttl" yaml:"mempool_ttl"`                           // max age for pending blobs before cleanup
	MempoolCleanupInterval time.Duration `mapstructure:"mempool_cleanup_interval" yaml:"mempool_cleanup_interval"` // how often to run stale pending blob cleanup
	WorkerCount            int           `mapstructure:"worker_count" yaml:"worker_count"`
	MaxBlockRetries        int           `mapstructure:"max_block_retries" yaml:"max_block_retries"`
	GapScanInterval        time.Duration `mapstructure:"gap_scan_interval" yaml:"gap_scan_interval"`
	MaxReorgDepth          int           `mapstructure:"max_reorg_depth" yaml:"max_reorg_depth"`
	RPCRateLimit           float64       `mapstructure:"rpc_rate_limit" yaml:"rpc_rate_limit"` // requests per second; 0 = no proactive limiting
}

// AttributionConfig holds dynamic attribution registry configuration.
type AttributionConfig struct {
	BlobListEnabled         bool          `mapstructure:"blob_list_enabled" yaml:"blob_list_enabled"`
	BlobListBaseURL         string        `mapstructure:"blob_list_base_url" yaml:"blob_list_base_url"`
	BlobListRefreshInterval time.Duration `mapstructure:"blob_list_refresh_interval" yaml:"blob_list_refresh_interval"`
	BlobListRequestTimeout  time.Duration `mapstructure:"blob_list_request_timeout" yaml:"blob_list_request_timeout"`
}

// WebSocketConfig holds the WebSocket configuration
type WebSocketConfig struct {
	PollInterval          time.Duration `mapstructure:"poll_interval" yaml:"poll_interval"`
	UsersThrottleInterval time.Duration `mapstructure:"users_throttle_interval" yaml:"users_throttle_interval"`
}

// Config holds the application configuration
type Config struct {
	Database    DatabaseConfig    `mapstructure:"database" yaml:"database"`
	Server      ServerConfig      `mapstructure:"server" yaml:"server"`
	CORS        CORSConfig        `mapstructure:"cors" yaml:"cors"`
	Logging     LoggingConfig     `mapstructure:"logging" yaml:"logging"`
	Indexer     IndexerConfig     `mapstructure:"indexer" yaml:"indexer"`
	Attribution AttributionConfig `mapstructure:"attribution" yaml:"attribution"`
	WebSocket   WebSocketConfig   `mapstructure:"websocket" yaml:"websocket"`
	Networks    []NetworkConfig   `mapstructure:"networks" yaml:"networks"`
}

// Load loads the configuration using Viper with full validation (for the indexer).
func Load() (*Config, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}

	if err := validateConfig(cfg); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return cfg, nil
}

// loadConfig loads and parses config without validation.
func loadConfig() (*Config, error) {
	v := viper.New()

	// Set default values
	v.SetDefault("server.port", 8080)
	v.SetDefault("server.dev_port", 0)
	v.SetDefault("server.dev_mode", false)
	v.SetDefault("server.read_timeout", "30s")
	v.SetDefault("server.write_timeout", "30s")
	v.SetDefault("server.idle_timeout", "120s")
	v.SetDefault("server.dev_api_key", "")
	v.SetDefault("server.shutdown_timeout", "15s")
	v.SetDefault("cors.enabled", true)
	v.SetDefault("cors.allowed_origins", []string{
		defaultCORSOriginLocalhost3000,
		defaultCORSOriginLocalhost3001,
		defaultCORSOriginLoopback3000,
		defaultCORSOriginLoopback3001,
	})
	v.SetDefault("cors.allowed_origin_patterns", []string{})
	v.SetDefault("cors.allow_all_origins", false)
	v.SetDefault("cors.allowed_methods", []string{corsMethodGET, corsMethodOptions})
	v.SetDefault("cors.allowed_headers", []string{corsHeaderAccept, corsHeaderContentType, corsHeaderAuthorization})
	v.SetDefault("cors.exposed_headers", []string{corsHeaderContentLength, corsHeaderETag})
	v.SetDefault("cors.allow_credentials", false)
	v.SetDefault("cors.max_age_seconds", 86400)
	v.SetDefault("logging.level", "info")
	v.SetDefault("logging.format", "json")
	v.SetDefault("database.run_migrations", false)
	v.SetDefault("database.schema_wait_timeout", "2m")
	v.SetDefault("database.schema_poll_interval", "2s")
	v.SetDefault("database.max_open_conns", 25)
	v.SetDefault("database.max_idle_conns", 10)
	v.SetDefault("database.conn_max_lifetime", "5m")
	v.SetDefault("database.conn_max_idle_time", "1m")
	v.SetDefault("indexer.version", "v1.0.0")
	v.SetDefault("indexer.batch_size", 100)
	v.SetDefault("indexer.polling_interval", "15s")
	v.SetDefault("indexer.mempool_polling_interval", "30s")
	v.SetDefault("indexer.mempool_ttl", "30m")
	v.SetDefault("indexer.mempool_cleanup_interval", "5m")
	v.SetDefault("indexer.worker_count", 4)
	v.SetDefault("indexer.max_block_retries", 3)
	v.SetDefault("indexer.gap_scan_interval", "5m")
	v.SetDefault("indexer.max_reorg_depth", 64)
	v.SetDefault("indexer.rpc_rate_limit", 0)
	v.SetDefault("attribution.blob_list_enabled", true)
	v.SetDefault("attribution.blob_list_base_url", "https://raw.githubusercontent.com/ahkc4/blob-list/main/artifacts/by-chain")
	v.SetDefault("attribution.blob_list_refresh_interval", "1h")
	v.SetDefault("attribution.blob_list_request_timeout", "10s")
	v.SetDefault("websocket.poll_interval", "3s")
	v.SetDefault("websocket.users_throttle_interval", "30s")
	v.SetDefault("networks", []NetworkConfig{})

	// Configure Viper to read from config file
	v.SetConfigName("config") // name of config file (without extension)
	v.SetConfigType("yaml")   // YAML format
	v.AddConfigPath(".")      // look for config in the working directory

	// Check if CONFIG_PATH environment variable is set
	configPath := os.Getenv("CONFIG_PATH")
	if configPath != "" {
		// Use the specified config file
		logger.Info("CONFIG_PATH environment variable set", zap.String("path", configPath))

		// Check if the file exists
		if _, err := os.Stat(configPath); os.IsNotExist(err) {
			logger.Warn("Config file not found at specified path", zap.String("path", configPath))

			// List files in the directory to help debug
			dir := filepath.Dir(configPath)
			if dir == "" {
				dir = "."
			}

			logger.Debug("Listing files in directory", zap.String("directory", dir))
			files, err := os.ReadDir(dir)
			if err != nil {
				logger.Error("Error reading directory", zap.String("directory", dir), zap.Error(err))
			} else {
				fileNames := make([]string, 0, len(files))
				for _, file := range files {
					fileNames = append(fileNames, file.Name())
				}
				logger.Debug("Directory contents", zap.String("directory", dir), zap.Strings("files", fileNames))
			}
		} else {
			logger.Info("Config file found", zap.String("path", configPath))
		}

		v.SetConfigFile(configPath)
	} else {
		logger.Info("CONFIG_PATH environment variable not set, using default config file")
	}

	// Read the config file
	if err := v.ReadInConfig(); err != nil {
		// It's okay if the config file doesn't exist
		var configFileNotFoundError viper.ConfigFileNotFoundError
		if !errors.As(err, &configFileNotFoundError) {
			logger.Error("Error reading config file", zap.Error(err))
			return nil, fmt.Errorf("failed to read config file: %w", err)
		}
		logger.Warn("Config file not found, continuing with defaults and environment variables")
	} else {
		logger.Info("Successfully loaded config file", zap.String("path", v.ConfigFileUsed()))
	}

	// Set up environment variable binding
	v.AutomaticEnv()
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	// Special handling for sensitive information and environment variables
	// that don't match our config structure exactly

	// Database URL - direct environment variable override
	if dbURL := os.Getenv("DB_URL"); dbURL != "" {
		v.Set("database.url", dbURL)
	}

	// Server port - direct environment variable override
	if portStr := os.Getenv("PORT"); portStr != "" {
		v.Set("server.port", portStr) // Viper will handle the conversion
	}

	// Development server port - direct environment variable override
	if devPortStr := os.Getenv("DEV_PORT"); devPortStr != "" {
		v.Set("server.dev_port", devPortStr) // Viper will handle the conversion
	}

	// Development mode - direct environment variable override
	if devMode := os.Getenv("DEV_MODE"); devMode != "" {
		v.Set("server.dev_mode", strings.EqualFold(devMode, "true"))
	}

	// Development API key for privileged dev endpoints
	if devAPIKey := os.Getenv("DEV_API_KEY"); devAPIKey != "" {
		v.Set("server.dev_api_key", devAPIKey)
	}

	applyCORSEnvOverrides(v)

	// Indexer version - direct environment variable override
	if version := os.Getenv("INDEXER_VERSION"); version != "" {
		v.Set("indexer.version", version)
	}

	// Log level - direct environment variable override
	if logLevel := os.Getenv("LOG_LEVEL"); logLevel != "" {
		v.Set("logging.level", logLevel)
	}

	// Handle legacy RPC_URL and START_BLOCK for backward compatibility
	networksEmpty := true
	if v.IsSet("networks") {
		networksVal := v.Get("networks")
		if networks, ok := networksVal.([]interface{}); ok {
			networksEmpty = len(networks) == 0
		}
	}

	if rpcURL := os.Getenv("RPC_URL"); rpcURL != "" && networksEmpty {
		// Create a default network if none exists
		startBlock := os.Getenv("START_BLOCK")
		if startBlock == "" {
			startBlock = "LATEST"
		}

		// Check if ETH_RPC_URL is set (newer variable name)
		if ethRPCURL := os.Getenv("ETH_RPC_URL"); ethRPCURL != "" {
			rpcURL = ethRPCURL // Prefer ETH_RPC_URL over RPC_URL
		}

		// Try to determine the network from the RPC URL
		networkName := networkMainnet
		chainID := 1

		// Check for common testnet URLs in the RPC URL
		rpcLower := strings.ToLower(rpcURL)
		switch {
		case strings.Contains(rpcLower, networkSepolia):
			networkName = networkSepolia
			chainID = 11155111
		case strings.Contains(rpcLower, "goerli"):
			networkName = "goerli"
			chainID = 5
		case strings.Contains(rpcLower, "holesky"):
			networkName = "holesky"
			chainID = 17000
		}

		// Add the network to Viper's configuration
		v.Set("networks", []map[string]interface{}{
			{
				"name":        networkName,
				"chain_id":    chainID,
				"rpc_url":     rpcURL,
				"start_block": startBlock,
				"enabled":     true,
			},
		})
	}

	// Create a config struct to unmarshal into
	var cfg Config

	// Unmarshal the configuration
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}
	cfg.CORS = normalizeCORSConfig(cfg.CORS)

	// Process network-specific environment variables
	// This needs to be done after unmarshaling because we need the network names
	for i := range cfg.Networks {
		network := &cfg.Networks[i]
		prefix := "NETWORK_" + strings.ToUpper(network.Name) + "_"

		if rpcURL := os.Getenv(prefix + "RPC_URL"); rpcURL != "" {
			network.RpcURL = rpcURL
		}

		if startBlock := os.Getenv(prefix + "START_BLOCK"); startBlock != "" {
			network.StartBlock = startBlock
		}

		if enabled := os.Getenv(prefix + "ENABLED"); enabled != "" {
			network.Enabled = strings.EqualFold(enabled, "true")
		}
	}

	// Parse duration strings into time.Duration
	var err error
	if cfg.Indexer.PollingInterval, err = parseDuration(v, "indexer.polling_interval", "polling_interval"); err != nil {
		return nil, err
	}
	if cfg.Indexer.MempoolPollingInterval, err = parseDuration(v, "indexer.mempool_polling_interval", "mempool_polling_interval"); err != nil {
		return nil, err
	}
	if cfg.Indexer.MempoolTTL, err = parseDuration(v, "indexer.mempool_ttl", "mempool_ttl"); err != nil {
		return nil, err
	}
	if cfg.Indexer.MempoolCleanupInterval, err = parseDuration(v, "indexer.mempool_cleanup_interval", "mempool_cleanup_interval"); err != nil {
		return nil, err
	}
	if cfg.Server.ShutdownTimeout, err = parseDuration(v, "server.shutdown_timeout", "shutdown_timeout"); err != nil {
		return nil, err
	}
	if cfg.Server.ReadTimeout, err = parseDuration(v, "server.read_timeout", "read_timeout"); err != nil {
		return nil, err
	}
	if cfg.Server.WriteTimeout, err = parseDuration(v, "server.write_timeout", "write_timeout"); err != nil {
		return nil, err
	}
	if cfg.Server.IdleTimeout, err = parseDuration(v, "server.idle_timeout", "idle_timeout"); err != nil {
		return nil, err
	}
	if cfg.Database.ConnMaxLifetime, err = parseDuration(v, "database.conn_max_lifetime", "conn_max_lifetime"); err != nil {
		return nil, err
	}
	if cfg.Database.ConnMaxIdleTime, err = parseDuration(v, "database.conn_max_idle_time", "conn_max_idle_time"); err != nil {
		return nil, err
	}
	if cfg.Database.SchemaWaitTimeout, err = parseDuration(v, "database.schema_wait_timeout", "schema_wait_timeout"); err != nil {
		return nil, err
	}
	if cfg.Database.SchemaPollInterval, err = parseDuration(v, "database.schema_poll_interval", "schema_poll_interval"); err != nil {
		return nil, err
	}
	if cfg.Indexer.GapScanInterval, err = parseDuration(v, "indexer.gap_scan_interval", "gap_scan_interval"); err != nil {
		return nil, err
	}
	if cfg.Attribution.BlobListRefreshInterval, err = parseDuration(v, "attribution.blob_list_refresh_interval", "blob_list_refresh_interval"); err != nil {
		return nil, err
	}
	if cfg.Attribution.BlobListRequestTimeout, err = parseDuration(v, "attribution.blob_list_request_timeout", "blob_list_request_timeout"); err != nil {
		return nil, err
	}
	if cfg.WebSocket.PollInterval, err = parseDuration(v, "websocket.poll_interval", "ws_poll_interval"); err != nil {
		return nil, err
	}
	if cfg.WebSocket.UsersThrottleInterval, err = parseDuration(v, "websocket.users_throttle_interval", "ws_users_throttle_interval"); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func parseDuration(v *viper.Viper, key, label string) (time.Duration, error) {
	duration, err := time.ParseDuration(v.GetString(key))
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", label, err)
	}
	return duration, nil
}

func applyCORSEnvOverrides(v *viper.Viper) {
	setEnvValue(v, "CORS_ENABLED", "cors.enabled")
	setEnvCSV(v, "CORS_ALLOWED_ORIGINS", "cors.allowed_origins")
	setEnvCSV(v, "CORS_ALLOWED_ORIGIN_PATTERNS", "cors.allowed_origin_patterns")
	setEnvValue(v, "CORS_ALLOW_ALL_ORIGINS", "cors.allow_all_origins")
	setEnvCSV(v, "CORS_ALLOWED_METHODS", "cors.allowed_methods")
	setEnvCSV(v, "CORS_ALLOWED_HEADERS", "cors.allowed_headers")
	setEnvCSV(v, "CORS_EXPOSED_HEADERS", "cors.exposed_headers")
	setEnvValue(v, "CORS_ALLOW_CREDENTIALS", "cors.allow_credentials")
	setEnvValue(v, "CORS_MAX_AGE_SECONDS", "cors.max_age_seconds")
}

func setEnvValue(v *viper.Viper, envName, key string) {
	if value, ok := os.LookupEnv(envName); ok {
		v.Set(key, value)
	}
}

func setEnvCSV(v *viper.Viper, envName, key string) {
	if value, ok := os.LookupEnv(envName); ok {
		v.Set(key, parseCommaSeparatedList(value))
	}
}

func parseCommaSeparatedList(value string) []string {
	parts := strings.Split(value, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			values = append(values, part)
		}
	}
	return values
}

func normalizeCORSConfig(cfg CORSConfig) CORSConfig {
	cfg.AllowedOrigins = normalizeStringList(cfg.AllowedOrigins)
	cfg.AllowedOriginPatterns = normalizeStringList(cfg.AllowedOriginPatterns)
	cfg.AllowedMethods = normalizeStringList(cfg.AllowedMethods)
	cfg.AllowedHeaders = normalizeStringList(cfg.AllowedHeaders)
	cfg.ExposedHeaders = normalizeStringList(cfg.ExposedHeaders)
	return cfg
}

func normalizeStringList(values []string) []string {
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			normalized = append(normalized, value)
		}
	}
	return normalized
}

// LoadForAPI loads configuration with relaxed validation (no RPC URL requirement).
func LoadForAPI() (*Config, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}

	if err := ValidateForAPI(cfg); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return cfg, nil
}

// validateConfig validates the configuration with full checks (used by indexer).
func validateConfig(cfg *Config) error {
	return validateConfigWithOptions(cfg, true)
}

// ValidateForAPI validates config for the API binary (RPC URLs not required).
func ValidateForAPI(cfg *Config) error {
	return validateConfigWithOptions(cfg, false)
}

// validateConfigWithOptions validates config. When requireRPC is true, each
// network must have an RPC URL and start block (indexer mode).
func validateConfigWithOptions(cfg *Config, requireRPC bool) error {
	logger.Info("Validating configuration")

	if err := validateServerPorts(cfg); err != nil {
		return err
	}
	if err := validateCORSConfig(cfg); err != nil {
		return err
	}

	// Validate database URL
	if cfg.Database.URL == "" {
		if os.Getenv("DB_URL") == "" {
			logger.Error("Database URL is required", zap.String("hint", "set DB_URL environment variable or add database.url to config file"))
			return fmt.Errorf("database URL is required - set DB_URL environment variable or add database.url to config file")
		}
		logger.Warn("Database URL is not set in config, but DB_URL environment variable is set")
		cfg.Database.URL = os.Getenv("DB_URL")
	}

	logger.Debug("Database URL configured", zap.String("url", maskConnectionString(cfg.Database.URL)))
	if err := validateDatabaseSSLMode(cfg); err != nil {
		return err
	}

	// Validate networks
	if len(cfg.Networks) == 0 {
		logger.Error("At least one network configuration is required")
		return fmt.Errorf("at least one network configuration is required")
	}
	logger.Info("Networks found in configuration", zap.Int("count", len(cfg.Networks)))

	for i, network := range cfg.Networks {
		logger.Debug("Validating network", zap.Int("index", i+1), zap.String("name", network.Name))

		if network.Name == "" {
			logger.Error("Network is missing a name", zap.Int("index", i+1))
			return fmt.Errorf("network #%d is missing a name", i+1)
		}

		if network.ChainID <= 0 {
			logger.Error("Network has invalid chain ID", zap.String("network", network.Name), zap.Int("chain_id", network.ChainID))
			return fmt.Errorf("network '%s' has an invalid chain ID: %d", network.Name, network.ChainID)
		}
		logger.Debug("Network chain id", zap.String("network", network.Name), zap.Int("chain_id", network.ChainID))

		if requireRPC {
			if network.RpcURL == "" {
				logger.Error("Network is missing an RPC URL", zap.String("network", network.Name))
				return fmt.Errorf("network '%s' is missing an RPC URL", network.Name)
			}
			logger.Debug("Network RPC URL configured", zap.String("network", network.Name), zap.String("rpc_url", maskURL(network.RpcURL)))

			if network.StartBlock == "" {
				logger.Error("Network is missing a start block", zap.String("network", network.Name))
				return fmt.Errorf("network '%s' is missing a start block", network.Name)
			}
			logger.Debug("Network start block configured", zap.String("network", network.Name), zap.String("start_block", network.StartBlock))
		}

		logger.Debug("Network enabled status", zap.String("network", network.Name), zap.Bool("enabled", network.Enabled))
	}

	logger.Info("Configuration validation successful")
	return nil
}

func validateCORSConfig(cfg *Config) error {
	if cfg.CORS.MaxAgeSeconds < 0 {
		return fmt.Errorf("cors.max_age_seconds must be greater than or equal to 0")
	}
	return nil
}

func validateServerPorts(cfg *Config) error {
	if cfg.Server.DevPort < 0 || cfg.Server.DevPort > 65535 {
		return fmt.Errorf("server.dev_port must be between 0 and 65535")
	}

	if cfg.Server.DevPort != 0 && cfg.Server.DevPort == cfg.Server.Port {
		return fmt.Errorf("server.dev_port must differ from server.port")
	}

	return nil
}

func validateDatabaseSSLMode(cfg *Config) error {
	dbURL, err := url.Parse(cfg.Database.URL)
	if err != nil {
		return fmt.Errorf("invalid database URL: %w", err)
	}

	sslMode := strings.ToLower(strings.TrimSpace(dbURL.Query().Get("sslmode")))
	if sslMode == "disable" && !cfg.Server.DevMode {
		return fmt.Errorf("database URL uses sslmode=disable while server.dev_mode=false")
	}

	if sslMode == "disable" && cfg.Server.DevMode {
		logger.Warn("Database TLS is disabled because dev mode is enabled")
	}

	return nil
}

// maskConnectionString masks a database connection string for security
func maskConnectionString(_ string) string {
	// Simple masking for now - in a real app you might want to parse the URL properly
	return maskedValue
}

// maskURL masks a URL for security
func maskURL(_ string) string {
	// Simple masking for now - in a real app you might want to parse the URL properly
	return maskedValue
}

// GetEnabledNetworks returns a slice of enabled network configurations
func (c *Config) GetEnabledNetworks() []NetworkConfig {
	var enabled []NetworkConfig
	for _, network := range c.Networks {
		if network.Enabled {
			enabled = append(enabled, network)
		}
	}
	return enabled
}

// GetNetworkByChainID returns a network configuration by chain ID
func (c *Config) GetNetworkByChainID(chainID int) (NetworkConfig, bool) {
	for _, network := range c.Networks {
		if network.ChainID == chainID {
			return network, true
		}
	}
	return NetworkConfig{}, false
}

// GetNetworkByName returns a network configuration by name
func (c *Config) GetNetworkByName(name string) (NetworkConfig, bool) {
	for _, network := range c.Networks {
		if network.Name == name {
			return network, true
		}
	}
	return NetworkConfig{}, false
}
