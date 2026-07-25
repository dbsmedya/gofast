package digest

import (
	"context"
	"math"
	"strings"
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

	// The representative rule makes these exact, not merely non-empty: AAAA is
	// its own most-frequent fingerprint, and its slowest row is the 3.0s one,
	// which touches both tables.
	if aaaa.Item != "SELECT orders customers" {
		t.Errorf("AAAA item = %q, want \"SELECT orders customers\" (the slowest row's tables)", aaaa.Item)
	}
	if aaaa.SampleSQL != "SELECT * FROM orders WHERE id = 2" {
		t.Errorf("AAAA sample_sql = %q, want the 3.0s row's sample", aaaa.SampleSQL)
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

func TestRunRepresentativeComesFromMostFrequentFingerprint(t *testing.T) {
	// One user runs a rare-but-slow query once and a frequent query three
	// times. The representative must describe the FREQUENT one, and its
	// sample must be that fingerprint's slowest instance -- not the slow
	// outlier from the other fingerprint.
	now := time.Now()
	entries := []*models.SlowLogEntry{
		{FingerprintID: "RARE", SanitizedSQL: "DELETE FROM audit WHERE id = ?",
			SampleSQL: "RARE_SAMPLE", User: "app", Host: "h", DB: "d",
			QueryTimeSec: 99, LockTimeSec: 0.1, RowsSent: 1, RowsExamined: 1,
			TS: now, Tables: []string{"audit"}, EventOffset: 1, EventHash: "r1"},
		{FingerprintID: "FREQ", SanitizedSQL: "SELECT * FROM orders WHERE id = ?",
			SampleSQL: "FREQ_FAST", User: "app", Host: "h", DB: "d",
			QueryTimeSec: 1, LockTimeSec: 0.1, RowsSent: 1, RowsExamined: 1,
			TS: now, Tables: []string{"orders"}, EventOffset: 2, EventHash: "f1"},
		{FingerprintID: "FREQ", SanitizedSQL: "SELECT * FROM orders WHERE id = ?",
			SampleSQL: "FREQ_MID", User: "app", Host: "h", DB: "d",
			QueryTimeSec: 2, LockTimeSec: 0.1, RowsSent: 1, RowsExamined: 1,
			TS: now, Tables: []string{"orders"}, EventOffset: 3, EventHash: "f2"},
		{FingerprintID: "FREQ", SanitizedSQL: "SELECT * FROM orders WHERE id = ?",
			SampleSQL: "FREQ_SLOWEST", User: "app", Host: "h", DB: "d",
			QueryTimeSec: 3, LockTimeSec: 0.1, RowsSent: 1, RowsExamined: 1,
			TS: now, Tables: []string{"orders"}, EventOffset: 4, EventHash: "f3"},
	}
	store := seedStore(t, entries)

	o := DefaultOptions()
	o.GroupBy = GroupUser
	rep, err := Run(context.Background(), store, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Classes) != 1 {
		t.Fatalf("classes = %d, want 1", len(rep.Classes))
	}
	c := rep.Classes[0]

	// FREQ has 3 calls vs RARE's 1, so FREQ wins despite RARE being far slower.
	if c.Item != "SELECT orders" {
		t.Errorf("Item = %q, want \"SELECT orders\" (the most frequent query)", c.Item)
	}
	// Within FREQ, the slowest instance supplies the sample.
	if c.SampleSQL != "FREQ_SLOWEST" {
		t.Errorf("SampleSQL = %q, want FREQ_SLOWEST", c.SampleSQL)
	}
	// A cross-row mixture would show up as the audit table with a FREQ sample.
	if strings.Contains(c.Item, "audit") {
		t.Errorf("Item mixed rows from another fingerprint: %q", c.Item)
	}
}

func TestRunTieBreakIsDeterministic(t *testing.T) {
	// Two fingerprints with identical total query time. Without the grp ASC
	// secondary sort, --limit 1 could return either across runs.
	now := time.Now()
	entries := []*models.SlowLogEntry{
		{FingerprintID: "AAAA", SanitizedSQL: "SELECT 1", SampleSQL: "a",
			User: "u", Host: "h", DB: "d", QueryTimeSec: 2, LockTimeSec: 0,
			RowsSent: 1, RowsExamined: 1, TS: now, Tables: []string{"t"},
			EventOffset: 1, EventHash: "a1"},
		{FingerprintID: "BBBB", SanitizedSQL: "SELECT 2", SampleSQL: "b",
			User: "u", Host: "h", DB: "d", QueryTimeSec: 2, LockTimeSec: 0,
			RowsSent: 1, RowsExamined: 1, TS: now, Tables: []string{"t"},
			EventOffset: 2, EventHash: "b1"},
	}
	store := seedStore(t, entries)

	o := DefaultOptions()
	o.Limit = 1
	var first string
	for i := 0; i < 5; i++ {
		rep, err := Run(context.Background(), store, o)
		if err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
		if len(rep.Classes) != 1 {
			t.Fatalf("run %d: classes = %d, want 1", i, len(rep.Classes))
		}
		if i == 0 {
			first = rep.Classes[0].ID
			continue
		}
		if rep.Classes[0].ID != first {
			t.Fatalf("tie broke differently across runs: %q then %q", first, rep.Classes[0].ID)
		}
	}
	// grp ASC means the lexicographically smaller key wins a tie.
	if first != "AAAA" {
		t.Errorf("tie winner = %q, want AAAA (grp ASC)", first)
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
