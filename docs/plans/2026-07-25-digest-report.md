# `gofast-cli digest` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `gofast-cli digest` subcommand that reproduces pt-query-digest's report over the DuckDB slow-log store, with markdown output by default and `--json` for piping to `jq`.

**Architecture:** A new `internal/digest` package owns options parsing, filter translation, SQL construction, and a typed `Report`. It executes three passes against the `slow_logs_with_source` view — global totals, a cheap ranking pass, then the expensive statistics pass restricted to the top N group keys. `cmd/cli` owns only flag wiring and the two renderers. Nothing is added to `pkg/`.

**Tech Stack:** Go 1.25.12, DuckDB via `github.com/marcboeker/go-duckdb/v2` (cgo), cobra, stdlib `encoding/json` and `text/tabwriter`-free manual markdown.

**Spec:** `docs/specs/2026-07-25-pt-query-digest-design.md`

## Global Constraints

- **`pkg/` must not change.** `git diff --stat pkg/` must be empty for this entire branch. Every symbol needed is already exported: `storage.NewStorage`, `storage.Storage.Query(ctx, sql, args...)`, `storage.RecordFilter`, `RecordFilter.BuildWhere()`, `models.ReportResult`.
- **Build requires `CGO_ENABLED=1`.** Every build and test command in this plan uses it. Toolchain go 1.25.12.
- **The REST API is locked.** No new endpoints, no `pkg/api/openapi.yaml` change, no `config.yaml` schema change. `scripts/run-contract-check.sh` must stay green.
- **No secrets; `gitleaks` must pass.**
- **Every SQL aggregate is wrapped in `CAST(... AS DOUBLE)`.** Verified by probe: `SUM` over a `UBIGINT` column (`rows_sent`, `rows_examined`) returns a `*big.Int` to Go, and `MIN`/`MAX` return `uint64`. Casting in SQL makes every numeric cell arrive as `float64`.
- **The only text ever interpolated into SQL** is a column or aggregate identifier drawn from a fixed allow-list. Every user-supplied value is a bound `?`.
- **Test seed rows must have distinct `EventOffset` values.** Verified by probe: `FinalizeIngestion` → `DedupeSlowLogs` deletes rows sharing `(file_hash, event_offset)` (`pkg/storage/duckdb.go:214`). Seeding three rows all with the default offset `0` silently leaves **one** row.
- Run `CGO_ENABLED=1 gofmt -l .` before each commit; it must print nothing.

---

### Task 1: Options, allow-lists, and `--order-by` parsing

**Files:**
- Create: `internal/digest/options.go`
- Test: `internal/digest/options_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `GroupKey`, `Attr`, `Agg`, `OrderSpec`, `Options`, `ParseGroupKey(string) (GroupKey, error)`, `ParseOrderBy(string) (OrderSpec, error)`, `DefaultOptions() Options`, and the unexported `GroupKey.column() string`, `GroupKey.isTables() bool`, `Attr.column() string`.

- [ ] **Step 1: Write the failing test**

Create `internal/digest/options_test.go`:

```go
package digest

import "testing"

