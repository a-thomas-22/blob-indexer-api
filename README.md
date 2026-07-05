# Blob Indexer API

A backend service to index all blob-related transactions from the Ethereum blockchain starting from EIP-4844 activation.

## Overview

The Blob Indexer API continuously indexes new blocks and pending blob transactions from the Ethereum blockchain, persists the data in a PostgreSQL database, and provides REST APIs to access the data.

## Features

- Continuously index new blocks and pending blob transactions
- Persist historical blob data with correct base fee, tip, and total cost
- Attribute blobs to known rollups based on from_address matching
- Provide simple, fast REST APIs to access blob data
- Support multiple Ethereum networks simultaneously (e.g., mainnet, Sepolia)
- Configuration-based RPC URLs (not stored in database for flexibility)
- In-process rate limiting (100 req/s per IP with burst of 200)
- Chain reorganization detection via block hash tracking
- Structured JSON logging with configurable levels
- Swagger/OpenAPI documentation
- AsyncAPI WebSocket message documentation
- Development dashboard with metrics, indexer status, and database stats

## Tech Stack

- Language: Go 1.26.1
- HTTP Framework: Chi
- Database: PostgreSQL (with sqlx query builder)
- Ethereum Client: go-ethereum (ethclient)
- Configuration: Viper (YAML + environment variables)
- Logging: Zap (structured JSON)
- Migrations: golang-migrate via `cmd/migrate`, `make db-migrate`, or Helm-managed migration containers
- API Docs: Swagger/OpenAPI (swag + http-swagger), plus AsyncAPI for WebSocket messages
- Deployment: Docker, Kubernetes with Helm, Tilt for development

## API Endpoints

### Network Endpoints
- `GET /api/v1/networks` - List all available networks
- `GET /api/v1/networks/{chainId}` - Get status of a specific network

### Blob Endpoints
- `GET /api/v1/blob/latest?network=mainnet&limit=10` - Fetch latest blobs from recent blocks
- `GET /api/v1/blob/mempool?network=mainnet&limit=10` - Fetch pending (unconfirmed) blob transactions
- `GET /api/v1/blob/pricing?network=mainnet&blocks=20` - Fetch recent blob pricing and utilization metrics
- `GET /api/v1/blob/{txHash}?network=mainnet` - Fetch a specific blob by transaction hash

### Block Endpoints
- `GET /api/v1/block/{number}?network=mainnet` - Fetch a single indexed block with its blobs and pricing (same shape as the WebSocket `new_block` event); 404 if the block is not indexed

### User Endpoints
- `GET /api/v1/users?network=mainnet&limit=10&range=24h` - Top blob users by blobs submitted; `range` (`1h`, `24h`, `7d`, `30d`, `all`; default `all`) scopes counts, spend, and share percentages to a recent window and is echoed back as `meta.range`
- `GET /api/v1/users/unattributed?network=mainnet&limit=10` - Top unattributed blob senders by blobs submitted (same `range` support)

### Stats Endpoints
- `GET /api/v1/stats?network=mainnet` - Historical blob cost trends, base fee history

### Status Endpoints
- `GET /api/v1/status?network=mainnet` - Indexer status

### WebSocket Endpoint
- `GET /api/v1/ws?network=mainnet` - Subscribe to live blob, stats, and user updates

WebSocket clients receive raw JSON text frames with a top-level `type` field
and a type-specific `data` payload. `users_update` events also carry a
top-level `range` field naming the aggregation window they cover (currently
always `all`), so clients viewing a different REST `range` can ignore them.
The committed AsyncAPI contract is in `docs/asyncapi.yaml`, and the API serves
it at `GET /asyncapi.yaml`.

Clients may send an optional subscription filter after connecting:

```json
{"subscribe":["new_block","mempool_update","stats_update","users_update"]}
```

If no subscription filter is sent, all event types are delivered. The server
also sends an application-level `{"type":"ping"}` heartbeat every 30 seconds.

### Development Endpoints
These endpoints are available only when `server.dev_mode` is enabled, and can be protected with `server.dev_api_key`. Set `server.dev_port` to serve them from a dedicated listener instead of the main API listener:
- `GET /api/v1/dev/metrics` - System-wide metrics (memory, goroutines, uptime)
- `GET /api/v1/dev/indexers` - Per-network indexer status
- `GET /api/v1/dev/database` - Database statistics
- `GET /api/v1/dev/logs` - Log entries
- `GET /api/v1/dev/queries` - Database query stats
- `GET /api/v1/dev/dashboard` - HTML development dashboard

Legacy `/api/*` paths redirect to `/api/v1/*`. API endpoints accept an optional `network` query parameter (name or chain ID) to specify which network to query. If not provided, the first enabled network is used.

## API Documentation

The API is documented using Swagger/OpenAPI. When the server is running, you can access the Swagger UI at:

