# GoFast Backend Dockerfile
# Multi-stage build for Go API with DuckDB support

# Build stage
FROM golang:1.25.12-bookworm AS builder

# Install build dependencies for DuckDB and apply current security updates
RUN apt-get update && apt-get upgrade -y && apt-get install -y --no-install-recommends \
    gcc \
    g++ \
    cmake \
    ninja-build \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /build

# Copy go mod files first for better caching
COPY go.mod go.sum ./
# Base image now matches go.mod's toolchain (1.25.12) — pin to it, no network fetch.
ENV GOTOOLCHAIN=local
RUN go mod download

# Copy source code
COPY . ./

# Build the API binary with CGO enabled (required for DuckDB)
ENV CGO_ENABLED=1
ENV GOOS=linux
# Base image now matches go.mod's toolchain (1.25.12) — pin to it, no network fetch.
ENV GOTOOLCHAIN=local
RUN go env GOTOOLCHAIN
# Build the CLI binary (parse/stats/query + the read-only machine API `serve`)
RUN go build -ldflags="-w -s" -o /build/gofast-cli ./cmd/cli

# Runtime stage
FROM debian:bookworm-slim

# Install runtime dependencies for DuckDB and apply current security updates
RUN apt-get update && apt-get upgrade -y && apt-get install -y --no-install-recommends \
    ca-certificates \
    libc6 \
    libgcc-s1 \
    libstdc++6 \
    wget \
    && rm -rf /var/lib/apt/lists/*

# Create non-root user for security
RUN useradd -m -u 1000 -s /bin/bash gofast

# Create app directories
RUN mkdir -p /app/data /app/logs && chown -R gofast:gofast /app

WORKDIR /app

# Copy binary from builder
COPY --from=builder /build/gofast-cli /app/gofast-cli
RUN chmod +x /app/gofast-cli

# Copy entrypoint script
COPY docker-entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh

# Copy container-specific runtime config
COPY config.docker.yaml /app/config.yaml
RUN chown gofast:gofast /app/config.yaml

# Switch to non-root user
USER gofast

# Environment variables
ENV GOFAST_DUCKDB_PATH=/app/data/gofast.duckdb
ENV GOFAST_PARSER_SLOW_LOG_DIR=/app/logs
ENV GOFAST_API_HOST=0.0.0.0
ENV GOFAST_API_PORT=8080

# Expose API port
EXPOSE 8080

# Persist database output and mounted log input
VOLUME ["/app/data", "/app/logs"]

# Note: Container defaults bind to 0.0.0.0 for Docker networking.
# Override GOFAST_API_HOST if a different bind address is required.

# Run the API server
ENTRYPOINT ["/app/entrypoint.sh"]
