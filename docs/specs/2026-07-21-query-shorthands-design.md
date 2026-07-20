# Design: ergonomic `query` shorthands

**Date:** 2026-07-21
**Status:** Approved — ready for implementation plan
**Scope:** `cmd/cli` only. `pkg/` public surface stays frozen.

## Problem

`gofast-cli query` today takes a single required positional raw-SQL string:

```bash
gofast-cli query "select sample_sql from slow_logs_with_source order by query_time_sec desc limit 10"
```

This is verbose and error-prone for the common slow-query triage cases. We want
ergonomic shorthand flags:

```bash
gofast-cli query --order-by query_time --group-by fingerprint \
  --filter "source_host=10.10.10.1" --limit 10 --json
```

…**without breaking backward compatibility** — the raw-SQL form and its current
output must keep working byte-for-byte.

## Constraints (from CLAUDE.md)

- `pkg/{api,storage,parser,models,config,fingerprint}` is the public contract;
  do not grow or change its exported surface casually. **This feature adds no new
  exported `pkg/` symbols.** It only *consumes* already-exported ones
  (`storage.RecordFilter` + `RecordFilter.BuildWhere()`, `storage.Storage.Query`).
- Build requires `CGO_ENABLED=1` (DuckDB cgo), toolchain go 1.25.12.
- All new work lives in `cmd/cli` (favored over expanding `pkg/`).
- No secrets; `gitleaks` must pass.

## What already exists (reused, not rebuilt)

`pkg/storage/records.go` provides the SQLi-safe machinery we build on:

- `RecordFilter` struct + `RecordFilter.BuildWhere()` — renders a **parameterized**
  `WHERE …` clause plus its bound `?` args. Every user value is bound as a
  placeholder, never spliced into SQL text. Exported — we call it directly.
- `Storage.Query(ctx, sql, args...)` — executes arbitrary SQL with bound args and
  returns `models.ReportResult{Columns, Rows, Count}`. Exported — we call it.
- Pattern precedent: ORDER BY column/direction are SQL *identifiers* (not values),
  so they can never be `?`-bound; `records.go` closes the injection by only ever
  interpolating a literal drawn from a fixed allow-list (`allowedOrderColumns` +
  `validateOrder`). We replicate this **allow-list-only interpolation** technique
  for the CLI's own identifiers, since those pkg helpers are unexported.

`FilterRecords`/`ListRecords` are **not** reused: they return *preview* columns
(`SUBSTRING(sample_sql, 1, 200) AS ..._preview`), which would truncate the SQL and
break the `--json | jq` raw-SQL use case. They remain untouched for the machine API.

## Design

### 1. Command & backward compatibility

`query` stays a single command. `Args` loosens from `cobra.ExactArgs(1)` to
`cobra.MaximumNArgs(1)`. Dispatch on input:

| Input | Path |
|---|---|
| Positional SQL present, no shorthand flags | **Legacy** raw-SQL path — unchanged behavior & output |
| No positional, ≥1 shorthand flag | **Structured** path (new) |
| Both positional SQL *and* shorthand flags | Error: `pass either raw SQL or the query flags, not both` |
| Neither | Print command usage/help |

"Shorthand flags" = any of `--filter`, `--group-by`, `--order-by`, `--asc`,
`--limit`, `--offset`. (`--json`/`--markdown` are output flags and are allowed on
*both* paths; they do not by themselves select the structured path.)

The legacy path keeps its exact current output (the `Columns:/Rows:` text block)
when no output-format flag is given, preserving README examples and any scraping.

### 2. Flags

```
--filter KEY OP VALUE   (repeatable, AND-combined)   see §3
--group-by KEY          single column only (last wins if repeated)   see §4
--order-by COL          friendly column/aggregate name               see §4/§5
--asc                   ascending order (default: descending)
--limit N               default 50
--offset N              default 0
--json                  output as array-of-objects (full columns)    see §6
--markdown              output as report-style markdown              see §6
```

`--json` and `--markdown` are mutually exclusive; giving both is an error.

### 3. `--filter` grammar

Format: `KEY OP VALUE`. Whitespace around `OP` is optional — `query_time>2` and
`query_time > 2` both parse. Repeatable; all filters are `AND`-combined.

Supported keys/operators map **exactly** onto what `RecordFilter` models (so the
existing safe builder covers every case; no new operators are invented):

| filter key | operators | maps to `RecordFilter` field |
|---|---|---|
| `user` | `=` | `Users` (IN-list) |
| `host` | `=` | `Hosts` (IN-list) |
| `db` | `=` | `DBs` (IN-list) |
| `source_host` | `=` | `SourceHosts` (IN-list) |
| `fingerprint` | `=` | `FingerprintID` |
| `table` | `=` | `Tables` (`list_contains`) |
| `query_time` | `>` `<` | `QueryTimeGT` / `QueryTimeLT` |
| `lock_time` | `>` `<` | `LockTimeGT` / `LockTimeLT` |
| `rows_examined` | `>` | `RowsExaminedGT` |
| `ts` | `>` `>=` `<` `<=` | `TSFrom` (≥, from `>`/`>=`) / `TSTo` (≤, from `<`/`<=`) — inclusive |

