#!/bin/bash
set -e

# GoFast API Docker Entrypoint

HOST="${GOFAST_API_HOST:-${GFAST_API_HOST:-0.0.0.0}}"
PORT="${GOFAST_API_PORT:-${GFAST_API_PORT:-8080}}"
DB_PATH="${GOFAST_DUCKDB_PATH:-${GFAST_DB_PATH:-/app/data/gofast.duckdb}}"
LOG_DIR="${GOFAST_PARSER_SLOW_LOG_DIR:-${GFAST_LOG_DIR:-/app/logs}}"
CONFIG_PATH="${GOFAST_CONFIG_PATH:-${GFAST_CONFIG_PATH:-/app/config.yaml}}"

export GOFAST_API_HOST="${HOST}"
export GOFAST_API_PORT="${PORT}"
export GOFAST_DUCKDB_PATH="${DB_PATH}"
export GOFAST_PARSER_SLOW_LOG_DIR="${LOG_DIR}"

echo "========================================"
echo "GoFast API Server"
echo "========================================"
echo "Config:       ${CONFIG_PATH}"
echo "Bind Address: ${HOST}:${PORT}"
echo "Log Directory:${LOG_DIR}"
echo "Database:     ${DB_PATH}"
echo "========================================"

# Run the machine API server (read-only). Auth is ON by default: set
# GOFAST_API_KEY, or pass --no-auth explicitly for trusted/local networks.
exec /app/gofast-cli serve \
    --config "${CONFIG_PATH}" \
    --duck-db-path "${DB_PATH}" \
    --port "${PORT}" \
    "$@"
