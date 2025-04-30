package config

import (
	"os"
	"testing"
	"time"
)

func TestViperConfig(t *testing.T) {
	// Save original environment variables to restore later
	originalConfigPath := os.Getenv("CONFIG_PATH")
	originalDBURL := os.Getenv("DB_URL")
	originalMainnetRPCURL := os.Getenv("NETWORK_MAINNET_RPC_URL")
	originalSepoliaRPCURL := os.Getenv("NETWORK_SEPOLIA_RPC_URL")

	// Clean up after the test
	defer func() {
		os.Setenv("CONFIG_PATH", originalConfigPath)
		os.Setenv("DB_URL", originalDBURL)
		os.Setenv("NETWORK_MAINNET_RPC_URL", originalMainnetRPCURL)
		os.Setenv("NETWORK_SEPOLIA_RPC_URL", originalSepoliaRPCURL)
	}()

	// Set up test environment
	os.Setenv("CONFIG_PATH", "../../railway-config.yaml")
	os.Setenv("DB_URL", "postgres://test:test@localhost:5432/testdb")
	os.Setenv("NETWORK_MAINNET_RPC_URL", "https://mainnet.example.com")
	os.Setenv("NETWORK_SEPOLIA_RPC_URL", "https://sepolia.example.com")

	// Load the configuration
	cfg, err := Load()
	if err != nil {
		// Print more debug information
		t.Logf("Error loading configuration: %v", err)

		// Try to load the railway-config.yaml file directly to see its contents
		data, readErr := os.ReadFile("../../railway-config.yaml")
		if readErr != nil {
			t.Logf("Error reading railway-config.yaml: %v", readErr)
		} else {
			t.Logf("railway-config.yaml contents:\n%s", string(data))
		}

		t.Fatalf("Failed to load configuration: %v", err)
	}

	// Verify database URL from environment variable
	if cfg.Database.URL != "postgres://test:test@localhost:5432/testdb" {
		t.Errorf("Expected DB URL to be from environment variable, got: %s", cfg.Database.URL)
	}

	// Verify server configuration from config file
	if cfg.Server.Port != 8080 {
		t.Errorf("Expected server port to be 8080, got: %d", cfg.Server.Port)
	}

	if cfg.Server.DevMode != false {
		t.Errorf("Expected server dev mode to be false, got: %v", cfg.Server.DevMode)
	}

	// Verify logging configuration from config file
	if cfg.Logging.Level != "info" {
		t.Errorf("Expected logging level to be info, got: %s", cfg.Logging.Level)
	}

	if cfg.Logging.Format != "json" {
		t.Errorf("Expected logging format to be json, got: %s", cfg.Logging.Format)
	}

	// Verify indexer configuration from config file
	if cfg.Indexer.Version != "v1.0.0" {
		t.Errorf("Expected indexer version to be v1.0.0, got: %s", cfg.Indexer.Version)
	}

	if cfg.Indexer.BatchSize != 100 {
		t.Errorf("Expected batch size to be 100, got: %d", cfg.Indexer.BatchSize)
	}

	if cfg.Indexer.PollingInterval != 15*time.Second {
		t.Errorf("Expected polling interval to be 15s, got: %s", cfg.Indexer.PollingInterval)
	}

	if cfg.Indexer.MempoolPollingInterval != 30*time.Second {
		t.Errorf("Expected mempool polling interval to be 30s, got: %s", cfg.Indexer.MempoolPollingInterval)
	}

	// Verify networks configuration
	if len(cfg.Networks) != 1 {
		t.Errorf("Expected 1 network, got: %d", len(cfg.Networks))
	}

	// Verify sepolia network configuration
	sepolia := cfg.Networks[0]
	if sepolia.Name != "sepolia" {
		t.Errorf("Expected second network name to be sepolia, got: %s", sepolia.Name)
	}

	if sepolia.ChainID != 11155111 {
		t.Errorf("Expected sepolia chain ID to be 11155111, got: %d", sepolia.ChainID)
	}

	if sepolia.StartBlock != "5187051" {
		t.Errorf("Expected sepolia start block to be 5187051, got: %s", sepolia.StartBlock)
	}

	// RPC URL should be from environment variable, not config file
	if sepolia.RpcURL != "https://sepolia.example.com" {
		t.Errorf("Expected sepolia RPC URL to be from environment variable, got: %s", sepolia.RpcURL)
	}

	if !sepolia.Enabled {
		t.Errorf("Expected sepolia to be enabled")
	}
}
