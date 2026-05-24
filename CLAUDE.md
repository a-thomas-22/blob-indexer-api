# CLAUDE.md

## Project Overview

Blob Indexer — a Go backend that indexes Ethereum blob transactions (EIP-4844) across multiple networks and serves them via REST APIs. The indexer and API are separate binaries sharing the same PostgreSQL database.

## Build & Run

```bash
make build          # Build both binaries → ./blob-indexer-api + ./blob-indexer
make build-api      # Build API server only
make build-indexer  # Build indexer only
make build-migrate  # Build migration runner only
make run-api        # Build and run API server
make run-indexer    # Build and run indexer
make test           # Run all tests
make docker-build   # Build both Docker images
make swagger        # Generate Swagger docs (swag init)
make seed-data      # Seed test data via cmd/testdata
make db-migrate     # Run database migrations
make db-rollback    # Rollback one migration
```

## Architecture

Two separate binaries:
- **API server** (`cmd/api/main.go`): HTTP server serving REST endpoints. Reads blob data and indexer status from PostgreSQL.
- **Indexer** (`cmd/indexer/main.go`): Connects to Ethereum RPC nodes, indexes blob transactions, writes to PostgreSQL.

Both share the same database. Production deployments run migrations with the dedicated migration runner: Helm uses a pre-install/pre-upgrade hook for external databases and init containers when the chart owns PostgreSQL. Runtime binaries only run migrations when `database.run_migrations: true` is explicitly configured, which is intended for local development.

### Key Packages

| Package | Path | Purpose |
|---------|------|---------|
| config | `internal/config/` | Viper-based config loading (YAML + env vars) |
| api | `internal/api/` | Chi HTTP router, handlers, middleware, rate limiting |
| db | `internal/db/` | PostgreSQL connection, migrations, models |
| indexer | `internal/indexer/` | Core block/blob indexing engine (one per network) |
| ethereum | `internal/ethereum/` | go-ethereum client wrapper (HTTP + WebSocket) |
| attribution | `internal/attribution/` | Maps sender addresses to known rollup names |
| logger | `internal/logger/` | Zap-based structured JSON logging |

### Database

- PostgreSQL with golang-migrate (migrations in `internal/db/migrations/`)
- Migrations run via `cmd/migrate`, `make db-migrate`, Helm-managed migration containers, or local `database.run_migrations: true`
- Key tables: `blobs`, `networks`, `blob_users`, `indexer_metadata`, `indexed_blocks`, `block_metrics`
- Connection pooling: 25 max open, 10 idle

### API Routes

Canonical routes are under `/api/v1`. Legacy `/api/*` paths redirect to `/api/v1/*`. Network selected via `?network=` query param (name or chain ID).

- `/api/v1/ws` — WebSocket updates
- `/api/v1/networks`, `/api/v1/networks/{chainId}` — network listing and status
- `/api/v1/blob/latest`, `/api/v1/blob/mempool`, `/api/v1/blob/pricing`, `/api/v1/blob/{txHash}` — blob queries
- `/api/v1/users` — top blob users
- `/api/v1/stats` — historical stats
- `/api/v1/status` — indexer status
- `/api/v1/dev/*` — development/debug endpoints (metrics, dashboard, logs, queries), gated by `server.dev_mode` and optional `server.dev_api_key`
- `/swagger/*` — Swagger UI

### Configuration

Loaded via Viper: first reads `config.yaml` (or `CONFIG_PATH`), then environment variable overrides.

Key env vars: `DB_URL`, `PORT`, `DEV_MODE`, `LOG_LEVEL`, `RPC_URL`/`ETH_RPC_URL`, `START_BLOCK`, `NETWORK_<NAME>_*`.

The API uses `config.LoadForAPI()` (RPC URLs optional). The indexer uses `config.Load()` (RPC URLs required).

## Local Checks (run before pushing)

Run `make ci` to execute all checks locally. GHA CI is a backstop — catch issues here first.

`make ci` runs these in order:
1. **Format** (`make fmt`): `gofmt -s` + `goimports` (auto-fixes in place)
2. **Vet** (`make vet`): `go vet ./...`
3. **Lint** (`make lint`): `golangci-lint run ./...` (config in `.golangci.yml`)
4. **Staticcheck** (`make staticcheck`): `staticcheck ./...`
5. **Test + Coverage** (`make test-coverage`): tests with 90% coverage threshold enforced
6. **Build** (`make build`): compiles both binaries
7. **Module verify**: `go mod verify`

Individual checks you can run:
- `make test` — quick test run (no coverage threshold)
- `make test-race` — tests with `-race` flag
- `make lint-fix` — lint with auto-fix

After any code change, at minimum run: `make fmt && make lint && make test`

PR titles must follow [Conventional Commits](https://www.conventionalcommits.org/): `feat:`, `fix:`, `deps:`, `docs:`, `style:`, `refactor:`, `perf:`, `test:`, `build:`, `ci:`, `chore:`, `revert:`.

## Code Conventions

- Go module: `github.com/a-thomas-22/blob-indexer-api`
- Go 1.26.1
- HTTP framework: Chi v5 with middleware stack (RequestID, RealIP, rate limit, logging, recovery, timeout, CORS)
- Database queries: sqlx with `lib/pq` driver
- Logging: use `logger.Info/Error/Fatal/Debug` (Zap wrapper in `internal/logger/`)
- Config: Viper with mapstructure tags for struct binding
- Errors: handlers use `respondJSON`/`respondError` helpers

## Releases

Managed by **release-please** (`.github/workflows/release-please.yml`). The app and Helm chart are versioned independently.

- PR titles must follow [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `deps:`, `chore:`, etc.) — enforced by CI
- On merge to `main`, release-please maintains a running release PR with changelog
- Merging the release PR creates a GitHub Release + tag, which triggers Docker/Helm publish workflows
- Config: `release-please-config.json`, `.release-please-manifest.json`
- Docker images: `ghcr.io/<owner>/blob-indexer-api-api`, `ghcr.io/<owner>/blob-indexer-api-indexer`
- Helm charts: `ghcr.io/<owner>/charts/blob-indexer` (OCI)

## Deployment

- **Docker**: Two images — `Dockerfile.api` (exposes port 8080) and `Dockerfile.indexer` (no exposed port)
- **Kubernetes**: Helm chart in `charts/blob-indexer/` with separate API and indexer deployments; PostgreSQL is provided externally
- **Tilt**: local K8s dev with hot reload (`Tiltfile` + `tilt-config.yaml`)