- **Multi-value equality** (string keys): repeat the flag
  (`--filter user=root --filter user=app`) **or** comma-list
  (`--filter user=root,app`) — both accumulate into a single `IN (root, app)`.
- **Numeric/timestamp keys**: repeating the same key+op overwrites (last wins),
  since `RecordFilter` holds a single value per bound.
- **`ts` VALUE** parses as RFC3339 (`2006-01-02T15:04:05Z07:00`) or date-only
  (`2006-01-02`); unparseable → error.
- **Unsupported key** → error listing valid keys.
- **Unsupported key/op combo** (e.g. `rows_examined < 5`) → error naming the
  operators that key supports (e.g. `rows_examined supports only '>'`).

**Shell caveat (documented in `--help` + README):** an unquoted
`--filter query_time > 2` makes the *shell* redirect stdout to a file named `2`.
Users must quote it: `--filter "query_time > 2"` (or write `--filter query_time>2`).

### 4. `--group-by` (single column)

Allowed group keys → grouped SQL column (CLI-local allow-list):

| `--group-by` value | GROUP BY target |
|---|---|
| `fingerprint` | `fingerprint_id` |
| `user` | `user` |
| `db` | `db` |
| `host` | `host` |
| `source_host` | `source_host` |
| `table` | each element of the `tables` array, via `UNNEST` — see §5 |

Only one group column (a plain string flag; last wins if passed twice). Unknown
value → error listing valid keys.

`table` grouping expands the `tables VARCHAR[]` array with `UNNEST`: a query that
touches N tables contributes one row to each of its N table-groups. This is the
expected per-table report semantics (each table's stats include every query that
touched it), but it means `calls`/`total_time` summed *across* table-groups can
exceed the total query count — by design, not a bug. Rows with a NULL/empty
`tables` array do not appear in a `--group-by table` report. Group keys are the
table names exactly as stored in the array (parser-dependent; may include the
quoted form).

### 5. Two structured modes

Both modes build SQL in `cmd/cli` over the `slow_logs_with_source` view, using
`RecordFilter.BuildWhere()` for the WHERE clause (bound `?` args) and CLI-local
allow-lists for the only interpolated identifiers (SELECT is a static template;
GROUP BY / ORDER BY columns / direction come from allow-lists). No user value is
ever interpolated into SQL text.

**Ungrouped** (no `--group-by`) — full-column rows, no truncation:

```sql
SELECT id, fingerprint_id, sanitized_sql, sample_sql, user, host, db,
       query_time_sec, lock_time_sec, rows_sent, rows_examined, ts, created_at,
       tables, source_host
FROM slow_logs_with_source
<where>
ORDER BY <order_col> <dir>
LIMIT ? OFFSET ?
```

`--order-by` friendly names → allow-listed columns:
`query_time`→`query_time_sec`, `lock_time`→`lock_time_sec`, `rows_examined`,
`rows_sent`, `ts`, `created_at`, `id`. Default: `query_time desc`. `--asc` flips.
Unknown `--order-by` → error listing valid names.

**Grouped** (`--group-by KEY`) — digest-style aggregates, one row per group:

```sql
SELECT <group_col> AS "group",
       COUNT(*)                    AS calls,
       ROUND(SUM(query_time_sec), 2) AS total_time,
       ROUND(AVG(query_time_sec), 2) AS avg_time,
       ROUND(MAX(query_time_sec), 2) AS max_time,
       ROUND(AVG(rows_examined), 0)  AS avg_rows,
       ANY_VALUE(sample_sql)         AS sample_sql
FROM slow_logs_with_source
<where>
GROUP BY <group_col>
ORDER BY <order_col> <dir>
LIMIT ? OFFSET ?
```

For `--group-by table` the same aggregate template runs over the unnested array —
the `FROM`/`GROUP BY`/`SELECT` group column become (exact DuckDB `UNNEST`-in-`FROM`
syntax confirmed at implementation):

```sql
SELECT t AS "group", COUNT(*) AS calls, ... , ANY_VALUE(sample_sql) AS sample_sql
FROM slow_logs_with_source, UNNEST(tables) AS u(t)
<where>
GROUP BY t
ORDER BY <order_col> <dir>
LIMIT ? OFFSET ?
```

The `<where>` clause is still `RecordFilter.BuildWhere()`'s parameterized output
(applied before/independently of the UNNEST), so a `table=…` filter combined with
`--group-by table` works.

`--order-by` in grouped mode ∈ {`total_time` (default), `count`, `avg_time`,
`max_time`, `avg_rows`}, referenced by alias. Default `total_time desc`. `--asc`
flips. Unknown → error listing valid aggregate names. `sample_sql` is full (not
truncated) so `--json | jq` yields real SQL.

### 6. Output formats

Both renderers are **generic — driven by the returned column names**, so they
serve the legacy raw-SQL path too (`query "SELECT sample_sql …" --json | jq`).
A column is rendered as SQL if its name is exactly `sql` or ends with `_sql`
(catches `sample_sql`, `sanitized_sql`; predictable, avoids false hits like a
hypothetical `mysql_version`).

