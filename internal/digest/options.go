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
