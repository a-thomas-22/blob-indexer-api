# Blob Indexer API

A backend service to index all blob-related transactions from the Ethereum blockchain starting from EIP-4844 activation.

## Overview

The Blob Indexer API continuously indexes new blocks and pending blob transactions from the Ethereum blockchain, persists the data in a PostgreSQL database, and provides REST APIs to access the data.

## Features

- Continuously index new blocks and pending blob transactions
- Persist historical blob data with correct base fee, tip, and total cost
- Attribute blobs to known rollups based on from_address matching
- Provide simple, fast REST APIs to access blob data
- Support full and partial reindexing safely after version changes
- Support multiple Ethereum networks simultaneously (e.g., mainnet, Sepolia)
- Configuration-based RPC URLs (not stored in database for flexibility)
- In-process rate limiting (100 req/s per IP with burst of 200)
- Chain reorganization detection via block hash tracking
- Structured JSON logging with configurable levels
- Swagger/OpenAPI documentation
- Development dashboard with metrics, indexer status, and database stats

## Tech Stack

- Language: Go 1.24
- HTTP Framework: Chi
- Database: PostgreSQL (with sqlx query builder)
- Ethereum Client: go-ethereum (ethclient)
- Configuration: Viper (YAML + environment variables)
- Logging: Zap (structured JSON)
- Migrations: golang-migrate via `cmd/migrate`, `make db-migrate`, or the Helm migration hook
- API Docs: Swagger/OpenAPI (swag + http-swagger)
- Deployment: Docker, Kubernetes with Helm, Tilt for development

## API Endpoints

### Network Endpoints
- `GET /api/networks` - List all available networks
- `GET /api/networks/{chainId}` - Get status of a specific network

### Blob Endpoints
- `GET /api/blob/latest?network=mainnet&limit=10` - Fetch latest blobs from recent blocks
- `GET /api/blob/mempool?network=mainnet&limit=10` - Fetch pending (unconfirmed) blob transactions
- `GET /api/blob/{txHash}?network=mainnet` - Fetch a specific blob by transaction hash

### User Endpoints
- `GET /api/users?network=mainnet&limit=10` - Top blob users by blobs submitted

### Stats Endpoints
- `GET /api/stats?network=mainnet` - Historical blob cost trends, base fee history

### Status Endpoints
- `GET /api/status?network=mainnet` - Indexer status

### Development Endpoints
These endpoints are always available for debugging and monitoring:
- `GET /api/dev/metrics` - System-wide metrics (memory, goroutines, uptime)
- `GET /api/dev/indexers` - Per-network indexer status
- `GET /api/dev/database` - Database statistics
- `GET /api/dev/logs` - Log entries
- `GET /api/dev/queries` - Database query stats
- `GET /api/dev/dashboard` - HTML development dashboard
- `POST /api/dev/reindex` - Trigger block reindexing (requires `dev_mode: true`)

All endpoints accept an optional `network` query parameter (name or chain ID) to specify which network to query. If not provided, the first enabled network is used.

## API Documentation

The API is documented using Swagger/OpenAPI. When the server is running, you can access the Swagger UI at:

```
http://localhost:8080/swagger/index.html
```

To generate the Swagger documentation:

```bash
make swagger
```

## Development

### Prerequisites

- Go 1.24 or later
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
  dev_mode: true

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
- `DEV_MODE` - Enable development mode (default: false)
- `LOG_LEVEL` - Logging level (default: info)
- `LOG_FORMAT` - Logging format (default: json)
- `INDEXER_VERSION` - Version of the indexer
- `CONFIG_PATH` - Path to YAML configuration file (optional)

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
4. Access the API at http://localhost:8080/api

### Makefile Targets

```bash
make build          # Build the binary
make build-migrate  # Build the migration runner
make run            # Build and run locally
make test           # Run all tests
make clean          # Remove built binary
make deps           # Download and tidy Go module dependencies
make docker-build   # Build Docker image
make docker-run     # Run Docker container
make tilt-up        # Start Tilt development environment
make seed-data      # Seed test data (runs cmd/testdata)
make swagger        # Generate Swagger/OpenAPI documentation
make db-migrate     # Run database migrations
make db-rollback    # Rollback one database migration
make helm-install   # Install Helm chart
make helm-upgrade   # Upgrade Helm release
make helm-uninstall # Uninstall Helm release
```

### Building and Running with Docker

```bash
# Build the Docker image
docker build -t blob-indexer-api .

# Run the container
docker run -p 8080:8080 \
  -e DB_URL="postgres://postgres:postgres@postgres:5432/blobindexer?sslmode=disable" \
  -e RPC_URL="https://mainnet.infura.io/v3/your-api-key" \
  -e START_BLOCK="LATEST-1000" \
  -e LOG_LEVEL="info" \
  blob-indexer-api
```

### Deploying with Helm

```bash
# Add the Bitnami repository for PostgreSQL dependency
helm repo add bitnami https://charts.bitnami.com/bitnami

# Update Helm repositories
helm repo update

# Install the chart
helm install blob-indexer ./charts/blob-indexer \
  --set blobIndexer.ethRpcUrl="https://mainnet.infura.io/v3/your-api-key" \
  --set blobIndexer.startBlock="LATEST-1000"
```

For multi-replica deployments, configure edge rate limiting on Ingress so limits are enforced across all pods. Set `ingress.annotations` in Helm values (for example with NGINX: `nginx.ingress.kubernetes.io/limit-rps` and `nginx.ingress.kubernetes.io/limit-burst-multiplier`).

## Releases

This project uses [release-please](https://github.com/googleapis/release-please) to automate releases. The Go app and Helm chart are versioned independently.

**How it works:**

1. PR titles must use [Conventional Commits](https://www.conventionalcommits.org/) format (e.g., `feat: add new endpoint`, `fix: correct query logic`) — this is enforced by CI
2. When PRs are merged to `main`, release-please automatically maintains a release PR with a generated changelog
3. Merging the release PR creates a GitHub Release, which triggers:
   - **Docker**: image pushed to `ghcr.io` with semver tags
   - **Helm**: chart packaged and pushed to `ghcr.io` as an OCI artifact

## Project Structure

```
blob-indexer-api/
├── cmd/
│   ├── server/                 # Main application entry point
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
├── docs/                       # Generated Swagger/OpenAPI documentation
├── .env.example                # Environment variable template
├── config.yaml                 # Default YAML configuration
├── Dockerfile                  # Multi-stage Docker build
├── Makefile                    # Build, test, and deployment tasks
├── Tiltfile                    # Tilt configuration for local development
├── tilt-config.yaml            # Tilt settings
├── go.mod                      # Go module definition
└── README.md                   # Project documentation
```

## License

MIT