**Defaults:**
- Legacy path, no format flag → current `Columns:/Rows:` text (backward compat).
- Structured path, no format flag → report-style markdown.

**`--json`** — array of objects, one per row, keyed by column name; full,
un-truncated values:

```json
[
  { "id": 42, "fingerprint_id": "a1b2c3d4", "user": "app", "host": "10.0.0.5",
    "db": "shop", "source_host": "10.10.10.1", "query_time_sec": 4.12,
    "rows_examined": 15200, "ts": "2026-07-20T10:00:00Z",
    "sample_sql": "SELECT * FROM orders WHERE customer_id = 8831 ...",
    "sanitized_sql": "SELECT * FROM orders WHERE customer_id = ?" }
]
```

Array columns (e.g. `tables`) serialize as JSON arrays. `ts`/timestamps serialize
as RFC3339 strings (matching `Storage.Query`'s existing conversion).

**`--markdown`** — report style, not a table. Each row is a section; every field
is shown; any `sql`/`*_sql` column is rendered as a ```` ```sql ```` fenced block:

````markdown
### #42 · query_time 4.12s · rows_examined 15,200

| field | value |
|---|---|
| fingerprint_id | a1b2c3d4 |
| user@host | app@10.0.0.5 |
| db | shop |
| source_host | 10.10.10.1 |
| lock_time_sec | 0.002 |
| rows_sent | 1 |
| ts | 2026-07-20T10:00:00Z |

```sql
SELECT * FROM orders WHERE customer_id = 8831 ORDER BY created_at DESC
```
````

Grouped mode uses the same shape: heading = `a1b2c3d4 · 1240 calls · total 372.05s`,
a small field table for `avg_time`/`max_time`/`avg_rows`, then the representative
`sample_sql` fenced.

The heading is a best-effort convenience: the renderer picks a couple of
well-known columns for the `### …` line when present (`id`/`group`, `query_time_sec`,
`rows_examined` ungrouped; `group`, `calls`, `total_time` grouped) and otherwise
falls back to a generic `### row N` heading — so it never fails on arbitrary
raw-SQL columns.

### 7. Validation & errors (summary)

The structured path errors early and explicitly (better UX than `records.go`'s
silent `id`-fallback, which stays as-is inside `pkg`):

- Both positional SQL and shorthand flags given.
- `--json` and `--markdown` both given.
- Unknown/misformatted `--filter` (bad key, unsupported op for key, unparseable
  numeric/timestamp value, missing operator).
- Unknown `--group-by` key.
- Unknown `--order-by` name (mode-appropriate list).

### 8. File layout

Extract the query command out of the now-bloated `cmd/cli/main.go`:

- `cmd/cli/query.go` — `queryCmd()`, flag definitions, path dispatch, `--filter`
  parsing → `storage.RecordFilter`, ungrouped/grouped SQL builders + CLI allow-lists.
- `cmd/cli/render.go` — the three renderers: legacy text (moved verbatim), JSON
  (array-of-objects), markdown (report style). All take `*models.ReportResult`.
- `cmd/cli/main.go` — keeps wiring (`rootCmd.AddCommand(queryCmd())`) only.

`main.go`'s legacy output code moves into `render.go`'s legacy renderer unchanged,
so the legacy path stays byte-for-byte identical.

## Testing (TDD)

New `cmd/cli/query_test.go` (following the existing `serve_test.go` pattern —
build a temp DuckDB store, seed rows, exercise the command):

1. **Backward compat:** positional SQL still runs; default output identical to
   current `Columns:/Rows:` text.
2. **Dispatch:** positional + shorthand flags → error; `--json` + `--markdown` → error.
3. **`--filter` parsing:** `KEY OP VALUE` with/without spaces; repeat + comma →
   `IN`; numeric `>`/`<`; `ts` range; unknown key → error; unsupported op → error.
4. **Ungrouped:** filters applied; full (un-truncated) `sample_sql` returned;
   `--order-by`/`--asc` honored; `--limit`/`--offset` honored.
5. **Grouped:** aggregate values correct (`calls`, `total_time`, `avg_time`,
   `max_time`, `avg_rows`); grouped `--order-by` allow-list; unknown → error.
   `--group-by table`: a row touching N tables lands in N groups; a NULL/empty
   `tables` row appears in no table-group.
6. **Renderers:** JSON is a valid array-of-objects with full SQL; markdown fences
   `*_sql` columns; both work on a legacy raw-SQL result too.
7. **SQLi safety:** a `--filter` value containing SQL metacharacters
   (e.g. `--filter "db=x'; DROP TABLE slow_logs;--"`) is bound as a `?` value and
   matches nothing / does not execute.

## Out of scope (v1)

- Additional operators beyond what `RecordFilter` models (`!=`, `>=`/`<=` on
  numeric columns, `<` on `rows_examined`, etc.).
- Multi-column grouping.
- A compact markdown *table* mode (report style only for now).
- Normalizing quoted vs unquoted table names in `--group-by table` output
  (keys are shown as stored in the `tables` array).

These are all additive and can follow later without breaking this design.
