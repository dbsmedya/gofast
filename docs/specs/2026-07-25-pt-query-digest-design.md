# Design: `gofast-cli digest` — a pt-query-digest-compatible report

**Date:** 2026-07-25
**Status:** Approved — ready for implementation plan
**Scope:** `internal/digest` (new) + `cmd/cli` (new files). `pkg/` is untouched.
**Release:** v1.0.0

## Problem

`gofast-cli` can parse slow logs into DuckDB, but reading the result requires
hand-written SQL:

```bash
gofast-cli query "select fingerprint_id, count(*), sum(query_time_sec) \
  from slow_logs_with_source group by 1 order by 3 desc limit 10"
```

Every MySQL DBA already knows a tool that answers this question:
`pt-query-digest`. Rather than invent a new reporting interface, we adopt the one
that has been in the field for ~20 years, so existing muscle memory transfers.

## Relationship to the 2026-07-21 decision

On 2026-07-21 a spec for `query` shorthands (`--filter/--group-by/--order-by`
plus preset reports) was **withdrawn** (commit `47920c6`) because `gofast-mcp`
already implements that analysis in Python over the REST API, and a Go copy would
mean two drifting implementations.

That decision is **superseded, deliberately and on narrower grounds**:

- The earlier proposal invented a *new* interface. This one reproduces an
  *existing, external* one — the value is familiarity, which the Python MCP tools
  (`top_queries`, `get_impacting_queries`) do not provide and are not trying to.
- The Python path stays a **peer, not a fallback**. Nothing here restricts or
  deprecates `gofast-mcp`. It remains the right tool for agents talking to a
  remote API.
- The Go path exists because operators frequently **cannot reach the collector**
  holding a large slow-log DuckDB file over the API — they are on the box, with
  the file, and no `serve` running.

The drift risk is accepted with eyes open: the digest's fixed report shape is
different in kind from the MCP tools' ad-hoc rankings, so the two are not
substitutes and are not expected to converge.

## Constraints

- **`pkg/` gains nothing.** Verified: every symbol the digest needs is already
  exported — `storage.NewStorage`, `storage.Storage.Query(ctx, sql, args...)`
  (`pkg/storage/duckdb.go:450`, already variadic), `storage.RecordFilter` +
  `RecordFilter.BuildWhere()` (`pkg/storage/records.go:72,86`), and
  `models.ReportResult`.
- **The REST API is locked.** No new endpoints, no `pkg/api/openapi.yaml` change,
  no `config.yaml` schema change. Enforced by a gate (§10).
- Build requires `CGO_ENABLED=1`; toolchain go 1.25.12.
- No secrets; `gitleaks` must pass.
- The DuckDB engine hardening (`2b56133`) blocks `read_csv`/`COPY`/`ATTACH`/
  `INSTALL`. The digest uses only builtin aggregates, so it is unaffected.

## Why `internal/` and not `pkg/`

`gofastd` (the enterprise binary) has different internals and does not need the
CLI's report: `gofast-enterprise/backend/internal/query` already owns the
dashboard's grouped-aggregation SQL, which is a *different product* — an
interactive, pageable table with user-chosen ordering, not a fixed-format report.

So the digest ships as `internal/`, and enterprise consumes it by **shipping the
`gofast-cli` binary in its image**, not by importing a library. Go's `internal`
rule makes that the only route, which is intended. Promotion to `pkg/digest`
later is a purely additive minor bump if a digest *screen* is ever designed;
freezing now would commit v1.0.0 to a shape guessed at for a consumer that does
not exist.

## 1. Command surface

```
gofast-cli digest [flags]

--group-by KEY      fingerprint (default) | user | db | host | source_host | tables
--order-by ATTR:AGG default Query_time:sum
--limit N           default 20
--filter "K OP V"   repeatable, AND-combined
--since VALUE       lower time bound
--until VALUE       upper time bound
--json              emit JSON; markdown otherwise
```

Global flags (`--config`, `--duck-db-path`) apply as they do to every command.
The store is opened **read-only**.

### Deviations from pt-query-digest (deliberate)

