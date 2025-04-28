package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// NetworkConfig holds the configuration for a single Ethereum network
type NetworkConfig struct {
	Name       string `yaml:"name"`
	ChainID    int    `yaml:"chain_id"`
	RpcURL     string `yaml:"rpc_url"`     // RPC URL is only stored in configuration, not in the database
	StartBlock string `yaml:"start_block"` // "LATEST", "LATEST-1000", or specific number
	Enabled    bool   `yaml:"enabled"`
}

// DatabaseConfig holds the database configuration
type DatabaseConfig struct {
	URL string `yaml:"url"`
}

// ServerConfig holds the server configuration
type ServerConfig struct {
	Port    int  `yaml:"port"`
	DevMode bool `yaml:"dev_mode"`
}

// LoggingConfig holds the logging configuration
type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// IndexerConfig holds the indexer configuration
type IndexerConfig struct {
	Version                string        `yaml:"version"`
	BatchSize              int           `yaml:"batch_size"`
	PollingInterval        time.Duration `yaml:"polling_interval"`
	MempoolPollingInterval time.Duration `yaml:"mempool_polling_interval"`
}

// Config holds the application configuration
type Config struct {
	Database DatabaseConfig  `yaml:"database"`
	Server   ServerConfig    `yaml:"server"`
	Logging  LoggingConfig   `yaml:"logging"`
	Indexer  IndexerConfig   `yaml:"indexer"`
	Networks []NetworkConfig `yaml:"networks"`
}

// Load loads the configuration from a YAML file and/or environment variables
func Load() (*Config, error) {
	// Default configuration
	cfg := &Config{
		Server: ServerConfig{
			Port:    8080,
			DevMode: false,
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "json",
		},
		Indexer: IndexerConfig{
			Version:                "v1.0.0",
			BatchSize:              100,
			PollingInterval:        15 * time.Second,
			MempoolPollingInterval: 30 * time.Second,
		},
		Networks: []NetworkConfig{},
	}

	// Try to load from config file
	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		// Look for config.yaml in the current directory
		if _, err := os.Stat("config.yaml"); err == nil {
			configPath = "config.yaml"
		}
	}

	if configPath != "" {
		if err := loadFromFile(cfg, configPath); err != nil {
			return nil, fmt.Errorf("failed to load config from file: %w", err)
		}
	}

	// Override with environment variables
	if err := overrideWithEnv(cfg); err != nil {
		return nil, fmt.Errorf("failed to override config with environment variables: %w", err)
	}

	// Validate configuration
	if err := validateConfig(cfg); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return cfg, nil
}

// loadFromFile loads configuration from a YAML file
func loadFromFile(cfg *Config, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read config file: %w", err)
	}

	// Determine if YAML or JSON based on file extension
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".yaml" || ext == ".yml" {
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return fmt.Errorf("failed to parse YAML config: %w", err)
		}
	} else {
		return fmt.Errorf("unsupported config file format: %s", ext)
	}

	return nil
}

// overrideWithEnv overrides configuration with environment variables
func overrideWithEnv(cfg *Config) error {
	// Database URL
	if dbURL := os.Getenv("DB_URL"); dbURL != "" {
		cfg.Database.URL = dbURL
	}

	// Server port
	if portStr := os.Getenv("PORT"); portStr != "" {
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return fmt.Errorf("invalid PORT value: %v", err)
		}
		cfg.Server.Port = port
	}

	// Development mode
	if devMode := os.Getenv("DEV_MODE"); devMode != "" {
		cfg.Server.DevMode = strings.ToLower(devMode) == "true"
	}

	// Indexer version
	if version := os.Getenv("INDEXER_VERSION"); version != "" {
		cfg.Indexer.Version = version
	}

	// Log level
	if logLevel := os.Getenv("LOG_LEVEL"); logLevel != "" {
		cfg.Logging.Level = logLevel
	}

	// Handle legacy RPC_URL and START_BLOCK for backward compatibility
	if rpcURL := os.Getenv("RPC_URL"); rpcURL != "" && len(cfg.Networks) == 0 {
		// Create a default mainnet network if none exists
		startBlock := os.Getenv("START_BLOCK")
		if startBlock == "" {
			startBlock = "LATEST"
		}

		cfg.Networks = append(cfg.Networks, NetworkConfig{
			Name:       "mainnet",
			ChainID:    1,
			RpcURL:     rpcURL,
			StartBlock: startBlock,
			Enabled:    true,
		})
	}

	// Override network-specific settings
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
			network.Enabled = strings.ToLower(enabled) == "true"
		}
	}

	return nil
}

// validateConfig validates the configuration
func validateConfig(cfg *Config) error {
	// Validate database URL
	if cfg.Database.URL == "" {
		return fmt.Errorf("database URL is required")
	}

	// Validate networks
	if len(cfg.Networks) == 0 {
		return fmt.Errorf("at least one network configuration is required")
	}

	for i, network := range cfg.Networks {
		if network.Name == "" {
			return fmt.Errorf("network #%d is missing a name", i+1)
		}

		if network.ChainID <= 0 {
			return fmt.Errorf("network '%s' has an invalid chain ID", network.Name)
		}

		if network.RpcURL == "" {
			return fmt.Errorf("network '%s' is missing an RPC URL", network.Name)
		}

		if network.StartBlock == "" {
			return fmt.Errorf("network '%s' is missing a start block", network.Name)
		}
	}

	return nil
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
