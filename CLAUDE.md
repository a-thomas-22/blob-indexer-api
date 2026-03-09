# CLAUDE.md

## Project Overview

Blob Indexer API — a Go backend that indexes Ethereum blob transactions (EIP-4844) across multiple networks and serves them via REST APIs. Data is stored in PostgreSQL.

## Build & Run

```bash
make build          # Build binary → ./blob-indexer-api
make run            # Build and run
make test           # Run all tests
make docker-build   # Build Docker image
make swagger        # Generate Swagger docs (swag init)
make seed-data      # Seed test data via cmd/testdata
make db-migrate     # Run database migrations
make db-rollback    # Rollback one migration
```

## Architecture

Entry point: `cmd/server/main.go`

Startup flow: load config → connect DB → run migrations → create Ethereum clients → start per-network indexers → start HTTP server → wait for shutdown signal.

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
- Migrations run automatically on startup
- Key tables: `blobs`, `networks`, `blob_users`, `indexer_metadata`, `indexed_blocks`
- Connection pooling: 25 max open, 10 idle

### API Routes

All routes under `/api`. Network selected via `?network=` query param (name or chain ID).

- `/api/networks`, `/api/networks/{chainId}` — network listing and status
- `/api/blob/latest`, `/api/blob/mempool`, `/api/blob/{txHash}` — blob queries
- `/api/users` — top blob users
- `/api/stats` — historical stats
- `/api/status` — indexer status
- `/api/dev/*` — development/debug endpoints (metrics, dashboard, logs, queries)
- `/swagger/*` — Swagger UI

### Configuration

Loaded via Viper: first reads `config.yaml` (or `CONFIG_PATH`), then environment variable overrides.

Key env vars: `DB_URL`, `PORT`, `DEV_MODE`, `LOG_LEVEL`, `RPC_URL`/`ETH_RPC_URL`, `START_BLOCK`, `NETWORK_<NAME>_*`.

## Code Conventions

- Go module: `github.com/a-thomas-22/blob-indexer-api`
- Go 1.24
- HTTP framework: Chi v5 with middleware stack (RequestID, RealIP, rate limit, logging, recovery, timeout, CORS)
- Database queries: sqlx with `lib/pq` driver
- Logging: use `logger.Info/Error/Fatal/Debug` (Zap wrapper in `internal/logger/`)
- Config: Viper with mapstructure tags for struct binding
- Errors: handlers use `respondJSON`/`respondError` helpers

## Releases

Managed by **release-please** (`.github/workflows/release-please.yml`). The app and Helm chart are versioned independently.

- PR titles must follow [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `chore:`, etc.) — enforced by CI
- On merge to `main`, release-please maintains a running release PR with changelog
- Merging the release PR creates a GitHub Release + tag, which triggers Docker/Helm publish workflows
- Config: `release-please-config.json`, `.release-please-manifest.json`
- Docker images: `ghcr.io/<owner>/blob-indexer-api`
- Helm charts: `ghcr.io/<owner>/charts/blob-indexer` (OCI)

## Deployment

- **Docker**: multi-stage build (Go 1.24 Alpine → Alpine runtime), exposes port 8080
- **Kubernetes**: Helm chart in `charts/blob-indexer/` with PostgreSQL dependency (Bitnami)
- **Tilt**: local K8s dev with hot reload (`Tiltfile` + `tilt-config.yaml`)
- **Railway**: `railway-config.yaml`
