# Tiltfile for blob-indexer-api

# === Configuration ===
port = str(local('grep -A 2 "server:" tilt-config.yaml | grep "port:" | awk \'{print $2}\'', quiet=True)).strip()

# === Docker Build ===
docker_build(
    'blob-indexer-api',
    '.',
    dockerfile='Dockerfile',
    live_update=[
        sync('.', '/app'),
        run('cd /app && go mod download', trigger=['./go.mod', './go.sum']),
        run('cd /app && go install ./cmd/server', trigger=['./cmd/**/*.go', './internal/**/*.go']),
    ]
)

# === PostgreSQL (inline, ephemeral) ===
# Using inline YAML instead of bitnami subchart for faster startup in dev.
# Data resets on restart (emptyDir) so only recent blobs are present.
postgres_yaml = """
apiVersion: v1
kind: Service
metadata:
  name: postgres
  labels:
    app: postgres
spec:
  ports:
  - port: 5432
    targetPort: 5432
    name: postgres
  selector:
    app: postgres
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: postgres
  labels:
    app: postgres
spec:
  selector:
    matchLabels:
      app: postgres
  strategy:
    type: Recreate
  template:
    metadata:
      labels:
        app: postgres
    spec:
      containers:
      - name: postgres
        image: postgres:14
        env:
        - name: POSTGRES_USER
          value: postgres
        - name: POSTGRES_PASSWORD
          value: postgres
        - name: POSTGRES_DB
          value: blobindexer
        ports:
        - containerPort: 5432
          name: postgres
        volumeMounts:
        - name: postgres-data
          mountPath: /var/lib/postgresql/data
      volumes:
      - name: postgres-data
        emptyDir: {}
"""
k8s_yaml(blob(postgres_yaml))
k8s_resource('postgres', port_forwards=['5432:5432'])

# === App Config (managed outside Helm for live-reload) ===
local_resource(
    'app-config',
    cmd='kubectl create configmap blob-indexer-config --from-file=config.yaml=tilt-config.yaml --dry-run=client -o yaml | kubectl apply -f -',
    deps=['tilt-config.yaml'],
)

# === Blob Indexer API (via Helm chart) ===
# Render the Helm chart with dev values. PostgreSQL and ConfigMap are managed
# separately above, so disable them in the chart.
k8s_yaml(helm(
    'charts/blob-indexer',
    name='blob-indexer',
    values='charts/blob-indexer/values-dev.yaml',
    set=[
        'postgresql.enabled=false',
        'configMap.create=false',
        'fullnameOverride=blob-indexer',
        'image.repository=blob-indexer-api',
        'image.pullPolicy=Never',
        'image.tag=latest',
    ]
))

k8s_resource(
    'blob-indexer',
    port_forwards=[port + ':' + port],
    resource_deps=['app-config', 'postgres'],
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
    'echo "Development dashboard available at: http://localhost:' + port + '/api/dev/dashboard"',
    labels=['dev-tools'],
)

# === Frontend (blob-flow) ===
# Runs the Next.js frontend locally with API proxy to the backend.
# Clone https://github.com/douvy/blob-flow as a sibling directory.
local_resource(
    'blob-flow',
    serve_cmd='cd ../blob-flow && npm install && npm run dev',
    deps=['../blob-flow/src', '../blob-flow/package.json'],
    resource_deps=['blob-indexer'],
    labels=['frontend'],
)
