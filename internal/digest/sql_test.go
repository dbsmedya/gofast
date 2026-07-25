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
	// The secondary key is required: without it a tie at the LIMIT boundary
	// decides which classes appear at all, nondeterministically.
	if !strings.Contains(q, "ORDER BY rank_val DESC, grp ASC") {
		t.Errorf("expected a stable secondary sort:\n%s", q)
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

func TestBuildStatsSQLCarriesNoRepresentativeColumns(t *testing.T) {
	// Representative fields moved to pass 4; three independent ANY_VALUE calls
	// could otherwise mix rows.
	q, _ := buildStatsSQL(DefaultOptions(), []string{"AAAA"})
	if strings.Contains(q, "ANY_VALUE") {
		t.Errorf("stats SQL must not use ANY_VALUE:\n%s", q)
	}
	for _, col := range []string{"sample_sql", "sanitized_sql"} {
		if strings.Contains(q, col) {
			t.Errorf("stats SQL must not select %s:\n%s", col, q)
		}
	}
}

func TestBuildRepresentativeSQLPicksOneRowPerGroup(t *testing.T) {
	q, args := buildRepresentativeSQL(DefaultOptions(), []string{"AAAA", "BBBB"})

	// All three fields share one ordering expression, so they resolve to the
	// same row.
	for _, want := range []string{
		"arg_max(sample_sql, query_time_sec)",
		"arg_max(sanitized_sql, query_time_sec)",
		"arg_max(tables, query_time_sec)",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("representative SQL missing %q:\n%s", want, q)
		}
	}
	// Most-frequent fingerprint wins, with a deterministic tie-break.
	if !strings.Contains(q, "ORDER BY fp_calls DESC, fingerprint_id ASC") {
		t.Errorf("expected a deterministic fingerprint ranking:\n%s", q)
	}
	if !strings.Contains(q, "PARTITION BY grp") {
		t.Errorf("expected per-group partitioning:\n%s", q)
	}
	if !strings.Contains(q, "WHERE rn = 1") {
		t.Errorf("expected exactly one row per group:\n%s", q)
	}
	if !strings.Contains(q, "GROUP BY grp, fingerprint_id") {
		t.Errorf("expected the per-fingerprint grain:\n%s", q)
	}
	if len(args) != 2 || args[0] != "AAAA" {
		t.Errorf("args = %v, want the bound group keys", args)
	}
	if strings.Contains(q, "AAAA") {
		t.Errorf("group key was interpolated into SQL:\n%s", q)
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
	if statsWidth != colStatsRowsExamined+statsPerAttr {
		t.Errorf("statsWidth %d does not match the last attribute block", statsWidth)
	}
	if repWidth != colRepTables+1 {
		t.Errorf("repWidth %d does not match the last representative index", repWidth)
	}
}
