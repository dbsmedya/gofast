# GoFast — MySQL Slow Log Analyzer

[![Go Version](https://img.shields.io/badge/go-1.25.12-blue.svg)](https://golang.org)
[![License](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)

Parse MySQL slow logs into DuckDB and report on them with a
**`pt-query-digest`-compatible CLI**. One static binary, no MySQL connection, no
Perl — point it at a slow log and get the report you already know how to read.

```bash
gofast-cli parse --slow-log-dir /var/log/mysql
gofast-cli digest --group-by fingerprint --order-by Query_time:sum --limit 10
```

```
## Profile

| Rank | Query ID         | Response time | Calls | R/Call | V/M  | Item                       |
|---|---|---|---|---|---|---|
| 1 | 4F361D3B54B0F76E | 76.90s 68.0%  | 2     | 38.45  | 2.37 | SELECT shop.order_items    |
| 2 | E28D9925CAA96CB9 | 21.50s 19.0%  | 2     | 10.75  | 0.51 | UPDATE warehouse.inventory |
| 3 | FC90AEDD92E20F6B | 7.90s 7.0%    | 1     | 7.90   | 0.00 | SELECT orders customers    |
```

Add `--json` and the same report pipes straight into `jq`.

---

## Contents

- [Why](#why)
- [Quick Start](#quick-start)
- [Installation](#installation)
- [CLI Usage](#cli-usage)
- [Machine API](#machine-api)
- [Configuration](#configuration)
- [Documentation](#documentation)

---

## Why

`pt-query-digest` needs Perl and re-reads the raw log every time. GoFast parses
once into DuckDB, then answers questions in milliseconds — while keeping
pt-query-digest's flag names, defaults, and report shape so your habits
transfer.

- **Query fingerprinting** — normalize literals to `?` to group query patterns
  (Percona's algorithm, via their `go-mysql` library)
- **Table extraction** — pull table names out of `FROM` / `JOIN` / `UPDATE`
- **DuckDB storage** — exact percentiles, fast aggregation, incremental
  re-parsing with deduplication
- **Two ways out** — the `digest` report, or raw SQL via `query`
- **A read-only machine API** for automation, served by the same binary

---

## Quick Start

```bash
# 1. Build (needs CGO for DuckDB)
make build

# 2. Parse your slow logs
./bin/gofast-cli --slow-log-dir /var/log/mysql parse -v

# 3. Report
./bin/gofast-cli digest --limit 10

# 4. Or as JSON
./bin/gofast-cli digest --limit 10 --json | jq '.classes[] | {id, calls, item}'
```

Parsing is incremental: re-running `parse` over a growing log adds only new
events.

---

## Installation

### Prerequisites

- Go **1.25.12** or higher
- A C compiler — DuckDB uses cgo, so `CGO_ENABLED=1` is required

### From source

```bash
git clone <repository-url>
cd gofast-cli
go mod download
make build          # -> bin/gofast-cli

# or install into $GOPATH/bin
make install
```

`make help` lists the other targets (`test`, `lint`, `fmt`, `vuln`).

---

## CLI Usage

```
gofast-cli [global flags] <command> [command flags]

Global flags:
  --config string        config file (default ./config.yaml)
  --slow-log-dir string  directory containing slow log files
  --duck-db-path string  path to the DuckDB database file
```

Commands: [`parse`](#parse), [`digest`](#digest), [`query`](#query),
[`stats`](#stats), [`serve`](#machine-api), `version`.

### `digest`

The main event: a `pt-query-digest`-style report — a ranked profile table
followed by a per-class detail section. Markdown by default; with `--json`,
stdout carries **only** JSON, so it is always safe to pipe.

```bash
# The ten fingerprints consuming the most total execution time
gofast-cli digest --group-by fingerprint --order-by Query_time:sum --limit 10

# One fingerprint in detail (the ID is what the profile table prints)
gofast-cli digest --filter "fingerprint = 4F361D3B54B0F76E"

# Queries slower than 2s against one database, last 24 hours
gofast-cli digest --filter "db = shop" --filter "query_time > 2" --since 24h

# Last week's worst tables by rows examined, as JSON
gofast-cli digest --group-by tables --order-by Rows_examined:sum \
  --since 7d --json | jq '.classes[] | {id, calls}'
```

| flag | meaning |
|---|---|
| `--group-by` | `fingerprint` (default), `user`, `db`, `host`, `source_host`, `tables` |
| `--order-by` | `ATTRIBUTE:AGGREGATE`, default `Query_time:sum` |
| `--limit` | classes to report, default `20` |
| `--filter` | `"KEY OP VALUE"`, repeatable, AND-combined |
| `--since` / `--until` | `7d`, `90m`, `2h`, `30s`, or `YYYY-MM-DD [HH:MM:SS]` |
| `--json` | emit JSON instead of markdown |

Attributes are `Query_time`, `Lock_time`, `Rows_sent`, `Rows_examined`;
aggregates are `sum`, `min`, `max`, `avg`, `cnt`. Ordering is always
worst-first.

> **Quote any filter containing `>` or `<`.** Unquoted,
> `--filter query_time > 2` makes the *shell* redirect output into a file
> named `2`.

Full reference — every flag, the report columns, what V/M means, and known
limits — in **[docs/README_DIGEST.md](docs/README_DIGEST.md)**.

### `parse`

```bash
gofast-cli parse                                   # the configured directory
gofast-cli parse --slow-log-dir /var/log/mysql     # a specific directory
gofast-cli parse --file /var/log/mysql/slow.log    # a single file
gofast-cli parse -v                                # verbose
```

```
=== Parse Results ===
Files processed: 3
Files skipped:   0
Files failed:    0
Entries parsed:  15234
Entries stored:  15234
Duration:        2.345s
```

### `query`

Raw SQL, for questions `digest` doesn't answer.

```bash
gofast-cli query "SELECT sample_sql, query_time_sec FROM slow_logs ORDER BY query_time_sec DESC LIMIT 10"
```

The schema and a set of ready-made queries are in
**[docs/README_SQL.md](docs/README_SQL.md)**.

### `stats`

```bash
gofast-cli stats
```

Prints entry counts, the time range covered, unique fingerprints, and
per-file ingestion totals.

---

## Machine API

`gofast-cli serve` exposes a **read-only** HTTP API for automation. It is the
same binary — there is no separate server to build.

```bash
export GOFAST_API_KEY=your-token
gofast-cli serve --port 8080
```

| endpoint | purpose |
|---|---|
| `GET /health`, `GET /api/v1/health` | liveness; no auth |
| `GET /api/v1/openapi.json` | the served OpenAPI spec; no auth |
| `GET /api/v1/sql/queries` | slow-query records; `limit`, `since`, `until` (defaults to the last 15 days) |
| `GET /api/v1/sql/databases` | databases present in the store |
| `POST /api/v1/sql/execute` | run a read-only SQL statement |

Everything under `/api/v1/sql/` requires `Authorization: Bearer $GOFAST_API_KEY`.
`--no-auth` disables that check and is **development only**.

```bash
curl http://localhost:8080/health

curl -H "Authorization: Bearer $GOFAST_API_KEY" \
  -X POST http://localhost:8080/api/v1/sql/execute \
  -H "Content-Type: application/json" \
  -d '{"sql": "SELECT COUNT(*) FROM slow_logs"}'
```

The OpenAPI document served at `/api/v1/openapi.json` is the contract of record.
See **[docs/README_API.md](docs/README_API.md)**.

---

## Configuration

Precedence: command-line flags → environment variables (`GOFAST_` prefix) →
config file → defaults.

```yaml
duckdb:
  path: "./data/gofast.duckdb"

parser:
  slow_log_dir: "./logs"
  batch_size: 1000
  file_patterns: ["*slow*", "*.log", "*mysql*"]
  workers: 4

api:
  host: "0.0.0.0"
  port: 8080
```

```bash
export GOFAST_DUCKDB_PATH="/data/gofast.duckdb"
export GOFAST_PARSER_SLOW_LOG_DIR="/var/log/mysql"
export GOFAST_PARSER_WORKERS="8"
export GOFAST_API_PORT="9090"
export GOFAST_API_KEY="your-token"     # required by `serve` unless --no-auth
```

---

## Architecture

```
        gofast-cli
   ┌─────────┴─────────┐
 parse            digest / query / serve
   │                   │
   ▼                   ▼
Parser Engine     internal/digest  ─┐
(percona/go-mysql  pkg/api          ├─▶ read-only
 + fingerprint)                     │
   │                   │            │
   └────────┬──────────┘            │
            ▼                       │
      DuckDB Storage ◀──────────────┘
        (pkg/storage)
```

`pkg/{api,storage,parser,models,config,fingerprint}` is the public library
contract; the digest engine lives in `internal/` and is CLI-only.

---

## Documentation

| doc | covers |
|---|---|
| [docs/README_DIGEST.md](docs/README_DIGEST.md) | the `digest` report — flags, columns, JSON schema, limits |
| [docs/README_SQL.md](docs/README_SQL.md) | schema, indexes, and example SQL for `query` |
| [docs/README_API.md](docs/README_API.md) | the read-only machine REST API |
| [docs/README_PARSER.md](docs/README_PARSER.md) | parser internals and fingerprinting |
| [perf_tests/README.md](perf_tests/README.md) | benchmarking harness |

---

## License

MIT — see [LICENSE](LICENSE).

## Acknowledgments

- [Percona](https://percona.com) for [go-mysql](https://github.com/percona/go-mysql) and for `pt-query-digest`, whose interface this reproduces
- [DuckDB](https://duckdb.org) for the analytical engine
- [Gin](https://gin-gonic.com) and [Cobra](https://cobra.dev) for the HTTP and CLI frameworks
