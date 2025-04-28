FROM golang:1.20-alpine AS builder

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git

# Copy go.mod and go.sum files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy the source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o blob-indexer-api ./cmd/server

# Create a minimal image
FROM alpine:latest

WORKDIR /app

# Install runtime dependencies
RUN apk add --no-cache ca-certificates tzdata

# Copy the binary from the builder stage
COPY --from=builder /app/blob-indexer-api .

# Copy migration files
COPY --from=builder /app/internal/db/migrations ./internal/db/migrations

# Expose the API port
EXPOSE 8080

# Set environment variables
ENV DB_URL="postgres://postgres:postgres@postgres:5432/blobindexer?sslmode=disable"
ENV RPC_URL="https://mainnet.infura.io/v3/your-api-key"
ENV START_BLOCK="LATEST-1000"
ENV INDEXER_VERSION="v1.0.0"
ENV PORT="8080"

# Run the application
CMD ["./blob-indexer-api"]