| pt-query-digest | here | why |
|---|---|---|
| `--filter` is Perl code | `KEY OP VALUE` grammar | Perl is not available; §2 |
| `--limit` defaults to `95%:20` | plain int, default `20` | simpler; percentage semantics not carried |
| `--output report\|json\|…` | `--json` boolean | matches the rest of the gofast tooling |
| `--group-by` takes a comma list, one report per value | single value | one report per invocation keeps stdout pipeable |
| aggregates `sum,min,max,cnt` | adds `avg` | ordering by mean latency is genuinely useful and additive |

Everything else keeps pt-query-digest's names, defaults, and semantics.

### `--order-by`

Format `ATTRIBUTE:AGGREGATE`. Both halves are matched **case-insensitively**, so
`Query_time:sum` and `query_time:sum` are equivalent.

| attribute | column |
|---|---|
| `Query_time` | `query_time_sec` |
| `Lock_time` | `lock_time_sec` |
| `Rows_sent` | `rows_sent` |
| `Rows_examined` | `rows_examined` |

| aggregate | SQL |
|---|---|
| `sum` | `SUM(col)` |
| `min` | `MIN(col)` |
| `max` | `MAX(col)` |
| `avg` | `AVG(col)` |
| `cnt` | `COUNT(*)` |

`cnt` ignores the attribute — `Query_time:cnt` and `Rows_sent:cnt` are the same
ordering (by call frequency). This matches pt-query-digest and is documented in
`--help` rather than rejected.

Ordering is always **descending** (worst first). pt-query-digest has no ascending
flag and neither do we.

### `--group-by`

| value | grouped by |
|---|---|
| `fingerprint` (default) | `fingerprint_id` |
| `user` | `user` |
| `db` | `db` |
| `host` | `host` |
| `source_host` | `source_host` |
| `tables` (alias `table`) | each element of `tables`, via `UNNEST` |

`tables` is pt-query-digest's spelling; `table` is accepted as an alias because
that is the `--filter` key name. Both are also accepted for the filter key.

Grouping by `tables` fans out: a query touching N tables contributes one row to
each of its N groups, so sums across table groups can exceed the total query
count. This is the intended per-table semantic, not a bug. Rows with a NULL or
empty `tables` array appear in no table group.

**NULL group keys.** `user`, `host`, `db`, and `source_host` are all nullable
(`source_host` comes from a LEFT JOIN, so it is NULL for rows whose file is
unknown). A NULL key cannot survive pass 3's `IN (?)` lookup, because
`NULL IN (NULL)` evaluates to NULL rather than true — the group would silently
vanish between passes. The group column is therefore `COALESCE(<col>, '')` in
**both** passes 2 and 3, so the key round-trips as a bindable empty string. It
renders as `(none)` in markdown and as `""` in JSON.

Consequence: a genuine empty-string value and a NULL collapse into the same
group. That is accepted — the distinction is not meaningful for slow-log
attributes, and the alternative (`IS NOT DISTINCT FROM` chains) complicates every
pass for no operational gain. `fingerprint_id` is `NOT NULL`, and `UNNEST` drops
NULL/empty arrays, so neither of those groupings is affected.

## 2. `--filter` grammar

Format `KEY OP VALUE`. Whitespace around `OP` is optional: `query_time>2` and
`query_time > 2` both parse. Repeatable; all filters are AND-combined.

Keys and operators map **exactly** onto what `RecordFilter` already models, so
`BuildWhere()` covers every case and no new operator is invented:

| key | ops | `RecordFilter` field |
|---|---|---|
| `fingerprint` | `=` | `FingerprintID` |
| `user` | `=` | `Users` (IN) |
| `host` | `=` | `Hosts` (IN) |
| `db` | `=` | `DBs` (IN) |
| `source_host` | `=` | `SourceHosts` (IN) |
| `table` (alias `tables`) | `=` | `Tables` (`list_contains`) — **single value only** |
| `query_time` | `>` `<` | `QueryTimeGT` / `QueryTimeLT` |
| `lock_time` | `>` `<` | `LockTimeGT` / `LockTimeLT` |
| `rows_examined` | `>` | `RowsExaminedGT` |

- **Multi-value equality** applies to `user`, `host`, `db`, and `source_host`
  only: repeat the flag (`--filter "user = root" --filter "user = app"`) or use
  a comma list (`--filter "user = root,app"`). Both accumulate into one
  `IN (…)`, so they mean **either**.
