#!/usr/bin/env bash
set -euo pipefail

# Verifies that per-network RPC URLs (which embed provider API keys) are injected
# as NETWORK_<NAME>_RPC_URL env vars sourced from a Secret, and never rendered
# into the credential-free ConfigMap.

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
CHART_DIR=$(cd "$SCRIPT_DIR/.." && pwd)
RELEASE=rpc-secret-test
FULLNAME="${RELEASE}-blob-indexer"

# A non-secret placeholder; gitleaks-safe.
PLACEHOLDER_URL="https://eth-mainnet.example.com/v2/PLACEHOLDER_KEY"

# Base values used by every case. Networks are defined in a values file (not via
# --set) because `--set appConfig.networks[0].rpc_url=...` without a base array
# replaces the whole element and drops name/chain_id. CI applies its --set RPC
# override on top of values-test.yaml for the same reason.
base_values() {
  cat <<YAML
appConfig:
  database:
    url: "postgres://u:p@pg:5432/blobindexer?sslmode=require"
  networks:
    - name: "mainnet"
      chain_id: 1
      rpc_url: "$1"
      start_block: "LATEST-1000"
      enabled: true
YAML
}

render() {
  local output_file=$1
  local rpc_url=$2
  shift 2
  local values_file
  values_file=$(mktemp)
  base_values "$rpc_url" > "$values_file"
  helm template "$RELEASE" "$CHART_DIR" -f "$values_file" "$@" > "$output_file"
  rm -f "$values_file"
}

assert() {
  local output_file=$1
  local expect_secret_name=$2     # name of the Secret env should reference
  local expect_chart_secret=$3    # "true" if the chart should render its own RPC Secret
  local expect_secret_value=$4    # "__none__" or the expected stringData value in the chart Secret

  ruby -ryaml - "$output_file" "$FULLNAME" "$expect_secret_name" "$expect_chart_secret" "$expect_secret_value" <<'RUBY'
output_file, fullname, expect_secret_name, expect_chart_secret, expect_secret_value = ARGV
docs = YAML.load_stream(File.read(output_file)).compact

def find_doc(docs, kind, name)
  docs.find { |d| d.is_a?(Hash) && d["kind"] == kind && d.dig("metadata", "name") == name }
end

def rpc_env(doc)
  containers = doc.dig("spec", "template", "spec", "containers") || []
  containers.flat_map { |c| c["env"] || [] }.find { |e| e["name"] == "NETWORK_MAINNET_RPC_URL" }
end

api = find_doc(docs, "Deployment", "#{fullname}-api") || raise("API Deployment missing")
indexer = find_doc(docs, "StatefulSet", "#{fullname}-indexer") || raise("Indexer StatefulSet missing")

[["API", api], ["Indexer", indexer]].each do |label, doc|
  env = rpc_env(doc) || raise("#{label}: NETWORK_MAINNET_RPC_URL env missing")
  ref = env.dig("valueFrom", "secretKeyRef") || raise("#{label}: RPC URL not sourced from a Secret")
  raise "#{label}: env value leaks a plaintext RPC URL" if env["value"]
  unless ref["name"] == expect_secret_name && ref["key"] == "NETWORK_MAINNET_RPC_URL"
    raise "#{label}: expected secretKeyRef #{expect_secret_name}/NETWORK_MAINNET_RPC_URL, got #{ref.inspect}"
  end
end

# The ConfigMap must never carry an rpc_url credential.
cm = find_doc(docs, "ConfigMap", "#{fullname}-config") || raise("ConfigMap missing")
config_yaml = cm.dig("data", "config.yaml") || raise("config.yaml missing from ConfigMap")
parsed = YAML.safe_load(config_yaml)
(parsed["networks"] || []).each do |net|
  unless net["rpc_url"].to_s.empty?
    raise "ConfigMap leaks rpc_url for network #{net["name"].inspect}: #{net["rpc_url"].inspect}"
  end
end

chart_secret = find_doc(docs, "Secret", expect_secret_name)
if expect_chart_secret == "true"
  raise "Expected chart-managed RPC Secret #{expect_secret_name}" unless chart_secret
  value = chart_secret.dig("stringData", "NETWORK_MAINNET_RPC_URL")
  if expect_secret_value != "__none__" && value != expect_secret_value
    raise "Chart RPC Secret value mismatch: expected #{expect_secret_value.inspect}, got #{value.inspect}"
  end
else
  raise "Did not expect a chart-managed RPC Secret #{expect_secret_name}" if chart_secret
end

puts "OK"
RUBY
}

tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT

# 1. Chart-managed Secret from values: rpc_url in values -> rendered into <fullname>-rpc.
chart_managed="$tmp_dir/chart-managed.yaml"
render "$chart_managed" "$PLACEHOLDER_URL"
assert "$chart_managed" "${FULLNAME}-rpc" "true" "$PLACEHOLDER_URL"

# 2. existingSecret: no chart Secret; env references the pre-provisioned Secret;
#    rpc_url left empty in values.
existing="$tmp_dir/existing.yaml"
render "$existing" "" \
  --set-string rpcSecret.existingSecret="prebaked-rpc"
assert "$existing" "prebaked-rpc" "false" "__none__"

# 3. Custom chart Secret name override.
custom_name="$tmp_dir/custom-name.yaml"
render "$custom_name" "$PLACEHOLDER_URL" \
  --set-string rpcSecret.name="custom-rpc"
assert "$custom_name" "custom-rpc" "true" "$PLACEHOLDER_URL"

echo "Chart RPC Secret template tests passed."
