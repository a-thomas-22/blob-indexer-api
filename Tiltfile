# Tiltfile for blob-indexer-api

# Get port from config for port forwarding
port = str(local('grep -A 2 "server:" tilt-config.yaml | grep "port:" | awk \'{print $2}\'', quiet=True)).strip()

# Create a ConfigMap for the configuration
local_resource(
  'app-config',
  cmd='kubectl create configmap app-config --from-file=tilt-config.yaml --dry-run=client -o yaml | kubectl apply -f -',
  deps=['tilt-config.yaml'],
)

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
        run('cd /app && go install ./cmd/server', trigger=['./cmd/**/*.go', './internal/**/*.go']),
    ]
)

# Define PostgreSQL deployment
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

# Define blob-indexer-api deployment
# Values are already defined above

blob_indexer_yaml = """
apiVersion: v1
kind: Service
metadata:
  name: blob-indexer-api
  labels:
    app: blob-indexer-api
spec:
  ports:
  - port: {port}
    targetPort: {port}
    name: http
  selector:
    app: blob-indexer-api
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: blob-indexer-api
  labels:
    app: blob-indexer-api
spec:
  selector:
    matchLabels:
      app: blob-indexer-api
  template:
    metadata:
      labels:
        app: blob-indexer-api
    spec:
      containers:
      - name: blob-indexer-api
        image: blob-indexer-api
        imagePullPolicy: Never
        env:
        - name: CONFIG_PATH
          value: /etc/config/tilt-config.yaml
        ports:
        - containerPort: {port}
          name: http
        volumeMounts:
        - name: config-volume
          mountPath: /etc/config
      volumes:
      - name: config-volume
        configMap:
          name: app-config
""".format(port=port)

k8s_yaml(blob(blob_indexer_yaml))

# Set up port forwards and dependencies
k8s_resource('blob-indexer-api', 
  port_forwards=[port + ':' + port],
  resource_deps=['app-config']
)
k8s_resource('postgres', port_forwards=['5432:5432'])

# Development tools as local resources
local_resource(
    'test',
    'go test ./...',
    deps=['./internal', './cmd'],
    labels=['tests']
)

local_resource(
    'seed-test-data',
    'go run cmd/testdata/main.go',
    auto_init=False,
    labels=['dev-tools']
)

# Add a resource to show development dashboard
local_resource(
    'dev-dashboard',
    'echo "Development dashboard available at: http://localhost:' + port + '/api/dev/dashboard"',
    labels=['dev-tools']
)