- **`table` accepts exactly one value.** A comma list or a repeated `table`
  filter is an error naming `--group-by tables` as the way to get a per-table
  breakdown.

  This is not an arbitrary restriction. `RecordFilter.BuildWhere()` renders each
  table value as its own `list_contains` predicate and joins all predicates with
  `AND` (`pkg/storage/records.go:104`), so `table = orders,customers` would mean
  "touches **both**" — verified against DuckDB — while the identical comma
  syntax on `user` means "**either**". Rather than ship two silently opposite
  meanings for one syntax, or hand-roll an OR predicate that no longer delegates
  to the audited `BuildWhere()`, multi-value table filters are rejected outright.

- **Empty values are rejected.** `--filter "user ="` and `--filter "user = ,"`
  both error rather than silently producing an unfiltered run.
- **Numeric keys:** repeating the same key+op overwrites (last wins), because
  `RecordFilter` holds a single bound per comparison.
- **`rows_sent` is not a filter key.** It is reported but not filterable —
  `RecordFilter` has no `RowsSent` field, and adding one would be a `pkg/`
  change, which this work explicitly does not make.
- **Unknown key** → error listing valid keys.
- **Unsupported key/op pair** (e.g. `rows_examined < 5`) → error naming the
  operators that key does support.

**Shell caveat** (documented in `--help` and the README): unquoted
`--filter query_time > 2` makes the *shell* redirect stdout into a file named
`2`. Users must quote: `--filter "query_time > 2"`.

## 3. `--since` / `--until`

Map onto `RecordFilter.TSFrom` / `TSTo` (both inclusive). Accepted formats match
pt-query-digest's documented set, minus the MySQL-expression form:

- **Relative:** `N[shmd]` — seconds, hours, minutes, days — resolved against the
  current time. A bare integer is seconds. `--since 7d`, `--since 90m`.
- **Absolute:** `YYYY-MM-DD` or `YYYY-MM-DD HH:MM:SS`.

pt-query-digest also accepts MySQL expressions such as
`CURRENT_DATE - INTERVAL 7 DAY`; that requires a MySQL connection to evaluate and
is **rejected with a clear error** naming the two supported forms.

Timestamps are interpreted in the machine's **local** timezone. This is not a
choice but a requirement of matching stored data: `pkg/parser/engine.go:475`
passes an empty `log.Options{}`, so the percona parser falls back to `time.Local`
when parsing slow-log timestamps (`log/slow/parser.go:81-83`). Interpreting
bounds in UTC would silently offset every comparison.

## 4. Report shape

The shape is fixed — it is not user-selectable. The schema stores four numeric
attributes, so pt-query-digest's `Bytes sent`, `Tmp tables`, and `Merge passes`
rows have no source and are simply absent.

Reported attributes: **Count**, **Exec time** (`query_time_sec`), **Lock time**
(`lock_time_sec`), **Rows sent** (`rows_sent`), **Rows examined**
(`rows_examined`).

### Profile

| column | meaning |
|---|---|
| Rank | position after ordering |
| Query ID | the group key (bare `fingerprint_id`, no `0x` prefix — see below) |
| Response time | class `SUM(query_time_sec)` and its % of the filtered total |
| Calls | `COUNT(*)` |
| R/Call | `AVG(query_time_sec)` |
| V/M | `var_samp(query_time_sec) / AVG(query_time_sec)` |
| Item | distilled query — verb + table list |

`Query ID` prints the stored `fingerprint_id` **without** pt-query-digest's `0x`
prefix, because that exact string is what `--filter "fingerprint = …"` accepts.
Copy-pasteability beats cosmetic fidelity here.

`Item` is derived as the leading SQL verb from `sanitized_sql` plus the `tables`
array (e.g. `SELECT orders`), reproducing pt-query-digest's `distill` output for
the common cases. It is **always** the distilled representative query, including
for non-fingerprint groupings — under `--group-by user`, `Query ID` is the
username and `Item` shows what that user predominantly runs, which is more
informative than repeating the key.

### The representative row

`Item` and `sample_sql` describe *one* query, so they must come from one row.
The selection rule is explicit and deterministic:

1. Within each group, rank fingerprints by call count, descending; ties break on
   `fingerprint_id` ascending.
2. Take the winning fingerprint — the query the group runs most often.
3. From that fingerprint's rows, take the **slowest** one
   (`arg_max(..., query_time_sec)`), so the sample shown is its worst instance.

All three representative fields (`sample_sql`, `sanitized_sql`, `tables`) are
drawn from that single row. `ANY_VALUE` is deliberately **not** used: three
independent `ANY_VALUE` calls carry no guarantee of agreeing on a row, so `Item`
could be distilled from one row's `sanitized_sql` combined with another row's
`tables`. In practice DuckDB happened to correlate them in testing, which makes
this a latent rather than visible defect — exactly the kind that appears later
under parallel aggregation on a large store.

For `--group-by fingerprint` step 1 is a no-op (the group *is* the fingerprint),
so the rule degenerates to "the slowest instance of this query", which is what a
digest should show anyway.

### Per-class detail

Columns `pct, total, min, max, avg, 95%, stddev, median` for each attribute.

- `pct` is per-attribute: the class's `SUM(attr)` over the **filtered
  population's** `SUM(attr)`. Filters therefore change percentages, which is
  correct — percentages describe the analysed set.
- The `Count` row carries only `pct` (calls / total calls) and `total` (calls);
  its min/max/avg/95%/stddev/median cells are blank, as in pt-query-digest.
- `95%` is `quantile_cont(col, 0.95)`; `median` is `quantile_cont(col, 0.5)`.
  DuckDB computes both exactly, where pt-query-digest approximates from
  bucketed histograms.
- `stddev` is `stddev_samp`. For a single-row group both `stddev_samp` and
  `var_samp` return NULL; these are coerced to `0`, making V/M `0` as well.
- If a global attribute sum is `0`, `pct` is `0` (no division by zero).

## 5. SQL design — four passes

All passes run against the `slow_logs_with_source` view and share one
`RecordFilter.BuildWhere()` result (clause text + bound args).

**Pass 1 — globals.** Feeds every `pct`.

```sql
SELECT COUNT(*), SUM(query_time_sec), SUM(lock_time_sec),
       SUM(rows_sent), SUM(rows_examined)
FROM slow_logs_with_source <where>
```

**Pass 2 — ranking.** Cheap: only the ordering aggregate, no percentiles.

```sql
SELECT <group_col> AS grp, COUNT(*) AS calls, <agg>(<attr>) AS rank_val
FROM slow_logs_with_source <where>
GROUP BY grp
ORDER BY rank_val DESC, grp ASC
LIMIT ?
```

The `grp ASC` secondary sort is required, not cosmetic: ties on `rank_val` are
common (two queries with identical total time, or any `--order-by` on a
low-cardinality attribute), and with `LIMIT` a tie at the boundary would
otherwise decide *which classes appear at all* nondeterministically.

**Pass 3 — statistics**, restricted to the group keys pass 2 returned:

```sql
SELECT <group_col> AS grp,
       COUNT(*),
       SUM(query_time_sec), MIN(query_time_sec), MAX(query_time_sec),
       AVG(query_time_sec), quantile_cont(query_time_sec, 0.95),
       stddev_samp(query_time_sec), quantile_cont(query_time_sec, 0.5),
       var_samp(query_time_sec),
       -- and the same seven statistics (SUM, MIN, MAX, AVG, p95, stddev,
       -- median) for lock_time_sec, rows_sent, and rows_examined.
       -- var_samp is computed for query_time_sec only, since V/M is
       -- defined over response time.
FROM slow_logs_with_source
<where> AND <group_col> IN (?, ?, …)
GROUP BY grp
```

**Pass 4 — representative rows**, also restricted to the ranked keys. It
implements the selection rule above: aggregate per (group, fingerprint), then
keep the highest-count fingerprint per group.

