# GoFast API Documentation

Complete REST API reference for GoFast MySQL Slow Log Analyzer.

**Base URL:** `http://localhost:8080`  
**Content-Type:** `application/json`

---

## Table of Contents

1. [Health & Status](#health--status)
2. [RAG SQL API](#rag-sql-api)
3. [Statistics](#statistics)
4. [Query](#query)
5. [Parse Jobs](#parse-jobs)
6. [Reports](#reports)
7. [Search](#search)

---

## Health & Status

### GET /health

Check API server health.

**Response:**
```json
{
  "status": "ok",
  "version": "1.0.0",
  "databases": ["production", "analytics"]
}
```

**Status Codes:**
- `200` - Server is healthy

---

## RAG SQL API

### Authentication

All RAG SQL endpoints require:

```http
Authorization: Bearer <GOFAST_API_KEY>
```

Server token source:
- `config.yaml` → `api.api_key`
- Environment override: `GOFAST_API_KEY` (or `GOFAST_API_API_KEY`)

On missing/invalid token:

```json
{
  "error": "invalid_token"
}
```

Protected endpoints:
- `GET /api/v1/sql/queries`
- `GET /api/v1/sql/databases`
- `POST /api/v1/sql/execute`

When client sends `Accept-Encoding: gzip`, responses are gzip-compressed.

### GET /api/v1/sql/queries

Returns slow queries pre-aggregated by fingerprint for remote RAG ingestion.

**Query Parameters:**
| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| limit | integer | 200 | Page size (`1..1000`) |
| cursor | string | - | Opaque base64 keyset cursor |
| since | string (RFC3339) | now - 15d | Lower bound for query timestamp |
| until | string (RFC3339) | now | Upper bound for query timestamp |
| min_execution_ms | number | 0 | Filter by cumulative execution time |
| database | string | - | Database/schema filter |
| known_hashes | string | - | Comma-separated SHA-256(text)[:16] hashes to exclude |

**Response:**
```json
{
  "data": [
    {
      "id": "a3f8c2d1e4b09712",
      "text": "SELECT id FROM users WHERE status = ?",
      "raw_query": "SELECT id FROM users WHERE status = 'active'",
      "source": "production",
      "execution_time_ms": 84230.5,
      "calls": 14872,
      "tables": ["users"],
      "latest_ts": "2026-03-04T22:15:30Z",
      "user": "app_ro",
      "host": "10.0.1.22",
      "rows_sent": 1,
      "rows_examined": 48300,
      "lock_time_sec": 0
    }
  ],
  "next_cursor": "eyJleGVjdXRpb25fdGltZV9tcyI6ODQyMzAuNSwiaWQiOiJhM2Y4YzJkMWU0YjA5NzEyIn0=",
  "has_more": true,
  "total_count": 4821
}
```

---

### GET /api/v1/sql/databases

Returns distinct source database/schema names.

**Response:**
```json
{
  "databases": ["production", "analytics", "reporting"]
}
```

---

### POST /api/v1/sql/execute

Executes a single read-only SELECT query with timeout enforcement.

**Request:**
```json
{
  "query": "SELECT fingerprint_id, COUNT(*) AS calls FROM slow_logs GROUP BY fingerprint_id ORDER BY calls DESC LIMIT 100",
  "database": "analytics",
  "timeout_ms": 10000
}
```

**Response:**
```json
{
  "columns": ["fingerprint_id", "calls"],
  "rows": [["a3f8c2d1e4b09712", 42]],
  "row_count": 1,
  "truncated": false,
  "execution_time_ms": 12
}
```

Safety behavior:
- Rejects non-SELECT/multi-statement queries with `422 unsafe_query`
- Enforces hard timeout cap
- Runs against read-only API storage

---

## Statistics

### GET /api/v1/stats

Get database statistics.

**Response:**
```json
{
  "total_entries": 15234,
  "oldest_entry": "2024-01-01T00:00:00Z",
  "newest_entry": "2024-01-31T23:59:59Z",
  "unique_fingerprints": 456
}
```

**Fields:**
| Field | Type | Description |
|-------|------|-------------|
| total_entries | integer | Total number of slow log entries |
| oldest_entry | string (ISO 8601) | Timestamp of oldest entry |
| newest_entry | string (ISO 8601) | Timestamp of newest entry |
| unique_fingerprints | integer | Number of unique query fingerprints |

---

## Query

### POST /api/v1/query

Execute a raw SQL query against the DuckDB database.

**⚠️ Warning:** This endpoint allows arbitrary SQL execution. Use with caution.

**Request:**
```json
{
  "sql": "SELECT * FROM slow_logs LIMIT 10"
}
```

**Response:**
```json
{
  "columns": ["id", "fingerprint", "query_time_sec", "ts"],
  "rows": [
    [1, "select * from users where id = ?", 2.5, "2024-01-15T10:30:45Z"],
    [2, "select count(*) from orders", 5.2, "2024-01-15T10:31:12Z"]
  ],
  "count": 2
}
```

**Fields:**
| Field | Type | Description |
|-------|------|-------------|
| columns | array of strings | Column names |
| rows | array of arrays | Row data (each row is an array of values) |
| count | integer | Number of rows returned |

**Status Codes:**
- `200` - Query executed successfully
- `400` - Invalid request (missing SQL)
- `500` - Query execution error

**Common Queries:**

```sql
-- Top 10 slowest queries
SELECT sample_sql, query_time_sec, ts 
FROM slow_logs 
ORDER BY query_time_sec DESC 
LIMIT 10;

-- Queries by fingerprint
SELECT 
    fingerprint,
    COUNT(*) as count,
    AVG(query_time_sec) as avg_time,
    MAX(query_time_sec) as max_time
FROM slow_logs
GROUP BY fingerprint
ORDER BY count DESC;

-- Queries by user
SELECT 
    user,
    COUNT(*) as query_count,
    AVG(query_time_sec) as avg_time
FROM slow_logs
GROUP BY user
ORDER BY query_count DESC;

-- Hourly breakdown
SELECT 
    strftime('%Y-%m-%d %H:00', ts) as hour,
    COUNT(*) as count,
    AVG(query_time_sec) as avg_time
FROM slow_logs
GROUP BY hour
ORDER BY hour;

-- Full table scan queries
SELECT sample_sql, query_time_sec, rows_examined, rows_sent
FROM slow_logs
WHERE rows_examined > 10000 AND rows_sent < 100
ORDER BY rows_examined DESC;
```

---

## Parse Jobs

### POST /api/v1/parse

Start an asynchronous parsing job.

**Request:**
```json
{
  "log_dir": "/var/log/mysql"
}
```

**Parameters:**
| Field | Type | Required | Description |
|-------|------|----------|-------------|
| log_dir | string | Yes | Directory containing slow log files |

**Response:**
```json
{
  "job_id": "job_1772278579815985000",
  "status": "pending",
  "log_dir": "/var/log/mysql"
}
```

**Status Codes:**
- `202` - Job accepted
- `400` - Invalid request

---

### GET /api/v1/parse

List all parse jobs.

**Response:**
```json
{
  "jobs": [
    {
      "id": "job_1772278579815985000",
      "status": "completed",
      "log_dir": "/var/log/mysql",
      "started_at": "2026-02-28T14:36:19.815989+03:00",
      "ended_at": "2026-02-28T14:36:25.123456+03:00",
      "result": {
        "files_processed": 5,
        "entries_parsed": 15234,
        "entries_stored": 15234,
        "duration": "5.307467s"
      }
    }
  ]
}
```

**Job Status Values:**
- `pending` - Job queued but not started
- `running` - Currently parsing
- `completed` - Successfully finished
- `failed` - Error occurred

---

### GET /api/v1/parse/:job_id

Get status of a specific job.

**Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| job_id | string | Job identifier |

**Response:**
```json
{
  "id": "job_1772278579815985000",
  "status": "completed",
  "log_dir": "/var/log/mysql",
  "started_at": "2026-02-28T14:36:19.815989+03:00",
  "ended_at": "2026-02-28T14:36:25.123456+03:00",
  "result": {
    "files_processed": 5,
    "entries_parsed": 15234,
    "entries_stored": 15234,
    "duration": "5.307467s"
  },
  "error": ""
}
```

**Status Codes:**
- `200` - Job found
- `404` - Job not found

---

## Reports

### GET /api/v1/reports/top-queries

Get top queries by total execution time.

**Query Parameters:**
| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| limit | integer | 10 | Maximum number of results |

**Response:**
```json
{
  "columns": ["fingerprint", "count", "avg_time", "max_time", "total_time"],
  "rows": [
    ["select count(*) from orders...", 45, 5.234, 8.123, 235.53],
    ["select * from users where...", 120, 1.234, 3.456, 148.08]
  ],
  "count": 2
}
```

**Fields:**
| Field | Description |
|-------|-------------|
| fingerprint | Query fingerprint |
| count | Number of executions |
| avg_time | Average query time (seconds) |
| max_time | Maximum query time (seconds) |
| total_time | Total time spent on this query (seconds) |

---

### GET /api/v1/reports/slowest-queries

Get individual slowest query executions.

**Query Parameters:**
| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| limit | integer | 10 | Maximum number of results |
| min_time | float | 0 | Minimum query time filter |

**Response:**
```json
{
  "columns": ["sample_sql", "query_time_sec", "ts", "user", "host", "db"],
  "rows": [
    ["SELECT * FROM large_table...", 15.234, "2024-01-15T10:30:45Z", "app_user", "192.168.1.100", "production"]
  ],
  "count": 1
}
```

---

### GET /api/v1/reports/table-usage

Get statistics about table usage.

**Response:**
```json
{
  "columns": ["table_name", "query_count", "avg_time"],
  "rows": [
    ["users", 523, 1.234],
    ["orders", 234, 2.567],
    ["order_items", 156, 3.891]
  ],
  "count": 3
}
```

**Fields:**
| Field | Description |
|-------|-------------|
| table_name | Table name (lowercase) |
| query_count | Number of queries involving this table |
| avg_time | Average query time for queries on this table |

---

### GET /api/v1/reports/timeline

Get query volume and performance over time.

**Query Parameters:**
| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| interval | string | hour | Time bucket size: `minute`, `hour`, `day` |

**Response:**
```json
{
  "columns": ["time_bucket", "query_count", "avg_time", "max_time"],
  "rows": [
    ["2024-01-15 10:00", 45, 2.34, 8.12],
    ["2024-01-15 11:00", 67, 1.98, 5.43],
    ["2024-01-15 12:00", 23, 3.45, 12.1]
  ],
  "count": 3
}
```

---

## Search

### GET /api/v1/search/fingerprint

Search for query fingerprints containing a pattern.

**Query Parameters:**
| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| q | string | Yes | Search query (case-insensitive) |

**Response:**
```json
{
  "columns": ["fingerprint", "sample_sql"],
  "rows": [
    ["select * from users where id = ?", "SELECT * FROM users WHERE id = 123"],
    ["select name from users where status = ?", "SELECT name FROM users WHERE status = 'active'"]
  ],
  "count": 2
}
```

---

### GET /api/v1/search/table

Find all queries involving a specific table.

**Query Parameters:**
| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| table | string | Yes | Table name to search for |

**Response:**
```json
{
  "columns": ["sample_sql", "query_time_sec", "ts", "tables"],
  "rows": [
    ["SELECT * FROM users WHERE...", 2.5, "2024-01-15T10:30:45Z", ["users"]],
    ["SELECT * FROM users JOIN orders...", 5.2, "2024-01-15T10:31:12Z", ["users", "orders"]]
  ],
  "count": 2
}
```

---

## Error Responses

All errors follow this format:

```json
{
  "error": "machine_readable_code",
  "message": "Human readable description.",
  "request_id": "550e8400-e29b-41d4-a716-446655440000"
}
```

**Common Status Codes:**
| Code | Meaning |
|------|---------|
| 200 | Success |
| 202 | Accepted (async job started) |
| 400 | Bad Request - Invalid parameters |
| 401 | Invalid bearer token (`{"error":"invalid_token"}` for auth failures) |
| 404 | Not Found |
| 408 | Query timeout |
| 422 | Unsafe query |
| 503 | DB unavailable |
| 500 | Internal Server Error |

---

## Rate Limiting

Currently not implemented. Planned for Phase 3.

---

## OpenAPI Specification

Full OpenAPI 3.0 spec available at `/docs/openapi.yaml` (coming in Phase 2).

---

## SDK Examples

### cURL

```bash
# Health check
curl http://localhost:8080/health

# Get stats
curl http://localhost:8080/api/v1/stats

# Execute query
curl -X POST http://localhost:8080/api/v1/query \
  -H "Content-Type: application/json" \
  -d '{"sql": "SELECT * FROM slow_logs LIMIT 5"}'

# Start parse job
curl -X POST http://localhost:8080/api/v1/parse \
  -H "Content-Type: application/json" \
  -d '{"log_dir": "/var/log/mysql"}'

# Search by table
curl "http://localhost:8080/api/v1/search/table?table=users"
```

### Python

```python
import requests

base_url = "http://localhost:8080"

# Get stats
response = requests.get(f"{base_url}/api/v1/stats")
stats = response.json()
print(f"Total entries: {stats['total_entries']}")

# Query
response = requests.post(
    f"{base_url}/api/v1/query",
    json={"sql": "SELECT * FROM slow_logs LIMIT 10"}
)
result = response.json()
for row in result['rows']:
    print(row)

# Start parse job
response = requests.post(
    f"{base_url}/api/v1/parse",
    json={"log_dir": "/var/log/mysql"}
)
job = response.json()
print(f"Job ID: {job['job_id']}")
```

### JavaScript/Node.js

```javascript
const axios = require('axios');

const baseUrl = 'http://localhost:8080';

// Get top queries
async function getTopQueries() {
  const response = await axios.get(`${baseUrl}/api/v1/reports/top-queries?limit=5`);
  console.log(response.data);
}

// Execute query
async function executeQuery(sql) {
  const response = await axios.post(`${baseUrl}/api/v1/query`, { sql });
  return response.data;
}

// Search by table
async function searchByTable(table) {
  const response = await axios.get(`${baseUrl}/api/v1/search/table`, {
    params: { table }
  });
  return response.data;
}
```

---

*Last updated: 2026-02-28*
