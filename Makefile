.PHONY: build run test test-coverage clean docker-build docker-run tilt-up seed-data

# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get
GOMOD=$(GOCMD) mod
BINARY_NAME=blob-indexer-api

all: test build

build:
	$(GOBUILD) -o $(BINARY_NAME) -v ./cmd/server

run:
	$(GOBUILD) -o $(BINARY_NAME) -v ./cmd/server
	./$(BINARY_NAME)

test:
	$(GOTEST) -v ./...

COVERAGE_THRESHOLD=50

test-coverage:
	$(GOTEST) ./... -coverprofile=coverage.out -count=1
	@COVERAGE=$$($(GOCMD) tool cover -func=coverage.out | grep total | awk '{print $$NF}' | tr -d '%'); \
	echo "Total coverage: $${COVERAGE}%"; \
	if [ $$(echo "$${COVERAGE} < $(COVERAGE_THRESHOLD)" | bc -l) -eq 1 ]; then \
		echo "FAIL: Coverage $${COVERAGE}% is below $(COVERAGE_THRESHOLD)% threshold"; \
		exit 1; \
	fi; \
	echo "OK: Coverage $${COVERAGE}% meets $(COVERAGE_THRESHOLD)% threshold"

clean:
	rm -f $(BINARY_NAME)
	rm -f $(BINARY_NAME).exe

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

helm-install:
	helm install blob-indexer ./charts/blob-indexer \
		--set blobIndexer.ethRpcUrl="https://mainnet.infura.io/v3/your-api-key" \
		--set blobIndexer.startBlock="LATEST-1000"

helm-upgrade:
	helm upgrade blob-indexer ./charts/blob-indexer \
		--set blobIndexer.ethRpcUrl="https://mainnet.infura.io/v3/your-api-key" \
		--set blobIndexer.startBlock="LATEST-1000"

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