```sql
WITH per_fp AS (
  SELECT <group_col> AS grp,
         fingerprint_id,
         COUNT(*) AS fp_calls,
         arg_max(sample_sql,    query_time_sec) AS sample_sql,
         arg_max(sanitized_sql, query_time_sec) AS sanitized_sql,
         arg_max(tables,        query_time_sec) AS tables
  FROM slow_logs_with_source
  <where> AND <group_col> IN (?, ?, …)
  GROUP BY grp, fingerprint_id
),
ranked AS (
  SELECT *, ROW_NUMBER() OVER (
             PARTITION BY grp
             ORDER BY fp_calls DESC, fingerprint_id ASC) AS rn
  FROM per_fp
)
SELECT grp, sample_sql, sanitized_sql, tables FROM ranked WHERE rn = 1
```

The three `arg_max` calls share one ordering expression, so they resolve to the
same row. Verified against DuckDB for both `VARCHAR` and `VARCHAR[]`.

Pass 4 is separate from pass 3 because the two work at different grains — pass 3
groups by `grp`, pass 4 by `(grp, fingerprint_id)` — and it is cheap: both are
already restricted to at most `--limit` group keys.

**Why several passes rather than one:** the motivating case is a large DuckDB
file on a collector. A single pass would compute percentiles and standard
deviations for every group in the table — potentially millions of fingerprints —
to display 20. Pass 2 narrows to N first, so the expensive per-group sorting in
passes 3 and 4 runs over N groups only.

### Injection safety

- The **only** interpolated SQL text is the group column and the order
  aggregate/attribute, each drawn from the fixed allow-lists in §1. They are
  identifiers and can never be `?`-bound; emitting only allow-list literals is
  the same technique `pkg/storage/records.go` already uses.
- Every user-supplied value — filter values, time bounds, the limit, and the
  pass-3 group keys — is a bound `?` placeholder.
- `SELECT` lists and aggregate bodies are hard-coded literals.

For `--group-by tables`, passes 2 and 3 run over
`FROM slow_logs_with_source, UNNEST(tables) AS u(t)` with `t` as the group
column; the `<where>` clause is unchanged, so a `table=` filter composes with
`--group-by tables`.

## 6. Output — JSON

`--json` writes **only** JSON to stdout. Every diagnostic, warning, and error
goes to stderr, so `gofast-cli digest --json | jq .` is always safe. This is a
hard requirement and gets its own test.

```json
{
  "group_by": "fingerprint",
  "order_by": "Query_time:sum",
  "limit": 20,
  "total": {
    "calls": 10432,
    "exec_time_sec": 903.12,
    "lock_time_sec": 12.4,
    "rows_sent": 8123456,
    "rows_examined": 91234567
  },
  "classes": [
    {
      "rank": 1,
      "id": "FDEA8D2993C9CAF3",
      "item": "SELECT orders",
      "calls": 1240,
      "response_time_pct": 41.2,
      "r_per_call": 0.3,
      "v_m": 0.42,
      "sample_sql": "SELECT * FROM orders WHERE customer_id = 8831",
      "attributes": {
        "exec_time": {
          "pct": 41.2, "total": 372.05, "min": 0.01, "max": 4.12,
          "avg": 0.3, "p95": 1.1, "stddev": 0.22, "median": 0.25
        },
        "lock_time":     { "pct": 3.1,  "total": 2.48,  "min": 0.0, "max": 0.9,   "avg": 0.002,  "p95": 0.008, "stddev": 0.011, "median": 0.001 },
        "rows_sent":     { "pct": 20.4, "total": 6260000, "min": 1, "max": 51000,  "avg": 5048.4, "p95": 22000, "stddev": 9100,  "median": 3100 },
        "rows_examined": { "pct": 33.2, "total": 18800000, "min": 12, "max": 512400, "avg": 15161.3, "p95": 91000, "stddev": 33000, "median": 8800 }
      }
    }
  ]
}
```

Numbers are raw — no unit suffixes and no human formatting. Formatting is a
markdown concern.

**Numeric precision.** `total.calls` is an exact integer (`COUNT(*)`). Every
other numeric field is a float64, because all SQL aggregates are cast to
`DOUBLE` — a uniform rule that exists because `SUM` over a `UBIGINT` column
(`rows_sent`, `rows_examined`) otherwise returns a `*big.Int` to Go. The
consequence is that row-count sums lose exact integer precision above 2^53
(9,007,199,254,740,992). This is accepted rather than special-cased: reaching it
requires summing nine quadrillion examined rows within a single class, and the
alternative reintroduces `*big.Int` handling plus a second cast rule. Recorded
under known limits.

