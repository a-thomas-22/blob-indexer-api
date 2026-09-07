package config

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSecret builds a throwaway MCP key long enough to satisfy
// MinMCPKeyLength. Fixtures compose their secrets from a repeated character
// rather than embedding realistic-looking literals, so secret scanners never
// see anything that resembles a real credential in the test data.
func fakeSecret(label string) string {
	return strings.Repeat("k", MinMCPKeyLength) + "-" + label
}

func validMCPConfig() *Config {
	return &Config{
		Database: DatabaseConfig{URL: "postgres://localhost:5432/test?sslmode=require"},
		Server:   ServerConfig{Port: 8080},
		Networks: []NetworkConfig{{Name: "mainnet", ChainID: 1, Enabled: true}},
		MCP: MCPConfig{
			Enabled:        true,
			Path:           "/mcp",
			RateLimitRPS:   5,
			RateLimitBurst: 20,
			Keys: []MCPKeyConfig{
				{Name: " community ", Key: " " + fakeSecret("community") + " ", Tools: []string{" get_blob_pricing ", ""}},
			},
		},
	}
}

func TestValidateMCPConfig_DisabledSkipsChecks(t *testing.T) {
	cfg := validMCPConfig()
	cfg.MCP.Enabled = false
	cfg.MCP.Keys = nil
	if err := ValidateForAPI(cfg); err != nil {
		t.Fatalf("disabled MCP must not require keys: %v", err)
	}
}

func TestValidateMCPConfig_SkippedForIndexer(t *testing.T) {
	// The indexer shares the ConfigMap (mcp.enabled + key names) but never
	// receives MCP_API_KEYS, so indexer-mode validation must not fail on the
	// missing secret.
	cfg := validMCPConfig()
	cfg.MCP.Keys[0].Key = ""
	cfg.Networks[0].RpcURL = "http://localhost:8545"
	cfg.Networks[0].StartBlock = "100"
	cfg.Networks[0].BeaconGenesisTime = 1700000000
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("indexer validation must ignore MCP secrets: %v", err)
	}
	if err := ValidateForAPI(cfg); err == nil {
		t.Fatal("API validation must still require the secret")
	}
}