```
http://localhost:8080/swagger/index.html
```

To generate the Swagger documentation:

```bash
make swagger
```

Generated Swagger files are intentionally ignored by Git. Build, test, Docker, and CI targets generate them when they need the `docs` package.

The WebSocket message contract is documented separately with AsyncAPI:

```
http://localhost:8080/asyncapi.yaml
```

## Development

### Prerequisites

- Go 1.26.1 or later
- Docker
- Kubernetes cluster (local or remote)
- Helm
- Tilt

### Configuration

The application can be configured using either a YAML configuration file or environment variables.

#### YAML Configuration

Create a `config.yaml` file:

```yaml
database:
  url: "postgres://postgres:postgres@localhost:5432/blobindexer?sslmode=disable"

server:
  port: 8080
  dev_port: 8081 # optional; 0 keeps dev endpoints on the main API listener
  dev_mode: true

cors:
  enabled: true
  allowed_origins:
    - "http://localhost:3000"
    - "http://localhost:3001"
    - "http://127.0.0.1:3000"
    - "http://127.0.0.1:3001"
  allowed_origin_patterns: []
  allow_all_origins: false
  allowed_methods: ["GET", "OPTIONS"]
  allowed_headers: ["Accept", "Content-Type", "Authorization"]
  exposed_headers: ["Content-Length", "ETag"]
  allow_credentials: false
  max_age_seconds: 86400

logging:
  level: "info"
  format: "json"

indexer:
  version: "v1.0.0"
  batch_size: 100
  polling_interval: 15s
  mempool_polling_interval: 30s

networks:
  - name: "mainnet"
    chain_id: 1
    rpc_url: "https://mainnet.infura.io/v3/your-key"
    start_block: "LATEST-1000"
    enabled: true

  - name: "sepolia"
    chain_id: 11155111
    rpc_url: "https://sepolia.infura.io/v3/your-key"
    start_block: "LATEST-100"
    enabled: true
```

Set the `CONFIG_PATH` environment variable to point to your config file:

```bash
export CONFIG_PATH=config.yaml
```

#### Environment Variables

Alternatively, you can use environment variables:

- `DB_URL` - PostgreSQL connection string
- `PORT` - API server port (default: 8080)
- `DEV_PORT` - Optional dedicated dev endpoint port (default: 0, disabled)
- `DEV_MODE` - Enable development mode (default: false)
- `LOG_LEVEL` - Logging level (default: info)
- `LOG_FORMAT` - Logging format (default: json)
- `INDEXER_VERSION` - Version of the indexer
- `DEV_API_KEY` - Optional API key for development endpoints
- `CONFIG_PATH` - Path to YAML configuration file (optional)
- `CORS_ENABLED` - Enable browser CORS responses (default: true)
- `CORS_ALLOWED_ORIGINS` - Comma-separated list of exact browser origins
- `CORS_ALLOWED_ORIGIN_PATTERNS` - Comma-separated wildcard origin patterns for preview deploys
- `CORS_ALLOW_ALL_ORIGINS` - Allow any request origin while still echoing the origin (default: false)
- `CORS_ALLOWED_METHODS` - Comma-separated allowed methods (default: GET,OPTIONS)
- `CORS_ALLOWED_HEADERS` - Comma-separated allowed request headers
- `CORS_EXPOSED_HEADERS` - Comma-separated response headers exposed to browsers
- `CORS_ALLOW_CREDENTIALS` - Send `Access-Control-Allow-Credentials: true` (default: false)
- `CORS_MAX_AGE_SECONDS` - Preflight cache duration in seconds (default: 86400)

For backward compatibility, you can configure a single network using:
- `RPC_URL` - Ethereum node endpoint
- `ETH_RPC_URL` - Ethereum node endpoint (takes precedence over `RPC_URL`)
- `START_BLOCK` - Starting block for indexing (e.g., "LATEST-1000")

For multiple networks, use the following pattern:
- `NETWORK_<NAME>_RPC_URL` - RPC URL for the network
- `NETWORK_<NAME>_START_BLOCK` - Starting block for the network
- `NETWORK_<NAME>_ENABLED` - Whether the network is enabled (true/false)

Example:
```
NETWORK_SEPOLIA_RPC_URL=https://sepolia.infura.io/v3/your-key
NETWORK_SEPOLIA_START_BLOCK=LATEST-100
NETWORK_SEPOLIA_ENABLED=true
```

### Running Locally with Tilt

1. Install Tilt: https://docs.tilt.dev/install.html
2. Create a `.env` file with your environment variables (see `.env.example`):
   ```
   ETH_RPC_URL=https://mainnet.infura.io/v3/your-api-key
   START_BLOCK=LATEST-1000
   ```
3. Start Tilt:
   ```
   tilt up
   ```
