# Tiltfile for blob-indexer-api

# Load environment variables from .env file if it exists
load('ext://dotenv', 'dotenv')
dotenv()

# Load Helm extension
load('ext://helm_resource', 'helm_resource', 'helm_repo')

# Add Bitnami Helm repo for PostgreSQL dependency
helm_repo('bitnami', 'https://charts.bitnami.com/bitnami')

# Build the blob-indexer-api image
docker_build(
    'blob-indexer-api',
    '.',
    dockerfile='Dockerfile',
    live_update=[
        # Sync local files to container
        sync('.', '/app'),
        # Run go install when go.mod changes
        run('cd /app && go mod download', trigger=['./go.mod', './go.sum']),
        # Restart the process when source files change
        run('cd /app && go install ./cmd/server', trigger=['./cmd/**/*.go', './internal/**/*.go', './pkg/**/*.go']),
    ]
)

# Define custom values for development
custom_values = {
    'image': {
        'repository': 'blob-indexer-api',
        'tag': 'latest',
        'pullPolicy': 'Never',  # Use locally built image
    },
    'blobIndexer': {
        'ethRpcUrl': os.environ.get('ETH_RPC_URL', 'https://mainnet.infura.io/v3/your-api-key'),
        'startBlock': os.environ.get('START_BLOCK', 'LATEST-1000'),
        'indexerVersion': 'dev',
        'devMode': 'true',
    },
    'postgresql': {
        'enabled': True,
    },
}

# Deploy the Helm chart
helm_resource(
    'blob-indexer',
    'charts/blob-indexer',
    flags=[
        '--values=charts/blob-indexer/values.yaml',
    ],
    values=custom_values,
    port_forwards=[
        '8080:8080',  # API service
        '5432:5432',  # PostgreSQL
    ],
    labels=['app'],
    resource_deps=[],
)

# Development tools as local resources
local_resource(
    'test',
    'go test ./...',
    deps=['./internal', './pkg', './cmd'],
    labels=['tests']
)

local_resource(
    'seed-test-data',
    'go run cmd/testdata/main.go',
    auto_init=False,
    trigger_mode=TRIGGER_MODE_MANUAL,
    labels=['dev-tools']
)

# Add a resource to show development dashboard
local_resource(
    'dev-dashboard',
    'echo "Development dashboard available at: http://localhost:8080/api/dev/dashboard"',
    labels=['dev-tools']
)
