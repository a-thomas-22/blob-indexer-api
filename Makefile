.PHONY: build run test test-coverage test-race lint lint-fix vet fmt clean docker-build docker-run tilt-up seed-data helm-dep-update helm-install-dev helm-install-prod helm-upgrade helm-uninstall

# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get
GOMOD=$(GOCMD) mod
BINARY_NAME=blob-indexer-api
COVERAGE_THRESHOLD ?= 80
COVERAGE_DIR=coverage
COVERAGE_FILE=$(COVERAGE_DIR)/coverage.out
COVERAGE_HTML=$(COVERAGE_DIR)/coverage.html

all: test build

build:
	$(GOBUILD) -o $(BINARY_NAME) -v ./cmd/server

run:
	$(GOBUILD) -o $(BINARY_NAME) -v ./cmd/server
	./$(BINARY_NAME)

test:
	$(GOTEST) -v ./...

test-coverage:
	@mkdir -p $(COVERAGE_DIR)
	$(GOTEST) -v -coverprofile=$(COVERAGE_FILE) -covermode=atomic ./...
	$(GOCMD) tool cover -html=$(COVERAGE_FILE) -o $(COVERAGE_HTML)
	$(GOCMD) tool cover -func=$(COVERAGE_FILE)
	@COVERAGE=$$($(GOCMD) tool cover -func=$(COVERAGE_FILE) | grep total | awk '{print $$NF}' | tr -d '%'); \
	echo "Total coverage: $${COVERAGE}%"; \
	if awk -v cov="$${COVERAGE}" -v threshold="$(COVERAGE_THRESHOLD)" 'BEGIN {exit !(cov < threshold)}'; then \
		echo "FAIL: Coverage $${COVERAGE}% is below $(COVERAGE_THRESHOLD)% threshold"; \
		exit 1; \
	fi; \
	echo "OK: Coverage $${COVERAGE}% meets $(COVERAGE_THRESHOLD)% threshold"

test-race:
	$(GOTEST) -v -race ./...

lint:
	golangci-lint run ./...

lint-fix:
	golangci-lint run --fix ./...

vet:
	$(GOCMD) vet ./...

fmt:
	gofmt -s -w .
	goimports -w -local github.com/a-thomas-22/blob-indexer-api .

clean:
	rm -f $(BINARY_NAME)
	rm -f $(BINARY_NAME).exe
	rm -rf $(COVERAGE_DIR)

deps:
	$(GOMOD) download
	$(GOMOD) tidy

docker-build:
	docker build -t $(BINARY_NAME) .

docker-run:
	docker run -p 8080:8080 \
		-e DB_URL="postgres://postgres:postgres@postgres:5432/blobindexer?sslmode=disable" \
		-e RPC_URL="https://mainnet.infura.io/v3/your-api-key" \
		-e START_BLOCK="LATEST-1000" \
		-e LOG_LEVEL="info" \
		$(BINARY_NAME)

tilt-up:
	tilt up

seed-data:
	$(GOCMD) run ./cmd/testdata/main.go

# Swagger documentation
swagger:
	swag init -g cmd/server/main.go -o docs

# Helm commands
helm-dep-update:
	helm dependency update ./charts/blob-indexer

helm-install-dev:
	helm install blob-indexer ./charts/blob-indexer \
		-f ./charts/blob-indexer/values-dev.yaml

helm-install-prod:
	helm install blob-indexer ./charts/blob-indexer \
		-f ./charts/blob-indexer/values-prod.yaml \
		--set externalDatabase.url="$(DB_URL)" \
		--set appConfig.networks[0].rpc_url="$(RPC_URL)"

helm-upgrade:
	helm upgrade blob-indexer ./charts/blob-indexer \
		-f ./charts/blob-indexer/values-dev.yaml

helm-uninstall:
	helm uninstall blob-indexer

# Database commands
db-migrate:
	$(GOCMD) run -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate \
		-database "$(DB_URL)" \
		-path ./internal/db/migrations \
		up

db-rollback:
	$(GOCMD) run -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate \
		-database "$(DB_URL)" \
		-path ./internal/db/migrations \
		down 1
