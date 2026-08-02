package config

import (
	"testing"
	"time"
)

func TestLoadForAPI(t *testing.T) {
	t.Setenv("CONFIG_PATH", "../../config.yaml")
	t.Setenv("DB_URL", "postgres://test:test@localhost:5432/testdb")

	// LoadForAPI should succeed even without RPC URLs set
	cfg, err := LoadForAPI()
	if err != nil {
		t.Fatalf("LoadForAPI failed: %v", err)
	}

	if cfg.Database.URL != "postgres://test:test@localhost:5432/testdb" {
		t.Errorf("Expected DB URL from env, got: %s", cfg.Database.URL)
	}
	if cfg.Database.SchemaWaitTimeout != 2*time.Minute {
		t.Errorf("Expected schema wait timeout to default to 2m, got: %s", cfg.Database.SchemaWaitTimeout)
	}
	if cfg.Database.SchemaPollInterval != 2*time.Second {
		t.Errorf("Expected schema poll interval to default to 2s, got: %s", cfg.Database.SchemaPollInterval)
	}
	if cfg.WebSocket.MaxClients != 10000 {
		t.Errorf("Expected websocket max_clients to default to 10000, got: %d", cfg.WebSocket.MaxClients)
	}
	if cfg.WebSocket.MaxConnsPerIP != 32 {
		t.Errorf("Expected websocket max_conns_per_ip to default to 32, got: %d", cfg.WebSocket.MaxConnsPerIP)
	}

	if len(cfg.Networks) == 0 {
		t.Fatal("Expected at least one network")
	}
}

func TestValidateForAPI_NoRPCRequired(t *testing.T) {
	cfg := &Config{
		Database: DatabaseConfig{URL: "postgres://localhost/test"},
		Networks: []NetworkConfig{
			{Name: "test", ChainID: 1, Enabled: true},
		},
	}

	if err := ValidateForAPI(cfg); err != nil {
		t.Errorf("ValidateForAPI should not require RPC URL, got: %v", err)
	}
}

func TestValidateForAPI_DevPortMustDifferFromMainPort(t *testing.T) {
	cfg := &Config{
		Database: DatabaseConfig{URL: "postgres://localhost/test"},
		Server:   ServerConfig{Port: 8080, DevPort: 8080},
		Networks: []NetworkConfig{
			{Name: "test", ChainID: 1, Enabled: true},
		},
	}

	if err := ValidateForAPI(cfg); err == nil {
		t.Fatal("expected validation error when dev_port equals port")
	}
}

func TestValidateForAPI_DevPortAllowsDisabledDedicatedListener(t *testing.T) {
	cfg := &Config{
		Database: DatabaseConfig{URL: "postgres://localhost/test"},
		Server:   ServerConfig{Port: 8080, DevPort: 0},
		Networks: []NetworkConfig{
			{Name: "test", ChainID: 1, Enabled: true},
		},
	}

	if err := ValidateForAPI(cfg); err != nil {
		t.Fatalf("expected validation to pass with dev_port disabled, got: %v", err)
	}
}

func TestValidateConfig_RequiresRPC(t *testing.T) {
	cfg := &Config{
		Database: DatabaseConfig{URL: "postgres://localhost/test"},
		Networks: []NetworkConfig{
			{Name: "test", ChainID: 1, Enabled: true},
		},
	}

	if err := validateConfig(cfg); err == nil {
		t.Error("validateConfig should require RPC URL")
	}
}

func TestValidateConfig_RequiresBeaconClock(t *testing.T) {
	// Indexed blob rows must carry the beacon slot, so indexer-mode
	// validation refuses a network whose slot derivation is impossible:
	// unknown chain ID with no beacon_genesis_time configured.
	cfg := &Config{
		Database: DatabaseConfig{URL: "postgres://localhost/test"},
		Networks: []NetworkConfig{
			{Name: "devnet", ChainID: 999999, RpcURL: "http://localhost:8545", StartBlock: "100", Enabled: true},
		},
	}
	if err := validateConfig(cfg); err == nil {
		t.Fatal("validateConfig should reject an unknown chain without beacon_genesis_time")
	}

	// Configuring the genesis time makes the same network valid.
	cfg.Networks[0].BeaconGenesisTime = 1700000000
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("expected validation to pass with beacon_genesis_time set, got: %v", err)
	}

	// Known chains need no configuration.
	cfg.Networks[0] = NetworkConfig{Name: "hoodi", ChainID: 560048, RpcURL: "http://localhost:8545", StartBlock: "100", Enabled: true}
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("expected validation to pass for a known chain, got: %v", err)
	}
}

