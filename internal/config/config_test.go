package config

import (
	"os"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	// Test with environment variables
	os.Setenv("DB_URL", "postgres://test:test@localhost:5432/test")
	os.Setenv("PORT", "9090")
	os.Setenv("RPC_URL", "https://test.infura.io/v3/test")
	os.Setenv("START_BLOCK", "12345")
	os.Setenv("INDEXER_VERSION", "v2.0.0")
	os.Setenv("DEV_MODE", "true")
	os.Setenv("LOG_LEVEL", "debug")

	// Load configuration
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Failed to load configuration: %v", err)
	}

	// Check database URL
	if cfg.Database.URL != "postgres://test:test@localhost:5432/test" {
		t.Errorf("Expected Database.URL to be 'postgres://test:test@localhost:5432/test', got '%s'", cfg.Database.URL)
	}

	// Check server port
	if cfg.Server.Port != 9090 {
		t.Errorf("Expected Server.Port to be 9090, got %d", cfg.Server.Port)
	}

	// Check dev mode
	if !cfg.Server.DevMode {
		t.Errorf("Expected Server.DevMode to be true, got %v", cfg.Server.DevMode)
	}

	// Check indexer version
	if cfg.Indexer.Version != "v2.0.0" {
		t.Errorf("Expected Indexer.Version to be 'v2.0.0', got '%s'", cfg.Indexer.Version)
	}

	// Check log level
	if cfg.Logging.Level != "debug" {
		t.Errorf("Expected Logging.Level to be 'debug', got '%s'", cfg.Logging.Level)
	}

	// Check networks
	if len(cfg.Networks) != 1 {
		t.Errorf("Expected 1 network, got %d", len(cfg.Networks))
	} else {
		network := cfg.Networks[0]
		if network.Name != "mainnet" {
			t.Errorf("Expected network name to be 'mainnet', got '%s'", network.Name)
		}
		if network.ChainID != 1 {
			t.Errorf("Expected network chain ID to be 1, got %d", network.ChainID)
		}
		if network.RpcURL != "https://test.infura.io/v3/test" {
			t.Errorf("Expected network RPC URL to be 'https://test.infura.io/v3/test', got '%s'", network.RpcURL)
		}
		if network.StartBlock != "12345" {
			t.Errorf("Expected network start block to be '12345', got '%s'", network.StartBlock)
		}
		if !network.Enabled {
			t.Errorf("Expected network to be enabled, got %v", network.Enabled)
		}
	}

	// Test with multiple networks
	os.Setenv("NETWORK_SEPOLIA_RPC_URL", "https://sepolia.infura.io/v3/test")
	os.Setenv("NETWORK_SEPOLIA_START_BLOCK", "5000")
	os.Setenv("NETWORK_SEPOLIA_ENABLED", "true")

	// Load configuration again
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Failed to load configuration: %v", err)
	}

	// Check networks
	if len(cfg.Networks) != 1 {
		t.Errorf("Expected 1 network, got %d", len(cfg.Networks))
	}

	// Clean up
	os.Unsetenv("DB_URL")
	os.Unsetenv("PORT")
	os.Unsetenv("RPC_URL")
	os.Unsetenv("START_BLOCK")
	os.Unsetenv("INDEXER_VERSION")
	os.Unsetenv("DEV_MODE")
	os.Unsetenv("LOG_LEVEL")
	os.Unsetenv("NETWORK_SEPOLIA_RPC_URL")
	os.Unsetenv("NETWORK_SEPOLIA_START_BLOCK")
	os.Unsetenv("NETWORK_SEPOLIA_ENABLED")
}

func TestGetEnabledNetworks(t *testing.T) {
	cfg := &Config{
		Networks: []NetworkConfig{
			{
				Name:       "mainnet",
				ChainID:    1,
				RpcURL:     "https://mainnet.infura.io/v3/test",
				StartBlock: "LATEST-1000",
				Enabled:    true,
			},
			{
				Name:       "sepolia",
				ChainID:    11155111,
				RpcURL:     "https://sepolia.infura.io/v3/test",
				StartBlock: "LATEST-100",
				Enabled:    false,
			},
		},
	}

	enabled := cfg.GetEnabledNetworks()
	if len(enabled) != 1 {
		t.Errorf("Expected 1 enabled network, got %d", len(enabled))
	}
	if enabled[0].Name != "mainnet" {
		t.Errorf("Expected enabled network to be 'mainnet', got '%s'", enabled[0].Name)
	}
}

func TestGetNetworkByChainID(t *testing.T) {
	cfg := &Config{
		Networks: []NetworkConfig{
			{
				Name:       "mainnet",
				ChainID:    1,
				RpcURL:     "https://mainnet.infura.io/v3/test",
				StartBlock: "LATEST-1000",
				Enabled:    true,
			},
			{
				Name:       "sepolia",
				ChainID:    11155111,
				RpcURL:     "https://sepolia.infura.io/v3/test",
				StartBlock: "LATEST-100",
				Enabled:    false,
			},
		},
	}

	// Test with existing chain ID
	network, found := cfg.GetNetworkByChainID(1)
	if !found {
		t.Errorf("Expected to find network with chain ID 1")
	}
	if network.Name != "mainnet" {
		t.Errorf("Expected network name to be 'mainnet', got '%s'", network.Name)
	}

	// Test with non-existent chain ID
	_, found = cfg.GetNetworkByChainID(999)
	if found {
		t.Errorf("Expected not to find network with chain ID 999")
	}
}