4. Access the API at http://localhost:8080/api/v1

### Makefile Targets

```bash
make build          # Build both binaries
make build-api      # Build API server only
make build-indexer  # Build indexer only
make build-migrate  # Build migration runner only
make run-api        # Build and run API server
make run-indexer    # Build and run indexer
make test           # Run all tests
make clean          # Remove built binary
make deps           # Download and tidy Go module dependencies
make docker-build   # Build both Docker images
make docker-run     # Run Docker container
make tilt-up        # Start Tilt development environment
make seed-data      # Seed test data (runs cmd/testdata)
make swagger        # Generate local Swagger/OpenAPI artifacts
make db-migrate     # Run database migrations
make db-rollback    # Rollback one database migration
make helm-dep-update # Update Helm chart dependencies
make helm-install-dev # Install Helm chart with dev values
make helm-install-prod # Install Helm chart with prod values
make helm-upgrade   # Upgrade Helm release
make helm-uninstall # Uninstall Helm release
```

### Building and Running with Docker

```bash
# Build the Docker images
make docker-build

# Run the API container
docker run -p 8080:8080 \
  -e DB_URL="postgres://postgres:postgres@postgres:5432/blobindexer?sslmode=disable" \
  -e LOG_LEVEL="info" \
  blob-indexer-api
```

### Deploying with Helm

```bash
# Install the chart. PostgreSQL is external to this chart; provide a DB URL.
helm install blob-indexer ./charts/blob-indexer \
  --set appConfig.database.url="postgres://user:pass@postgres.example.com:5432/blobindexer?sslmode=require" \
  --set appConfig.networks[0].name="mainnet" \
  --set appConfig.networks[0].chain_id=1 \
  --set appConfig.networks[0].rpc_url="https://mainnet.infura.io/v3/your-api-key" \
  --set appConfig.networks[0].start_block="LATEST-1000" \
  --set appConfig.networks[0].enabled=true
```

For GitOps installs with an externally managed database Secret, disable Secret creation and point the chart at the existing key. The API, indexer, and migration Job all use the same Secret by default:

```yaml
migrations:
  enabled: true
databaseSecret:
  create: false
  name: prod-blob-indexer-blob-indexer-db
  key: DB_URL
```

For multi-replica deployments, configure edge rate limiting on Ingress so limits are enforced across all pods. Set `ingress.annotations` in Helm values (for example with NGINX: `nginx.ingress.kubernetes.io/limit-rps` and `nginx.ingress.kubernetes.io/limit-burst-multiplier`).

To expose dev endpoints through a separate Kubernetes Service, set `appConfig.server.dev_port` and enable `devService`. The chart will create `<release>-dev` targeting only `/api/v1/dev/*` on the dedicated listener.

## Releases

This project uses [release-please](https://github.com/googleapis/release-please) to automate releases. The Go app and Helm chart are versioned independently.

**How it works:**

1. PR titles must use [Conventional Commits](https://www.conventionalcommits.org/) format (e.g., `feat: add new endpoint`, `fix: correct query logic`, `deps: bump module`) — this is enforced by CI
2. When PRs are merged to `main`, release-please automatically maintains a release PR with a generated changelog
3. Merging the release PR creates a GitHub Release
4. App releases publish Docker images to Harbor at `registry.ahkc.win/public/blob-indexer-api-api` and `registry.ahkc.win/public/blob-indexer-api-indexer`; chart releases package and push the Helm chart to `oci://registry.ahkc.win/public/charts`

## Project Structure

```
blob-indexer-api/
├── cmd/
│   ├── api/                    # API server binary
│   ├── indexer/                # Indexer binary
│   ├── migrate/                # Database migration runner
│   └── testdata/               # Test data seeding utility
├── internal/
│   ├── api/                    # API handlers, router, middleware, rate limiting
│   ├── attribution/            # User attribution (maps addresses to rollup names)
│   ├── config/                 # Configuration management (Viper-based)
│   ├── db/                     # Database access layer
│   │   ├── migrations/         # SQL migration files
│   │   └── models/             # Database models
│   ├── ethereum/               # Ethereum client wrapper
│   ├── indexer/                # Core indexing logic
│   └── logger/                 # Centralized structured logging (Zap)
├── charts/                     # Helm chart for Kubernetes deployment
├── docs/                       # Local generated Swagger/OpenAPI artifacts
├── .env.example                # Environment variable template
├── config.yaml                 # Default YAML configuration
├── Dockerfile.api              # API Docker image
├── Dockerfile.indexer          # Indexer Docker image
├── Makefile                    # Build, test, and deployment tasks
├── Tiltfile                    # Tilt configuration for local development
├── tilt-config.yaml            # Tilt settings
├── go.mod                      # Go module definition
└── README.md                   # Project documentation
```

## License

MIT