func TestValidateForAPI_DoesNotRequireBeaconClock(t *testing.T) {
	// The API serves whatever a past indexer wrote and omits slot when it
	// cannot derive it, so an unknown chain without beacon_genesis_time must
	// stay valid in API mode.
	cfg := &Config{
		Database: DatabaseConfig{URL: "postgres://localhost/test"},
		Networks: []NetworkConfig{
			{Name: "devnet", ChainID: 999999, Enabled: true},
		},
	}
	if err := ValidateForAPI(cfg); err != nil {
		t.Fatalf("ValidateForAPI should not require a beacon clock, got: %v", err)
	}
}

func TestValidateDatabaseSSLMode_ProdRejectsDisable(t *testing.T) {
	cfg := &Config{
		Database: DatabaseConfig{URL: "postgres://localhost:5432/test?sslmode=disable"},
		Server:   ServerConfig{DevMode: false},
		Networks: []NetworkConfig{
			{Name: "test", ChainID: 1, Enabled: true},
		},
	}

	if err := ValidateForAPI(cfg); err == nil {
		t.Fatal("expected validation error for sslmode=disable with dev_mode=false")
	}
}

func TestValidateDatabaseSSLMode_DevAllowsDisable(t *testing.T) {
	cfg := &Config{
		Database: DatabaseConfig{URL: "postgres://localhost:5432/test?sslmode=disable"},
		Server:   ServerConfig{DevMode: true, DevAPIKey: "test-key"},
		Networks: []NetworkConfig{
			{Name: "test", ChainID: 1, Enabled: true},
		},
	}

	if err := ValidateForAPI(cfg); err != nil {
		t.Fatalf("expected validation to pass in dev mode, got: %v", err)
	}
}

func TestViperConfig(t *testing.T) {
	t.Setenv("CONFIG_PATH", "../../config.yaml")
	t.Setenv("DB_URL", "postgres://test:test@localhost:5432/testdb")
	t.Setenv("NETWORK_MAINNET_RPC_URL", "https://mainnet.example.com")
	t.Setenv("NETWORK_SEPOLIA_RPC_URL", "https://sepolia.example.com")

	// Load the configuration
	cfg, err := Load()
	if err != nil {
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

	if cfg.Server.DevMode != true {
		t.Errorf("Expected server dev mode to be true, got: %v", cfg.Server.DevMode)
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

	if cfg.Indexer.BatchSize != 50 {
		t.Errorf("Expected batch size to be 50, got: %d", cfg.Indexer.BatchSize)
	}

	if cfg.Indexer.PollingInterval != 5*time.Second {
		t.Errorf("Expected polling interval to be 5s, got: %s", cfg.Indexer.PollingInterval)
	}

	if cfg.Indexer.MempoolPollingInterval != 15*time.Second {
		t.Errorf("Expected mempool polling interval to be 15s, got: %s", cfg.Indexer.MempoolPollingInterval)
	}

	// Verify networks configuration
	if len(cfg.Networks) != 2 {
		t.Fatalf("Expected 2 networks, got: %d", len(cfg.Networks))
	}

	var sepolia *NetworkConfig
	for i := range cfg.Networks {
		if cfg.Networks[i].Name == "sepolia" {
			sepolia = &cfg.Networks[i]
			break
		}
	}

	if sepolia == nil {
		t.Fatal("Expected sepolia network to be configured")
	}

	if sepolia.ChainID != 11155111 {
		t.Errorf("Expected sepolia chain ID to be 11155111, got: %d", sepolia.ChainID)
	}

	if sepolia.StartBlock != "LATEST-100" {
		t.Errorf("Expected sepolia start block to be LATEST-100, got: %s", sepolia.StartBlock)
	}

	// RPC URL should be from environment variable, not config file
	if sepolia.RpcURL != "https://sepolia.example.com" {
		t.Errorf("Expected sepolia RPC URL to be from environment variable, got: %s", sepolia.RpcURL)
	}

	if !sepolia.Enabled {
		t.Errorf("Expected sepolia to be enabled")
	}
}
