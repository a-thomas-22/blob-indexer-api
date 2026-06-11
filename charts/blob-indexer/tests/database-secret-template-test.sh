#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
CHART_DIR=$(cd "$SCRIPT_DIR/.." && pwd)
RELEASE=db-secret-test
FULLNAME="${RELEASE}-blob-indexer"

render() {
  local output_file=$1
  shift
  helm template "$RELEASE" "$CHART_DIR" "$@" > "$output_file"
}

assert_rendered_refs() {
  local rendered_file=$1
  local expected_app_secret=$2
  local expected_app_key=$3
  local expected_migration_secret=$4
  local expected_migration_key=$5
  local expect_created_secret=$6

  ruby -ryaml - "$rendered_file" "$FULLNAME" "$expected_app_secret" "$expected_app_key" \
    "$expected_migration_secret" "$expected_migration_key" "$expect_created_secret" <<'RUBY'
rendered_file, fullname, app_secret, app_key, migration_secret, migration_key, expect_created_secret = ARGV
docs = YAML.load_stream(File.read(rendered_file)).compact

def find_doc(docs, kind, name)
  docs.find { |doc| doc.is_a?(Hash) && doc["kind"] == kind && doc.dig("metadata", "name") == name }
end

def db_url_ref(doc)
  env = db_url_env(doc)

  ref = env.dig("valueFrom", "secretKeyRef")
  raise "DB_URL is not sourced from a Secret in #{doc.dig("kind")} #{doc.dig("metadata", "name")}" unless ref

  [ref["name"], ref["key"]]
end

def db_url_env(doc)
  containers = doc.dig("spec", "template", "spec", "containers") || []
  env = containers.flat_map { |container| container["env"] || [] }.find { |item| item["name"] == "DB_URL" }
  raise "DB_URL env var missing in #{doc.dig("kind")} #{doc.dig("metadata", "name")}" unless env

  env
end

def assert_equal(label, expected, actual)
  return if expected == actual

  raise "#{label}: expected #{expected.inspect}, got #{actual.inspect}"
end

api = find_doc(docs, "Deployment", "#{fullname}-api") || raise("API Deployment missing")
indexer = find_doc(docs, "StatefulSet", "#{fullname}-indexer") || raise("Indexer StatefulSet missing")
job = find_doc(docs, "Job", "#{fullname}-migrate") || raise("Migration Job missing")

assert_equal("API DB Secret ref", [app_secret, app_key], db_url_ref(api))
assert_equal("Indexer DB Secret ref", [app_secret, app_key], db_url_ref(indexer))

if migration_secret == "__literal__"
  migration_env = db_url_env(job)
  raise "Expected migration DB_URL to use a literal value" unless migration_env["value"]
  raise "Did not expect migration DB_URL to use a Secret" if migration_env.dig("valueFrom", "secretKeyRef")
else
  assert_equal("Migration DB Secret ref", [migration_secret, migration_key], db_url_ref(job))
end

created_secret = find_doc(docs, "Secret", app_secret)
if expect_created_secret == "true"
  raise "Expected chart-created DB Secret #{app_secret} to render" unless created_secret
  raise "Expected chart-created DB Secret key #{app_key}" unless created_secret.dig("stringData", app_key)
else
  raise "Did not expect chart-created DB Secret #{app_secret} to render" if created_secret
end
RUBY
}

tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT

chart_created="$tmp_dir/chart-created.yaml"
render "$chart_created" \
  --set-string appConfig.database.url="postgres://user:pass@postgres:5432/blobindexer?sslmode=require"
assert_rendered_refs "$chart_created" "${FULLNAME}-db" "DB_URL" "__literal__" "" "true"

external_secret="$tmp_dir/external-secret.yaml"
render "$external_secret" \
  --set databaseSecret.create=false \
  --set-string databaseSecret.name="external-db" \
  --set-string databaseSecret.key="CUSTOM_DB_URL"
assert_rendered_refs "$external_secret" "external-db" "CUSTOM_DB_URL" "external-db" "CUSTOM_DB_URL" "false"

migration_override="$tmp_dir/migration-override.yaml"
render "$migration_override" \
  --set databaseSecret.create=false \
  --set-string databaseSecret.name="shared-db" \
  --set-string databaseSecret.key="SHARED_DB_URL" \
  --set-string migrations.databaseSecret.name="migration-db" \
  --set-string migrations.databaseSecret.key="MIGRATION_DB_URL"
assert_rendered_refs "$migration_override" "shared-db" "SHARED_DB_URL" "migration-db" "MIGRATION_DB_URL" "false"

echo "Chart DB Secret template tests passed."
