#!/usr/bin/env bash
# run-contract-check.sh — Executable machine-contract baseline runner.
#
# Boots the OSS `gofast-cli serve` machine API (pkg/api) against a freshly
# generated Task-0.1 fixture DB and runs the vendored Python contract checker
# (scripts/check_remote_api.py, pinned from gofast-mcp) against it, proving
# the "machine contract" (the REST API surface consumed by ApiChunker / RAG
# ingestion) is green with no skipped checks.
#
# Exit code: this script's exit code IS the checker's exit code
# (0 = pass, 1 = fail, 2 = setup error).
#
# Phase 1 switch (Task 1.7) is done: the server boot moved from the frozen
# `cmd/api` binary to `gofast-cli serve`, and the checker invocation moved
# from the sibling `../gofast-mcp/check_remote_api.py` path to the vendored,
# CI-reachable `scripts/check_remote_api.py`.
#
# Must be run from the repository root (so gofast-cli's config.Load("") finds
# ./config.yaml).
#
# STDOUT CONTRACT: this script writes ONLY the checker's own output to
# stdout — that is the recorded baseline (scripts/testdata/check_remote_api.
# baseline.txt) and it must be byte-for-byte reproducible. All runner-internal
# progress, and the booted server's logs, go to stderr or a log file under
# $TMP instead.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

SERVER_PID=""
TMP="$(mktemp -d)"

cleanup() {
  if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  rm -rf "$TMP"
}
trap cleanup EXIT

echo "==> Generating fixture logs into $TMP/logs" >&2
CGO_ENABLED=1 go run ./internal/contracttest/cmd/genfixture "$TMP/logs" >&2

echo "==> Building bin/gofast-cli" >&2
mkdir -p bin
CGO_ENABLED=1 go build -o bin/gofast-cli ./cmd/cli >&2

echo "==> Seeding fixture DuckDB at $TMP/fixture.duckdb" >&2
./bin/gofast-cli parse --slow-log-dir "$TMP/logs" --duck-db-path "$TMP/fixture.duckdb" >"$TMP/seed.log" 2>&1

echo "==> Booting API server (read-only) on :8080" >&2
GOFAST_API_KEY=test-token ./bin/gofast-cli serve --duck-db-path "$TMP/fixture.duckdb" --port 8080 >"$TMP/server.log" 2>&1 &
SERVER_PID=$!

echo "==> Waiting for /health readiness" >&2
READY=0
for _ in $(seq 1 30); do
  if curl -sf -o /dev/null http://localhost:8080/health; then
    READY=1
    break
  fi
  sleep 0.2
done
if [[ "$READY" -ne 1 ]]; then
  echo "ERROR: server did not become ready on :8080 within ~6s" >&2
  echo "----- server.log -----" >&2
  cat "$TMP/server.log" >&2 || true
  exit 1
fi

echo "==> Running check_remote_api.py" >&2
set +e
python3 scripts/check_remote_api.py --base-url http://localhost:8080/api/v1 --api-key test-token
RC=$?
set -e

exit $RC