func TestGetNetworkByName(t *testing.T) {
	cfg := &Config{
		Networks: []NetworkConfig{
			{
				Name:       "mainnet",
				ChainID:    1,
				RpcURL:     "https://mainnet.infura.io/v3/test",
				StartBlock: "LATEST-1000",
				Enabled:    true,
			},
			{
				Name:       "sepolia",
				ChainID:    11155111,
				RpcURL:     "https://sepolia.infura.io/v3/test",
				StartBlock: "LATEST-100",
				Enabled:    false,
			},
		},
	}

	// Test with existing name
	network, found := cfg.GetNetworkByName("mainnet")
	if !found {
		t.Errorf("Expected to find network with name 'mainnet'")
	}
	if network.ChainID != 1 {
		t.Errorf("Expected network chain ID to be 1, got %d", network.ChainID)
	}

	// Test with non-existent name
	_, found = cfg.GetNetworkByName("goerli")
	if found {
		t.Errorf("Expected not to find network with name 'goerli'")
	}
}

func TestLoadFromFile(t *testing.T) {
	// Create a temporary config file
	content := `
database:
  url: "postgres://test:test@localhost:5432/test"

server:
  port: 9090
  dev_mode: true

logging:
  level: "debug"
  format: "json"

indexer:
  version: "v2.0.0"
  batch_size: 200
  polling_interval: 30s
  mempool_polling_interval: 60s

networks:
  - name: "mainnet"
    chain_id: 1
    rpc_url: "https://mainnet.infura.io/v3/test"
    start_block: "LATEST-1000"
    enabled: true
    
  - name: "sepolia"
    chain_id: 11155111
    rpc_url: "https://sepolia.infura.io/v3/test"
    start_block: "LATEST-100"
    enabled: false
`
	tmpfile, err := os.CreateTemp("", "config-*.yaml")
	if err != nil {
		t.Fatalf("Failed to create temporary file: %v", err)
	}
	defer os.Remove(tmpfile.Name())

	if _, err := tmpfile.Write([]byte(content)); err != nil {
		t.Fatalf("Failed to write to temporary file: %v", err)
	}
	if err := tmpfile.Close(); err != nil {
		t.Fatalf("Failed to close temporary file: %v", err)
	}

	// Set the config path environment variable
	os.Setenv("CONFIG_PATH", tmpfile.Name())
	defer os.Unsetenv("CONFIG_PATH")

	// Load configuration
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Failed to load configuration: %v", err)
	}

	// Check database URL
	if cfg.Database.URL != "postgres://test:test@localhost:5432/test" {
		t.Errorf("Expected Database.URL to be 'postgres://test:test@localhost:5432/test', got '%s'", cfg.Database.URL)
	}

	// Check server port
	if cfg.Server.Port != 9090 {
		t.Errorf("Expected Server.Port to be 9090, got %d", cfg.Server.Port)
	}

	// Check dev mode
	if !cfg.Server.DevMode {
		t.Errorf("Expected Server.DevMode to be true, got %v", cfg.Server.DevMode)
	}

	// Check indexer version
	if cfg.Indexer.Version != "v2.0.0" {
		t.Errorf("Expected Indexer.Version to be 'v2.0.0', got '%s'", cfg.Indexer.Version)
	}

	// Check indexer batch size
	if cfg.Indexer.BatchSize != 200 {
		t.Errorf("Expected Indexer.BatchSize to be 200, got %d", cfg.Indexer.BatchSize)
	}

	// Check indexer polling interval
	if cfg.Indexer.PollingInterval != 30*time.Second {
		t.Errorf("Expected Indexer.PollingInterval to be 30s, got %s", cfg.Indexer.PollingInterval)
	}

	// Check indexer mempool polling interval
	if cfg.Indexer.MempoolPollingInterval != 60*time.Second {
		t.Errorf("Expected Indexer.MempoolPollingInterval to be 60s, got %s", cfg.Indexer.MempoolPollingInterval)
	}

	// Check log level
	if cfg.Logging.Level != "debug" {
		t.Errorf("Expected Logging.Level to be 'debug', got '%s'", cfg.Logging.Level)
	}

	// Check networks
	if len(cfg.Networks) != 2 {
		t.Errorf("Expected 2 networks, got %d", len(cfg.Networks))
	} else {
		// Check mainnet
		mainnet := cfg.Networks[0]
		if mainnet.Name != "mainnet" {
			t.Errorf("Expected network name to be 'mainnet', got '%s'", mainnet.Name)
		}
		if mainnet.ChainID != 1 {
			t.Errorf("Expected network chain ID to be 1, got %d", mainnet.ChainID)
		}
		if mainnet.RpcURL != "https://mainnet.infura.io/v3/test" {
			t.Errorf("Expected network RPC URL to be 'https://mainnet.infura.io/v3/test', got '%s'", mainnet.RpcURL)
		}
		if mainnet.StartBlock != "LATEST-1000" {
			t.Errorf("Expected network start block to be 'LATEST-1000', got '%s'", mainnet.StartBlock)
		}
		if !mainnet.Enabled {
			t.Errorf("Expected network to be enabled, got %v", mainnet.Enabled)
		}

		// Check sepolia
		sepolia := cfg.Networks[1]
		if sepolia.Name != "sepolia" {
			t.Errorf("Expected network name to be 'sepolia', got '%s'", sepolia.Name)
		}
		if sepolia.ChainID != 11155111 {
			t.Errorf("Expected network chain ID to be 11155111, got %d", sepolia.ChainID)
		}
		if sepolia.RpcURL != "https://sepolia.infura.io/v3/test" {
			t.Errorf("Expected network RPC URL to be 'https://sepolia.infura.io/v3/test', got '%s'", sepolia.RpcURL)
		}
		if sepolia.StartBlock != "LATEST-100" {
			t.Errorf("Expected network start block to be 'LATEST-100', got '%s'", sepolia.StartBlock)
		}
		if sepolia.Enabled {
			t.Errorf("Expected network to be disabled, got %v", sepolia.Enabled)
		}
	}
}
