#!/usr/bin/env bash
set -euo pipefail

# Verifies that every indexer config key (internal/config/config.go
# IndexerConfig) is rendered into the ConfigMap, so operator overrides under
# appConfig.indexer are applied instead of being silently dropped in favor of
# the binary's viper defaults.

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
CHART_DIR=$(cd "$SCRIPT_DIR/.." && pwd)
REPO_DIR=$(cd "$CHART_DIR/../.." && pwd)
RELEASE=indexer-config-test
FULLNAME="${RELEASE}-blob-indexer"

# Derive the expected key set from the Go struct so a field added to
# IndexerConfig without chart plumbing fails this test. The sed strips tag
# options (e.g. `yaml:"key,omitempty"`) and the grep drops `yaml:"-"` fields.
expected_keys=$(awk '/^type IndexerConfig struct {/,/^}/' "$REPO_DIR/internal/config/config.go" \
  | grep -o 'yaml:"[^"]*"' | sed 's/^yaml:"//; s/"$//; s/,.*//' | grep -v '^-$' | grep -v '^$' \
  | sort | paste -sd, -)
if [ -z "$expected_keys" ]; then
  echo "Failed to extract IndexerConfig yaml keys from internal/config/config.go" >&2
  exit 1
fi

assert() {
  local output_file=$1
  local expected_json=$2

  ruby -ryaml -rjson - "$output_file" "$FULLNAME" "$expected_keys" "$expected_json" <<'RUBY'
output_file, fullname, expected_keys, expected_json = ARGV
docs = YAML.load_stream(File.read(output_file)).compact
cm = docs.find { |d| d.is_a?(Hash) && d["kind"] == "ConfigMap" && d.dig("metadata", "name") == "#{fullname}-config" }
cm || raise("ConfigMap missing")
config_yaml = cm.dig("data", "config.yaml") || raise("config.yaml missing from ConfigMap")
indexer = YAML.safe_load(config_yaml)["indexer"] || raise("indexer section missing from config.yaml")

rendered_keys = indexer.keys.sort
struct_keys = expected_keys.split(",").sort
unless rendered_keys == struct_keys
  raise "indexer key set mismatch:\n  IndexerConfig: #{struct_keys.inspect}\n  ConfigMap:     #{rendered_keys.inspect}"
end

JSON.parse(expected_json).each do |key, expected|
  actual = indexer[key]
  unless actual == expected && actual.class == expected.class
    raise "indexer.#{key}: expected #{expected.inspect} (#{expected.class}), got #{actual.inspect} (#{actual.class})"
  end
end

puts "OK"
RUBY
}

tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT

# 1. Chart defaults: every key renders with the values.yaml default (durations
#    as strings so viper's time.ParseDuration accepts them, counts as ints).
defaults="$tmp_dir/defaults.yaml"
helm template "$RELEASE" "$CHART_DIR" > "$defaults"
assert "$defaults" '{
  "version": "v1.0.0",
  "batch_size": 500,
  "polling_interval": "15s",
  "mempool_polling_interval": "30s",
  "mempool_ttl": "30m",
  "mempool_cleanup_interval": "5m",
  "worker_count": 4,
  "max_block_retries": 3,
  "gap_scan_interval": "5m",
  "max_reorg_depth": 64,
  "startup_gap_scan_blocks": 10000,
  "rpc_rate_limit": 0,
  "priority_fee_backfill_enabled": true,
  "priority_fee_backfill_pause": "250ms"
}'

# 2. Operator overrides: every key set under appConfig.indexer must land in the
#    rendered config. startup_gap_scan_blocks=0 guards the zero-value case (a
#    `| default` pipe would swallow it) and rpc_rate_limit exercises a float.
overrides="$tmp_dir/overrides.yaml"
helm template "$RELEASE" "$CHART_DIR" \
  --set-string appConfig.indexer.version=v9.9.9 \
  --set appConfig.indexer.batch_size=42 \
  --set-string appConfig.indexer.polling_interval=7s \
  --set-string appConfig.indexer.mempool_polling_interval=11s \
  --set-string appConfig.indexer.mempool_ttl=1h \
  --set-string appConfig.indexer.mempool_cleanup_interval=2m \
  --set appConfig.indexer.worker_count=8 \
  --set appConfig.indexer.max_block_retries=5 \
  --set-string appConfig.indexer.gap_scan_interval=10m \
  --set appConfig.indexer.max_reorg_depth=128 \
  --set appConfig.indexer.startup_gap_scan_blocks=0 \
  --set appConfig.indexer.rpc_rate_limit=12.5 \
  --set appConfig.indexer.priority_fee_backfill_enabled=false \
  --set-string appConfig.indexer.priority_fee_backfill_pause=2s \
  > "$overrides"
assert "$overrides" '{
  "version": "v9.9.9",
  "batch_size": 42,
  "polling_interval": "7s",
  "mempool_polling_interval": "11s",
  "mempool_ttl": "1h",
  "mempool_cleanup_interval": "2m",
  "worker_count": 8,
  "max_block_retries": 5,
  "gap_scan_interval": "10m",
  "max_reorg_depth": 128,
  "startup_gap_scan_blocks": 0,
  "rpc_rate_limit": 12.5,
  "priority_fee_backfill_enabled": false,
  "priority_fee_backfill_pause": "2s"
}'

echo "Indexer ConfigMap template tests passed."
