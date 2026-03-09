package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/viper"
)

const (
	networkMainnet = "mainnet"
	networkSepolia = "sepolia"
	maskedValue    = "****"
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
	URL string `mapstructure:"url" yaml:"url"`
}

// ServerConfig holds the server configuration
type ServerConfig struct {
	Port            int           `mapstructure:"port" yaml:"port"`
	DevMode         bool          `mapstructure:"dev_mode" yaml:"dev_mode"`
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout" yaml:"shutdown_timeout"`
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
}

// Config holds the application configuration
type Config struct {
	Database DatabaseConfig  `mapstructure:"database" yaml:"database"`
	Server   ServerConfig    `mapstructure:"server" yaml:"server"`
	Logging  LoggingConfig   `mapstructure:"logging" yaml:"logging"`
	Indexer  IndexerConfig   `mapstructure:"indexer" yaml:"indexer"`
	Networks []NetworkConfig `mapstructure:"networks" yaml:"networks"`
}

// Load loads the configuration using Viper from a YAML file and/or environment variables
func Load() (*Config, error) {
	v := viper.New()

	// Set default values
	v.SetDefault("server.port", 8080)
	v.SetDefault("server.dev_mode", false)
	v.SetDefault("server.shutdown_timeout", "15s")
	v.SetDefault("logging.level", "info")
	v.SetDefault("logging.format", "json")
	v.SetDefault("indexer.version", "v1.0.0")
	v.SetDefault("indexer.batch_size", 100)
	v.SetDefault("indexer.polling_interval", "15s")
	v.SetDefault("indexer.mempool_polling_interval", "30s")
	v.SetDefault("networks", []NetworkConfig{})

	// Configure Viper to read from config file
	v.SetConfigName("config") // name of config file (without extension)
	v.SetConfigType("yaml")   // YAML format
	v.AddConfigPath(".")      // look for config in the working directory

	// Check if CONFIG_PATH environment variable is set
	configPath := os.Getenv("CONFIG_PATH")
	if configPath != "" {
		// Use the specified config file
		fmt.Printf("CONFIG_PATH environment variable set to: %s\n", configPath)

		// Check if the file exists
		if _, err := os.Stat(configPath); os.IsNotExist(err) {
			fmt.Printf("WARNING: Config file not found at path: %s\n", configPath)

			// List files in the directory to help debug
			dir := filepath.Dir(configPath)
			if dir == "" {
				dir = "."
			}

			fmt.Printf("Listing files in directory: %s\n", dir)
			files, err := os.ReadDir(dir)
			if err != nil {
				fmt.Printf("Error reading directory: %v\n", err)
			} else {
				for _, file := range files {
					fmt.Printf("  - %s\n", file.Name())
				}
			}
		} else {
			fmt.Printf("Config file found at path: %s\n", configPath)
		}

		v.SetConfigFile(configPath)
	} else {
		fmt.Println("CONFIG_PATH environment variable not set, using default config file")
	}

	// Read the config file
	if err := v.ReadInConfig(); err != nil {
		// It's okay if the config file doesn't exist
		var configFileNotFoundError viper.ConfigFileNotFoundError
		if !errors.As(err, &configFileNotFoundError) {
			fmt.Printf("Error reading config file: %v\n", err)
			return nil, fmt.Errorf("failed to read config file: %w", err)
		}
		fmt.Println("Config file not found, continuing with defaults and environment variables")
	} else {
		fmt.Printf("Successfully loaded config from: %s\n", v.ConfigFileUsed())
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

	// Development mode - direct environment variable override
	if devMode := os.Getenv("DEV_MODE"); devMode != "" {
		v.Set("server.dev_mode", strings.EqualFold(devMode, "true"))
	}

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
	pollingInterval, err := time.ParseDuration(v.GetString("indexer.polling_interval"))
	if err != nil {
		return nil, fmt.Errorf("invalid polling_interval: %w", err)
	}
	cfg.Indexer.PollingInterval = pollingInterval

	mempoolPollingInterval, err := time.ParseDuration(v.GetString("indexer.mempool_polling_interval"))
	if err != nil {
		return nil, fmt.Errorf("invalid mempool_polling_interval: %w", err)
	}
	cfg.Indexer.MempoolPollingInterval = mempoolPollingInterval

	shutdownTimeout, err := time.ParseDuration(v.GetString("server.shutdown_timeout"))
	if err != nil {
		return nil, fmt.Errorf("invalid shutdown_timeout: %w", err)
	}
	cfg.Server.ShutdownTimeout = shutdownTimeout

	// Validate configuration
	if err := validateConfig(&cfg); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return &cfg, nil
}

// validateConfig validates the configuration
func validateConfig(cfg *Config) error {
	fmt.Println("Validating configuration...")

	// Validate database URL
	if cfg.Database.URL == "" {
		// Check if DB_URL environment variable is set
		if os.Getenv("DB_URL") == "" {
			fmt.Println("ERROR: Database URL is required - set DB_URL environment variable or add database.url to config file")
			return fmt.Errorf("database URL is required - set DB_URL environment variable or add database.url to config file")
		}
		fmt.Println("WARNING: Database URL is not set in config, but DB_URL environment variable is set")
		// Set the database URL from the environment variable
		cfg.Database.URL = os.Getenv("DB_URL")
	}

	fmt.Printf("Database URL: %s (masked for security)\n", maskConnectionString(cfg.Database.URL))

	// Validate networks
	if len(cfg.Networks) == 0 {
		fmt.Println("ERROR: At least one network configuration is required")
		return fmt.Errorf("at least one network configuration is required")
	}
	fmt.Printf("Found %d network(s) in configuration\n", len(cfg.Networks))

	for i, network := range cfg.Networks {
		fmt.Printf("Validating network #%d: %s\n", i+1, network.Name)

		if network.Name == "" {
			fmt.Printf("ERROR: Network #%d is missing a name\n", i+1)
			return fmt.Errorf("network #%d is missing a name", i+1)
		}

		if network.ChainID <= 0 {
			fmt.Printf("ERROR: Network '%s' has an invalid chain ID: %d\n", network.Name, network.ChainID)
			return fmt.Errorf("network '%s' has an invalid chain ID: %d", network.Name, network.ChainID)
		}
		fmt.Printf("  Chain ID: %d\n", network.ChainID)

		if network.RpcURL == "" {
			fmt.Printf("ERROR: Network '%s' is missing an RPC URL\n", network.Name)
			return fmt.Errorf("network '%s' is missing an RPC URL", network.Name)
		}
		fmt.Printf("  RPC URL: %s (masked for security)\n", maskURL(network.RpcURL))

		if network.StartBlock == "" {
			fmt.Printf("ERROR: Network '%s' is missing a start block\n", network.Name)
			return fmt.Errorf("network '%s' is missing a start block", network.Name)
		}
		fmt.Printf("  Start Block: %s\n", network.StartBlock)

		fmt.Printf("  Enabled: %v\n", network.Enabled)
	}

	fmt.Println("Configuration validation successful")
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
