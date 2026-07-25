# SQL Reference

`gofast-cli query` runs raw SQL against the DuckDB store, for questions the
[`digest`](README_DIGEST.md) report doesn't answer. If you just want to know
which queries are hurting, use `digest` first — it produces a ranked report
without any SQL.

```bash
gofast-cli query "SELECT sample_sql, query_time_sec FROM slow_logs ORDER BY query_time_sec DESC LIMIT 10"
```

The store is opened read-only, and the DuckDB engine is hardened: `ATTACH`,
`COPY`, `INSTALL`, and the filesystem-reading functions (`read_csv` and friends)
are blocked.

## Schema

### `slow_logs`

| Column | Type | Description |
|---|---|---|
| `id` | BIGINT | Auto-increment primary key |
| `file_hash` | VARCHAR(16) | Source file identity — joins to `files.hash` |
| `fingerprint_id` | VARCHAR(16) | Hash of the normalized query pattern (NOT NULL) |
| `sanitized_sql` | VARCHAR | Query with literals replaced by `?` |
| `sample_sql` | VARCHAR | One original query as logged |
| `user` | VARCHAR | MySQL user |
| `host` | VARCHAR | Client host |
| `db` | VARCHAR | Database in effect (from `USE`) |
| `query_time_sec` | DOUBLE | Execution time |
| `lock_time_sec` | DOUBLE | Lock wait time |
| `rows_sent` | UBIGINT | Rows returned to the client |
| `rows_examined` | UBIGINT | Rows examined by the server |
| `ts` | TIMESTAMP | Log entry timestamp (local timezone) |
| `created_at` | TIMESTAMP | Ingestion timestamp |
| `tables` | VARCHAR[] | Tables the query touches |
| `event_offset` | BIGINT | Byte offset in the source file |
| `event_hash` | VARCHAR(16) | Event identity, for deduplication |

There is **no `fingerprint` column** — the normalized pattern lives in
`sanitized_sql`, and `fingerprint_id` is its hash.

`user`, `host`, and `db` are nullable. Dedupe keys on
`(file_hash, event_offset)`.

### `slow_logs_with_source`

A view over `slow_logs` that adds **`source_host`**, derived from the source
file's name (extension stripped) via a LEFT JOIN to `files`. It is `NULL` when
the file is unknown, so `COALESCE(source_host, '')` is usually what you want
when grouping.

Prefer this view over the bare table whenever you care where a query came from.

### Indexes

`idx_fingerprint_id`, `idx_ts`, `idx_db`, `idx_user`, `idx_query_time`,
`idx_slow_logs_file_hash`, and `idx_files_path` on `files(file_path)`.

## Example queries

Every query below was run against a real store.

### Slowest individual queries

```sql
SELECT sample_sql, query_time_sec, ts
FROM slow_logs
ORDER BY query_time_sec DESC
LIMIT 10;
```

### Query patterns by total time

The `digest` command reports this with percentiles and percentages; use the SQL
form when you want a shape `digest` doesn't offer.

```sql
SELECT fingerprint_id,
       ANY_VALUE(sanitized_sql) AS pattern,
       COUNT(*)                 AS calls,
       AVG(query_time_sec)      AS avg_time,
       SUM(query_time_sec)      AS total_time
FROM slow_logs
GROUP BY fingerprint_id
ORDER BY total_time DESC
LIMIT 20;
```

### Every instance of one pattern

`fingerprint_id` is the same value the `digest` profile table prints as
`Query ID`, so you can copy it straight across.

```sql
SELECT sample_sql, query_time_sec, ts
FROM slow_logs
WHERE fingerprint_id = '4F361D3B54B0F76E'
ORDER BY query_time_sec DESC;
```

### Probable full table scans

Many rows examined, few returned.

```sql
SELECT sample_sql, rows_examined, rows_sent
FROM slow_logs
WHERE rows_examined > 10000 AND rows_sent < 100
ORDER BY rows_examined DESC;
```

### Hourly volume

```sql
SELECT strftime(ts, '%Y-%m-%d %H:00') AS hour,
       COUNT(*)            AS calls,
       AVG(query_time_sec) AS avg_time
FROM slow_logs
GROUP BY hour
ORDER BY hour;
```

Note DuckDB's argument order: `strftime(ts, format)`.

### By user

```sql
SELECT user,
       COUNT(*)            AS calls,
       AVG(query_time_sec) AS avg_time,
       MAX(query_time_sec) AS max_time
FROM slow_logs
GROUP BY user
ORDER BY calls DESC;
```

### Queries touching one table

```sql
SELECT sample_sql, query_time_sec, ts
FROM slow_logs
WHERE list_contains(tables, 'orders')
ORDER BY query_time_sec DESC;
```

### Per-table totals

`tables` is an array, so `UNNEST` fans a query out across every table it
touches. Totals across table groups can therefore exceed the query count —
that is the intended per-table semantic.

```sql
SELECT t          AS tbl,
       COUNT(*)   AS calls,
       SUM(query_time_sec) AS total_time
FROM slow_logs, UNNEST(tables) AS u(t)
GROUP BY t
ORDER BY total_time DESC;
```

Table names are stored exactly as the parser saw them: qualified
(`shop.order_items`) when a `USE` statement was in effect, bare
(`order_items`) otherwise. The same physical table can appear as both.

### Per source host

```sql
SELECT COALESCE(source_host, '') AS source_host,
       COUNT(*)                  AS calls,
       SUM(query_time_sec)       AS total_time
FROM slow_logs_with_source
GROUP BY source_host
ORDER BY total_time DESC;
```

The `COALESCE` matters: `source_host` is NULL for rows whose file is unknown,
and a NULL group would drop out of any subsequent `IN (...)` lookup.
