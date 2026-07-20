# GoFast - MySQL Slow Log Analyzer

[![Go Version](https://img.shields.io/badge/go-1.21+-blue.svg)](https://golang.org)
[![License](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)

A high-performance MySQL slow log parser and analyzer built with Go. Features query fingerprinting, analytical storage in DuckDB, and both CLI and REST API interfaces.

> **Current Status:** Phase 1 Complete ✅  
> Core parsing, storage, CLI, and API are fully functional.

---

## 📋 Table of Contents

- [Features](#features)
- [Quick Start](#quick-start)
- [Installation](#installation)
- [CLI Usage](#cli-usage)
- [API Usage](#api-usage)
- [Configuration](#configuration)
- [Architecture](#architecture)
- [Example Queries](#example-queries)
- [Performance Testing](#performance-testing)
- [Roadmap](#roadmap)
- [Documentation](#documentation)

---

## ✨ Features

### Core Capabilities
- **🔍 MySQL Slow Log Parsing** - Parse standard MySQL and Percona Server slow logs using the proven Percona go-mysql library
- **🔐 Query Fingerprinting** - Normalize queries by replacing literals with `?` to identify query patterns
- **📊 Table Extraction** - Automatically extract table names from queries (FROM, JOIN, UPDATE, etc.)
- **💾 DuckDB Storage** - Fast analytical storage with SQL query support
- **⚡ Batch Processing** - Efficient batch inserts for handling large log files

### Interfaces
- **💻 CLI Tool** - Command-line interface for parsing and querying (standalone operation)
- **🌐 REST API** - HTTP API for integration with monitoring systems and automation

### Analytics
- **📈 Built-in Reports** - Top queries, slowest queries, table usage, timeline
- **🔎 Search** - Search by fingerprint pattern or table name
- **📉 Statistics** - Database metrics and summary information

---

## 🚀 Quick Start

### 1. Build

```bash
cd gofast
make build
```

This creates two binaries:
- `bin/gofast-cli` - Command-line tool
- `bin/gofast-api` - HTTP API server

### 2. Parse Slow Logs (CLI)

```bash
# Create a logs directory and add your slow log files
mkdir -p logs
cp /var/log/mysql/slow.log logs/

# Parse all logs in the directory
./bin/gofast-cli --slow-log-dir ./logs parse -v
```

### 3. Query the Database

```bash
# Show statistics
./bin/gofast-cli stats

# Run a SQL query
./bin/gofast-cli query "SELECT fingerprint, query_time_sec FROM slow_logs ORDER BY query_time_sec DESC LIMIT 5"
```

### 4. Start the API Server

```bash
# Start the server
./bin/gofast-api

# In another terminal, test the API
curl http://localhost:8080/health
curl http://localhost:8080/api/v1/stats
```

---

## 📦 Installation

### Prerequisites
- Go 1.21 or higher
- C compiler (for DuckDB CGO bindings)

### From Source

```bash
# Clone the repository
git clone <repository-url>
cd gofast

# Download dependencies
go mod download

# Build both binaries
make build

# Or install to $GOPATH/bin
make install
```

### macOS (Homebrew - Coming Soon)

```bash
brew tap gofast/tap
brew install gofast
```

---

## 💻 CLI Usage

### Global Flags

```bash
./bin/gofast-cli [global flags] [command] [command flags]

Global Flags:
  --config string        Config file path (default: ./config.yaml)
  --slow-log-dir string  Directory containing slow log files
  --duck-db-path string  Path to DuckDB database file
```

### Commands

#### `parse` - Parse MySQL Slow Logs

```bash
# Parse all logs in the configured/default directory
./bin/gofast-cli parse

# Parse specific directory
./bin/gofast-cli parse --slow-log-dir /var/log/mysql

# Parse a single file
./bin/gofast-cli parse --file /var/log/mysql/slow-query.log

# Parse with verbose output
./bin/gofast-cli parse -v

# Parse with custom database path
./bin/gofast-cli parse --duck-db-path /data/analytics.duckdb
```

**Example Output:**
```
Parsing directory: ./logs

=== Parse Results ===
Files processed: 3
Entries parsed:  15234
Entries stored:  15234
Duration:        2.345s

Total time: 2.412s
```

#### `stats` - Show Database Statistics

```bash
./bin/gofast-cli stats
```

**Example Output:**
```
=== Database Statistics ===
total_entries: 15234
oldest_entry: 2024-01-01 00:00:00 +0000 UTC
newest_entry: 2024-01-31 23:59:59 +0000 UTC
unique_fingerprints: 456
```

#### `query` - Execute SQL Queries

```bash
# Execute a SQL query directly
./bin/gofast-cli query "SELECT * FROM slow_logs LIMIT 10"

# Top 10 slowest queries
./bin/gofast-cli query "SELECT sample_sql, query_time_sec FROM slow_logs ORDER BY query_time_sec DESC LIMIT 10"

# Query count by user
./bin/gofast-cli query "SELECT user, COUNT(*) FROM slow_logs GROUP BY user"
```

#### `version` - Show Version

```bash
./bin/gofast-cli version
```

---

## 🌐 API Usage

### Starting the Server

```bash
# Default (port 8080)
./bin/gofast-api

# Custom port
./bin/gofast-api --port 9090

# With custom config
./bin/gofast-api --config /etc/gofast/config.yaml

# With custom database
./bin/gofast-api --duck-db-path /data/analytics.duckdb
```

### API Endpoints

#### Health Check
```bash
curl http://localhost:8080/health
# {"status":"ok"}
```

#### Get Statistics
```bash
curl http://localhost:8080/api/v1/stats
```

Response:
```json
{
  "total_entries": 15234,
  "oldest_entry": "2024-01-01T00:00:00Z",
  "newest_entry": "2024-01-31T23:59:59Z",
  "unique_fingerprints": 456
}
```

#### Execute Raw Query
```bash
curl -X POST http://localhost:8080/api/v1/query \
  -H "Content-Type: application/json" \
  -d '{"sql": "SELECT * FROM slow_logs LIMIT 5"}'
```

Response:
```json
{
  "columns": ["id", "fingerprint", "query_time_sec", "ts"],
  "rows": [
    [1, "select * from users where id = ?", 2.5, "2024-01-15T10:30:45Z"]
  ],
  "count": 1
}
```

#### Start Parse Job (Async)
```bash
curl -X POST http://localhost:8080/api/v1/parse \
  -H "Content-Type: application/json" \
  -d '{"log_dir": "/var/log/mysql"}'
```

Response:
```json
{
  "job_id": "job_1772278579815985000",
  "status": "pending",
  "log_dir": "/var/log/mysql"
}
```

#### Check Job Status
```bash
curl http://localhost:8080/api/v1/parse/job_1772278579815985000
```

#### Reports

**Top Queries by Total Time:**
```bash
curl "http://localhost:8080/api/v1/reports/top-queries?limit=10"
```

**Slowest Individual Queries:**
```bash
curl "http://localhost:8080/api/v1/reports/slowest-queries?limit=10&min_time=1.0"
```

**Table Usage Statistics:**
```bash
curl http://localhost:8080/api/v1/reports/table-usage
```

**Query Timeline:**
```bash
# By hour (default)
curl "http://localhost:8080/api/v1/reports/timeline?interval=hour"

# By day
curl "http://localhost:8080/api/v1/reports/timeline?interval=day"
```

#### Search

**Search by Fingerprint:**
```bash
curl "http://localhost:8080/api/v1/search/fingerprint?q=SELECT%20FROM%20users"
```

**Search by Table:**
```bash
curl "http://localhost:8080/api/v1/search/table?table=users"
```

---

## ⚙️ Configuration

Configuration can be provided via (in order of precedence):
1. Command-line flags
2. Environment variables (prefix: `GOFAST_`)
3. Configuration file
4. Default values

### Configuration File (config.yaml)

```yaml
# DuckDB database settings
duckdb:
  path: "./gofast.duckdb"    # Path to database file

# Parser settings
parser:
  slow_log_dir: "./logs"     # Default slow log directory
  batch_size: 1000           # Batch insert size

# API server settings
api:
  host: "0.0.0.0"            # Bind address
  port: 8080                 # Listen port
```

### Environment Variables

```bash
export GOFAST_DUCKDB_PATH="/data/gofast.duckdb"
export GOFAST_PARSER_SLOW_LOG_DIR="/var/log/mysql"
export GOFAST_PARSER_BATCH_SIZE="5000"
export GOFAST_API_HOST="127.0.0.1"
export GOFAST_API_PORT="9090"
```

---

## 🗄️ Database Schema

### slow_logs Table

| Column | Type | Description |
|--------|------|-------------|
| id | BIGINT | Auto-increment primary key |
| fingerprint_id | VARCHAR(16) | MD5 hash of fingerprint (16 chars) |
| fingerprint | VARCHAR | Normalized query fingerprint |
| sanitized_sql | VARCHAR | Query with literals replaced |
| sample_sql | VARCHAR | Original query sample |
| user | VARCHAR | MySQL user |
| host | VARCHAR | Client host |
| db | VARCHAR | Database name |
| query_time_sec | DOUBLE | Query execution time |
| lock_time_sec | DOUBLE | Lock wait time |
| rows_sent | UBIGINT | Rows returned to client |
| rows_examined | UBIGINT | Rows examined by query |
| ts | TIMESTAMP | Log entry timestamp |
| created_at | TIMESTAMP | Processing timestamp |
| tables | VARCHAR[] | Array of involved tables |

### Indexes

- `idx_fingerprint_id` - For grouping by fingerprint hash
- `idx_fingerprint` - For grouping by query pattern
- `idx_ts` - For time-range queries
- `idx_db` - For database filtering
- `idx_user` - For user analysis
- `idx_query_time` - For slow query identification

---

## 🏗️ Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                        GoFast                                │
├─────────────────────────────────────────────────────────────┤
│  ┌─────────────┐     ┌─────────────┐                       │
│  │  CLI Tool   │     │  API Server │                       │
│  │  (cobra)    │     │   (gin)     │                       │
│  └──────┬──────┘     └──────┬──────┘                       │
│         │                   │                               │
│         └─────────┬─────────┘                               │
│                   │                                         │
│         ┌─────────▼─────────┐                               │
│         │   Parser Engine   │                               │
│         │  (percona/go-mysql│                               │
│         │   + fingerprint)  │                               │
│         └─────────┬─────────┘                               │
│                   │                                         │
│         ┌─────────▼─────────┐                               │
│         │   DuckDB Storage  │                               │
│         │   (go-duckdb)     │                               │
│         └───────────────────┘                               │
└─────────────────────────────────────────────────────────────┘
```

Both CLI and API share the same parsing engine and storage layer, ensuring consistent behavior.

---

## 📊 Example Queries

### Find Top 10 Slowest Queries
```sql
SELECT sample_sql, query_time_sec, ts 
FROM slow_logs 
ORDER BY query_time_sec DESC 
LIMIT 10;
```

### Query Patterns by Frequency (using Fingerprint ID)
```sql
SELECT 
    fingerprint_id,
    fingerprint,
    COUNT(*) as count,
    AVG(query_time_sec) as avg_time,
    MAX(query_time_sec) as max_time,
    SUM(query_time_sec) as total_time
FROM slow_logs
GROUP BY fingerprint_id, fingerprint
ORDER BY count DESC;
```

### Find Queries by Fingerprint ID
```sql
SELECT sample_sql, query_time_sec, ts
FROM slow_logs
WHERE fingerprint_id = '186ACE4EB29D2518'
ORDER BY query_time_sec DESC;
```

### Full Table Scans
```sql
SELECT sample_sql, query_time_sec, rows_examined, rows_sent
FROM slow_logs
WHERE rows_examined > 10000 AND rows_sent < 100
ORDER BY rows_examined DESC;
```

### Hourly Query Volume
```sql
SELECT 
    strftime('%Y-%m-%d %H:00', ts) as hour,
    COUNT(*) as query_count,
    AVG(query_time_sec) as avg_time
FROM slow_logs
GROUP BY hour
ORDER BY hour;
```

### Queries by User
```sql
SELECT 
    user,
    COUNT(*) as query_count,
    AVG(query_time_sec) as avg_time,
    MAX(query_time_sec) as max_time
FROM slow_logs
GROUP BY user
ORDER BY query_count DESC;
```

### Find Queries on Specific Table
```sql
SELECT sample_sql, query_time_sec, ts
FROM slow_logs
WHERE list_contains(tables, 'users')
ORDER BY query_time_sec DESC;
```

---

## 🧪 Performance Testing

GoFast includes a comprehensive performance testing framework for benchmarking parsing performance across different dataset sizes.

### Quick Test

```bash
# Run performance tests on your slow logs
./perf_tests/run_perf_tests.sh --data-dir=/app_data/slow_logs
```

### Using Your Own Data

```bash
# Organize your slow logs by dataset
mkdir -p /app_data/slow_logs/production_sample
cp /var/log/mysql/slow.log /app_data/slow_logs/production_sample/

# Run tests
go run perf_tests/main.go --data-dir=/app_data/slow_logs --output=./perf_results
```

### Generated Reports

The performance tool generates three reports:

1. **performance_report.html** - Interactive report with charts
2. **performance_report.json** - Machine-readable data
3. **performance_report.txt** - Plain text summary

### Metrics Measured

- **Throughput** - MB/s parsing speed
- **Entries/sec** - Log entries processed per second
- **Memory Usage** - Peak memory consumption
- **Parse Time** - Total time to process dataset

See [perf_tests/README.md](perf_tests/README.md) for detailed documentation.

---

## 🗺️ Roadmap

### Phase 1: Core Platform ✅ COMPLETE
- [x] MySQL slow log parsing
- [x] Query fingerprinting (Percona algorithm)
- [x] Table extraction from queries
- [x] DuckDB storage with indexing
- [x] CLI tool with parse/query/stats commands
- [x] REST API with async job support
- [x] Built-in reports (top queries, slowest, table usage, timeline)
- [x] Search by fingerprint and table


---

## 📚 Documentation

- [API Documentation](docs/README_API.md) - Machine REST API reference

---

## 🤝 Contributing

Contributions are welcome! Please see [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines.

## 📝 License

MIT License - see [LICENSE](LICENSE) file for details.

## 🙏 Acknowledgments

- [Percona](https://percona.com) for the excellent [go-mysql](https://github.com/percona/go-mysql) library
- [DuckDB](https://duckdb.org) for the analytical database
- [Gin](https://gin-gonic.com) for the HTTP web framework

---

*Built with ❤️ using Go, DuckDB, and Percona's go-mysql library.*
