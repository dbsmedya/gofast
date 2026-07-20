# SQL Parser Documentation

Comprehensive documentation for GoFast's SQL query parser and validator used in the REST API.

---

## Table of Contents

1. [Overview](#overview)
2. [Architecture](#architecture)
3. [Tokenization](#tokenization)
4. [Validation Logic](#validation-logic)
5. [Security Features](#security-features)
6. [API Usage](#api-usage)
7. [Error Handling](#error-handling)
8. [Limitations](#limitations)
9. [Implementation Details](#implementation-details)

---

## Overview

GoFast includes a custom SQL parser designed to validate and sanitize user-submitted queries for the `/api/v1/sql/execute` endpoint. The parser ensures that only safe, read-only SELECT queries can be executed against the DuckDB database.

### Purpose

- **Security**: Prevent data modification, schema changes, and unauthorized access
- **Safety**: Block potentially dangerous operations (DROP, DELETE, etc.)
- **Compliance**: Ensure queries are truly read-only
- **Performance**: Fast validation with minimal overhead

### Key Features

- Token-based SQL parsing with state tracking
- Support for complex SELECT and WITH...SELECT queries
- Comment handling (both `--` and `/* */` styles)
- String literal handling (single quotes, double quotes, backticks)
- Forbidden keyword detection
- System schema access prevention

---

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                    SQL Query Input                          │
└─────────────────────────┬───────────────────────────────────┘
                          │
              ┌───────────▼───────────┐
              │     Tokenizer         │
              │                       │
              │ • Character scanning  │
              │ • State tracking      │
              │ • Comment skipping    │
              │ • String handling     │
              └───────────┬───────────┘
                          │
              ┌───────────▼───────────┐
              │    Token Stream       │
              │  ["select", "*",      │
              │   "from", "users"]    │
              └───────────┬───────────┘
                          │
              ┌───────────▼───────────┐
              │      Validator        │
              │                       │
              │ • First token check   │
              │ • Forbidden words     │
              │ • Schema access       │
              │ • Multi-statement     │
              └───────────┬───────────┘
                          │
              ┌───────────▼───────────┐
              │    Validation Result  │
              │    (ok or error)      │
              └───────────────────────┘
```

### Components

| Component | Location | Responsibility |
|-----------|----------|----------------|
| `tokenizeSQL()` | `cmd/api/main.go` | Converts SQL string into token array |
| `validateReadOnlySelectQuery()` | `cmd/api/main.go` | Validates token stream against rules |
| Helper functions | `cmd/api/main.go` | Type conversion utilities (`toFloat64`, `toInt64`, etc.) |

---

## Tokenization

The tokenizer performs a single-pass scan of the SQL input, converting it into a normalized token stream.

### Token Types

| Type | Examples | Handling |
|------|----------|----------|
| **Keywords** | `SELECT`, `FROM`, `WHERE` | Lowercased, delimited by non-word chars |
| **Identifiers** | `table_name`, `column1` | Preserved with underscores and dots |
| **Operators** | `=`, `<`, `>`, `+`, `-` | Single-char delimiters |
| **Literals** | `'string'`, `123` | Strings ignored (content not tokenized) |
| **Comments** | `-- line`, `/* block */` | Skipped entirely |

### State Machine

The tokenizer maintains state for:

```go
// String/quote states
inSingle      // Inside 'string'
inDouble      // Inside "identifier"
inBacktick    // Inside `identifier`

// Comment states
inLineComment     // Inside -- comment
inBlockComment    // Inside /* */ comment
```

### Tokenization Rules

1. **Word Characters** (`[a-zA-Z0-9_.$]`) accumulate into tokens
2. **Non-word Characters** delimit tokens and are discarded (except semicolon)
3. **Semicolon** (`;`) triggers an error (multi-statement prevention)
4. **Comments** are skipped and don't produce tokens
5. **String Literals** are consumed but their content is not tokenized

### Example Tokenization

```sql
-- Input
SELECT u.id, u.name FROM users u WHERE u.status = 'active' AND u.created_at > '2024-01-01'

-- Tokens
["select", "u.id", "u.name", "from", "users", "u", "where", "u.status", "and", "u.created_at"]
```

```sql
-- Input with comments
SELECT /* count all */ COUNT(*) FROM users -- get user count
WHERE status = 'active'

-- Tokens
["select", "count", "from", "users", "where", "status"]
```

---

## Validation Logic

### Step 1: First Token Check

The query must start with an allowed statement type:

```go
allowedFirstTokens := []string{"select", "with"}
```

If the first token is not `select` or `with`, validation fails.

### Step 2: WITH Clause Validation

For queries starting with `WITH`:

```go
// Must contain a SELECT somewhere in the query
if first == "with" {
    hasSelect := false
    for _, token := range tokens {
        if token == "select" {
            hasSelect = true
            break
        }
    }
    if !hasSelect {
        return error("with query must contain a select")
    }
}
```

This allows CTEs (Common Table Expressions):
```sql
WITH ranked_users AS (
    SELECT id, name, ROW_NUMBER() OVER (ORDER BY created_at) as rank
    FROM users
)
SELECT * FROM ranked_users WHERE rank <= 10
```

### Step 3: Forbidden Keyword Check

The parser maintains a blacklist of forbidden operations:

```go
forbidden := map[string]bool{
    "insert": true, "update": true, "delete": true,
    "drop": true, "alter": true, "create": true,
    "truncate": true, "set": true, "grant": true,
    "revoke": true, "attach": true, "copy": true,
    "export": true, "import": true, "call": true,
    "vacuum": true, "analyze": true, "pragma": true,
    "begin": true, "commit": true, "rollback": true,
    "transaction": true,
}
```

Any match results in validation failure with "unsafe statement detected".

### Step 4: System Schema Protection

Access to system schemas is blocked:

```go
forbiddenSchemas := map[string]bool{
    "information_schema": true,
    "pg_catalog": true,
    "sqlite_master": true,
}

// Also blocks any schema starting with "duckdb_"
if strings.HasPrefix(token, "duckdb_") {
    return error("system schema access is not allowed")
}
```

This prevents:
- Information schema enumeration
- DuckDB internal tables access
- PostgreSQL catalog access

---

## Security Features

### 1. Multi-Statement Prevention

Semicolons in queries are rejected:

```go
if ch == ';' {
    return nil, fmt.Errorf("multiple statements are not allowed")
}
```

This prevents injection attacks like:
```sql
SELECT * FROM users; DROP TABLE users; --
```

### 2. Comment Injection Protection

Comments are properly parsed and skipped, preventing bypass techniques:

```sql
-- Blocked: DELETE/**/FROM users
-- The tokenizer skips the block comment, but DELETE is still detected
```

### 3. String Literal Handling

String contents are not tokenized, preventing quote-based bypasses:

```sql
-- Input
SELECT 'update users set admin = true'

-- Tokens
["select"]  -- The UPDATE inside the string is not a token
```

### 4. Case Normalization

All tokens are lowercased for consistent matching:

```sql
-- These all produce the same tokens
SELECT * FROM users
select * from users
SeLeCt * FrOm users
```

### 5. Timeout Protection

Query execution has configurable timeouts (default 10s, max 30s):

```go
timeoutMS := 10000  // Default
if req.TimeoutMS != nil {
    timeoutMS = min(*req.TimeoutMS, 30000)
}
```

### 6. Row Limit Protection

Hard cap of 10,000 rows prevents memory exhaustion:

```go
const rowCap = 10000
wrappedQuery := fmt.Sprintf("SELECT * FROM (%s) AS q LIMIT %d", query, rowCap+1)
```

---

## API Usage

### Endpoint

```
POST /api/v1/sql/execute
Authorization: Bearer <GOFAST_API_KEY>
Content-Type: application/json
```

### Request Format

```json
{
  "query": "SELECT * FROM slow_logs LIMIT 10",
  "database": "optional_db_filter",
  "timeout_ms": 5000
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `query` | string | Yes | SQL query to execute |
| `database` | string | No | Database name filter |
| `timeout_ms` | int | No | Query timeout (default: 10000, max: 30000) |

### Response Format

**Success:**
```json
{
  "columns": ["id", "fingerprint_id", "query_time_sec"],
  "rows": [
    [1, "abc123...", 2.5],
    [2, "def456...", 1.8]
  ],
  "row_count": 2,
  "truncated": false,
  "execution_time_ms": 45
}
```

**Error:**
```json
{
  "error": "unsafe_query",
  "message": "Only a single read-only SELECT query is allowed.",
  "request_id": "550e8400-e29b-41d4-a716-446655440000"
}
```

### Example Queries

**Basic SELECT:**
```bash
curl -X POST http://localhost:8080/api/v1/sql/execute \
  -H "Authorization: Bearer $GOFAST_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"query": "SELECT COUNT(*) FROM slow_logs"}'
```

**WITH Clause (CTE):**
```bash
curl -X POST http://localhost:8080/api/v1/sql/execute \
  -H "Authorization: Bearer $GOFAST_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "query": "WITH slow_queries AS (SELECT * FROM slow_logs WHERE query_time_sec > 1) SELECT * FROM slow_queries LIMIT 10"
  }'
```

**Aggregate Query:**
```bash
curl -X POST http://localhost:8080/api/v1/sql/execute \
  -H "Authorization: Bearer $GOFAST_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "query": "SELECT fingerprint_id, COUNT(*) as cnt, AVG(query_time_sec) as avg_time FROM slow_logs GROUP BY fingerprint_id ORDER BY cnt DESC LIMIT 20"
  }'
```

---

## Error Handling

### Validation Errors

| Error Code | HTTP Status | Description |
|------------|-------------|-------------|
| `invalid_param` | 400 | Invalid request body or parameters |
| `unsafe_query` | 422 | Query contains forbidden keywords or statements |

### Execution Errors

| Error Code | HTTP Status | Description |
|------------|-------------|-------------|
| `query_timeout` | 408 | Query exceeded timeout limit |
| `internal_error` | 500 | Database execution failed |
| `db_unavailable` | 503 | Database connection unavailable |

### Tokenization Errors

| Message | Cause |
|---------|-------|
| `multiple statements are not allowed` | Semicolon detected in query |
| `unterminated SQL input` | Unclosed string or comment |
| `empty query` | No valid tokens found |

### Validation Error Examples

```sql
-- Error: multiple statements are not allowed
SELECT * FROM users; DELETE FROM users

-- Error: only select queries are allowed
INSERT INTO users VALUES (1, 'test')

-- Error: unsafe statement detected
DROP TABLE slow_logs

-- Error: system schema access is not allowed
SELECT * FROM information_schema.tables

-- Error: with query must contain a select
WITH x AS (DELETE FROM users) SELECT 1
```

---

## Limitations

### 1. Syntax Awareness

The parser is **token-based**, not a full SQL AST parser. It does not:
- Understand complex nested structures
- Validate column or table existence
- Check SQL syntax correctness (DuckDB handles this)
- Parse expressions or operator precedence

### 2. Context Sensitivity

Some edge cases may not be caught:

```sql
-- Technically safe but might be flagged (if 'update' appears as literal)
SELECT 'update' as action FROM logs

-- Workaround: use aliasing
SELECT 'modify' as action FROM logs
```

### 3. DuckDB-Specific Features

Some DuckDB features may not work:
- `PIVOT` / `UNPIVOT` (not explicitly tested)
- `MACRO` definitions (blocked by forbidden keywords)
- `COPY` statements (explicitly blocked)

### 4. Unicode Handling

The tokenizer operates on bytes, not runes. Non-ASCII identifiers may not tokenize correctly.

### 5. Subquery Depth

No explicit limit on subquery nesting, but complex queries may hit:
- Timeout limits
- Row limits
- DuckDB's own complexity limits

---

## Implementation Details

### Tokenizer Code Structure

```go
func tokenizeSQL(query string) ([]string, error) {
    tokens := make([]string, 0, 32)
    var current strings.Builder
    
    // State flags
    inSingle := false
    inDouble := false
    inBacktick := false
    inLineComment := false
    inBlockComment := false
    
    // Helper to flush current token
    flush := func() {
        if current.Len() > 0 {
            tokens = append(tokens, strings.ToLower(current.String()))
            current.Reset()
        }
    }
    
    // Character-by-character scan
    for i := 0; i < len(query); i++ {
        ch := query[i]
        // ... state machine logic
    }
    
    flush()  // Don't forget final token
    return tokens, nil
}
```

### Validation Code Structure

```go
func validateReadOnlySelectQuery(raw string) error {
    // Step 1: Tokenize
    tokens, err := tokenizeSQL(raw)
    if err != nil {
        return err
    }
    
    // Step 2: Check for tokens
    if len(tokens) == 0 {
        return fmt.Errorf("empty query")
    }
    
    // Step 3: Validate first token
    first := tokens[0]
    if first != "select" && first != "with" {
        return fmt.Errorf("only select queries are allowed")
    }
    
    // Step 4: WITH must contain SELECT
    if first == "with" {
        // ... check for select in tokens
    }
    
    // Step 5: Check forbidden keywords
    for _, token := range tokens {
        if forbidden[token] {
            return fmt.Errorf("unsafe statement detected")
        }
        if forbiddenSchemas[token] || strings.HasPrefix(token, "duckdb_") {
            return fmt.Errorf("system schema access is not allowed")
        }
    }
    
    return nil
}
```

### Testing the Parser

```go
// Unit test example
func TestTokenizeSQL(t *testing.T) {
    tests := []struct {
        input    string
        expected []string
        wantErr  bool
    }{
        {
            input:    "SELECT * FROM users",
            expected: []string{"select", "*", "from", "users"},
        },
        {
            input:    "SELECT * FROM users; DROP TABLE users",
            wantErr:  true, // multi-statement
        },
    }
    
    for _, tt := range tests {
        tokens, err := tokenizeSQL(tt.input)
        if tt.wantErr {
            assert.Error(t, err)
        } else {
            assert.NoError(t, err)
            assert.Equal(t, tt.expected, tokens)
        }
    }
}
```

---

## Future Enhancements

### Potential Improvements

1. **Full AST Parser**: Integrate a proper SQL parser for more accurate validation
2. **Query Complexity Analysis**: Limit based on estimated query cost
3. **Allow List**: Configurable list of allowed tables/columns
4. **Query Whitelist**: Pre-approved query patterns
5. **Audit Logging**: Log all executed queries for security review

### Alternative Approaches

| Approach | Pros | Cons |
|----------|------|------|
| Current (Token-based) | Fast, simple, no dependencies | Limited semantic understanding |
| DuckDB EXPLAIN | Leverages database parser | Requires connection, slower |
| pg_query (C extension) | Full AST parsing | CGO dependency, heavier |
| ANTLR Grammar | Complete SQL support | Complex, large dependency |

---

*Last updated: 2026-03-14*
