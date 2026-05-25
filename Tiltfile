# Tiltfile for blob-indexer-api

# === Safety: only allow local dev clusters ===
allow_k8s_contexts('kind-dev')

# === Configuration ===
port = str(local('grep -A 3 "server:" tilt-config.yaml | grep -E "^[[:space:]]+port:" | awk \'{print $2}\'', quiet=True)).strip()
dev_port = str(local('grep -A 3 "server:" tilt-config.yaml | grep -E "^[[:space:]]+dev_port:" | awk \'{print $2}\'', quiet=True)).strip()
api_port_forwards = [port + ':' + port]
dashboard_port = port
if dev_port != '' and dev_port != '0':
    api_port_forwards.append(dev_port + ':' + dev_port)
    dashboard_port = dev_port

# === Docker Builds ===
docker_build(
    'blob-indexer-api',
    '.',
    dockerfile='Dockerfile.api',
    live_update=[
        sync('.', '/app'),
        run('cd /app && go mod download', trigger=['./go.mod', './go.sum']),
        run('cd /app && go build -o blob-indexer-api ./cmd/api', trigger=['./cmd/api/**/*.go', './internal/**/*.go']),
    ]
)

docker_build(
    'blob-indexer-indexer',
    '.',
    dockerfile='Dockerfile.indexer',
    live_update=[
        sync('.', '/app'),
        run('cd /app && go mod download', trigger=['./go.mod', './go.sum']),
        run('cd /app && go build -o blob-indexer ./cmd/indexer', trigger=['./cmd/indexer/**/*.go', './internal/**/*.go']),
    ]
)

# === App Config (managed outside Helm for live-reload) ===
local_resource(
    'app-config',
    cmd='kubectl create configmap blob-indexer-config --from-file=config.yaml=tilt-config.yaml --dry-run=client -o yaml | kubectl apply -f -',
    deps=['tilt-config.yaml'],
)

# === Local PostgreSQL (dev-only, outside the Helm chart) ===
k8s_yaml("""
apiVersion: apps/v1
kind: Deployment
metadata:
  name: blob-indexer-postgresql
  labels:
    app.kubernetes.io/name: blob-indexer-postgresql
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: blob-indexer-postgresql
  template:
    metadata:
      labels:
        app.kubernetes.io/name: blob-indexer-postgresql
    spec:
      containers:
        - name: postgresql
          image: postgres:16-alpine
          ports:
            - containerPort: 5432
          env:
            - name: POSTGRES_USER
              value: postgres
            - name: POSTGRES_PASSWORD
              value: postgres
            - name: POSTGRES_DB
              value: blobindexer
---
apiVersion: v1
kind: Service
metadata:
  name: blob-indexer-postgresql
  labels:
    app.kubernetes.io/name: blob-indexer-postgresql
spec:
  selector:
    app.kubernetes.io/name: blob-indexer-postgresql
  ports:
    - name: postgresql
      port: 5432
      targetPort: 5432
""")

# === Blob Indexer (via Helm chart) ===
# Render the Helm chart with dev values. ConfigMap is managed separately above
# for live-reload, so disable it in the chart.
k8s_yaml(helm(
    'charts/blob-indexer',
    name='blob-indexer',
    values='charts/blob-indexer/values-dev.yaml',
    set=[
        'configMap.create=false',
        'fullnameOverride=blob-indexer',
        'api.image.repository=blob-indexer-api',
        'indexer.image.repository=blob-indexer-indexer',
        'image.pullPolicy=Never',
        'image.tag=latest',
    ]
))

k8s_resource(
    'blob-indexer-postgresql',
    port_forwards=['5432:5432'],
)

k8s_resource(
    'blob-indexer-api',
    port_forwards=api_port_forwards,
    resource_deps=['app-config', 'blob-indexer-postgresql'],
)

k8s_resource(
    'blob-indexer-indexer',
    resource_deps=['app-config', 'blob-indexer-postgresql'],
)

# === Development Tools ===
local_resource(
    'test',
    'go test ./...',
    deps=['./internal', './cmd'],
    labels=['tests'],
)

local_resource(
    'seed-test-data',
    'go run cmd/testdata/main.go',
    auto_init=False,
    labels=['dev-tools'],
)

local_resource(
    'dev-dashboard',
    'echo "Development dashboard available at: http://localhost:' + dashboard_port + '/api/v1/dev/dashboard"',
    labels=['dev-tools'],
)
