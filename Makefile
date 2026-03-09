.PHONY: build build-api build-indexer run-api run-indexer test test-coverage test-race lint lint-fix vet fmt clean docker-build docker-run tilt-up seed-data helm-dep-update helm-install-dev helm-install-prod helm-upgrade helm-uninstall ci staticcheck

# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get
GOMOD=$(GOCMD) mod
API_BINARY=blob-indexer-api
INDEXER_BINARY=blob-indexer
COVERAGE_THRESHOLD ?= 90
COVERAGE_PACKAGES ?= ./internal/... ./docs
COVERAGE_DIR=coverage
COVERAGE_FILE=$(COVERAGE_DIR)/coverage.out
COVERAGE_HTML=$(COVERAGE_DIR)/coverage.html

all: test build

build: build-api build-indexer

build-api:
	$(GOBUILD) -o $(API_BINARY) -v ./cmd/api

build-indexer:
	$(GOBUILD) -o $(INDEXER_BINARY) -v ./cmd/indexer

run-api: build-api
	./$(API_BINARY)

run-indexer: build-indexer
	./$(INDEXER_BINARY)

test:
	$(GOTEST) -v ./...

test-coverage:
	@mkdir -p $(COVERAGE_DIR)
	$(GOTEST) -v -coverprofile=$(COVERAGE_FILE) -covermode=atomic $(COVERAGE_PACKAGES)
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

staticcheck:
	staticcheck ./...

# Run all CI-equivalent checks locally (format, lint, vet, staticcheck, test, build, mod verify)
ci: fmt vet lint staticcheck test-coverage build
	$(GOMOD) verify
	@echo "All CI checks passed."

clean:
	rm -f $(API_BINARY)
	rm -f $(INDEXER_BINARY)
	rm -f $(API_BINARY).exe
	rm -f $(INDEXER_BINARY).exe
	rm -rf $(COVERAGE_DIR)

deps:
	$(GOMOD) download
	$(GOMOD) tidy

docker-build:
	docker build -f Dockerfile.api -t $(API_BINARY) .
	docker build -f Dockerfile.indexer -t $(INDEXER_BINARY) .

docker-run:
	docker run -p 8080:8080 \
		-e DB_URL="postgres://postgres:postgres@postgres:5432/blobindexer?sslmode=disable" \
		-e LOG_LEVEL="info" \
		$(API_BINARY)

tilt-up:
	tilt up

seed-data:
	$(GOCMD) run ./cmd/testdata/main.go

# Swagger documentation
swagger:
	swag init -g cmd/api/main.go -o docs

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
