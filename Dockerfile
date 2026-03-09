FROM golang:1.24-alpine AS builder

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
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -ldflags "-X main.version=${VERSION}" -o blob-indexer-api ./cmd/server

# Create a minimal image
FROM alpine:latest

WORKDIR /app

# Install runtime dependencies
RUN apk add --no-cache ca-certificates tzdata

# Copy the binary from the builder stage
COPY --from=builder /app/blob-indexer-api .

# Copy migration files
COPY --from=builder /app/internal/db/migrations ./internal/db/migrations

# Copy configuration files
COPY --from=builder /app/config.yaml ./

# Expose the API port
EXPOSE 8080

# Run the application
CMD ["./blob-indexer-api"]
