# CLAUDE.md

Guidance for Claude Code working in **gofast** (`github.com/dbsmedya/gofast`) — an open-source MySQL slow-query analysis core: a slow-log parser + DuckDB storage + a read-only machine REST API.

## What this repo is

A Go module shipping the parser/storage/fingerprint libraries (`pkg/`) and two entry points in `cmd/cli`:
- `gofast-cli parse` / `stats` / `query` — CLI over the parser + DuckDB store.
- `gofast-cli serve` — the read-only machine API (`pkg/api`, routes under `/api/v1/`).

## Hard rules

1. **`pkg/` is the public contract.** `pkg/{api,storage,parser,models,config,fingerprint}` is consumed by downstream integrators. Treat exported-signature/behavior changes as versioned events (breaking = major, additive = minor); don't change them casually.
2. **Build requires `CGO_ENABLED=1`** (DuckDB uses cgo): `CGO_ENABLED=1 go build ./...`. Toolchain **go 1.25.12**.
3. **The stable contract is the REST machine API + `config.yaml` schema**, specified by the served OpenAPI (`pkg/api/openapi.yaml`, served at `/api/v1/openapi.json`). `scripts/run-contract-check.sh` must stay green.
4. **No secrets; `gitleaks` must pass.**

## Layout

- `cmd/cli/` — the `gofast-cli` binary (parse/stats/query/serve).
- `pkg/` — the public library + machine-API router (`pkg/api`).
- `internal/` — non-public support (contract-test harness).
- `docs/` — `README_API.md` (machine REST API), `README_PARSER.md` (parser internals).