## 7. Output — markdown (default)

````markdown
## Profile

| Rank | Query ID | Response time | Calls | R/Call | V/M | Item |
|---|---|---|---|---|---|---|
| 1 | FDEA8D2993C9CAF3 | 372.05s 41.2% | 1240 | 0.30 | 0.42 | SELECT orders |
| 2 | 9B2C1A77E0D4F881 | 88.10s 9.8% | 310 | 0.28 | 0.11 | UPDATE inventory |

## Query 1: FDEA8D2993C9CAF3

| attribute | pct | total | min | max | avg | 95% | stddev | median |
|---|---|---|---|---|---|---|---|---|
| Count | 12 | 1240 | | | | | | |
| Exec time | 41 | 372.05s | 0.01s | 4.12s | 0.30s | 1.10s | 0.22s | 0.25s |
| Lock time | 3 | 2.48s | 0us | 0.9s | 2ms | 8ms | 11ms | 1ms |
| Rows sent | 20 | 6.26M | 1 | 51k | 5.0k | 22k | 9.1k | 3.1k |
| Rows examined | 33 | 18.8M | 12 | 512k | 15.2k | 91k | 33k | 8.8k |

```sql
SELECT * FROM orders WHERE customer_id = 8831
```
````

Formatting rules: times use `us`/`ms`/`s` by magnitude; counts use `k`/`M`
thresholds; `pct` is a whole number. Sample SQL is emitted **untruncated** inside
a fenced block.

## 8. Empty and error results

- **No rows match:** markdown prints `No queries matched.`; JSON prints the
  envelope with zeroed totals and `"classes": []`. Exit code `0` in both cases —
  an empty result is a valid answer.
- **Store missing or unreadable:** message on stderr, exit `1`, and **no partial
  stdout** — a truncated JSON document must never be emitted.

## 9. Validation errors

All are reported before any query runs:

- unknown `--group-by` value (lists valid keys)
- malformed `--order-by` (missing `:`, unknown attribute, unknown aggregate —
  each names the valid set)
- `--limit` less than 1
- malformed `--filter` (no operator, unknown key, unsupported key/op pair)
- unparseable `--filter` numeric value
- unparseable `--since`/`--until`, including the rejected MySQL-expression form
- `--since` later than `--until`

## 10. File layout

- `internal/digest/options.go` — `Options`, allow-lists, validation.
- `internal/digest/filter.go` — `--filter`/`--since`/`--until` parsing →
  `storage.RecordFilter`.
- `internal/digest/sql.go` — the four SQL builders.
- `internal/digest/run.go` — executes the passes, assembles the typed `Report`.
- `internal/digest/report.go` — `Report`, `Class`, `Stats`, `Totals` types with
  JSON tags.
- `cmd/cli/digest.go` — cobra command, flags → `digest.Options`.
- `cmd/cli/digest_render.go` — markdown and JSON renderers.
- `cmd/cli/main.go` — extract the existing root-command construction into
  `newRootCmd()` so tests can execute the real command tree, and register
  `digestCmd()`. `main()` becomes `newRootCmd().Execute()` plus its existing
  error handling; no command's behavior changes.

## 11. Testing (TDD)

Tests are written before the code they cover.

**`internal/digest`**, against a seeded temporary DuckDB store:

1. **Aggregates** — a fixture with hand-computed expected values verifies
   `total/min/max/avg/95%/stddev/median` per attribute, plus V/M.
2. **Single-row group** — NULL `stddev_samp`/`var_samp` coerce to `0`.
3. **Percentages** — `pct` is relative to the filtered population, not the whole
   table: the same class shows different `pct` with and without a filter.
4. **Ordering** — each aggregate (`sum`/`min`/`max`/`avg`/`cnt`) orders as
   expected; `cnt` matches call frequency regardless of attribute.
5. **`--group-by tables`** — a row touching N tables lands in N groups; a
   NULL/empty `tables` row appears in none; a `table=` filter composes.
