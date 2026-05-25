package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoad_WithEnvVars(t *testing.T) {
	// Create a temp config file
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.yaml")
	configContent := `
database:
  url: "postgres://localhost:5432/testdb"
server:
  port: 9090
  dev_mode: true
logging:
  level: debug
  format: json
indexer:
  version: "v-test"
  batch_size: 50
  polling_interval: "10s"
  mempool_polling_interval: "20s"
networks:
  - name: testnet
    chain_id: 42
    rpc_url: "http://localhost:8545"
    start_block: "100"
    enabled: true
`
	if err := os.WriteFile(configFile, []byte(configContent), 0o644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	t.Setenv("CONFIG_PATH", configFile)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Database.URL != "postgres://localhost:5432/testdb" {
		t.Errorf("expected database URL 'postgres://localhost:5432/testdb', got %q", cfg.Database.URL)
	}
	if cfg.Server.Port != 9090 {
		t.Errorf("expected port 9090, got %d", cfg.Server.Port)
	}
	if !cfg.Server.DevMode {
		t.Error("expected dev_mode = true")
	}
	if cfg.Logging.Level != "debug" {
		t.Errorf("expected log level 'debug', got %q", cfg.Logging.Level)
	}
	if cfg.Indexer.Version != "v-test" {
		t.Errorf("expected indexer version 'v-test', got %q", cfg.Indexer.Version)
	}
	if cfg.Indexer.BatchSize != 50 {
		t.Errorf("expected batch_size 50, got %d", cfg.Indexer.BatchSize)
	}
	if len(cfg.Networks) != 1 {
		t.Fatalf("expected 1 network, got %d", len(cfg.Networks))
	}
	if cfg.Networks[0].Name != "testnet" {
		t.Errorf("expected network name 'testnet', got %q", cfg.Networks[0].Name)
	}
}

func TestLoad_DBURLOverride(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.yaml")
	configContent := `
database:
  url: "postgres://original:5432/db"
networks:
  - name: testnet
    chain_id: 1
    rpc_url: "http://localhost:8545"
    start_block: "0"
    enabled: true
`
	if err := os.WriteFile(configFile, []byte(configContent), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_PATH", configFile)
	t.Setenv("DB_URL", "postgres://override:5432/db")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Database.URL != "postgres://override:5432/db" {
		t.Errorf("expected DB_URL override, got %q", cfg.Database.URL)
	}
}

func TestLoad_PortOverride(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.yaml")
	configContent := `
database:
  url: "postgres://localhost:5432/db"
networks:
  - name: testnet
    chain_id: 1
    rpc_url: "http://localhost:8545"
    start_block: "0"
    enabled: true
`
	if err := os.WriteFile(configFile, []byte(configContent), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_PATH", configFile)
	t.Setenv("PORT", "3000")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.Port != 3000 {
		t.Errorf("expected port 3000, got %d", cfg.Server.Port)
	}
}

func TestLoad_DevPortOverride(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.yaml")
	configContent := `
database:
  url: "postgres://localhost:5432/db"
networks:
  - name: testnet
    chain_id: 1
    rpc_url: "http://localhost:8545"
    start_block: "0"
    enabled: true
`
	if err := os.WriteFile(configFile, []byte(configContent), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_PATH", configFile)
	t.Setenv("DEV_PORT", "3001")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.DevPort != 3001 {
		t.Errorf("expected dev port 3001, got %d", cfg.Server.DevPort)
	}
}

func TestLoad_DevModeOverride(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.yaml")
	configContent := `
database:
  url: "postgres://localhost:5432/db"
server:
  dev_mode: false
networks:
  - name: testnet
    chain_id: 1
    rpc_url: "http://localhost:8545"
    start_block: "0"
    enabled: true
`
	if err := os.WriteFile(configFile, []byte(configContent), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_PATH", configFile)
	t.Setenv("DEV_MODE", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Server.DevMode {
		t.Error("expected dev_mode = true from env override")
	}
}

func TestLoad_IndexerVersionOverride(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.yaml")
	configContent := `
database:
  url: "postgres://localhost:5432/db"
networks:
  - name: testnet
    chain_id: 1
    rpc_url: "http://localhost:8545"
    start_block: "0"
    enabled: true
`
	if err := os.WriteFile(configFile, []byte(configContent), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_PATH", configFile)
	t.Setenv("INDEXER_VERSION", "v-env")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Indexer.Version != "v-env" {
		t.Errorf("expected indexer version 'v-env', got %q", cfg.Indexer.Version)
	}
}

func TestLoad_LogLevelOverride(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.yaml")
	configContent := `
database:
  url: "postgres://localhost:5432/db"
networks:
  - name: testnet
    chain_id: 1
    rpc_url: "http://localhost:8545"
    start_block: "0"
    enabled: true
`
	if err := os.WriteFile(configFile, []byte(configContent), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_PATH", configFile)
	t.Setenv("LOG_LEVEL", "warn")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Logging.Level != "warn" {
		t.Errorf("expected log level 'warn', got %q", cfg.Logging.Level)
	}
}

func TestLoad_LegacyRPCURL(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.yaml")
	configContent := `
database:
  url: "postgres://localhost:5432/db"
`
	if err := os.WriteFile(configFile, []byte(configContent), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_PATH", configFile)
	t.Setenv("RPC_URL", "http://localhost:8545")
	t.Setenv("START_BLOCK", "12345")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Networks) != 1 {
		t.Fatalf("expected 1 network from legacy RPC_URL, got %d", len(cfg.Networks))
	}
	if cfg.Networks[0].Name != "mainnet" {
		t.Errorf("expected network name 'mainnet', got %q", cfg.Networks[0].Name)
	}
	if cfg.Networks[0].ChainID != 1 {
		t.Errorf("expected chain ID 1, got %d", cfg.Networks[0].ChainID)
	}
	if cfg.Networks[0].StartBlock != "12345" {
		t.Errorf("expected start block '12345', got %q", cfg.Networks[0].StartBlock)
	}
}

func TestLoad_LegacyRPCURL_Sepolia(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.yaml")
	configContent := `
database:
  url: "postgres://localhost:5432/db"
`
	if err := os.WriteFile(configFile, []byte(configContent), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_PATH", configFile)
	t.Setenv("RPC_URL", "https://sepolia.infura.io/v3/xxx")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Networks) != 1 {
		t.Fatalf("expected 1 network, got %d", len(cfg.Networks))
	}
	if cfg.Networks[0].Name != "sepolia" {
		t.Errorf("expected network name 'sepolia', got %q", cfg.Networks[0].Name)
	}
	if cfg.Networks[0].ChainID != 11155111 {
		t.Errorf("expected chain ID 11155111, got %d", cfg.Networks[0].ChainID)
	}
}

func TestLoad_LegacyRPCURL_Holesky(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.yaml")
	configContent := `
database:
  url: "postgres://localhost:5432/db"
`
	if err := os.WriteFile(configFile, []byte(configContent), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_PATH", configFile)
	t.Setenv("RPC_URL", "https://holesky.example.com/rpc")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Networks[0].Name != "holesky" {
		t.Errorf("expected network name 'holesky', got %q", cfg.Networks[0].Name)
	}
	if cfg.Networks[0].ChainID != 17000 {
		t.Errorf("expected chain ID 17000, got %d", cfg.Networks[0].ChainID)
	}
}

func TestLoad_LegacyRPCURL_Goerli(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.yaml")
	configContent := `
database:
  url: "postgres://localhost:5432/db"
`
	if err := os.WriteFile(configFile, []byte(configContent), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_PATH", configFile)
	t.Setenv("RPC_URL", "https://goerli.infura.io/v3/xxx")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Networks[0].Name != "goerli" {
		t.Errorf("expected network name 'goerli', got %q", cfg.Networks[0].Name)
	}
	if cfg.Networks[0].ChainID != 5 {
		t.Errorf("expected chain ID 5, got %d", cfg.Networks[0].ChainID)
	}
}

func TestLoad_LegacyRPCURL_DefaultStartBlock(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.yaml")
	configContent := `
database:
  url: "postgres://localhost:5432/db"
`
	if err := os.WriteFile(configFile, []byte(configContent), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_PATH", configFile)
	t.Setenv("RPC_URL", "http://localhost:8545")
	// Don't set START_BLOCK - should default to "LATEST"

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Networks[0].StartBlock != "LATEST" {
		t.Errorf("expected start block 'LATEST', got %q", cfg.Networks[0].StartBlock)
	}
}

func TestLoad_NetworkEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.yaml")
	configContent := `
database:
  url: "postgres://localhost:5432/db"
networks:
  - name: testnet
    chain_id: 1
    rpc_url: "http://original:8545"
    start_block: "0"
    enabled: true
`
	if err := os.WriteFile(configFile, []byte(configContent), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_PATH", configFile)
	t.Setenv("NETWORK_TESTNET_RPC_URL", "http://override:8545")
	t.Setenv("NETWORK_TESTNET_START_BLOCK", "999")
	t.Setenv("NETWORK_TESTNET_ENABLED", "false")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Networks[0].RpcURL != "http://override:8545" {
		t.Errorf("expected RPC URL override, got %q", cfg.Networks[0].RpcURL)
	}
	if cfg.Networks[0].StartBlock != "999" {
		t.Errorf("expected start block '999', got %q", cfg.Networks[0].StartBlock)
	}
	if cfg.Networks[0].Enabled {
		t.Error("expected enabled = false from env override")
	}
}

func TestLoad_MissingConfigFile(t *testing.T) {
	t.Setenv("CONFIG_PATH", "/nonexistent/config.yaml")
	_, err := Load()
	if err == nil {
		t.Error("expected error for missing config file")
	}
}

func TestLoad_InvalidPollingInterval(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.yaml")
	configContent := `
database:
  url: "postgres://localhost:5432/db"
indexer:
  polling_interval: "not-a-duration"
networks:
  - name: testnet
    chain_id: 1
    rpc_url: "http://localhost:8545"
    start_block: "0"
    enabled: true
`
	if err := os.WriteFile(configFile, []byte(configContent), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_PATH", configFile)

	_, err := Load()
	if err == nil {
		t.Error("expected error for invalid polling interval")
	}
}

func TestLoad_ValidationFailure(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.yaml")
	configContent := `
server:
  port: 8080
`
	if err := os.WriteFile(configFile, []byte(configContent), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_PATH", configFile)
	t.Setenv("DB_URL", "")

	_, err := Load()
	if err == nil {
		t.Error("expected validation error")
	}
}

func TestLoad_InvalidMempoolPollingInterval(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.yaml")
	configContent := `
database:
  url: "postgres://localhost:5432/db"
indexer:
  polling_interval: "10s"
  mempool_polling_interval: "not-a-duration"
networks:
  - name: testnet
    chain_id: 1
    rpc_url: "http://localhost:8545"
    start_block: "0"
    enabled: true
`
	if err := os.WriteFile(configFile, []byte(configContent), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_PATH", configFile)

	_, err := Load()
	if err == nil {
		t.Error("expected error for invalid mempool polling interval")
	}
}

func TestLoad_InvalidMempoolTTL(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.yaml")
	configContent := `
database:
  url: "postgres://localhost:5432/db"
indexer:
  polling_interval: "10s"
  mempool_ttl: "not-a-duration"
networks:
  - name: testnet
    chain_id: 1
    rpc_url: "http://localhost:8545"
    start_block: "0"
    enabled: true
`
	if err := os.WriteFile(configFile, []byte(configContent), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_PATH", configFile)

	_, err := Load()
	if err == nil {
		t.Error("expected error for invalid mempool TTL")
	}
}

func TestLoad_InvalidMempoolCleanupInterval(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.yaml")
	configContent := `
database:
  url: "postgres://localhost:5432/db"
indexer:
  polling_interval: "10s"
  mempool_cleanup_interval: "not-a-duration"
networks:
  - name: testnet
    chain_id: 1
    rpc_url: "http://localhost:8545"
    start_block: "0"
    enabled: true
`
	if err := os.WriteFile(configFile, []byte(configContent), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_PATH", configFile)

	_, err := Load()
	if err == nil {
		t.Error("expected error for invalid mempool cleanup interval")
	}
}

func TestLoad_MempoolTTLDefaults(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.yaml")
	configContent := `
database:
  url: "postgres://localhost:5432/db"
networks:
  - name: testnet
    chain_id: 1
    rpc_url: "http://localhost:8545"
    start_block: "0"
    enabled: true
`
	if err := os.WriteFile(configFile, []byte(configContent), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_PATH", configFile)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Indexer.MempoolTTL != 30*time.Minute {
		t.Errorf("expected default mempool TTL 30m, got %s", cfg.Indexer.MempoolTTL)
	}
	if cfg.Indexer.MempoolCleanupInterval != 5*time.Minute {
		t.Errorf("expected default mempool cleanup interval 5m, got %s", cfg.Indexer.MempoolCleanupInterval)
	}
}

func TestLoad_ETHRPCURLOverride(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.yaml")
	configContent := `
database:
  url: "postgres://localhost:5432/db"
`
	if err := os.WriteFile(configFile, []byte(configContent), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_PATH", configFile)
	t.Setenv("RPC_URL", "http://original:8545")
	t.Setenv("ETH_RPC_URL", "http://preferred:8545")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Networks[0].RpcURL != "http://preferred:8545" {
		t.Errorf("expected ETH_RPC_URL to take precedence, got %q", cfg.Networks[0].RpcURL)
	}
}

func TestLoad_NoConfigFile(t *testing.T) {
	// Test loading with no config file set and no CONFIG_PATH
	t.Setenv("CONFIG_PATH", "")
	t.Setenv("DB_URL", "postgres://localhost:5432/db")
	t.Setenv("RPC_URL", "http://localhost:8545")
	t.Setenv("START_BLOCK", "0")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Database.URL != "postgres://localhost:5432/db" {
		t.Errorf("expected DB URL from env, got %q", cfg.Database.URL)
	}
}

func TestLoad_MissingConfigFileInExistingDir(t *testing.T) {
	// Set CONFIG_PATH to a non-existent file in an existing directory
	// This triggers the directory listing debug path (lines 96-99)
	dir := t.TempDir()
	t.Setenv("CONFIG_PATH", filepath.Join(dir, "nonexistent.yaml"))

	_, err := Load()
	if err == nil {
		t.Error("expected error for missing config file")
	}
}

func TestValidateConfig_DBURLFromEnv(t *testing.T) {
	cfg := &Config{
		Database: DatabaseConfig{URL: ""},
		Networks: []NetworkConfig{
			{Name: "test", ChainID: 1, RpcURL: "http://localhost:8545", StartBlock: "0", Enabled: true},
		},
	}
	t.Setenv("DB_URL", "postgres://from-env:5432/db")
	err := validateConfig(cfg)
	if err != nil {
		t.Errorf("expected no error when DB_URL is set via env, got: %v", err)
	}
	if cfg.Database.URL != "postgres://from-env:5432/db" {
		t.Errorf("expected DB URL from env, got %q", cfg.Database.URL)
	}
}