func TestValidateMCPConfig_PathAcceptsNestedLiteral(t *testing.T) {
	cfg := validMCPConfig()
	cfg.MCP.Path = "/llm/v2/mcp-1.0_x~y"
	if err := ValidateForAPI(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateMCPConfig_NormalizesKeys(t *testing.T) {
	cfg := validMCPConfig()
	if err := ValidateForAPI(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	key := cfg.MCP.Keys[0]
	if key.Name != "community" || key.Key != fakeSecret("community") {
		t.Fatalf("expected trimmed name/key, got %q / %q", key.Name, key.Key)
	}
	if len(key.Tools) != 1 || key.Tools[0] != "get_blob_pricing" {
		t.Fatalf("expected normalized tools, got %v", key.Tools)
	}
}

func TestValidateMCPConfig_Errors(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(cfg *Config)
		want   string
	}{
		{"no keys", func(c *Config) { c.MCP.Keys = nil }, "mcp.keys must contain at least one key"},
		{"relative path", func(c *Config) { c.MCP.Path = "mcp" }, "mcp.path must be a literal absolute path"},
		{"root path", func(c *Config) { c.MCP.Path = "/" }, "mcp.path must be a literal absolute path"},
		{"trailing slash", func(c *Config) { c.MCP.Path = "/mcp/" }, "mcp.path must be a literal absolute path"},
		{"chi wildcard", func(c *Config) { c.MCP.Path = "/mcp/*" }, "mcp.path must be a literal absolute path"},
		{"chi param", func(c *Config) { c.MCP.Path = "/{mcp}" }, "mcp.path must be a literal absolute path"},
		{"whitespace", func(c *Config) { c.MCP.Path = "/mcp x" }, "mcp.path must be a literal absolute path"},
		{"query syntax", func(c *Config) { c.MCP.Path = "/mcp?x=1" }, "mcp.path must be a literal absolute path"},
		{"api path", func(c *Config) { c.MCP.Path = "/api/v1/mcp" }, "collides with the reserved route /api"},
		{"api exact", func(c *Config) { c.MCP.Path = "/api" }, "collides with the reserved route /api"},
		{"metrics path", func(c *Config) { c.MCP.Path = "/metrics" }, "collides with the reserved route /metrics"},
		{"swagger path", func(c *Config) { c.MCP.Path = "/swagger/mcp" }, "collides with the reserved route /swagger"},
		{"asyncapi path", func(c *Config) { c.MCP.Path = "/asyncapi.yaml" }, "collides with the reserved route /asyncapi.yaml"},
		{"zero rps", func(c *Config) { c.MCP.RateLimitRPS = 0 }, "mcp.rate_limit_rps"},
		{"nan rps", func(c *Config) { c.MCP.RateLimitRPS = math.NaN() }, "mcp.rate_limit_rps"},
		{"inf rps", func(c *Config) { c.MCP.RateLimitRPS = math.Inf(1) }, "mcp.rate_limit_rps"},
		{"huge rps", func(c *Config) { c.MCP.RateLimitRPS = MaxMCPRateLimitRPS + 1 }, "mcp.rate_limit_rps"},
		{"zero burst", func(c *Config) { c.MCP.RateLimitBurst = 0 }, "mcp.rate_limit_burst"},
		{"huge burst", func(c *Config) { c.MCP.RateLimitBurst = MaxMCPRateLimitRPS + 1 }, "mcp.rate_limit_burst"},
		{"missing name", func(c *Config) { c.MCP.Keys[0].Name = "" }, "mcp.keys[0] is missing a name"},
		{"missing secret", func(c *Config) { c.MCP.Keys[0].Key = "" }, `mcp key "community" has no secret`},
		{"short secret", func(c *Config) { c.MCP.Keys[0].Key = "tooshort" }, "shorter than 16 characters"},
		{"duplicate name", func(c *Config) {
			c.MCP.Keys = append(c.MCP.Keys, MCPKeyConfig{Name: "community", Key: fakeSecret("dup")})
		}, `duplicate name "community"`},
		{"duplicate secret", func(c *Config) {
			c.MCP.Keys = append(c.MCP.Keys, MCPKeyConfig{Name: "other", Key: fakeSecret("community")})
		}, `reuses another key's secret`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validMCPConfig()
			tc.mutate(cfg)
			err := ValidateForAPI(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestParseMCPAPIKeysEnv(t *testing.T) {
	keys, err := parseMCPAPIKeysEnv(" community:" + fakeSecret("community") + " , ops:" + fakeSecret("ops") + ":get_blob_pricing|list_networks| ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %+v", keys)
	}
	if keys[0].Name != "community" || keys[0].Key != fakeSecret("community") || keys[0].Tools != nil {
		t.Fatalf("keys[0] = %+v", keys[0])
	}
	if keys[1].Name != "ops" || keys[1].Key != fakeSecret("ops") || strings.Join(keys[1].Tools, ",") != "get_blob_pricing,list_networks" {
		t.Fatalf("keys[1] = %+v", keys[1])
	}

	// Malformed entries are errors, never silently dropped.
	for _, bad := range []string{"bad", ":nokey", "noname:", "ok:" + fakeSecret("ok") + ",bad"} {
		if _, err := parseMCPAPIKeysEnv(bad); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
	// Empty and whitespace-only values parse to no keys.
	if keys, err := parseMCPAPIKeysEnv(" , "); err != nil || len(keys) != 0 {
		t.Fatalf("expected no keys, got %+v %v", keys, err)
	}
}

func TestMergeMCPKeys(t *testing.T) {
	base := []MCPKeyConfig{
		{Name: "community", Tools: []string{"get_blob_pricing"}},
		{Name: "untouched", Key: fakeSecret("untouched")},
	}

	// Declared key: env supplies the secret only; the allowlist is untouched.
	merged, err := mergeMCPKeys(base, []MCPKeyConfig{{Name: "community", Key: fakeSecret("from-env")}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(merged) != 2 || merged[0].Key != fakeSecret("from-env") || strings.Join(merged[0].Tools, ",") != "get_blob_pricing" {
		t.Fatalf("env secret must fill YAML entry and keep its allowlist: %+v", merged)
	}
	if merged[1].Name != "untouched" || merged[1].Key != base[1].Key || len(merged[1].Tools) != 0 {
		t.Fatalf("unmatched YAML entry changed: %+v", merged[1])
	}
	if base[0].Key != "" {
		t.Fatal("mergeMCPKeys mutated its input")
	}

	// The Secret may not widen or alter the permission model.
	if _, err := mergeMCPKeys(base, []MCPKeyConfig{{Name: "community", Key: fakeSecret("env"), Tools: []string{"search"}}}); err == nil || !strings.Contains(err.Error(), "tool allowlists must be set in mcp.keys") {
		t.Fatalf("env tools for a declared key must be rejected, got %v", err)
	}
	if _, err := mergeMCPKeys(base, []MCPKeyConfig{{Name: "backdoor", Key: fakeSecret("env")}}); err == nil || !strings.Contains(err.Error(), `key "backdoor" is not declared in mcp.keys`) {
		t.Fatalf("undeclared env key must be rejected, got %v", err)
	}

	// With no YAML keys the env var is the whole configuration, tools included.
	merged, err = mergeMCPKeys(nil, []MCPKeyConfig{{Name: "solo", Key: fakeSecret("solo"), Tools: []string{"search"}}})
	if err != nil || len(merged) != 1 || merged[0].Name != "solo" || strings.Join(merged[0].Tools, ",") != "search" {
		t.Fatalf("env-only configuration not honored: %+v %v", merged, err)
	}
}

func TestLoadForAPI_MCPFromEnvAndFile(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.yaml")
	content := `
database:
  url: "postgres://localhost:5432/testdb?sslmode=require"
mcp:
  keys:
    - name: community
      tools: [get_blob_pricing, get_blob_market_overview]
networks:
  - name: testnet
    chain_id: 42
    enabled: true
`
	if err := os.WriteFile(configFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", configFile)
	t.Setenv("MCP_ENABLED", "true")
	t.Setenv("MCP_PATH", "/llm")
	t.Setenv("MCP_RATE_LIMIT_RPS", "2.5")
	t.Setenv("MCP_RATE_LIMIT_BURST", "7")
	t.Setenv("MCP_API_KEYS", "community:"+fakeSecret("community"))

	cfg, err := LoadForAPI()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.MCP.Enabled || cfg.MCP.Path != "/llm" || cfg.MCP.RateLimitRPS != 2.5 || cfg.MCP.RateLimitBurst != 7 {
		t.Fatalf("scalar env overrides not applied: %+v", cfg.MCP)
	}
	if len(cfg.MCP.Keys) != 1 {
		t.Fatalf("expected 1 key, got %+v", cfg.MCP.Keys)
	}
	if cfg.MCP.Keys[0].Key != fakeSecret("community") || strings.Join(cfg.MCP.Keys[0].Tools, ",") != "get_blob_pricing,get_blob_market_overview" {
		t.Fatalf("YAML allowlist + env secret not merged: %+v", cfg.MCP.Keys[0])
	}

	// A Secret naming an undeclared principal is a load error, not a new key.
	t.Setenv("MCP_API_KEYS", "community:"+fakeSecret("community")+",backdoor:"+fakeSecret("backdoor"))
	if _, err := LoadForAPI(); err == nil || !strings.Contains(err.Error(), "invalid MCP_API_KEYS") {
		t.Fatalf("expected undeclared env key to fail load, got %v", err)
	}
	// Malformed entries fail load too.
	t.Setenv("MCP_API_KEYS", "community")
	if _, err := LoadForAPI(); err == nil || !strings.Contains(err.Error(), "invalid MCP_API_KEYS") {
		t.Fatalf("expected malformed env to fail load, got %v", err)
	}
}

func TestLoadForAPI_MCPEnvOnlyConfiguration(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.yaml")
	content := `
database:
  url: "postgres://localhost:5432/testdb?sslmode=require"
networks:
  - name: testnet
    chain_id: 42
    enabled: true
`
	if err := os.WriteFile(configFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", configFile)
	t.Setenv("MCP_ENABLED", "true")
	t.Setenv("MCP_API_KEYS", "community:"+fakeSecret("community")+",ops:"+fakeSecret("ops")+":search")

	cfg, err := LoadForAPI()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.MCP.Keys) != 2 || cfg.MCP.Keys[1].Name != "ops" || strings.Join(cfg.MCP.Keys[1].Tools, ",") != "search" {
		t.Fatalf("env-only keys not loaded: %+v", cfg.MCP.Keys)
	}
}

func TestLoadForAPI_MCPDefaultsDisabled(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.yaml")
	content := `
database:
  url: "postgres://localhost:5432/testdb?sslmode=require"
networks:
  - name: testnet
    chain_id: 42
    enabled: true
`
	if err := os.WriteFile(configFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", configFile)
	cfg, err := LoadForAPI()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MCP.Enabled || cfg.MCP.Path != "/mcp" || cfg.MCP.RateLimitRPS != 5 || cfg.MCP.RateLimitBurst != 20 || len(cfg.MCP.Keys) != 0 {
		t.Fatalf("unexpected MCP defaults: %+v", cfg.MCP)
	}
}

func TestLoadForAPI_MCPEnabledWithoutKeysFails(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.yaml")
	content := `
database:
  url: "postgres://localhost:5432/testdb?sslmode=require"
mcp:
  enabled: true
networks:
  - name: testnet
    chain_id: 42
    enabled: true
`
	if err := os.WriteFile(configFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", configFile)
	_, err := LoadForAPI()
	if err == nil || !strings.Contains(err.Error(), "mcp.keys must contain at least one key") {
		t.Fatalf("expected fail-closed error, got %v", err)
	}
}