6. **Filter parsing** — spaced and unspaced forms; repeat and comma both produce
   `IN` for `user`/`host`/`db`/`source_host`; numeric bounds; unknown key,
   unsupported key/op, and `rows_sent` (explicitly unsupported) each error.
   A multi-value `table` filter errors in both the comma and repeated forms.
   An empty or comma-only value errors rather than running unfiltered.
7. **Time bounds** — relative `N[shmd]` and absolute forms; MySQL-expression form
   rejected; `--since` after `--until` rejected.
8. **Limit** — pass 2 narrows before pass 3; fewer groups than `--limit` is fine.
9. **NULL group keys survive both passes** — rows with a NULL `db` form a group
   that is still present after pass 3 (the regression guard for the
   `NULL IN (NULL)` trap), and render as `(none)`.
10. **Empty result** — zeroed totals, no classes, no error.
11. **Injection** — `--filter "db=x'; DROP TABLE slow_logs;--"` binds as a value,
    matches nothing, and leaves `slow_logs` intact.
12. **Representative row is one row** — a group whose most-frequent fingerprint
    differs from its slowest query yields `Item`, `sanitized_sql`, and `tables`
    all drawn from the most-frequent fingerprint, and `sample_sql` from that
    fingerprint's slowest instance. A cross-row mixture fails the test.
13. **Tie determinism** — two groups with identical `rank_val` return in a
    stable order across repeated runs, and `--limit 1` selects the same one
    every time.

**`cmd/cli`**, following the existing `serve_test.go` pattern:

14. **`--json` stdout is pure JSON** — parses with `encoding/json` with no
    preamble; a warning-producing run still yields parseable stdout.
15. **Markdown** — profile and per-class tables render; sample SQL is fenced and
    untruncated.
16. **Exit codes** — empty result exits `0`; missing store exits `1` with empty
    stdout.
17. **End-to-end through the root command** — the digest is reachable as a
    registered subcommand (not merely by calling its helper directly), flags
    parse, and output lands on the command's writer so it can be captured.
18. **`query` is unchanged** — its existing output is byte-for-byte identical.

## 12. API-locked gate

Run before tagging, and in CI **for this feature branch only**:

- `git diff --stat pkg/` is empty for this change.
- `scripts/run-contract-check.sh` passes.
- `pkg/api/openapi.yaml` is unmodified.
- `CGO_ENABLED=1 go build ./...` and `go test ./...` pass.
- `gitleaks` passes.

The gate is **branch-scoped, not permanent repository policy**. Its purpose is
to prove *this* feature stayed out of `pkg/`. As standing policy it would block
every future `pkg/` change, contradicting the versioning rule in CLAUDE.md that
treats such changes as legitimate versioned events (additive = minor,
breaking = major) — turning "change deliberately" into "never change". The CI
job is removed when this work merges; `scripts/check-pkg-locked.sh` remains for
manual use.

## 13. Release and cross-repo follow-ups

Tag **v1.0.0**. The feature is additive; the major version declares the existing
REST API and `config.yaml` contract stable.

Two follow-ups live in `gofast-enterprise` and are **not** built here:

1. Add the `gofast-cli` binary to the enterprise image so operators get `digest`
   alongside `gofastd`.
2. Bump `backend/go.mod` from `github.com/dbsmedya/gofast v0.9.4` to `v1.0.0`.

**Honest note:** because `digest` is `internal/`, the version bump alone gives
enterprise nothing functionally — shipping the binary is the change that
delivers the feature there.

## 14. Out of scope (v1)

- `--limit`'s `95%:20` percentage semantics.
- Multiple `--group-by` values producing multiple reports per run.
- `--filter` operators beyond what `RecordFilter` models (`!=`, `LIKE`, `OR`,
  `>=`/`<=`, `<` on `rows_examined`).
- Multi-value `table` filters, in either AND or OR form (see §2).
- Filtering on `rows_sent` (needs a `pkg/` change).
- Exact integer precision for row-count sums above 2^53 (see §6).
- Ascending order.
- pt-query-digest's `distill` as a `--group-by` value (the `Item` column already
  distills within each class).
- `--output slowlog` / `json-anon`, `--report-format` section selection,
  `--sample`, `--outliers`.
- Promotion of `internal/digest` to `pkg/digest`.

Each is additive and can follow without breaking this design.
