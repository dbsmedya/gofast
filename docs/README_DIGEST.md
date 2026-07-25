# `gofast-cli digest` — pt-query-digest-compatible reporting

`digest` aggregates the slow queries already stored in DuckDB into the report
shape `pt-query-digest` has produced for ~20 years: a ranked profile table
followed by a per-class detail section. Flag names, defaults, and semantics
follow `pt-query-digest` so existing muscle memory transfers; output is markdown
by default and JSON with `--json`.

## Flags

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

```bash
# The ten fingerprints consuming the most total execution time
gofast-cli digest --group-by fingerprint --order-by Query_time:sum --limit 10

# One fingerprint in detail
gofast-cli digest --filter "fingerprint = FDEA8D2993C9CAF3"

# Last week's worst tables by rows examined, as JSON
gofast-cli digest --group-by tables --order-by Rows_examined:sum \
  --since 7d --json | jq '.classes[] | {id, calls}'
```

## Deviations from pt-query-digest (deliberate)

| pt-query-digest | here | why |
|---|---|---|
| `--filter` is Perl code | `KEY OP VALUE` grammar | Perl is not available |
| `--limit` defaults to `95%:20` | plain int, default `20` | simpler; percentage semantics not carried |
| `--output report\|json\|…` | `--json` boolean | matches the rest of the gofast tooling |
| `--group-by` takes a comma list, one report per value | single value | one report per invocation keeps stdout pipeable |
| aggregates `sum,min,max,cnt` | adds `avg` | ordering by mean latency is genuinely useful and additive |

Everything else keeps pt-query-digest's names, defaults, and semantics.

## `--order-by`

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
ordering (by call frequency). This matches pt-query-digest.

Ordering is always **descending** (worst first). pt-query-digest has no ascending
flag and neither do we.

## `--group-by`

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
unknown). Such groups are preserved and render as `(none)` in markdown and `""`
in JSON. A genuine empty-string value and a NULL collapse into the same group;
the distinction is not meaningful for slow-log attributes.

## `--filter`

Format `KEY OP VALUE`. Whitespace around `OP` is optional: `query_time>2` and
`query_time > 2` both parse. Repeatable; all filters are AND-combined.

| key | ops |
|---|---|
| `fingerprint` | `=` |
| `user` | `=` |
| `host` | `=` |
| `db` | `=` |
| `source_host` | `=` |
| `table` (alias `tables`) | `=` — **single value only** |
| `query_time` | `>` `<` |
| `lock_time` | `>` `<` |
| `rows_examined` | `>` |

- **Multi-value equality** applies to `user`, `host`, `db`, and `source_host`
  only: repeat the flag (`--filter "user = root" --filter "user = app"`) or use
  a comma list (`--filter "user = root,app"`). Both accumulate into one
  `IN (…)`, so they mean **either**.
- **`table` accepts exactly one value.** A comma list or a repeated `table`
  filter is an error naming `--group-by tables` as the way to get a per-table
  breakdown. Each table value renders as its own `list_contains` predicate and
  predicates are joined with `AND`, so `table = orders,customers` would mean
  "touches **both**" while the identical comma syntax on `user` means
  "**either**". Rather than ship two silently opposite meanings for one syntax,
  multi-value table filters are rejected outright.
- **Empty values are rejected.** `--filter "user ="` and `--filter "user = ,"`
  both error rather than silently producing an unfiltered run.
- **Numeric keys:** repeating the same key+op overwrites (last wins).
- **`rows_sent` is not a filter key.** It is reported but not filterable.
- **Unknown key** → error listing valid keys.
- **Unsupported key/op pair** (e.g. `rows_examined < 5`) → error naming the
  operators that key does support.

**Shell caveat:** unquoted `--filter query_time > 2` makes the *shell* redirect
stdout into a file named `2`. Quote it: `--filter "query_time > 2"`.

## `--since` / `--until`

Both bounds are inclusive.

- **Relative:** `N[shmd]` — seconds, hours, minutes, days — resolved against the
  current time. A bare integer is seconds. `--since 7d`, `--since 90m`.
- **Absolute:** `YYYY-MM-DD` or `YYYY-MM-DD HH:MM:SS`.

pt-query-digest also accepts MySQL expressions such as
`CURRENT_DATE - INTERVAL 7 DAY`; that requires a MySQL connection to evaluate and
is **rejected with a clear error** naming the two supported forms.

Timestamps are interpreted in the machine's **local** timezone. This is not a
choice but a requirement of matching stored data: the slow-log parser falls back
to `time.Local` when parsing timestamps, so interpreting bounds in UTC would
silently offset every comparison.

## Report columns

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

**V/M** is the variance-to-mean ratio of response time: a high value marks an
erratic query whose latency swings widely, which is often more actionable than a
uniformly slow one.

`Query ID` prints the stored `fingerprint_id` **without** pt-query-digest's `0x`
prefix, because that exact string is what `--filter "fingerprint = …"` accepts.
Copy-pasteability beats cosmetic fidelity here.

`Item` is derived as the leading SQL verb from `sanitized_sql` plus the `tables`
array (e.g. `SELECT orders`), reproducing pt-query-digest's `distill` output for
the common cases. It is **always** the distilled representative query, including
for non-fingerprint groupings — under `--group-by user`, `Query ID` is the
username and `Item` shows what that user predominantly runs.

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

## How the representative row is chosen

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
`tables`.

For `--group-by fingerprint` step 1 is a no-op (the group *is* the fingerprint),
so the rule degenerates to "the slowest instance of this query", which is what a
digest should show anyway.

## JSON output

`--json` writes **only** JSON to stdout. Every diagnostic, warning, and error
goes to stderr, so `gofast-cli digest --json | jq .` is always safe.

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
(9,007,199,254,740,992). Reaching it requires summing nine quadrillion examined
rows within a single class.

## Empty and error results

- **No rows match:** markdown prints `No queries matched.`; JSON prints the
  envelope with zeroed totals and `"classes": []`. Exit code `0` in both cases —
  an empty result is a valid answer.
- **Store missing or unreadable:** message on stderr, exit `1`, and **no partial
  stdout** — a truncated JSON document is never emitted.

## Known limits

- `--limit`'s `95%:20` percentage semantics.
- Multiple `--group-by` values producing multiple reports per run.
- `--filter` operators beyond `=`, `>`, `<` as tabulated above (no `!=`, `LIKE`,
  `OR`, `>=`/`<=`, or `<` on `rows_examined`).
- Multi-value `table` filters, in either AND or OR form.
- Filtering on `rows_sent`.
- Exact integer precision for row-count sums above 2^53.
- Ascending order.
- pt-query-digest's `distill` as a `--group-by` value (the `Item` column already
  distills within each class).
- `--output slowlog` / `json-anon`, `--report-format` section selection,
  `--sample`, `--outliers`.
- **Table names are grouped exactly as the parser stored them.** The parser
  qualifies a table with its schema only when a `USE` statement was in effect,
  so the same physical table can appear as both `order_items` and
  `shop.order_items` under `--group-by tables`. Grouping by `fingerprint` is
  unaffected. Normalising this belongs to the parser, not the digest.

Each is additive and can follow without breaking this design.
