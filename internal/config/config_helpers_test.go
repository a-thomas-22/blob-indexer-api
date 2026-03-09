package config

import (
	"testing"
)

func TestGetEnabledNetworks(t *testing.T) {
	cfg := &Config{
		Networks: []NetworkConfig{
			{Name: "mainnet", ChainID: 1, Enabled: true},
			{Name: "sepolia", ChainID: 11155111, Enabled: false},
			{Name: "holesky", ChainID: 17000, Enabled: true},
		},
	}

	enabled := cfg.GetEnabledNetworks()
	if len(enabled) != 2 {
		t.Fatalf("expected 2 enabled networks, got %d", len(enabled))
	}

	if enabled[0].Name != "mainnet" {
		t.Errorf("expected first enabled network 'mainnet', got %q", enabled[0].Name)
	}
	if enabled[1].Name != "holesky" {
		t.Errorf("expected second enabled network 'holesky', got %q", enabled[1].Name)
	}
}

func TestGetEnabledNetworks_NoneEnabled(t *testing.T) {
	cfg := &Config{
		Networks: []NetworkConfig{
			{Name: "mainnet", Enabled: false},
		},
	}

	enabled := cfg.GetEnabledNetworks()
	if len(enabled) != 0 {
		t.Errorf("expected 0 enabled networks, got %d", len(enabled))
	}
}

func TestGetEnabledNetworks_Empty(t *testing.T) {
	cfg := &Config{}
	enabled := cfg.GetEnabledNetworks()
	if len(enabled) != 0 {
		t.Errorf("expected 0 enabled networks, got %d", len(enabled))
	}
}

func TestGetNetworkByChainID(t *testing.T) {
	cfg := &Config{
		Networks: []NetworkConfig{
			{Name: "mainnet", ChainID: 1},
			{Name: "sepolia", ChainID: 11155111},
		},
	}

	network, ok := cfg.GetNetworkByChainID(1)
	if !ok {
		t.Fatal("expected to find mainnet")
	}
	if network.Name != "mainnet" {
		t.Errorf("expected 'mainnet', got %q", network.Name)
	}

	network, ok = cfg.GetNetworkByChainID(11155111)
	if !ok {
		t.Fatal("expected to find sepolia")
	}
	if network.Name != "sepolia" {
		t.Errorf("expected 'sepolia', got %q", network.Name)
	}
}

func TestGetNetworkByChainID_NotFound(t *testing.T) {
	cfg := &Config{
		Networks: []NetworkConfig{
			{Name: "mainnet", ChainID: 1},
		},
	}

	_, ok := cfg.GetNetworkByChainID(999)
	if ok {
		t.Error("expected not to find network with chain ID 999")
	}
}

func TestGetNetworkByName(t *testing.T) {
	cfg := &Config{
		Networks: []NetworkConfig{
			{Name: "mainnet", ChainID: 1},
			{Name: "sepolia", ChainID: 11155111},
		},
	}

	network, ok := cfg.GetNetworkByName("sepolia")
	if !ok {
		t.Fatal("expected to find sepolia")
	}
	if network.ChainID != 11155111 {
		t.Errorf("expected chain ID 11155111, got %d", network.ChainID)
	}
}

func TestGetNetworkByName_NotFound(t *testing.T) {
	cfg := &Config{
		Networks: []NetworkConfig{
			{Name: "mainnet", ChainID: 1},
		},
	}

	_, ok := cfg.GetNetworkByName("goerli")
	if ok {
		t.Error("expected not to find network with name 'goerli'")
	}
}

func TestMaskConnectionString(t *testing.T) {
	result := maskConnectionString("postgres://user:pass@localhost:5432/db")
	if result != "****" {
		t.Errorf("expected '****', got %q", result)
	}
}

func TestMaskURL(t *testing.T) {
	result := maskURL("https://mainnet.infura.io/v3/my-api-key")
	if result != "****" {
		t.Errorf("expected '****', got %q", result)
	}
}

func TestNetworkConfigStruct(t *testing.T) {
	nc := NetworkConfig{
		Name:       "mainnet",
		ChainID:    1,
		RpcURL:     "https://mainnet.example.com",
		StartBlock: "LATEST-1000",
		Enabled:    true,
	}

	if nc.Name != "mainnet" {
		t.Errorf("expected Name 'mainnet', got %q", nc.Name)
	}
	if nc.ChainID != 1 {
		t.Errorf("expected ChainID 1, got %d", nc.ChainID)
	}
	if nc.RpcURL != "https://mainnet.example.com" {
		t.Errorf("expected RpcURL, got %q", nc.RpcURL)
	}
	if nc.StartBlock != "LATEST-1000" {
		t.Errorf("expected StartBlock 'LATEST-1000', got %q", nc.StartBlock)
	}
	if !nc.Enabled {
		t.Error("expected Enabled to be true")
	}
}
