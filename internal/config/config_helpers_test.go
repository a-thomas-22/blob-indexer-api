package config

import (
	"testing"
)

func TestGetEnabledNetworks(t *testing.T) {
	cfg := &Config{
		Networks: []NetworkConfig{
			{Name: "mainnet", ChainID: 1, Enabled: true},
			{Name: "testnet", ChainID: 5, Enabled: false},
			{Name: "sepolia", ChainID: 11155111, Enabled: true},
		},
	}

	enabled := cfg.GetEnabledNetworks()
	if len(enabled) != 2 {
		t.Fatalf("expected 2 enabled networks, got %d", len(enabled))
	}
	if enabled[0].Name != "mainnet" {
		t.Errorf("expected first enabled network 'mainnet', got %q", enabled[0].Name)
	}
	if enabled[1].Name != "sepolia" {
		t.Errorf("expected second enabled network 'sepolia', got %q", enabled[1].Name)
	}
}

func TestGetEnabledNetworks_NoneEnabled(t *testing.T) {
	cfg := &Config{
		Networks: []NetworkConfig{
			{Name: "mainnet", ChainID: 1, Enabled: false},
		},
	}
	enabled := cfg.GetEnabledNetworks()
	if len(enabled) != 0 {
		t.Fatalf("expected 0 enabled networks, got %d", len(enabled))
	}
}

func TestGetNetworkByChainID_Found(t *testing.T) {
	cfg := &Config{
		Networks: []NetworkConfig{
			{Name: "mainnet", ChainID: 1, Enabled: true},
			{Name: "sepolia", ChainID: 11155111, Enabled: true},
		},
	}

	n, ok := cfg.GetNetworkByChainID(11155111)
	if !ok {
		t.Fatal("expected to find network")
	}
	if n.Name != "sepolia" {
		t.Errorf("expected 'sepolia', got %q", n.Name)
	}
}

func TestGetNetworkByChainID_NotFound(t *testing.T) {
	cfg := &Config{
		Networks: []NetworkConfig{
			{Name: "mainnet", ChainID: 1, Enabled: true},
		},
	}

	_, ok := cfg.GetNetworkByChainID(999)
	if ok {
		t.Error("expected not found")
	}
}

func TestGetNetworkByName_Found(t *testing.T) {
	cfg := &Config{
		Networks: []NetworkConfig{
			{Name: "mainnet", ChainID: 1, Enabled: true},
		},
	}

	n, ok := cfg.GetNetworkByName("mainnet")
	if !ok {
		t.Fatal("expected to find network")
	}
	if n.ChainID != 1 {
		t.Errorf("expected chain ID 1, got %d", n.ChainID)
	}
}

func TestGetNetworkByName_NotFound(t *testing.T) {
	cfg := &Config{
		Networks: []NetworkConfig{
			{Name: "mainnet", ChainID: 1, Enabled: true},
		},
	}

	_, ok := cfg.GetNetworkByName("goerli")
	if ok {
		t.Error("expected not found")
	}
}

func TestValidateConfig_MissingDatabaseURL(t *testing.T) {
	cfg := &Config{
		Networks: []NetworkConfig{
			{Name: "test", ChainID: 1, RpcURL: "http://localhost:8545", StartBlock: "0", Enabled: true},
		},
	}
	// Temporarily unset DB_URL to test validation
	t.Setenv("DB_URL", "")
	err := validateConfig(cfg)
	if err == nil {
		t.Error("expected error for missing database URL")
	}
}

func TestValidateConfig_MissingNetworks(t *testing.T) {
	cfg := &Config{
		Database: DatabaseConfig{URL: "postgres://localhost/test"},
	}
	err := validateConfig(cfg)
	if err == nil {
		t.Error("expected error for missing networks")
	}
}

func TestValidateConfig_MissingNetworkName(t *testing.T) {
	cfg := &Config{
		Database: DatabaseConfig{URL: "postgres://localhost/test"},
		Networks: []NetworkConfig{
			{Name: "", ChainID: 1, RpcURL: "http://localhost:8545", StartBlock: "0", Enabled: true},
		},
	}
	err := validateConfig(cfg)
	if err == nil {
		t.Error("expected error for missing network name")
	}
}

func TestValidateConfig_InvalidChainID(t *testing.T) {
	cfg := &Config{
		Database: DatabaseConfig{URL: "postgres://localhost/test"},
		Networks: []NetworkConfig{
			{Name: "test", ChainID: 0, RpcURL: "http://localhost:8545", StartBlock: "0", Enabled: true},
		},
	}
	err := validateConfig(cfg)
	if err == nil {
		t.Error("expected error for invalid chain ID")
	}
}

func TestValidateConfig_MissingRpcURL(t *testing.T) {
	cfg := &Config{
		Database: DatabaseConfig{URL: "postgres://localhost/test"},
		Networks: []NetworkConfig{
			{Name: "test", ChainID: 1, RpcURL: "", StartBlock: "0", Enabled: true},
		},
	}
	err := validateConfig(cfg)
	if err == nil {
		t.Error("expected error for missing RPC URL")
	}
}

func TestValidateConfig_MissingStartBlock(t *testing.T) {
	cfg := &Config{
		Database: DatabaseConfig{URL: "postgres://localhost/test"},
		Networks: []NetworkConfig{
			{Name: "test", ChainID: 1, RpcURL: "http://localhost:8545", StartBlock: "", Enabled: true},
		},
	}
	err := validateConfig(cfg)
	if err == nil {
		t.Error("expected error for missing start block")
	}
}

func TestValidateConfig_Success(t *testing.T) {
	cfg := &Config{
		Database: DatabaseConfig{URL: "postgres://localhost/test"},
		Networks: []NetworkConfig{
			{Name: "test", ChainID: 1, RpcURL: "http://localhost:8545", StartBlock: "0", Enabled: true},
		},
	}
	err := validateConfig(cfg)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateConfig_CORSCredentialsWithAllowAllOrigins(t *testing.T) {
	cfg := &Config{
		Database: DatabaseConfig{URL: "postgres://localhost/test"},
		Networks: []NetworkConfig{
			{Name: "test", ChainID: 1, RpcURL: "http://localhost:8545", StartBlock: "0", Enabled: true},
		},
		CORS: CORSConfig{AllowAllOrigins: true, AllowCredentials: true},
	}
	if err := validateConfig(cfg); err == nil {
		t.Fatal("expected error for allow_credentials combined with allow-all origins")
	}
}

func TestValidateConfig_DevModeRequiresAPIKey(t *testing.T) {
	cfg := &Config{
		Database: DatabaseConfig{URL: "postgres://localhost/test"},
		Networks: []NetworkConfig{
			{Name: "test", ChainID: 1, RpcURL: "http://localhost:8545", StartBlock: "0", Enabled: true},
		},
		Server: ServerConfig{DevMode: true, DevAPIKey: ""},
	}
	if err := validateConfig(cfg); err == nil {
		t.Fatal("expected error when dev_mode is true but dev_api_key is empty")
	}
}

func TestMaskConnectionString(t *testing.T) {
	result := maskConnectionString("postgres://user:pass@localhost:5432/db")
	if result != "****" {
		t.Errorf("expected '****', got %q", result)
	}
}

func TestMaskURL(t *testing.T) {
	result := maskURL("https://api.example.com/v1?key=secret")
	if result != "****" {
		t.Errorf("expected '****', got %q", result)
	}
}