func TestParseGroupKey(t *testing.T) {
	cases := []struct {
		in   string
		want GroupKey
	}{
		{"fingerprint", GroupFingerprint},
		{"FINGERPRINT", GroupFingerprint},
		{"user", GroupUser},
		{"db", GroupDB},
		{"host", GroupHost},
		{"source_host", GroupSourceHost},
		{"tables", GroupTables},
		{"table", GroupTables}, // alias
	}
	for _, c := range cases {
		got, err := ParseGroupKey(c.in)
		if err != nil {
			t.Fatalf("ParseGroupKey(%q): unexpected error %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("ParseGroupKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseGroupKeyRejectsUnknown(t *testing.T) {
	_, err := ParseGroupKey("nope")
	if err == nil {
		t.Fatal("expected an error for an unknown group key")
	}
	// The message must list the valid keys so the user can recover.
	for _, want := range []string{"fingerprint", "source_host", "tables"} {
		if !contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestParseOrderBy(t *testing.T) {
	cases := []struct {
		in       string
		wantAttr Attr
		wantAgg  Agg
	}{
		{"Query_time:sum", AttrQueryTime, AggSum},
		{"query_time:sum", AttrQueryTime, AggSum},   // case-insensitive attribute
		{"Query_time:SUM", AttrQueryTime, AggSum},   // case-insensitive aggregate
		{"Lock_time:max", AttrLockTime, AggMax},
		{"Rows_sent:min", AttrRowsSent, AggMin},
		{"Rows_examined:avg", AttrRowsExamined, AggAvg},
		{"Query_time:cnt", AttrQueryTime, AggCnt},
	}
	for _, c := range cases {
		got, err := ParseOrderBy(c.in)
		if err != nil {
			t.Fatalf("ParseOrderBy(%q): unexpected error %v", c.in, err)
		}
		if got.Attr != c.wantAttr || got.Agg != c.wantAgg {
			t.Errorf("ParseOrderBy(%q) = %+v, want {%q %q}", c.in, got, c.wantAttr, c.wantAgg)
		}
	}
}

func TestParseOrderByErrors(t *testing.T) {
	cases := []struct {
		in      string
		mustSay string
	}{
		{"Query_time", ":"},              // missing separator
		{"Bogus_attr:sum", "Query_time"}, // unknown attribute lists valid ones
		{"Query_time:median", "sum"},     // unknown aggregate lists valid ones
		{"", ":"},
	}
	for _, c := range cases {
		_, err := ParseOrderBy(c.in)
		if err == nil {
			t.Fatalf("ParseOrderBy(%q): expected an error", c.in)
		}
		if !contains(err.Error(), c.mustSay) {
			t.Errorf("ParseOrderBy(%q) error %q does not mention %q", c.in, err, c.mustSay)
		}
	}
}

func TestDefaultOptionsMatchPtQueryDigest(t *testing.T) {
	o := DefaultOptions()
	if o.GroupBy != GroupFingerprint {
		t.Errorf("default GroupBy = %q, want fingerprint", o.GroupBy)
	}
	if o.OrderBy.Attr != AttrQueryTime || o.OrderBy.Agg != AggSum {
		t.Errorf("default OrderBy = %+v, want Query_time:sum", o.OrderBy)
	}
	if o.Limit != 20 {
		t.Errorf("default Limit = %d, want 20", o.Limit)
	}
}

func TestValidateRejectsBadLimit(t *testing.T) {
	o := DefaultOptions()
	o.Limit = 0
	if err := o.Validate(); err == nil {
		t.Fatal("expected an error for limit 0")
	}
	o.Limit = -3
	if err := o.Validate(); err == nil {
		t.Fatal("expected an error for a negative limit")
	}
}

func TestColumnMapping(t *testing.T) {
	if got := GroupFingerprint.column(); got != "fingerprint_id" {
		t.Errorf("fingerprint column = %q, want fingerprint_id", got)
	}
	if got := GroupSourceHost.column(); got != "source_host" {
		t.Errorf("source_host column = %q", got)
	}
	if !GroupTables.isTables() {
		t.Error("GroupTables.isTables() = false")
	}
	if GroupUser.isTables() {
		t.Error("GroupUser.isTables() = true")
	}
	if got := AttrQueryTime.column(); got != "query_time_sec" {
		t.Errorf("Query_time column = %q, want query_time_sec", got)
	}
	if got := AttrRowsExamined.column(); got != "rows_examined" {
		t.Errorf("Rows_examined column = %q", got)
	}
}

// contains is a tiny helper so the tests read clearly.
func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `CGO_ENABLED=1 go test ./internal/digest/ -run 'TestParse|TestDefault|TestValidate|TestColumn' -v`
Expected: FAIL — the package does not exist yet (`no Go files in .../internal/digest`).

- [ ] **Step 3: Write the implementation**

Create `internal/digest/options.go`:

```go
// Package digest builds pt-query-digest-compatible reports over the DuckDB
// slow-log store. It is deliberately internal: gofastd does not consume it, and
// keeping it unexported avoids freezing a public contract at v1.0.0. See
// docs/specs/2026-07-25-pt-query-digest-design.md.
package digest

import (
	"fmt"
	"strings"

	"github.com/dbsmedya/gofast/pkg/storage"
)

// GroupKey is an allow-listed --group-by value. Because every GroupKey is one
// of the constants below, GroupKey.column() can be interpolated into SQL text
// safely: it is never user input, only a value drawn from a fixed set.
type GroupKey string

const (
	GroupFingerprint GroupKey = "fingerprint"
	GroupUser        GroupKey = "user"
	GroupDB          GroupKey = "db"
	GroupHost        GroupKey = "host"
	GroupSourceHost  GroupKey = "source_host"
	GroupTables      GroupKey = "tables"
)

// groupKeys maps accepted spellings to canonical keys. "table" is accepted as
// an alias for "tables" because that is the --filter key name; pt-query-digest
// itself spells it "tables".
var groupKeys = map[string]GroupKey{
	"fingerprint": GroupFingerprint,
	"user":        GroupUser,
	"db":          GroupDB,
	"host":        GroupHost,
	"source_host": GroupSourceHost,
	"tables":      GroupTables,
	"table":       GroupTables,
}

// groupKeyList is the human-facing ordering used in error messages.
var groupKeyList = []string{"fingerprint", "user", "db", "host", "source_host", "tables"}

// ParseGroupKey resolves a --group-by value case-insensitively.
func ParseGroupKey(s string) (GroupKey, error) {
	if g, ok := groupKeys[strings.ToLower(strings.TrimSpace(s))]; ok {
		return g, nil
	}
	return "", fmt.Errorf("unknown --group-by %q; valid keys: %s",
		s, strings.Join(groupKeyList, ", "))
}

// column returns the SQL column this key groups on. For GroupTables the group
// column is the UNNEST alias instead; callers must check isTables() first.
func (g GroupKey) column() string {
	switch g {
	case GroupFingerprint:
		return "fingerprint_id"
	case GroupUser:
		return "user"
	case GroupDB:
		return "db"
	case GroupHost:
		return "host"
	case GroupSourceHost:
		return "source_host"
	case GroupTables:
		return unnestAlias
	}
	return ""
}

// unnestAlias is the column name bound by `UNNEST(tables) AS u(t)`.
const unnestAlias = "t"

// isTables reports whether this grouping requires UNNEST fan-out over the
// tables array.
func (g GroupKey) isTables() bool { return g == GroupTables }

// Attr is an allow-listed slow-log attribute, spelled as pt-query-digest
// spells it.
type Attr string

const (
	AttrQueryTime    Attr = "Query_time"
	AttrLockTime     Attr = "Lock_time"
	AttrRowsSent     Attr = "Rows_sent"
	AttrRowsExamined Attr = "Rows_examined"
)

var attrs = map[string]Attr{
	"query_time":    AttrQueryTime,
	"lock_time":     AttrLockTime,
	"rows_sent":     AttrRowsSent,
	"rows_examined": AttrRowsExamined,
}

var attrList = []string{"Query_time", "Lock_time", "Rows_sent", "Rows_examined"}

// column returns the DuckDB column backing this attribute.
func (a Attr) column() string {
	switch a {
	case AttrQueryTime:
		return "query_time_sec"
	case AttrLockTime:
		return "lock_time_sec"
	case AttrRowsSent:
		return "rows_sent"
	case AttrRowsExamined:
		return "rows_examined"
	}
	return ""
}

// Agg is an allow-listed --order-by aggregate. pt-query-digest supports
// sum/min/max/cnt; "avg" is a deliberate addition (see the spec's deviation
// table) because ordering by mean latency is useful and purely additive.
type Agg string

const (
	AggSum Agg = "sum"
	AggMin Agg = "min"
	AggMax Agg = "max"
	AggAvg Agg = "avg"
	AggCnt Agg = "cnt"
)

var aggs = map[string]Agg{
	"sum": AggSum,
	"min": AggMin,
	"max": AggMax,
	"avg": AggAvg,
	"cnt": AggCnt,
}

var aggList = []string{"sum", "min", "max", "avg", "cnt"}

// OrderSpec is a parsed ATTRIBUTE:AGGREGATE pair.
type OrderSpec struct {
	Attr Attr
	Agg  Agg
}

// String renders the spec back in its canonical input form, for the report
// envelope and error messages.
func (o OrderSpec) String() string { return string(o.Attr) + ":" + string(o.Agg) }

// ParseOrderBy parses "Query_time:sum". Both halves are case-insensitive.
func ParseOrderBy(s string) (OrderSpec, error) {
	raw := strings.TrimSpace(s)
	attrPart, aggPart, found := strings.Cut(raw, ":")
	if !found {
		return OrderSpec{}, fmt.Errorf(
			"invalid --order-by %q: expected ATTRIBUTE:AGGREGATE, for example Query_time:sum", s)
	}
	a, ok := attrs[strings.ToLower(strings.TrimSpace(attrPart))]
	if !ok {
		return OrderSpec{}, fmt.Errorf("unknown --order-by attribute %q; valid attributes: %s",
			attrPart, strings.Join(attrList, ", "))
	}
	g, ok := aggs[strings.ToLower(strings.TrimSpace(aggPart))]
	if !ok {
		return OrderSpec{}, fmt.Errorf("unknown --order-by aggregate %q; valid aggregates: %s",
			aggPart, strings.Join(aggList, ", "))
	}
	return OrderSpec{Attr: a, Agg: g}, nil
}

// Options is a fully-validated digest request.
type Options struct {
	GroupBy GroupKey
	OrderBy OrderSpec
	Limit   int
	Filter  storage.RecordFilter
}

// DefaultOptions returns pt-query-digest's defaults, except for Limit: this
// tool takes a plain integer (default 20) rather than pt-query-digest's
// "95%:20" percentage-or-count form.
func DefaultOptions() Options {
	return Options{
		GroupBy: GroupFingerprint,
		OrderBy: OrderSpec{Attr: AttrQueryTime, Agg: AggSum},
		Limit:   20,
	}
}

// Validate checks the parts that parsing alone cannot.
func (o Options) Validate() error {
	if o.Limit < 1 {
		return fmt.Errorf("--limit must be at least 1, got %d", o.Limit)
	}
	if _, ok := groupKeys[string(o.GroupBy)]; !ok {
		return fmt.Errorf("invalid group key %q", o.GroupBy)
	}
	if o.OrderBy.Attr.column() == "" {
		return fmt.Errorf("invalid order-by attribute %q", o.OrderBy.Attr)
	}
	if _, ok := aggs[string(o.OrderBy.Agg)]; !ok {
		return fmt.Errorf("invalid order-by aggregate %q", o.OrderBy.Agg)
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `CGO_ENABLED=1 go test ./internal/digest/ -v`
Expected: PASS — all seven tests green.

- [ ] **Step 5: Commit**

```bash
CGO_ENABLED=1 gofmt -l . && git add internal/digest/options.go internal/digest/options_test.go
git commit -m "feat(digest): options, allow-lists, and --order-by parsing"
```

---

### Task 2: `--filter`, `--since`, and `--until` parsing

**Files:**
- Create: `internal/digest/filter.go`
- Test: `internal/digest/filter_test.go`

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces: `ParseFilters([]string) (storage.RecordFilter, error)`, `ParseTimeBound(string, time.Time) (time.Time, error)`, `ApplyTimeBounds(*storage.RecordFilter, string, string, time.Time) error`.

- [ ] **Step 1: Write the failing test**

Create `internal/digest/filter_test.go`:

```go
package digest

import (
	"testing"
	"time"
)

func TestParseFiltersStringEquality(t *testing.T) {
	f, err := ParseFilters([]string{"fingerprint = FDEA8D2993C9CAF3"})
	if err != nil {
		t.Fatalf("ParseFilters: %v", err)
	}
	if f.FingerprintID != "FDEA8D2993C9CAF3" {
		t.Errorf("FingerprintID = %q", f.FingerprintID)
	}
}

func TestParseFiltersWhitespaceOptional(t *testing.T) {
	spaced, err := ParseFilters([]string{"query_time > 2"})
	if err != nil {
		t.Fatalf("spaced: %v", err)
	}
	tight, err := ParseFilters([]string{"query_time>2"})
	if err != nil {
		t.Fatalf("tight: %v", err)
	}
	if spaced.QueryTimeGT == nil || tight.QueryTimeGT == nil {
		t.Fatal("QueryTimeGT not set")
	}
	if *spaced.QueryTimeGT != 2 || *tight.QueryTimeGT != 2 {
		t.Errorf("got %v and %v, want 2 and 2", *spaced.QueryTimeGT, *tight.QueryTimeGT)
	}
}

func TestParseFiltersRepeatAndCommaBothAccumulate(t *testing.T) {
	repeated, err := ParseFilters([]string{"user = root", "user = app"})
	if err != nil {
		t.Fatalf("repeated: %v", err)
	}
	if len(repeated.Users) != 2 || repeated.Users[0] != "root" || repeated.Users[1] != "app" {
		t.Errorf("repeated Users = %v, want [root app]", repeated.Users)
	}

	comma, err := ParseFilters([]string{"user = root,app"})
	if err != nil {
		t.Fatalf("comma: %v", err)
	}
	if len(comma.Users) != 2 || comma.Users[0] != "root" || comma.Users[1] != "app" {
		t.Errorf("comma Users = %v, want [root app]", comma.Users)
	}
}

func TestParseFiltersNumericLastWins(t *testing.T) {
	f, err := ParseFilters([]string{"query_time > 2", "query_time > 5"})
	if err != nil {
		t.Fatalf("ParseFilters: %v", err)
	}
	if f.QueryTimeGT == nil || *f.QueryTimeGT != 5 {
		t.Errorf("QueryTimeGT = %v, want 5 (last wins)", f.QueryTimeGT)
	}
}

func TestParseFiltersAllKeys(t *testing.T) {
	f, err := ParseFilters([]string{
		"user = u", "host = h", "db = d", "source_host = s",
		"table = orders", "fingerprint = F",
		"query_time > 1", "query_time < 9",
		"lock_time > 0.1", "lock_time < 0.9",
		"rows_examined > 100",
	})
	if err != nil {
		t.Fatalf("ParseFilters: %v", err)
	}
	if len(f.Users) != 1 || len(f.Hosts) != 1 || len(f.DBs) != 1 ||
		len(f.SourceHosts) != 1 || len(f.Tables) != 1 {
		t.Errorf("string IN-lists not all populated: %+v", f)
	}
	if f.QueryTimeGT == nil || f.QueryTimeLT == nil ||
		f.LockTimeGT == nil || f.LockTimeLT == nil || f.RowsExaminedGT == nil {
		t.Errorf("numeric bounds not all populated: %+v", f)
	}
}

func TestParseFiltersTablesAlias(t *testing.T) {
	f, err := ParseFilters([]string{"tables = orders"})
	if err != nil {
		t.Fatalf("ParseFilters: %v", err)
	}
	if len(f.Tables) != 1 || f.Tables[0] != "orders" {
		t.Errorf("Tables = %v, want [orders]", f.Tables)
	}
}

func TestParseFiltersErrors(t *testing.T) {
	cases := []struct {
		in      string
		mustSay string
	}{
		{"no_operator_here", "operator"},
		{"bogus = 1", "unknown"},
		{"rows_examined < 5", ">"},        // unsupported op names the supported one
		{"query_time > abc", "number"},    // unparseable numeric
		{"rows_sent > 5", "rows_sent"},    // reportable but not filterable
	}
	for _, c := range cases {
		_, err := ParseFilters([]string{c.in})
		if err == nil {
			t.Fatalf("ParseFilters(%q): expected an error", c.in)
		}
		if !contains(err.Error(), c.mustSay) {
			t.Errorf("ParseFilters(%q) error %q does not mention %q", c.in, err, c.mustSay)
		}
	}
}

func TestParseTimeBoundRelative(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.Local)
	cases := []struct {
		in   string
		want time.Time
	}{
		{"7d", now.Add(-7 * 24 * time.Hour)},
		{"90m", now.Add(-90 * time.Minute)},
		{"2h", now.Add(-2 * time.Hour)},
		{"30s", now.Add(-30 * time.Second)},
		{"30", now.Add(-30 * time.Second)}, // bare integer is seconds
	}
	for _, c := range cases {
		got, err := ParseTimeBound(c.in, now)
		if err != nil {
			t.Fatalf("ParseTimeBound(%q): %v", c.in, err)
		}
		if !got.Equal(c.want) {
			t.Errorf("ParseTimeBound(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseTimeBoundAbsoluteUsesLocalZone(t *testing.T) {
	now := time.Now()
	got, err := ParseTimeBound("2026-07-20", now)
	if err != nil {
		t.Fatalf("date-only: %v", err)
	}
	want := time.Date(2026, 7, 20, 0, 0, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("date-only = %v, want %v", got, want)
	}

	got, err = ParseTimeBound("2026-07-20 13:45:01", now)
	if err != nil {
		t.Fatalf("datetime: %v", err)
	}
	want = time.Date(2026, 7, 20, 13, 45, 1, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("datetime = %v, want %v", got, want)
	}
}

func TestParseTimeBoundRejectsMySQLExpression(t *testing.T) {
	_, err := ParseTimeBound("CURRENT_DATE - INTERVAL 7 DAY", time.Now())
	if err == nil {
		t.Fatal("expected an error for a MySQL expression")
	}
	// The message must name the two supported forms.
	if !contains(err.Error(), "YYYY-MM-DD") {
		t.Errorf("error %q does not name the absolute form", err)
	}
}

func TestApplyTimeBounds(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.Local)
	f := storage.RecordFilter{}
	if err := ApplyTimeBounds(&f, "7d", "2026-07-24", now); err != nil {
		t.Fatalf("ApplyTimeBounds: %v", err)
	}
	if f.TSFrom == nil || f.TSTo == nil {
		t.Fatal("TSFrom/TSTo not set")
	}
	if !f.TSFrom.Equal(now.Add(-7 * 24 * time.Hour)) {
		t.Errorf("TSFrom = %v", *f.TSFrom)
	}
}

func TestApplyTimeBoundsRejectsInvertedRange(t *testing.T) {
	now := time.Now()
	f := storage.RecordFilter{}
	err := ApplyTimeBounds(&f, "2026-07-24", "2026-07-20", now)
	if err == nil {
		t.Fatal("expected an error when --since is after --until")
	}
}
```

The test file's import block is:

```go
import (
	"testing"
	"time"

	"github.com/dbsmedya/gofast/pkg/storage"
)
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `CGO_ENABLED=1 go test ./internal/digest/ -run 'TestParseFilters|TestParseTimeBound|TestApplyTimeBounds' -v`
Expected: FAIL — `undefined: ParseFilters`.

- [ ] **Step 3: Write the implementation**

Create `internal/digest/filter.go`:

```go
package digest

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/dbsmedya/gofast/pkg/storage"
)

// filterKeyList is the human-facing list used in error messages.
var filterKeyList = []string{
	"fingerprint", "user", "host", "db", "source_host", "table",
	"query_time", "lock_time", "rows_examined",
}

// ParseFilters turns repeated "KEY OP VALUE" strings into a RecordFilter.
// Filters are AND-combined. String keys accumulate into IN-lists (repeat the
// flag or use a comma list); numeric keys overwrite, because RecordFilter holds
// a single bound per comparison.
//
// Every value ends up as a bound `?` via RecordFilter.BuildWhere(), so no user
// input reaches SQL text.
func ParseFilters(exprs []string) (storage.RecordFilter, error) {
	var f storage.RecordFilter
	for _, raw := range exprs {
		key, op, val, err := splitFilter(raw)
		if err != nil {
			return f, err
		}
		if err := applyFilter(&f, key, op, val, raw); err != nil {
			return f, err
		}
	}
	return f, nil
}

// splitFilter separates "KEY OP VALUE". The two-character operators are not
// supported, so a single scan for the first operator character is enough.
func splitFilter(raw string) (key, op, val string, err error) {
	s := strings.TrimSpace(raw)
	idx := strings.IndexAny(s, "=<>")
	if idx < 0 {
		return "", "", "", fmt.Errorf(
			"invalid --filter %q: missing an operator (expected KEY = VALUE, KEY > VALUE, or KEY < VALUE)", raw)
	}
	key = strings.ToLower(strings.TrimSpace(s[:idx]))
	op = string(s[idx])
	val = strings.TrimSpace(s[idx+1:])
	if key == "" {
		return "", "", "", fmt.Errorf("invalid --filter %q: empty key", raw)
	}
	if val == "" {
		return "", "", "", fmt.Errorf("invalid --filter %q: empty value", raw)
	}
	return key, op, val, nil
}

// splitList expands a comma list into trimmed, non-empty values.
func splitList(val string) []string {
	parts := strings.Split(val, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func applyFilter(f *storage.RecordFilter, key, op, val, raw string) error {
	// String keys: equality only, accumulating into IN-lists.
	stringTargets := map[string]*[]string{
		"user":        &f.Users,
		"host":        &f.Hosts,
		"db":          &f.DBs,
		"source_host": &f.SourceHosts,
		"table":       &f.Tables,
		"tables":      &f.Tables, // alias, matching --group-by
	}
	if target, ok := stringTargets[key]; ok {
		if op != "=" {
			return fmt.Errorf("--filter %q: %s supports only '='", raw, key)
		}
		*target = append(*target, splitList(val)...)
		return nil
	}

	if key == "fingerprint" {
		if op != "=" {
			return fmt.Errorf("--filter %q: fingerprint supports only '='", raw)
		}
		f.FingerprintID = val
		return nil
	}

	num := func() (float64, error) {
		v, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return 0, fmt.Errorf("--filter %q: %q is not a number", raw, val)
		}
		return v, nil
	}

	switch key {
	case "query_time":
		v, err := num()
		if err != nil {
			return err
		}
		if op == ">" {
			f.QueryTimeGT = &v
		} else if op == "<" {
			f.QueryTimeLT = &v
		} else {
			return fmt.Errorf("--filter %q: query_time supports only '>' and '<'", raw)
		}
	case "lock_time":
		v, err := num()
		if err != nil {
			return err
		}
		if op == ">" {
			f.LockTimeGT = &v
		} else if op == "<" {
			f.LockTimeLT = &v
		} else {
			return fmt.Errorf("--filter %q: lock_time supports only '>' and '<'", raw)
		}
	case "rows_examined":
		if op != ">" {
			return fmt.Errorf("--filter %q: rows_examined supports only '>'", raw)
		}
		v, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return fmt.Errorf("--filter %q: %q is not a whole number", raw, val)
		}
		f.RowsExaminedGT = &v
	case "rows_sent":
		// Reported but not filterable: RecordFilter has no RowsSent field and
		// adding one would change pkg/, which this feature does not do.
		return fmt.Errorf("--filter %q: rows_sent is reported but cannot be filtered; valid keys: %s",
			raw, strings.Join(filterKeyList, ", "))
	default:
		return fmt.Errorf("--filter %q: unknown key %q; valid keys: %s",
			raw, key, strings.Join(filterKeyList, ", "))
	}
	return nil
}

// relativeUnits maps pt-query-digest's suffixes to durations.
var relativeUnits = map[byte]time.Duration{
	's': time.Second,
	'm': time.Minute,
	'h': time.Hour,
	'd': 24 * time.Hour,
}

// ParseTimeBound accepts pt-query-digest's relative form (N[shmd], bare integer
// meaning seconds) resolved against now, or an absolute "YYYY-MM-DD" /
// "YYYY-MM-DD HH:MM:SS".
//
// Absolute values are interpreted in the LOCAL zone, because the slow-log
// parser stores timestamps that way: pkg/parser/engine.go passes an empty
// log.Options{}, so percona's parser falls back to time.Local. Using UTC here
// would silently offset every comparison.
func ParseTimeBound(s string, now time.Time) (time.Time, error) {
	v := strings.TrimSpace(s)
	if v == "" {
		return time.Time{}, fmt.Errorf("empty time value")
	}

	// Relative: all digits, optionally followed by one unit suffix.
	unit := time.Second
	digits := v
	if last := v[len(v)-1]; last < '0' || last > '9' {
		u, ok := relativeUnits[last]
		if ok {
			unit = u
			digits = v[:len(v)-1]
		} else {
			digits = "" // force the absolute path below
		}
	}
	if digits != "" {
		if n, err := strconv.ParseInt(digits, 10, 64); err == nil {
			return now.Add(-time.Duration(n) * unit), nil
		}
	}

	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, v, time.Local); err == nil {
			return t, nil
		}
	}

	return time.Time{}, fmt.Errorf(
		"invalid time value %q: expected a relative value like 7d, 90m, 2h, 30s, "+
			"or an absolute \"YYYY-MM-DD\" / \"YYYY-MM-DD HH:MM:SS\" "+
			"(MySQL expressions such as CURRENT_DATE - INTERVAL 7 DAY are not supported)", s)
}

// ApplyTimeBounds parses since/until (either may be empty) onto f as the
// inclusive TSFrom/TSTo bounds.
func ApplyTimeBounds(f *storage.RecordFilter, since, until string, now time.Time) error {
	if strings.TrimSpace(since) != "" {
		t, err := ParseTimeBound(since, now)
		if err != nil {
			return fmt.Errorf("--since: %w", err)
		}
		f.TSFrom = &t
	}
	if strings.TrimSpace(until) != "" {
		t, err := ParseTimeBound(until, now)
		if err != nil {
			return fmt.Errorf("--until: %w", err)
		}
		f.TSTo = &t
	}
	if f.TSFrom != nil && f.TSTo != nil && f.TSFrom.After(*f.TSTo) {
		return fmt.Errorf("--since (%s) is after --until (%s)",
			f.TSFrom.Format(time.RFC3339), f.TSTo.Format(time.RFC3339))
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `CGO_ENABLED=1 go test ./internal/digest/ -v`
Expected: PASS — Task 1 and Task 2 tests all green.

- [ ] **Step 5: Commit**

```bash
CGO_ENABLED=1 gofmt -l . && git add internal/digest/filter.go internal/digest/filter_test.go
git commit -m "feat(digest): --filter grammar and --since/--until parsing"
```

---

### Task 3: Report types and numeric coercion

**Files:**
- Create: `internal/digest/report.go`
- Create: `internal/digest/scan.go`
- Test: `internal/digest/scan_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `Report`, `Class`, `Stats`, `Totals`, the attribute-key constants `AttrKeyExecTime`/`AttrKeyLockTime`/`AttrKeyRowsSent`/`AttrKeyRowsExamined`, and the unexported `toFloat(any) float64`, `toInt64(any) int64`, `toStr(any) string`, `toStrs(any) []string`, `pct(part, whole float64) float64`.

- [ ] **Step 1: Write the failing test**

Create `internal/digest/scan_test.go`:

```go
package digest

import (
	"encoding/json"
	"math/big"
	"testing"
)

func TestToFloatHandlesEveryDriverType(t *testing.T) {
	// Verified against DuckDB: SUM over a UBIGINT column returns *big.Int,
	// MIN/MAX return uint64, COUNT(*) returns int64, and stddev_samp over a
	// single-row group returns nil. toFloat must survive all of them even
	// though the SQL casts to DOUBLE defensively.
	cases := []struct {
		in   any
		want float64
	}{
		{float64(2.5), 2.5},
		{int64(7), 7},
		{uint64(9), 9},
		{int32(4), 4},
		{int(3), 3},
		{big.NewInt(40), 40},
		{nil, 0},
		{"not a number", 0},
	}
	for _, c := range cases {
		if got := toFloat(c.in); got != c.want {
			t.Errorf("toFloat(%#v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestToInt64(t *testing.T) {
	if got := toInt64(int64(5)); got != 5 {
		t.Errorf("toInt64(int64) = %d", got)
	}
	if got := toInt64(float64(6)); got != 6 {
		t.Errorf("toInt64(float64) = %d", got)
	}
	if got := toInt64(nil); got != 0 {
		t.Errorf("toInt64(nil) = %d", got)
	}
}

func TestToStrAndToStrs(t *testing.T) {
	if got := toStr("x"); got != "x" {
		t.Errorf("toStr = %q", got)
	}
	if got := toStr(nil); got != "" {
		t.Errorf("toStr(nil) = %q, want empty", got)
	}
	got := toStrs([]string{"a", "b"})
	if len(got) != 2 || got[0] != "a" {
		t.Errorf("toStrs = %v", got)
	}
	if got := toStrs(nil); len(got) != 0 {
		t.Errorf("toStrs(nil) = %v, want empty", got)
	}
}

func TestPctGuardsZeroDenominator(t *testing.T) {
	if got := pct(5, 0); got != 0 {
		t.Errorf("pct(5,0) = %v, want 0", got)
	}
	if got := pct(1, 4); got != 25 {
		t.Errorf("pct(1,4) = %v, want 25", got)
	}
}

func TestReportJSONShape(t *testing.T) {
	r := &Report{
		GroupBy: "fingerprint",
		OrderBy: "Query_time:sum",
		Limit:   20,
		Total:   Totals{Calls: 3, ExecTimeSec: 9},
		Classes: []Class{{
			Rank: 1, ID: "AAAA", Item: "SELECT orders", Calls: 2,
			Attributes: map[string]Stats{AttrKeyExecTime: {Total: 4}},
		}},
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Spot-check the contract-relevant key names from the spec.
	for _, want := range []string{
		`"group_by"`, `"order_by"`, `"total"`, `"classes"`,
		`"exec_time_sec"`, `"rank"`, `"r_per_call"`, `"v_m"`,
		`"sample_sql"`, `"attributes"`, `"exec_time"`, `"p95"`,
	} {
		if !contains(string(b), want) {
			t.Errorf("JSON is missing %s\ngot: %s", want, b)
		}
	}
}

func TestReportEmptyClassesMarshalsAsArrayNotNull(t *testing.T) {
	r := &Report{GroupBy: "fingerprint", Classes: []Class{}}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !contains(string(b), `"classes":[]`) {
		t.Errorf("empty classes must marshal as [], got: %s", b)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `CGO_ENABLED=1 go test ./internal/digest/ -run 'TestTo|TestPct|TestReport' -v`
Expected: FAIL — `undefined: toFloat`, `undefined: Report`.

- [ ] **Step 3: Write the implementation**

Create `internal/digest/report.go`:

```go
package digest

// Attribute keys used in Class.Attributes and in the JSON output.
const (
	AttrKeyExecTime     = "exec_time"
	AttrKeyLockTime     = "lock_time"
	AttrKeyRowsSent     = "rows_sent"
	AttrKeyRowsExamined = "rows_examined"
)

// Stats is one attribute's statistics within a class, matching
// pt-query-digest's per-query detail columns.
type Stats struct {
	Pct    float64 `json:"pct"`
	Total  float64 `json:"total"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
	Avg    float64 `json:"avg"`
	P95    float64 `json:"p95"`
	Stddev float64 `json:"stddev"`
	Median float64 `json:"median"`
}

// Totals are the population-wide sums over the FILTERED set. Every Stats.Pct is
// computed against these, so filters change percentages by design.
type Totals struct {
	Calls        int64   `json:"calls"`
	ExecTimeSec  float64 `json:"exec_time_sec"`
	LockTimeSec  float64 `json:"lock_time_sec"`
	RowsSent     float64 `json:"rows_sent"`
	RowsExamined float64 `json:"rows_examined"`
}

// Class is one grouped query class — one row of the profile table plus its
// detail section.
type Class struct {
	Rank            int              `json:"rank"`
	ID              string           `json:"id"`
	Item            string           `json:"item"`
	Calls           int64            `json:"calls"`
	ResponseTimePct float64          `json:"response_time_pct"`
	RPerCall        float64          `json:"r_per_call"`
	VM              float64          `json:"v_m"`
	SampleSQL       string           `json:"sample_sql"`
	Attributes      map[string]Stats `json:"attributes"`
}

// Report is a complete digest run.
type Report struct {
	GroupBy string  `json:"group_by"`
	OrderBy string  `json:"order_by"`
	Limit   int     `json:"limit"`
	Total   Totals  `json:"total"`
	Classes []Class `json:"classes"`
}
```

Create `internal/digest/scan.go`:

```go
package digest

import "math/big"

// toFloat coerces a driver value to float64.
//
// The SQL wraps every aggregate in CAST(... AS DOUBLE) so values normally
// arrive as float64, but this stays defensive: DuckDB returns *big.Int for
// SUM over UBIGINT columns, uint64 for MIN/MAX over them, int64 for COUNT(*),
// and nil for stddev_samp/var_samp over a single-row group. A nil (SQL NULL)
// becomes 0, which is what the spec requires for the single-row case.
func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int64:
		return float64(n)
	case int32:
		return float64(n)
	case int:
		return float64(n)
	case uint64:
		return float64(n)
	case uint32:
		return float64(n)
	case *big.Int:
		if n == nil {
			return 0
		}
		f, _ := new(big.Float).SetInt(n).Float64()
		return f
	default:
		return 0
	}
}

// toInt64 coerces a driver value to int64, for counts.
func toInt64(v any) int64 { return int64(toFloat(v)) }

// toStr coerces a driver value to string; SQL NULL becomes "".
func toStr(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []byte:
		return string(s)
	default:
		return ""
	}
}

// toStrs coerces a driver value to a string slice. Storage.Query already
// converts DuckDB list columns to []string.
func toStrs(v any) []string {
	switch a := v.(type) {
	case []string:
		return a
	case []any:
		out := make([]string, 0, len(a))
		for _, item := range a {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// pct returns part/whole as a percentage, guarding a zero denominator (an
// all-zero attribute such as rows_sent over write-only traffic).
func pct(part, whole float64) float64 {
	if whole == 0 {
		return 0
	}
	return part / whole * 100
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `CGO_ENABLED=1 go test ./internal/digest/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
CGO_ENABLED=1 gofmt -l . && git add internal/digest/report.go internal/digest/scan.go internal/digest/scan_test.go
git commit -m "feat(digest): report types and driver value coercion"
```

---

### Task 4: Query distillation (the `Item` column)

**Files:**
- Create: `internal/digest/distill.go`
- Test: `internal/digest/distill_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `Distill(sanitizedSQL string, tables []string) string`.

- [ ] **Step 1: Write the failing test**

Create `internal/digest/distill_test.go`:

```go
package digest

import "testing"

func TestDistill(t *testing.T) {
	cases := []struct {
		name   string
		sql    string
		tables []string
		want   string
	}{
		{"select with one table", "SELECT * FROM orders WHERE id = ?", []string{"orders"}, "SELECT orders"},
		{"verb is uppercased", "select * from orders", []string{"orders"}, "SELECT orders"},
		{"multiple tables joined by space", "SELECT * FROM a JOIN b", []string{"a", "b"}, "SELECT a b"},
		{"update", "UPDATE inventory SET n = ?", []string{"inventory"}, "UPDATE inventory"},
		{"no tables yields the bare verb", "SHOW STATUS", nil, "SHOW"},
		{"leading whitespace and newlines", "\n\t SELECT 1", nil, "SELECT"},
		{"empty sql yields empty string", "", nil, ""},
		{"whitespace-only sql yields empty string", "   ", nil, ""},
		{"empty sql but tables present still yields verb-less tables", "", []string{"orders"}, "orders"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Distill(c.sql, c.tables); got != c.want {
				t.Errorf("Distill(%q, %v) = %q, want %q", c.sql, c.tables, got, c.want)
			}
		})
	}
}

func TestDistillStripsQuotedTableNames(t *testing.T) {
	// The parser may store table names in quoted form; the report should show
	// them plainly.
	if got := Distill("SELECT 1", []string{`"orders"`}); got != "SELECT orders" {
		t.Errorf("Distill with quoted table = %q, want \"SELECT orders\"", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `CGO_ENABLED=1 go test ./internal/digest/ -run TestDistill -v`
Expected: FAIL — `undefined: Distill`.

- [ ] **Step 3: Write the implementation**

Create `internal/digest/distill.go`:

```go
package digest

import "strings"

// Distill renders pt-query-digest's "Item" column: the leading SQL verb
// followed by the tables the query touches, e.g. "SELECT orders".
//
// It is always the distilled representative query, including for non-fingerprint
// groupings — under --group-by user the Query ID column already shows the
// username, so repeating it here would waste the column.
func Distill(sanitizedSQL string, tables []string) string {
	parts := make([]string, 0, len(tables)+1)

	if verb := leadingVerb(sanitizedSQL); verb != "" {
		parts = append(parts, verb)
	}
	for _, t := range tables {
		if clean := strings.Trim(strings.TrimSpace(t), `"`+"`"); clean != "" {
			parts = append(parts, clean)
		}
	}
	return strings.Join(parts, " ")
}

// leadingVerb returns the first whitespace-delimited token, uppercased.
func leadingVerb(sql string) string {
	for _, field := range strings.Fields(sql) {
		return strings.ToUpper(field)
	}
	return ""
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `CGO_ENABLED=1 go test ./internal/digest/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
CGO_ENABLED=1 gofmt -l . && git add internal/digest/distill.go internal/digest/distill_test.go
git commit -m "feat(digest): distill queries into the Item column"
```

---

### Task 5: SQL builders for the three passes

**Files:**
- Create: `internal/digest/sql.go`
- Test: `internal/digest/sql_test.go`

**Interfaces:**
- Consumes: `Options`, `GroupKey.column()`, `GroupKey.isTables()`, `Attr.column()`, `unnestAlias` (Task 1).
- Produces: `buildGlobalsSQL(storage.RecordFilter) (string, []any)`, `buildRankSQL(Options) (string, []any)`, `buildStatsSQL(Options, []string) (string, []any)`, the ranking-layout constants `colRankGroup`, `colRankCalls`, `colRankValue`, and the stats-layout constants `statsPerAttr`, `colStatsGroup`, `colStatsCalls`, `colStatsQueryTime`, `colStatsVarSamp`, `colStatsLockTime`, `colStatsRowsSent`, `colStatsRowsExamined`, `colStatsSampleSQL`, `colStatsSanitizedSQL`, `colStatsTables`, `statsWidth`.

- [ ] **Step 1: Write the failing test**

Create `internal/digest/sql_test.go`:

```go
package digest

import (
	"strings"
	"testing"

	"github.com/dbsmedya/gofast/pkg/storage"
)

func TestBuildGlobalsSQLCastsEveryAggregate(t *testing.T) {
	q, args := buildGlobalsSQL(storage.RecordFilter{})
	if len(args) != 0 {
		t.Errorf("args = %v, want none for an empty filter", args)
	}
	// Every aggregate must be cast: SUM over UBIGINT otherwise returns *big.Int.
	if strings.Count(q, "CAST(") != 5 {
		t.Errorf("expected 5 CASTs (count + 4 sums), got %d in:\n%s", strings.Count(q, "CAST("), q)
	}
	for _, want := range []string{"COUNT(*)", "SUM(query_time_sec)", "SUM(lock_time_sec)",
		"SUM(rows_sent)", "SUM(rows_examined)", "FROM slow_logs_with_source"} {
		if !strings.Contains(q, want) {
			t.Errorf("globals SQL missing %q:\n%s", want, q)
		}
	}
}

func TestBuildGlobalsSQLBindsFilterValues(t *testing.T) {
	f, err := ParseFilters([]string{"db = shop", "query_time > 2"})
	if err != nil {
		t.Fatalf("ParseFilters: %v", err)
	}
	q, args := buildGlobalsSQL(f)
	if !strings.Contains(q, "WHERE") {
		t.Errorf("expected a WHERE clause:\n%s", q)
	}
	if len(args) != 2 {
		t.Errorf("args = %v, want 2 bound values", args)
	}
	// The literal value must never appear in the SQL text.
	if strings.Contains(q, "shop") {
		t.Errorf("filter value was interpolated into SQL:\n%s", q)
	}
}

func TestBuildRankSQLCoalescesGroupColumn(t *testing.T) {
	o := DefaultOptions()
	o.GroupBy = GroupDB
	q, args := buildRankSQL(o)
	// COALESCE is mandatory: a NULL key cannot survive pass 3's IN (?) lookup,
	// because NULL IN (NULL) is NULL rather than true.
	if !strings.Contains(q, "COALESCE(db, '')") {
		t.Errorf("group column is not coalesced:\n%s", q)
	}
	if !strings.Contains(q, "ORDER BY rank_val DESC") {
		t.Errorf("expected descending order:\n%s", q)
	}
	if !strings.Contains(q, "LIMIT ?") {
		t.Errorf("limit must be bound, not interpolated:\n%s", q)
	}
	if len(args) != 1 || args[0] != 20 {
		t.Errorf("args = %v, want [20]", args)
	}
}

func TestBuildRankSQLAggregates(t *testing.T) {
	cases := []struct {
		agg  Agg
		want string
	}{
		{AggSum, "SUM(query_time_sec)"},
		{AggMin, "MIN(query_time_sec)"},
		{AggMax, "MAX(query_time_sec)"},
		{AggAvg, "AVG(query_time_sec)"},
		{AggCnt, "COUNT(*)"}, // cnt ignores the attribute
	}
	for _, c := range cases {
		o := DefaultOptions()
		o.OrderBy = OrderSpec{Attr: AttrQueryTime, Agg: c.agg}
		q, _ := buildRankSQL(o)
		if !strings.Contains(q, c.want) {
			t.Errorf("agg %q: SQL missing %q:\n%s", c.agg, c.want, q)
		}
	}
}

func TestBuildRankSQLTablesUsesUnnest(t *testing.T) {
	o := DefaultOptions()
	o.GroupBy = GroupTables
	q, _ := buildRankSQL(o)
	if !strings.Contains(q, "UNNEST(tables) AS u(t)") {
		t.Errorf("expected UNNEST fan-out:\n%s", q)
	}
	if !strings.Contains(q, "COALESCE(t, '')") {
		t.Errorf("expected the unnest alias to be grouped:\n%s", q)
	}
}

func TestBuildStatsSQLBindsGroupKeys(t *testing.T) {
	o := DefaultOptions()
	q, args := buildStatsSQL(o, []string{"AAAA", "BBBB"})
	if !strings.Contains(q, "IN (?,?)") {
		t.Errorf("group keys must be bound placeholders:\n%s", q)
	}
	if len(args) != 2 || args[0] != "AAAA" {
		t.Errorf("args = %v, want [AAAA BBBB]", args)
	}
	if strings.Contains(q, "AAAA") {
		t.Errorf("group key was interpolated into SQL:\n%s", q)
	}
}

func TestBuildStatsSQLAppendsToExistingWhere(t *testing.T) {
	f, err := ParseFilters([]string{"db = shop"})
	if err != nil {
		t.Fatalf("ParseFilters: %v", err)
	}
	o := DefaultOptions()
	o.Filter = f
	q, args := buildStatsSQL(o, []string{"AAAA"})
	if strings.Count(q, "WHERE") != 1 {
		t.Errorf("expected exactly one WHERE keyword:\n%s", q)
	}
	if !strings.Contains(q, " AND COALESCE(fingerprint_id, '') IN (?)") {
		t.Errorf("expected the key predicate to be ANDed on:\n%s", q)
	}
	// Filter arg first, then the group key.
	if len(args) != 2 || args[0] != "shop" || args[1] != "AAAA" {
		t.Errorf("args = %v, want [shop AAAA]", args)
	}
}

func TestBuildStatsSQLHasEveryStatistic(t *testing.T) {
	o := DefaultOptions()
	q, _ := buildStatsSQL(o, []string{"AAAA"})
	for _, col := range []string{"query_time_sec", "lock_time_sec", "rows_sent", "rows_examined"} {
		for _, fn := range []string{"SUM(", "MIN(", "MAX(", "AVG(", "stddev_samp("} {
			if !strings.Contains(q, fn+col) {
				t.Errorf("stats SQL missing %s%s:\n%s", fn, col, q)
			}
		}
		if !strings.Contains(q, "quantile_cont("+col+", 0.95)") {
			t.Errorf("stats SQL missing p95 for %s", col)
		}
		if !strings.Contains(q, "quantile_cont("+col+", 0.5)") {
			t.Errorf("stats SQL missing median for %s", col)
		}
	}
	// var_samp is query-time only: V/M is defined over response time.
	if strings.Count(q, "var_samp(") != 1 {
		t.Errorf("expected exactly one var_samp, got %d", strings.Count(q, "var_samp("))
	}
	if !strings.Contains(q, "var_samp(query_time_sec)") {
		t.Errorf("var_samp must be over query_time_sec:\n%s", q)
	}
}

func TestStatsColumnIndicesAreConsistent(t *testing.T) {
	// The scan in run.go indexes by position; these constants must not drift.
	if colStatsGroup != 0 || colStatsCalls != 1 || colStatsQueryTime != 2 {
		t.Errorf("leading indices changed: %d %d %d", colStatsGroup, colStatsCalls, colStatsQueryTime)
	}
	if colStatsVarSamp != colStatsQueryTime+statsPerAttr {
		t.Errorf("var_samp index %d does not follow the query_time block", colStatsVarSamp)
	}
	if colStatsLockTime != colStatsVarSamp+1 {
		t.Errorf("lock_time index %d is wrong", colStatsLockTime)
	}
	if colStatsRowsSent != colStatsLockTime+statsPerAttr {
		t.Errorf("rows_sent index %d is wrong", colStatsRowsSent)
	}
	if colStatsRowsExamined != colStatsRowsSent+statsPerAttr {
		t.Errorf("rows_examined index %d is wrong", colStatsRowsExamined)
	}
	if colStatsSampleSQL != colStatsRowsExamined+statsPerAttr {
		t.Errorf("sample_sql index %d is wrong", colStatsSampleSQL)
	}
	if statsWidth != colStatsTables+1 {
		t.Errorf("statsWidth %d does not match the last index %d", statsWidth, colStatsTables)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `CGO_ENABLED=1 go test ./internal/digest/ -run 'TestBuild|TestStatsColumn' -v`
Expected: FAIL — `undefined: buildGlobalsSQL`.

- [ ] **Step 3: Write the implementation**

Create `internal/digest/sql.go`:

```go
package digest

import (
	"fmt"
	"strings"

	"github.com/dbsmedya/gofast/pkg/storage"
)

// view is the enrichment view every pass reads; it adds source_host to
// slow_logs via the files join.
const view = "slow_logs_with_source"

// unnestFrom is the FROM clause used when grouping by tables. A query touching
// N tables contributes one row to each of its N groups.
const unnestFrom = view + ", UNNEST(tables) AS u(t)"

// Stats column layout for the pass-3 result. run.go indexes by position, so
// these constants and the SELECT list in buildStatsSQL must change together.
// Pass-2 (ranking) result layout.
const (
	colRankGroup = 0
	colRankCalls = 1
	colRankValue = 2
)

const (
	statsPerAttr = 7 // sum, min, max, avg, p95, stddev, median

	colStatsGroup        = 0
	colStatsCalls        = 1
	colStatsQueryTime    = 2                                    // .. +6
	colStatsVarSamp      = colStatsQueryTime + statsPerAttr     // 9
	colStatsLockTime     = colStatsVarSamp + 1                  // 10 .. +6
	colStatsRowsSent     = colStatsLockTime + statsPerAttr      // 17 .. +6
	colStatsRowsExamined = colStatsRowsSent + statsPerAttr      // 24 .. +6
	colStatsSampleSQL    = colStatsRowsExamined + statsPerAttr  // 31
	colStatsSanitizedSQL = colStatsSampleSQL + 1                // 32
	colStatsTables       = colStatsSanitizedSQL + 1             // 33
	statsWidth           = colStatsTables + 1                   // 34
)

// groupExpr renders the grouping expression for o.
//
// COALESCE is not cosmetic: user, host, db and source_host are all nullable
// (source_host comes from a LEFT JOIN), and a NULL key cannot survive pass 3's
// `IN (?)` lookup because NULL IN (NULL) evaluates to NULL, not true. Verified
// against DuckDB: the uncoalesced form silently drops the group. The empty
// string it produces renders as "(none)".
func groupExpr(o Options) string {
	return fmt.Sprintf("COALESCE(%s, '')", o.GroupBy.column())
}

// fromClause picks the plain view or the UNNEST fan-out.
func fromClause(o Options) string {
	if o.GroupBy.isTables() {
		return unnestFrom
	}
	return view
}

// aggExpr renders the ordering aggregate. "cnt" ignores the attribute and
// counts rows, matching pt-query-digest.
//
// Both halves come from the allow-lists in options.go, so this interpolation
// only ever emits a fixed literal.
func aggExpr(o OrderSpec) string {
	if o.Agg == AggCnt {
		return "COUNT(*)"
	}
	return fmt.Sprintf("%s(%s)", strings.ToUpper(string(o.Agg)), o.Attr.column())
}

// buildGlobalsSQL is pass 1: population-wide totals feeding every Stats.Pct.
//
// It deliberately reads the plain view even when grouping by tables, so the
// denominators stay the real totals rather than fan-out-inflated ones.
func buildGlobalsSQL(f storage.RecordFilter) (string, []any) {
	where, args := f.BuildWhere()
	q := `
		SELECT CAST(COUNT(*) AS DOUBLE),
		       CAST(SUM(query_time_sec) AS DOUBLE),
		       CAST(SUM(lock_time_sec) AS DOUBLE),
		       CAST(SUM(rows_sent) AS DOUBLE),
		       CAST(SUM(rows_examined) AS DOUBLE)
		FROM ` + view + `
		` + where
	return q, args
}

// buildRankSQL is pass 2: the cheap ranking pass. It computes only the ordering
// aggregate — no percentiles — so narrowing to the top N stays fast on large
// stores.
func buildRankSQL(o Options) (string, []any) {
	where, args := o.Filter.BuildWhere()
	grp := groupExpr(o)
	q := fmt.Sprintf(`
		SELECT %s AS grp,
		       CAST(COUNT(*) AS DOUBLE) AS calls,
		       CAST(%s AS DOUBLE) AS rank_val
		FROM %s
		%s
		GROUP BY grp
		ORDER BY rank_val DESC
		LIMIT ?`, grp, aggExpr(o.OrderBy), fromClause(o), where)
	args = append(args, o.Limit)
	return q, args
}

// statFragment renders the seven statistics for one attribute column. Every one
// is cast to DOUBLE so Go always receives a float64 (SUM over a UBIGINT column
// otherwise arrives as *big.Int).
func statFragment(col string) string {
	return fmt.Sprintf(`CAST(SUM(%[1]s) AS DOUBLE),
		       CAST(MIN(%[1]s) AS DOUBLE),
		       CAST(MAX(%[1]s) AS DOUBLE),
		       CAST(AVG(%[1]s) AS DOUBLE),
		       CAST(quantile_cont(%[1]s, 0.95) AS DOUBLE),
		       CAST(stddev_samp(%[1]s) AS DOUBLE),
		       CAST(quantile_cont(%[1]s, 0.5) AS DOUBLE)`, col)
}

// buildStatsSQL is pass 3: the full statistics matrix, restricted to the group
// keys pass 2 returned. keys are bound as placeholders.
func buildStatsSQL(o Options, keys []string) (string, []any) {
	where, args := o.Filter.BuildWhere()
	grp := groupExpr(o)

	ph := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	cond := fmt.Sprintf("%s IN (%s)", grp, ph)
	if where == "" {
		where = "WHERE " + cond
	} else {
		where += " AND " + cond
	}
	for _, k := range keys {
		args = append(args, k)
	}

	q := fmt.Sprintf(`
		SELECT %s AS grp,
		       CAST(COUNT(*) AS DOUBLE),
		       %s,
		       CAST(var_samp(query_time_sec) AS DOUBLE),
		       %s,
		       %s,
		       %s,
		       ANY_VALUE(sample_sql),
		       ANY_VALUE(sanitized_sql),
		       ANY_VALUE(tables)
		FROM %s
		%s
		GROUP BY grp`,
		grp,
		statFragment("query_time_sec"),
		statFragment("lock_time_sec"),
		statFragment("rows_sent"),
		statFragment("rows_examined"),
		fromClause(o),
		where)
	return q, args
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `CGO_ENABLED=1 go test ./internal/digest/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
CGO_ENABLED=1 gofmt -l . && git add internal/digest/sql.go internal/digest/sql_test.go
git commit -m "feat(digest): SQL builders for the globals, ranking, and stats passes"
```

---

### Task 6: `Run` — execute the three passes

**Files:**
- Create: `internal/digest/run.go`
- Create: `internal/digest/testdata_test.go`
- Test: `internal/digest/run_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1–5.
- Produces: `Querier` interface, `Run(ctx context.Context, q Querier, o Options) (*Report, error)`.

- [ ] **Step 1: Write the seeding helper and the failing test**

Create `internal/digest/testdata_test.go`:

```go
package digest

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/dbsmedya/gofast/pkg/models"
	"github.com/dbsmedya/gofast/pkg/storage"
)

// seedStore writes entries into a fresh temp DuckDB and returns a read-only
// handle.
//
// CRITICAL: every entry must carry a DISTINCT EventOffset. FinalizeIngestion
// runs DedupeSlowLogs, which deletes rows sharing (file_hash, event_offset)
// (pkg/storage/duckdb.go:214). Leaving the default 0 on several entries
// silently collapses them to one row and makes every expected value wrong.
func seedStore(t *testing.T, entries []*models.SlowLogEntry) *storage.Storage {
	t.Helper()

	seen := map[int64]bool{}
	for i, e := range entries {
		if seen[e.EventOffset] {
			t.Fatalf("entry %d reuses EventOffset %d; offsets must be distinct or dedupe drops rows",
				i, e.EventOffset)
		}
		seen[e.EventOffset] = true
	}

	path := filepath.Join(t.TempDir(), "digest.duckdb")
	rw, err := storage.NewStorage(path, false)
	if err != nil {
		t.Fatalf("open rw: %v", err)
	}
	if err := rw.DropSlowLogIndexes(); err != nil {
		t.Fatalf("drop indexes: %v", err)
	}
	if err := rw.InsertSlowLogBatch(context.Background(), entries, "testfile"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := rw.FinalizeIngestion(context.Background()); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if err := rw.Close(); err != nil {
		t.Fatalf("close rw: %v", err)
	}

	ro, err := storage.NewStorage(path, true)
	if err != nil {
		t.Fatalf("open ro: %v", err)
	}
	t.Cleanup(func() { _ = ro.Close() })
	return ro
}

// fixtureEntries is the canonical three-row fixture. Its expected aggregates are
// hand-computed in run_test.go:
//
//	AAAA: query_time 1.0 and 3.0  -> sum 4.0, min 1.0, max 3.0, avg 2.0,
//	                                 median 2.0, p95 2.9, stddev sqrt(2),
//	                                 var 2.0, V/M = 2.0/2.0 = 1.0
//	BBBB: query_time 5.0          -> sum 5.0, stddev/var NULL -> 0, V/M 0
//	totals: calls 3, exec 9.0, lock 0.9, rows_sent 40, rows_examined 900
func fixtureEntries(now time.Time) []*models.SlowLogEntry {
	return []*models.SlowLogEntry{
		{
			FingerprintID: "AAAA", SanitizedSQL: "SELECT * FROM orders WHERE id = ?",
			SampleSQL: "SELECT * FROM orders WHERE id = 1",
			User:      "app", Host: "h1", DB: "shop",
			QueryTimeSec: 1.0, LockTimeSec: 0.1, RowsSent: 10, RowsExamined: 100,
			TS: now, Tables: []string{"orders"}, EventOffset: 1, EventHash: "e1",
		},
		{
			FingerprintID: "AAAA", SanitizedSQL: "SELECT * FROM orders WHERE id = ?",
			SampleSQL: "SELECT * FROM orders WHERE id = 2",
			User:      "app", Host: "h1", DB: "shop",
			QueryTimeSec: 3.0, LockTimeSec: 0.3, RowsSent: 30, RowsExamined: 300,
			TS: now, Tables: []string{"orders", "customers"}, EventOffset: 2, EventHash: "e2",
		},
		{
			FingerprintID: "BBBB", SanitizedSQL: "UPDATE inventory SET n = ?",
			SampleSQL: "UPDATE inventory SET n = 5",
			User:      "batch", Host: "h2", DB: "",
			QueryTimeSec: 5.0, LockTimeSec: 0.5, RowsSent: 0, RowsExamined: 500,
			TS: now, Tables: []string{"inventory"}, EventOffset: 3, EventHash: "e3",
		},
	}
}

// approx compares floats with a tolerance, since quantile_cont interpolates
// (p95 over {1.0, 3.0} is 2.8999999999999995, not exactly 2.9).
func approx(t *testing.T, label string, got, want float64) {
	t.Helper()
	const tol = 1e-9
	if diff := got - want; diff > tol || diff < -tol {
		t.Errorf("%s = %v, want %v", label, got, want)
	}
}
```

Create `internal/digest/run_test.go`:

```go
package digest

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/dbsmedya/gofast/pkg/models"
)

func TestRunGroupsByFingerprintWithExactStatistics(t *testing.T) {
	store := seedStore(t, fixtureEntries(time.Now()))
	o := DefaultOptions()

	rep, err := Run(context.Background(), store, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Totals over the whole (unfiltered) population.
	if rep.Total.Calls != 3 {
		t.Errorf("total calls = %d, want 3", rep.Total.Calls)
	}
	approx(t, "total exec", rep.Total.ExecTimeSec, 9.0)
	approx(t, "total lock", rep.Total.LockTimeSec, 0.9)
	approx(t, "total rows_sent", rep.Total.RowsSent, 40)
	approx(t, "total rows_examined", rep.Total.RowsExamined, 900)

	if len(rep.Classes) != 2 {
		t.Fatalf("classes = %d, want 2", len(rep.Classes))
	}

	// Query_time:sum descending -> BBBB (5.0) outranks AAAA (4.0).
	if rep.Classes[0].ID != "BBBB" || rep.Classes[1].ID != "AAAA" {
		t.Fatalf("ranking = [%s %s], want [BBBB AAAA]", rep.Classes[0].ID, rep.Classes[1].ID)
	}
	if rep.Classes[0].Rank != 1 || rep.Classes[1].Rank != 2 {
		t.Errorf("ranks = %d,%d want 1,2", rep.Classes[0].Rank, rep.Classes[1].Rank)
	}

	aaaa := rep.Classes[1]
	if aaaa.Calls != 2 {
		t.Errorf("AAAA calls = %d, want 2", aaaa.Calls)
	}
	exec := aaaa.Attributes[AttrKeyExecTime]
	approx(t, "AAAA exec total", exec.Total, 4.0)
	approx(t, "AAAA exec min", exec.Min, 1.0)
	approx(t, "AAAA exec max", exec.Max, 3.0)
	approx(t, "AAAA exec avg", exec.Avg, 2.0)
	approx(t, "AAAA exec median", exec.Median, 2.0)
	approx(t, "AAAA exec p95", exec.P95, 2.9)
	approx(t, "AAAA exec stddev", exec.Stddev, math.Sqrt(2))
	approx(t, "AAAA exec pct", exec.Pct, 4.0/9.0*100)
	approx(t, "AAAA R/Call", aaaa.RPerCall, 2.0)
	// V/M = var_samp / avg = 2.0 / 2.0.
	approx(t, "AAAA V/M", aaaa.VM, 1.0)
	approx(t, "AAAA response time pct", aaaa.ResponseTimePct, 4.0/9.0*100)

	if aaaa.Item != "SELECT orders" && aaaa.Item != "SELECT orders customers" {
		t.Errorf("AAAA item = %q, want a distilled SELECT over orders", aaaa.Item)
	}
	if aaaa.SampleSQL == "" {
		t.Error("AAAA sample_sql is empty")
	}
}

func TestRunSingleRowGroupZeroesStddevAndVM(t *testing.T) {
	store := seedStore(t, fixtureEntries(time.Now()))
	rep, err := Run(context.Background(), store, DefaultOptions())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	bbbb := rep.Classes[0]
	if bbbb.ID != "BBBB" {
		t.Fatalf("expected BBBB first, got %s", bbbb.ID)
	}
	// stddev_samp and var_samp are NULL over a single row; both must become 0.
	approx(t, "BBBB stddev", bbbb.Attributes[AttrKeyExecTime].Stddev, 0)
	approx(t, "BBBB V/M", bbbb.VM, 0)
}

func TestRunNullGroupKeySurvivesBothPasses(t *testing.T) {
	// source_host is NULL here: no matching files row exists for "testfile".
	// Without COALESCE the group is lost between pass 2 and pass 3.
	store := seedStore(t, fixtureEntries(time.Now()))
	o := DefaultOptions()
	o.GroupBy = GroupSourceHost

	rep, err := Run(context.Background(), store, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Classes) != 1 {
		t.Fatalf("classes = %d, want 1 (the NULL source_host group)", len(rep.Classes))
	}
	if rep.Classes[0].Calls != 3 {
		t.Errorf("NULL group calls = %d, want 3", rep.Classes[0].Calls)
	}
	if rep.Classes[0].ID != "" {
		t.Errorf("NULL group id = %q, want an empty string", rep.Classes[0].ID)
	}
}

func TestRunGroupByTablesFansOut(t *testing.T) {
	store := seedStore(t, fixtureEntries(time.Now()))
	o := DefaultOptions()
	o.GroupBy = GroupTables

	rep, err := Run(context.Background(), store, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	byName := map[string]int64{}
	for _, c := range rep.Classes {
		byName[c.ID] = c.Calls
	}
	// orders appears in two rows, customers and inventory in one each; the
	// group calls therefore sum to 4 across only 3 stored rows.
	if byName["orders"] != 2 || byName["customers"] != 1 || byName["inventory"] != 1 {
		t.Errorf("table fan-out = %v, want orders:2 customers:1 inventory:1", byName)
	}
	if rep.Total.Calls != 3 {
		t.Errorf("totals must stay un-inflated: calls = %d, want 3", rep.Total.Calls)
	}
}

func TestRunGroupByTablesSkipsRowsWithoutTables(t *testing.T) {
	// UNNEST over an empty list yields no rows, so a query with no recorded
	// tables belongs to no table group at all.
	now := time.Now()
	entries := fixtureEntries(now)
	entries = append(entries, &models.SlowLogEntry{
		FingerprintID: "CCCC", SanitizedSQL: "SHOW STATUS", SampleSQL: "SHOW STATUS",
		User: "admin", Host: "h3", DB: "shop",
		QueryTimeSec: 9, LockTimeSec: 0, RowsSent: 1, RowsExamined: 1,
		TS: now, Tables: nil, EventOffset: 4, EventHash: "e4",
	})
	store := seedStore(t, entries)

	o := DefaultOptions()
	o.GroupBy = GroupTables
	rep, err := Run(context.Background(), store, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, c := range rep.Classes {
		if c.ID == "" {
			t.Errorf("a table-less row produced a group: %+v", c)
		}
	}
	// Totals still count all four rows, including the table-less one.
	if rep.Total.Calls != 4 {
		t.Errorf("total calls = %d, want 4", rep.Total.Calls)
	}
	// CCCC contributes 9.0s to the totals but to no table group.
	approx(t, "total exec", rep.Total.ExecTimeSec, 18.0)
}

func TestRunTableFilterComposesWithGroupByTables(t *testing.T) {
	store := seedStore(t, fixtureEntries(time.Now()))
	f, err := ParseFilters([]string{"table = orders"})
	if err != nil {
		t.Fatalf("ParseFilters: %v", err)
	}
	o := DefaultOptions()
	o.GroupBy = GroupTables
	o.Filter = f

	rep, err := Run(context.Background(), store, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The filter keeps only rows touching orders (the two AAAA rows); the
	// fan-out then still emits a customers group, because the second AAAA row
	// touches both tables.
	byName := map[string]int64{}
	for _, c := range rep.Classes {
		byName[c.ID] = c.Calls
	}
	if byName["orders"] != 2 {
		t.Errorf("orders calls = %d, want 2", byName["orders"])
	}
	if _, present := byName["inventory"]; present {
		t.Errorf("inventory must be filtered out entirely: %v", byName)
	}
}

func TestRunFiltersChangePercentages(t *testing.T) {
	store := seedStore(t, fixtureEntries(time.Now()))

	unfiltered, err := Run(context.Background(), store, DefaultOptions())
	if err != nil {
		t.Fatalf("unfiltered: %v", err)
	}

	f, err := ParseFilters([]string{"db = shop"})
	if err != nil {
		t.Fatalf("ParseFilters: %v", err)
	}
	o := DefaultOptions()
	o.Filter = f
	filtered, err := Run(context.Background(), store, o)
	if err != nil {
		t.Fatalf("filtered: %v", err)
	}

	if len(filtered.Classes) != 1 || filtered.Classes[0].ID != "AAAA" {
		t.Fatalf("filtered classes = %+v, want only AAAA", filtered.Classes)
	}
	// Unfiltered AAAA is 4/9; filtered it is the whole population, so 100%.
	approx(t, "filtered AAAA pct", filtered.Classes[0].Attributes[AttrKeyExecTime].Pct, 100)
	if unfiltered.Classes[1].Attributes[AttrKeyExecTime].Pct >= 100 {
		t.Error("unfiltered pct should be below 100")
	}
}

func TestRunOrderByAggregates(t *testing.T) {
	store := seedStore(t, fixtureEntries(time.Now()))

	// Rows_examined:sum -> BBBB 500 vs AAAA 400.
	o := DefaultOptions()
	o.OrderBy = OrderSpec{Attr: AttrRowsExamined, Agg: AggSum}
	rep, err := Run(context.Background(), store, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Classes[0].ID != "BBBB" {
		t.Errorf("Rows_examined:sum first = %s, want BBBB", rep.Classes[0].ID)
	}

	// Query_time:cnt -> AAAA has 2 calls, BBBB has 1.
	o.OrderBy = OrderSpec{Attr: AttrQueryTime, Agg: AggCnt}
	rep, err = Run(context.Background(), store, o)
	if err != nil {
		t.Fatalf("Run cnt: %v", err)
	}
	if rep.Classes[0].ID != "AAAA" {
		t.Errorf("Query_time:cnt first = %s, want AAAA", rep.Classes[0].ID)
	}
}

func TestRunLimitNarrows(t *testing.T) {
	store := seedStore(t, fixtureEntries(time.Now()))
	o := DefaultOptions()
	o.Limit = 1
	rep, err := Run(context.Background(), store, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Classes) != 1 {
		t.Errorf("classes = %d, want 1", len(rep.Classes))
	}
	// Totals still describe the whole population, not just the shown class.
	if rep.Total.Calls != 3 {
		t.Errorf("total calls = %d, want 3", rep.Total.Calls)
	}
}

func TestRunEmptyResultIsNotAnError(t *testing.T) {
	store := seedStore(t, fixtureEntries(time.Now()))
	f, err := ParseFilters([]string{"db = nonexistent"})
	if err != nil {
		t.Fatalf("ParseFilters: %v", err)
	}
	o := DefaultOptions()
	o.Filter = f

	rep, err := Run(context.Background(), store, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Classes) != 0 {
		t.Errorf("classes = %d, want 0", len(rep.Classes))
	}
	if rep.Total.Calls != 0 {
		t.Errorf("total calls = %d, want 0", rep.Total.Calls)
	}
	if rep.Classes == nil {
		t.Error("Classes must be an empty slice, not nil, so JSON renders []")
	}
}

func TestRunFilterValuesAreBoundNotInterpolated(t *testing.T) {
	store := seedStore(t, fixtureEntries(time.Now()))
	f, err := ParseFilters([]string{`db = x'; DROP TABLE slow_logs;--`})
	if err != nil {
		t.Fatalf("ParseFilters: %v", err)
	}
	o := DefaultOptions()
	o.Filter = f

	rep, err := Run(context.Background(), store, o)
	if err != nil {
		t.Fatalf("Run must not fail on a hostile value: %v", err)
	}
	if len(rep.Classes) != 0 {
		t.Errorf("hostile value matched %d classes, want 0", len(rep.Classes))
	}

	// The table must still be there.
	after, err := Run(context.Background(), store, DefaultOptions())
	if err != nil {
		t.Fatalf("Run after injection attempt: %v", err)
	}
	if after.Total.Calls != 3 {
		t.Fatalf("slow_logs was modified: calls = %d, want 3", after.Total.Calls)
	}
}

func TestRunValidatesOptions(t *testing.T) {
	store := seedStore(t, fixtureEntries(time.Now()))
	o := DefaultOptions()
	o.Limit = 0
	if _, err := Run(context.Background(), store, o); err == nil {
		t.Fatal("expected Run to reject an invalid limit")
	}
}

func TestRunEnvelopeEchoesRequest(t *testing.T) {
	store := seedStore(t, fixtureEntries(time.Now()))
	o := DefaultOptions()
	o.GroupBy = GroupUser
	o.OrderBy = OrderSpec{Attr: AttrLockTime, Agg: AggMax}
	o.Limit = 7

	rep, err := Run(context.Background(), store, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.GroupBy != "user" {
		t.Errorf("GroupBy = %q", rep.GroupBy)
	}
	if rep.OrderBy != "Lock_time:max" {
		t.Errorf("OrderBy = %q", rep.OrderBy)
	}
	if rep.Limit != 7 {
		t.Errorf("Limit = %d", rep.Limit)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `CGO_ENABLED=1 go test ./internal/digest/ -run TestRun -v`
Expected: FAIL — `undefined: Run`.

- [ ] **Step 3: Write the implementation**

Create `internal/digest/run.go`:

```go
package digest

import (
	"context"
	"fmt"

	"github.com/dbsmedya/gofast/pkg/models"
)

// Querier is the slice of storage.Storage this package needs. Taking an
// interface keeps Run testable and documents that the digest only ever reads.
type Querier interface {
	Query(ctx context.Context, query string, args ...interface{}) (*models.ReportResult, error)
}

// Run executes the digest in three passes: population totals, a cheap ranking
// pass that narrows to o.Limit groups, then the full statistics matrix for just
// those groups. Splitting the work this way keeps percentile and standard
// deviation computation off every group in a large store.
func Run(ctx context.Context, q Querier, o Options) (*Report, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}

	rep := &Report{
		GroupBy: string(o.GroupBy),
		OrderBy: o.OrderBy.String(),
		Limit:   o.Limit,
		Classes: []Class{}, // never nil: JSON must render [] rather than null
	}

	// Pass 1 — population totals, feeding every percentage.
	totals, err := runGlobals(ctx, q, o)
	if err != nil {
		return nil, err
	}
	rep.Total = totals

	// Pass 2 — ranking.
	keys, err := runRanking(ctx, q, o)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return rep, nil
	}

	// Pass 3 — statistics for the ranked keys. The totals from pass 1 are
	// threaded in because every Stats.Pct is relative to them.
	statsByKey, err := runStats(ctx, q, o, keys, rep.Total)
	if err != nil {
		return nil, err
	}

	for i, key := range keys {
		cls, ok := statsByKey[key]
		if !ok {
			// Pass 3 lost a key that pass 2 produced. With COALESCE on both
			// sides this should be impossible; surface it rather than emit a
			// silently short report.
			return nil, fmt.Errorf("internal: group %q ranked but has no statistics", key)
		}
		cls.Rank = i + 1
		rep.Classes = append(rep.Classes, cls)
	}
	return rep, nil
}

func runGlobals(ctx context.Context, q Querier, o Options) (Totals, error) {
	sql, args := buildGlobalsSQL(o.Filter)
	res, err := q.Query(ctx, sql, args...)
	if err != nil {
		return Totals{}, fmt.Errorf("digest totals: %w", err)
	}
	if len(res.Rows) == 0 {
		return Totals{}, nil
	}
	r := res.Rows[0]
	return Totals{
		Calls:        toInt64(r[0]),
		ExecTimeSec:  toFloat(r[1]),
		LockTimeSec:  toFloat(r[2]),
		RowsSent:     toFloat(r[3]),
		RowsExamined: toFloat(r[4]),
	}, nil
}

// runRanking returns the ordered group keys, worst first.
func runRanking(ctx context.Context, q Querier, o Options) ([]string, error) {
	sql, args := buildRankSQL(o)
	res, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("digest ranking: %w", err)
	}
	keys := make([]string, 0, len(res.Rows))
	for _, row := range res.Rows {
		keys = append(keys, toStr(row[colRankGroup]))
	}
	return keys, nil
}

// runStats returns one Class per group key, without Rank (Run assigns it from
// the ranking order). totals supplies the denominators for every percentage.
func runStats(ctx context.Context, q Querier, o Options, keys []string, totals Totals) (map[string]Class, error) {
	sql, args := buildStatsSQL(o, keys)
	res, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("digest statistics: %w", err)
	}

	out := make(map[string]Class, len(res.Rows))
	for _, row := range res.Rows {
		if len(row) < statsWidth {
			return nil, fmt.Errorf("internal: statistics row has %d columns, want %d", len(row), statsWidth)
		}

		calls := toInt64(row[colStatsCalls])
		exec := readStats(row, colStatsQueryTime, totals.ExecTimeSec)

		cls := Class{
			ID:        toStr(row[colStatsGroup]),
			Calls:     calls,
			SampleSQL: toStr(row[colStatsSampleSQL]),
			Item:      Distill(toStr(row[colStatsSanitizedSQL]), toStrs(row[colStatsTables])),
			Attributes: map[string]Stats{
				AttrKeyExecTime:     exec,
				AttrKeyLockTime:     readStats(row, colStatsLockTime, totals.LockTimeSec),
				AttrKeyRowsSent:     readStats(row, colStatsRowsSent, totals.RowsSent),
				AttrKeyRowsExamined: readStats(row, colStatsRowsExamined, totals.RowsExamined),
			},
		}
		cls.ResponseTimePct = exec.Pct
		cls.RPerCall = exec.Avg
		// V/M is the variance-to-mean ratio of response time: a high value
		// marks an erratic query. NULL var_samp (single-row group) is already
		// coerced to 0 by toFloat.
		if exec.Avg != 0 {
			cls.VM = toFloat(row[colStatsVarSamp]) / exec.Avg
		}
		out[cls.ID] = cls
	}
	return out, nil
}

// readStats pulls one attribute's seven statistics starting at base.
func readStats(row []interface{}, base int, populationTotal float64) Stats {
	total := toFloat(row[base])
	return Stats{
		Total:  total,
		Min:    toFloat(row[base+1]),
		Max:    toFloat(row[base+2]),
		Avg:    toFloat(row[base+3]),
		P95:    toFloat(row[base+4]),
		Stddev: toFloat(row[base+5]),
		Median: toFloat(row[base+6]),
		Pct:    pct(total, populationTotal),
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `CGO_ENABLED=1 go test ./internal/digest/ -v`
Expected: PASS — all eleven `TestRun*` cases green alongside Tasks 1–5.

- [ ] **Step 5: Commit**

```bash
CGO_ENABLED=1 gofmt -l . && git add internal/digest/run.go internal/digest/run_test.go internal/digest/testdata_test.go
git commit -m "feat(digest): execute the three passes into a typed report"
```

---

### Task 7: Value formatting and the markdown renderer

**Files:**
- Create: `cmd/cli/digest_render.go`
- Test: `cmd/cli/digest_render_test.go`

**Interfaces:**
- Consumes: `digest.Report`, `digest.Class`, `digest.Stats`, the `digest.AttrKey*` constants (Task 3/6).
- Produces: `renderMarkdown(io.Writer, *digest.Report) error`, `formatDuration(float64) string`, `formatCount(float64) string`.

- [ ] **Step 1: Write the failing test**

Create `cmd/cli/digest_render_test.go`:

```go
package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/dbsmedya/gofast/internal/digest"
)

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{0.000004, "4us"},
		{0.002, "2ms"},
		{1.5, "1.50s"},
		{372.05, "372.05s"},
	}
	for _, c := range cases {
		if got := formatDuration(c.in); got != c.want {
			t.Errorf("formatDuration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatCount(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{51, "51"},
		{5100, "5.1k"},
		{6260000, "6.26M"},
	}
	for _, c := range cases {
		if got := formatCount(c.in); got != c.want {
			t.Errorf("formatCount(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func sampleReport() *digest.Report {
	return &digest.Report{
		GroupBy: "fingerprint",
		OrderBy: "Query_time:sum",
		Limit:   20,
		Total: digest.Totals{
			Calls: 3, ExecTimeSec: 9, LockTimeSec: 0.9,
			RowsSent: 40, RowsExamined: 900,
		},
		Classes: []digest.Class{{
			Rank: 1, ID: "FDEA8D2993C9CAF3", Item: "SELECT orders",
			Calls: 2, ResponseTimePct: 44.4, RPerCall: 2, VM: 1,
			SampleSQL: "SELECT * FROM orders WHERE customer_id = 8831",
			Attributes: map[string]digest.Stats{
				digest.AttrKeyExecTime:     {Pct: 44.4, Total: 4, Min: 1, Max: 3, Avg: 2, P95: 2.9, Stddev: 1.41, Median: 2},
				digest.AttrKeyLockTime:     {Pct: 44.4, Total: 0.4, Min: 0.1, Max: 0.3, Avg: 0.2, P95: 0.29, Stddev: 0.14, Median: 0.2},
				digest.AttrKeyRowsSent:     {Pct: 100, Total: 40, Min: 10, Max: 30, Avg: 20, P95: 29, Stddev: 14, Median: 20},
				digest.AttrKeyRowsExamined: {Pct: 44.4, Total: 400, Min: 100, Max: 300, Avg: 200, P95: 290, Stddev: 141, Median: 200},
			},
		}},
	}
}

func TestRenderMarkdownProfileTable(t *testing.T) {
	var buf bytes.Buffer
	if err := renderMarkdown(&buf, sampleReport()); err != nil {
		t.Fatalf("renderMarkdown: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"## Profile",
		"| Rank | Query ID | Response time | Calls | R/Call | V/M | Item |",
		"FDEA8D2993C9CAF3",
		"SELECT orders",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown missing %q:\n%s", want, out)
		}
	}
}

func TestRenderMarkdownDetailTableAndFencedSQL(t *testing.T) {
	var buf bytes.Buffer
	if err := renderMarkdown(&buf, sampleReport()); err != nil {
		t.Fatalf("renderMarkdown: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"| attribute | pct | total | min | max | avg | 95% | stddev | median |",
		"| Count |",
		"| Exec time |",
		"| Lock time |",
		"| Rows sent |",
		"| Rows examined |",
		"```sql",
		"SELECT * FROM orders WHERE customer_id = 8831",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown missing %q:\n%s", want, out)
		}
	}
}

func TestRenderMarkdownCountRowLeavesDistributionCellsBlank(t *testing.T) {
	var buf bytes.Buffer
	if err := renderMarkdown(&buf, sampleReport()); err != nil {
		t.Fatalf("renderMarkdown: %v", err)
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(line, "| Count |") {
			// pct and total are filled; the six distribution cells are blank,
			// matching pt-query-digest.
			if strings.Count(line, "|") != 10 {
				t.Errorf("Count row has the wrong cell count: %q", line)
			}
			if strings.Contains(line, "| 2.9 |") {
				t.Errorf("Count row must not carry distribution values: %q", line)
			}
			return
		}
	}
	t.Fatal("no Count row found")
}

func TestRenderMarkdownNullGroupRendersAsNone(t *testing.T) {
	rep := sampleReport()
	rep.Classes[0].ID = ""
	var buf bytes.Buffer
	if err := renderMarkdown(&buf, rep); err != nil {
		t.Fatalf("renderMarkdown: %v", err)
	}
	if !strings.Contains(buf.String(), "(none)") {
		t.Errorf("empty group key should render as (none):\n%s", buf.String())
	}
}

func TestRenderMarkdownEmptyReport(t *testing.T) {
	rep := &digest.Report{GroupBy: "fingerprint", OrderBy: "Query_time:sum", Classes: []digest.Class{}}
	var buf bytes.Buffer
	if err := renderMarkdown(&buf, rep); err != nil {
		t.Fatalf("renderMarkdown: %v", err)
	}
	if !strings.Contains(buf.String(), "No queries matched.") {
		t.Errorf("empty report should say so:\n%s", buf.String())
	}
}

func TestRenderMarkdownEscapesPipesInSampleSQL(t *testing.T) {
	rep := sampleReport()
	// A pipe inside a table cell would break the markdown table. The sample SQL
	// lives in a fenced block, but Item does not.
	rep.Classes[0].Item = "SELECT a|b"
	var buf bytes.Buffer
	if err := renderMarkdown(&buf, rep); err != nil {
		t.Fatalf("renderMarkdown: %v", err)
	}
	if !strings.Contains(buf.String(), `a\|b`) {
		t.Errorf("pipe in a table cell must be escaped:\n%s", buf.String())
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `CGO_ENABLED=1 go test ./cmd/cli/ -run 'TestFormat|TestRenderMarkdown' -v`
Expected: FAIL — `undefined: formatDuration`.

- [ ] **Step 3: Write the implementation**

Create `cmd/cli/digest_render.go`:

```go
package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/dbsmedya/gofast/internal/digest"
)

// attrRows is the fixed report order. The schema stores only these four numeric
// attributes, so pt-query-digest's Bytes sent / Tmp tables / Merge passes rows
// have no source here.
var attrRows = []struct {
	label string
	key   string
	isDur bool
}{
	{"Exec time", digest.AttrKeyExecTime, true},
	{"Lock time", digest.AttrKeyLockTime, true},
	{"Rows sent", digest.AttrKeyRowsSent, false},
	{"Rows examined", digest.AttrKeyRowsExamined, false},
}

// formatDuration renders seconds with a magnitude-appropriate unit.
func formatDuration(sec float64) string {
	switch {
	case sec == 0:
		return "0"
	case sec < 0.001:
		return fmt.Sprintf("%.0fus", sec*1e6)
	case sec < 1:
		return fmt.Sprintf("%.0fms", sec*1e3)
	default:
		return fmt.Sprintf("%.2fs", sec)
	}
}

// formatCount renders a row count with k/M thresholds.
func formatCount(n float64) string {
	switch {
	case n == 0:
		return "0"
	case n >= 1e6:
		return fmt.Sprintf("%.2fM", n/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%.1fk", n/1e3)
	default:
		return fmt.Sprintf("%.0f", n)
	}
}

// cell escapes a value for use inside a markdown table cell.
func cell(s string) string {
	if s == "" {
		return "(none)"
	}
	return strings.ReplaceAll(s, "|", `\|`)
}

// renderMarkdown writes the profile table followed by one detail section per
// class.
func renderMarkdown(w io.Writer, r *digest.Report) error {
	if len(r.Classes) == 0 {
		_, err := fmt.Fprintf(w, "No queries matched.\n")
		return err
	}

	var b strings.Builder

	b.WriteString("## Profile\n\n")
	b.WriteString("| Rank | Query ID | Response time | Calls | R/Call | V/M | Item |\n")
	b.WriteString("|---|---|---|---|---|---|---|\n")
	for _, c := range r.Classes {
		exec := c.Attributes[digest.AttrKeyExecTime]
		fmt.Fprintf(&b, "| %d | %s | %s %.1f%% | %d | %.2f | %.2f | %s |\n",
			c.Rank, cell(c.ID), formatDuration(exec.Total), c.ResponseTimePct,
			c.Calls, c.RPerCall, c.VM, cell(c.Item))
	}

	for _, c := range r.Classes {
		fmt.Fprintf(&b, "\n## Query %d: %s\n\n", c.Rank, cell(c.ID))
		b.WriteString("| attribute | pct | total | min | max | avg | 95% | stddev | median |\n")
		b.WriteString("|---|---|---|---|---|---|---|---|---|\n")

		// The Count row carries only pct and total, as pt-query-digest does.
		countPct := 0.0
		if r.Total.Calls != 0 {
			countPct = float64(c.Calls) / float64(r.Total.Calls) * 100
		}
		fmt.Fprintf(&b, "| Count | %.0f | %d | | | | | | |\n", countPct, c.Calls)

		for _, a := range attrRows {
			s := c.Attributes[a.key]
			format := formatCount
			if a.isDur {
				format = formatDuration
			}
			fmt.Fprintf(&b, "| %s | %.0f | %s | %s | %s | %s | %s | %s | %s |\n",
				a.label, s.Pct,
				format(s.Total), format(s.Min), format(s.Max), format(s.Avg),
				format(s.P95), format(s.Stddev), format(s.Median))
		}

		if c.SampleSQL != "" {
			// Untruncated, and fenced so pipes and newlines cannot break the
			// surrounding tables.
			fmt.Fprintf(&b, "\n```sql\n%s\n```\n", c.SampleSQL)
		}
	}

	_, err := io.WriteString(w, b.String())
	return err
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `CGO_ENABLED=1 go test ./cmd/cli/ -v`
Expected: PASS — the render tests plus the existing `TestBuildServeRouter_HealthAndAuth`.

- [ ] **Step 5: Commit**

```bash
CGO_ENABLED=1 gofmt -l . && git add cmd/cli/digest_render.go cmd/cli/digest_render_test.go
git commit -m "feat(digest): markdown renderer with pt-query-digest layout"
```

---

### Task 8: The `digest` cobra command and JSON output

**Files:**
- Create: `cmd/cli/digest.go`
- Modify: `cmd/cli/main.go:66` (add one `rootCmd.AddCommand(digestCmd())` line)
- Test: `cmd/cli/digest_test.go`

**Interfaces:**
- Consumes: `digest.Run`, `digest.Options`, `digest.ParseGroupKey`, `digest.ParseOrderBy`, `digest.ParseFilters`, `digest.ApplyTimeBounds`, `renderMarkdown`.
- Produces: `digestCmd() *cobra.Command`, `buildDigestOptions(digestFlags, time.Time) (digest.Options, error)`, `runDigest(*config.Config, digest.Options, bool, io.Writer) error`.

- [ ] **Step 1: Write the failing test**

Create `cmd/cli/digest_test.go`:

```go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbsmedya/gofast/pkg/config"
	"github.com/dbsmedya/gofast/pkg/models"
	"github.com/dbsmedya/gofast/pkg/storage"
)

// seedCLIStore builds a small DuckDB file and returns its path.
//
// Distinct EventOffsets are mandatory: FinalizeIngestion dedupes on
// (file_hash, event_offset), so equal offsets collapse the rows into one.
func seedCLIStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cli.duckdb")
	rw, err := storage.NewStorage(path, false)
	if err != nil {
		t.Fatalf("open rw: %v", err)
	}
	if err := rw.DropSlowLogIndexes(); err != nil {
		t.Fatalf("drop indexes: %v", err)
	}
	now := time.Now()
	entries := []*models.SlowLogEntry{
		{FingerprintID: "AAAA", SanitizedSQL: "SELECT * FROM orders WHERE id = ?",
			SampleSQL: "SELECT * FROM orders WHERE id = 1", User: "app", Host: "h1", DB: "shop",
			QueryTimeSec: 1, LockTimeSec: 0.1, RowsSent: 10, RowsExamined: 100,
			TS: now, Tables: []string{"orders"}, EventOffset: 1, EventHash: "e1"},
		{FingerprintID: "BBBB", SanitizedSQL: "UPDATE inventory SET n = ?",
			SampleSQL: "UPDATE inventory SET n = 5", User: "batch", Host: "h2", DB: "ops",
			QueryTimeSec: 5, LockTimeSec: 0.5, RowsSent: 0, RowsExamined: 500,
			TS: now, Tables: []string{"inventory"}, EventOffset: 2, EventHash: "e2"},
	}
	if err := rw.InsertSlowLogBatch(context.Background(), entries, "testfile"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := rw.FinalizeIngestion(context.Background()); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if err := rw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return path
}

func testConfig(path string) *config.Config {
	cfg := &config.Config{}
	cfg.DuckDB.Path = path
	return cfg
}

func TestRunDigestJSONIsPureJSON(t *testing.T) {
	cfg := testConfig(seedCLIStore(t))
	var buf bytes.Buffer
	if err := runDigest(cfg, digestDefaults(), true, &buf); err != nil {
		t.Fatalf("runDigest: %v", err)
	}

	// The whole of stdout must parse; any banner or preamble breaks `| jq .`.
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not pure JSON: %v\n%s", err, buf.String())
	}
	for _, key := range []string{"group_by", "order_by", "limit", "total", "classes"} {
		if _, ok := got[key]; !ok {
			t.Errorf("JSON missing %q: %s", key, buf.String())
		}
	}
}

func TestRunDigestJSONEmptyResultStillParses(t *testing.T) {
	cfg := testConfig(seedCLIStore(t))
	o := digestDefaults()
	f, err := digestFilterFor("db = nonexistent")
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	o.Filter = f

	var buf bytes.Buffer
	if err := runDigest(cfg, o, true, &buf); err != nil {
		t.Fatalf("runDigest: %v", err)
	}
	var got struct {
		Classes []any `json:"classes"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("empty result is not valid JSON: %v\n%s", err, buf.String())
	}
	if len(got.Classes) != 0 {
		t.Errorf("classes = %d, want 0", len(got.Classes))
	}
	if !strings.Contains(buf.String(), `"classes": []`) &&
		!strings.Contains(buf.String(), `"classes":[]`) {
		t.Errorf("classes must marshal as [], not null: %s", buf.String())
	}
}

func TestRunDigestMarkdownDefault(t *testing.T) {
	cfg := testConfig(seedCLIStore(t))
	var buf bytes.Buffer
	if err := runDigest(cfg, digestDefaults(), false, &buf); err != nil {
		t.Fatalf("runDigest: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "## Profile") {
		t.Errorf("expected a markdown profile table:\n%s", out)
	}
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("markdown mode must not emit JSON:\n%s", out)
	}
}

func TestRunDigestMissingStoreErrorsWithoutPartialOutput(t *testing.T) {
	cfg := testConfig(filepath.Join(t.TempDir(), "does-not-exist.duckdb"))
	var buf bytes.Buffer
	err := runDigest(cfg, digestDefaults(), true, &buf)
	if err == nil {
		t.Fatal("expected an error for a missing store")
	}
	if buf.Len() != 0 {
		t.Errorf("no output may be written on failure, got: %s", buf.String())
	}
}

func TestBuildDigestOptionsWiresEveryFlag(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.Local)
	f := digestFlags{
		groupBy: "user",
		orderBy: "Lock_time:max",
		limit:   5,
		filters: []string{"db = shop", "query_time > 2"},
		since:   "7d",
		until:   "2026-07-25",
	}
	o, err := buildDigestOptions(f, now)
	if err != nil {
		t.Fatalf("buildDigestOptions: %v", err)
	}
	if string(o.GroupBy) != "user" {
		t.Errorf("GroupBy = %q", o.GroupBy)
	}
	if o.OrderBy.String() != "Lock_time:max" {
		t.Errorf("OrderBy = %q", o.OrderBy.String())
	}
	if o.Limit != 5 {
		t.Errorf("Limit = %d", o.Limit)
	}
	if len(o.Filter.DBs) != 1 || o.Filter.QueryTimeGT == nil {
		t.Errorf("filters not applied: %+v", o.Filter)
	}
	if o.Filter.TSFrom == nil || o.Filter.TSTo == nil {
		t.Errorf("time bounds not applied: %+v", o.Filter)
	}
}

func TestBuildDigestOptionsSurfacesParseErrors(t *testing.T) {
	now := time.Now()
	cases := []digestFlags{
		{groupBy: "bogus", orderBy: "Query_time:sum", limit: 20},
		{groupBy: "fingerprint", orderBy: "nope", limit: 20},
		{groupBy: "fingerprint", orderBy: "Query_time:sum", limit: 0},
		{groupBy: "fingerprint", orderBy: "Query_time:sum", limit: 20, filters: []string{"bad"}},
		{groupBy: "fingerprint", orderBy: "Query_time:sum", limit: 20, since: "not-a-time"},
	}
	for i, f := range cases {
		if _, err := buildDigestOptions(f, now); err == nil {
			t.Errorf("case %d (%+v): expected an error", i, f)
		}
	}
}

func TestDigestCmdIsRegisteredWithSilencedUsage(t *testing.T) {
	cmd := digestCmd()
	if cmd.Use != "digest" {
		t.Errorf("Use = %q, want digest", cmd.Use)
	}
	// Usage on stdout would corrupt `--json | jq .`.
	if !cmd.SilenceUsage {
		t.Error("SilenceUsage must be true so errors never print usage to stdout")
	}
	for _, name := range []string{"group-by", "order-by", "limit", "filter", "since", "until", "json"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("missing flag --%s", name)
		}
	}
}

// digestDefaults and digestFilterFor are small test helpers.
func digestDefaults() digest.Options { return digest.DefaultOptions() }

func digestFilterFor(expr string) (storage.RecordFilter, error) {
	return digest.ParseFilters([]string{expr})
}
```

Add `"github.com/dbsmedya/gofast/internal/digest"` to the test file's imports.

- [ ] **Step 2: Run the test to verify it fails**

Run: `CGO_ENABLED=1 go test ./cmd/cli/ -run 'TestRunDigest|TestBuildDigest|TestDigestCmd' -v`
Expected: FAIL — `undefined: runDigest`.

- [ ] **Step 3: Write the implementation**

Create `cmd/cli/digest.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/dbsmedya/gofast/internal/digest"
	"github.com/dbsmedya/gofast/pkg/config"
	"github.com/dbsmedya/gofast/pkg/storage"
	"github.com/spf13/cobra"
)

// digestFlags is the raw flag input, before parsing and validation.
type digestFlags struct {
	groupBy string
	orderBy string
	limit   int
	filters []string
	since   string
	until   string
	json    bool
}

// buildDigestOptions parses raw flags into validated Options. now is injected
// so relative --since/--until values are testable.
func buildDigestOptions(f digestFlags, now time.Time) (digest.Options, error) {
	o := digest.DefaultOptions()

	g, err := digest.ParseGroupKey(f.groupBy)
	if err != nil {
		return o, err
	}
	o.GroupBy = g

	ord, err := digest.ParseOrderBy(f.orderBy)
	if err != nil {
		return o, err
	}
	o.OrderBy = ord
	o.Limit = f.limit

	filter, err := digest.ParseFilters(f.filters)
	if err != nil {
		return o, err
	}
	if err := digest.ApplyTimeBounds(&filter, f.since, f.until, now); err != nil {
		return o, err
	}
	o.Filter = filter

	if err := o.Validate(); err != nil {
		return o, err
	}
	return o, nil
}

// runDigest opens the store read-only, runs the digest, and renders it.
//
// Nothing is written to out until the report is complete, so a failure never
// leaves a truncated JSON document on stdout.
func runDigest(cfg *config.Config, o digest.Options, asJSON bool, out io.Writer) error {
	store, err := storage.NewStorage(cfg.GetDSN(), true)
	if err != nil {
		return fmt.Errorf("failed to open storage: %w", err)
	}
	defer store.Close()

	rep, err := digest.Run(context.Background(), store, o)
	if err != nil {
		return err
	}

	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	return renderMarkdown(out, rep)
}

func digestCmd() *cobra.Command {
	var f digestFlags

	cmd := &cobra.Command{
		Use:   "digest",
		Short: "Report slow queries in pt-query-digest's format",
		Long: `Aggregate stored slow queries into a pt-query-digest-style report.

Flag names and defaults follow pt-query-digest so existing habits transfer:

  gofast-cli digest --group-by fingerprint --order-by Query_time:sum --limit 10

--order-by takes ATTRIBUTE:AGGREGATE. Attributes are Query_time, Lock_time,
Rows_sent and Rows_examined; aggregates are sum, min, max, avg and cnt. The
"cnt" aggregate counts rows and ignores the attribute, so Query_time:cnt orders
by call frequency. Results are always ordered worst-first.

--filter takes "KEY OP VALUE" and may be repeated; all filters are ANDed:

  gofast-cli digest --filter "fingerprint = FDEA8D2993C9CAF3"
  gofast-cli digest --filter "db = shop" --filter "query_time > 2"

String keys (user, host, db, source_host, table) accept a comma list or a
repeated flag, both producing an IN match. Numeric keys are query_time and
lock_time (> and <) and rows_examined (>). rows_sent is reported but cannot be
filtered.

Quote filters that contain > or <, or the shell will redirect output:
  --filter "query_time > 2"    not    --filter query_time > 2

--since and --until accept a relative value (7d, 90m, 2h, 30s; a bare integer
means seconds) or an absolute "YYYY-MM-DD" / "YYYY-MM-DD HH:MM:SS", read in the
machine's local timezone.

Output is markdown by default. With --json, stdout carries only JSON, so it can
be piped straight into jq.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true, // keep usage off stdout so --json | jq stays valid
		RunE: func(cmd *cobra.Command, args []string) error {
			if configLoaded == nil {
				return fmt.Errorf("config not loaded")
			}
			o, err := buildDigestOptions(f, time.Now())
			if err != nil {
				return err
			}
			return runDigest(configLoaded, o, f.json, os.Stdout)
		},
	}

	cmd.Flags().StringVar(&f.groupBy, "group-by", "fingerprint",
		"group by fingerprint, user, db, host, source_host, or tables")
	cmd.Flags().StringVar(&f.orderBy, "order-by", "Query_time:sum",
		"order by ATTRIBUTE:AGGREGATE, e.g. Query_time:sum")
	cmd.Flags().IntVar(&f.limit, "limit", 20, "number of query classes to report")
	cmd.Flags().StringArrayVar(&f.filters, "filter", nil,
		`filter as "KEY OP VALUE"; repeatable and ANDed`)
	cmd.Flags().StringVar(&f.since, "since", "", "only queries at or after this time")
	cmd.Flags().StringVar(&f.until, "until", "", "only queries at or before this time")
	cmd.Flags().BoolVar(&f.json, "json", false, "emit JSON instead of markdown")

	return cmd
}
```

- [ ] **Step 4: Register the command**

In `cmd/cli/main.go`, add one line to the command list around line 66:

```go
	rootCmd.AddCommand(queryCmd())
	rootCmd.AddCommand(digestCmd())
	rootCmd.AddCommand(versionCmd())
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `CGO_ENABLED=1 go test ./... -v`
Expected: PASS across every package.

- [ ] **Step 6: Verify by hand against real data**

```bash
CGO_ENABLED=1 go build -o bin/gofast-cli ./cmd/cli
./bin/gofast-cli digest --help
./bin/gofast-cli --duck-db-path ./data/gofast.duckdb digest --limit 5
./bin/gofast-cli --duck-db-path ./data/gofast.duckdb digest --limit 5 --json | jq .
./bin/gofast-cli --duck-db-path ./data/gofast.duckdb digest --group-by tables --order-by Rows_examined:sum --limit 5
```

Expected: the markdown report renders; the `jq .` pipe succeeds with no parse error. If `data/gofast.duckdb` does not exist, parse a log first with `./bin/gofast-cli parse` or point `--duck-db-path` at an existing store.

- [ ] **Step 7: Commit**

```bash
CGO_ENABLED=1 gofmt -l . && git add cmd/cli/digest.go cmd/cli/digest_test.go cmd/cli/main.go
git commit -m "feat(digest): add the digest subcommand with markdown and JSON output"
```

---

### Task 9: Documentation, the API-locked gate, and release prep

**Files:**
- Create: `docs/README_DIGEST.md`
- Modify: `README.md` (add a digest section to the CLI usage area)
- Create: `scripts/check-pkg-locked.sh`
- Modify: `.github/workflows/` CI workflow (add the gate step)

**Interfaces:**
- Consumes: the finished command from Task 8.
- Produces: no Go symbols.

- [ ] **Step 1: Write the API-locked gate script**

Create `scripts/check-pkg-locked.sh`:

```bash
#!/usr/bin/env bash
# Fails if this branch changed the public pkg/ contract or the served OpenAPI
# spec. The digest feature is deliberately confined to internal/ and cmd/.
set -euo pipefail

BASE="${1:-origin/main}"

if ! git rev-parse --verify "$BASE" >/dev/null 2>&1; then
  echo "check-pkg-locked: base ref '$BASE' not found" >&2
  exit 2
fi

changed="$(git diff --name-only "$BASE"...HEAD -- pkg/ || true)"
if [ -n "$changed" ]; then
  echo "check-pkg-locked: pkg/ must not change on this branch:" >&2
  echo "$changed" >&2
  exit 1
fi

echo "check-pkg-locked: pkg/ is unchanged against $BASE"
```

Make it executable:

```bash
chmod +x scripts/check-pkg-locked.sh
```

- [ ] **Step 2: Run the gate to verify it passes**

Run: `./scripts/check-pkg-locked.sh origin/main`
Expected: `check-pkg-locked: pkg/ is unchanged against origin/main`

If it fails, a `pkg/` file was modified — revert that change; the feature must not need it.

- [ ] **Step 3: Run the full verification suite**

```bash
CGO_ENABLED=1 go build ./...
CGO_ENABLED=1 go test ./...
CGO_ENABLED=1 gofmt -l .
./scripts/run-contract-check.sh
./scripts/check-pkg-locked.sh origin/main
```

Expected: builds clean, all tests pass, `gofmt -l` prints nothing, the contract check reports success, and the pkg gate passes. Record the actual output — do not claim success without reading it.

- [ ] **Step 4: Write the reference documentation**

Create `docs/README_DIGEST.md`. Copy these sections verbatim from
`docs/specs/2026-07-25-pt-query-digest-design.md`, since they are already written
and must not drift:

| doc section | source in the spec |
|---|---|
| Flags | §1 command surface block |
| Deviations from pt-query-digest | §1 deviations table |
| `--order-by` attributes and aggregates | §1 two mapping tables |
| `--group-by` values, aliases, and the `tables` fan-out caveat | §1 group table + fan-out paragraph |
| `--filter` grammar | §2 key/operator table + the multi-value, `rows_sent`, and shell-quoting notes |
| `--since` / `--until` | §3 in full, including the local-timezone rationale |
| Report columns and what V/M means | §4 profile table + per-class detail bullets |
| JSON schema | §6 example, verbatim |
| Known limits | §14 out-of-scope list |

Open with a two-sentence statement of what the command is for, and add this
worked example after the flag table:

````markdown
```bash
# The ten fingerprints consuming the most total execution time
gofast-cli digest --group-by fingerprint --order-by Query_time:sum --limit 10

# One fingerprint in detail
gofast-cli digest --filter "fingerprint = FDEA8D2993C9CAF3"

# Last week's worst tables by rows examined, as JSON
gofast-cli digest --group-by tables --order-by Rows_examined:sum \
  --since 7d --json | jq '.classes[] | {id, calls}'
```
````

- [ ] **Step 5: Add a README section**

In `README.md`, alongside the existing `parse`/`stats`/`query` CLI examples, add
a `### digest` section containing the same three commands as Step 4's worked
example, one sentence noting that output is markdown unless `--json` is passed,
and a pointer to `docs/README_DIGEST.md`.

- [ ] **Step 6: Wire the gate into CI**

Add a new job to `.github/workflows/ci.yml`. It needs full history, so it uses
`fetch-depth: 0` exactly as the existing `gitleaks` job (line ~102) does:

```yaml
  pkg-locked:
    name: pkg-locked
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
      - name: Assert pkg/ is unchanged
        run: |
          git fetch origin "${{ github.base_ref || 'main' }}" --depth=0
          ./scripts/check-pkg-locked.sh "origin/${{ github.base_ref || 'main' }}"
```

No `setup-go` step is needed — the script is pure git.

- [ ] **Step 7: Commit**

```bash
CGO_ENABLED=1 gofmt -l . && git add docs/README_DIGEST.md README.md scripts/check-pkg-locked.sh .github/workflows/
git commit -m "docs: document the digest command and gate pkg/ against changes"
```

- [ ] **Step 8: Release prep**

The tag itself is the maintainer's call, not this plan's. When tagging:

```bash
# from main, after this branch merges
git tag -a v1.0.0 -m "v1.0.0: pt-query-digest-compatible digest command; API contract declared stable"
git push origin v1.0.0
```

Then, in `gofast-enterprise` (a separate repo, tracked as follow-up work — **not** part of this plan):

1. Add the `gofast-cli` binary to the enterprise image so operators get `digest` alongside `gofastd`.
2. `go get github.com/dbsmedya/gofast@v1.0.0 && go mod tidy` in `backend/`.

Note that step 2 is functionally a no-op on its own: `digest` lives in `internal/`, so enterprise cannot import it. Step 1 is what actually delivers the feature there.

---

## Verification Checklist

Before declaring the feature complete, run every command and read its output:

- [ ] `CGO_ENABLED=1 go build ./...` — clean
- [ ] `CGO_ENABLED=1 go test ./...` — all green
- [ ] `CGO_ENABLED=1 gofmt -l .` — prints nothing
- [ ] `golangci-lint run ./...` — clean
- [ ] `./scripts/run-contract-check.sh` — green
- [ ] `./scripts/check-pkg-locked.sh origin/main` — pkg/ unchanged
- [ ] `git diff --stat origin/main...HEAD -- pkg/` — empty
- [ ] `gitleaks detect --no-git` — no findings
- [ ] `./bin/gofast-cli digest --json | jq .` against a real store — parses
- [ ] `git diff origin/main...HEAD -- cmd/cli/main.go` — shows **only** the added
      `rootCmd.AddCommand(digestCmd())` line, proving `query`, `parse`, `stats`
      and `serve` are untouched
- [ ] `./bin/gofast-cli query "select 1"` — output identical to the same command
      run from `origin/main`
