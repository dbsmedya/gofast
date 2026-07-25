package digest

import (
	"testing"
	"time"

	"github.com/dbsmedya/gofast/pkg/storage"
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

func TestParseFiltersRejectsMultiValueTable(t *testing.T) {
	// RecordFilter.BuildWhere() ANDs each table predicate, so a comma list
	// would mean "touches BOTH" while the same syntax on user means "either".
	// Rather than ship two opposite meanings for one syntax, reject it.
	cases := [][]string{
		{"table = orders,customers"},             // comma list
		{"table = orders", "table = customers"},  // repeated flag
		{"tables = a,b"},                         // via the alias
		{"table = orders", "tables = customers"}, // mixed spellings
	}
	for _, c := range cases {
		_, err := ParseFilters(c)
		if err == nil {
			t.Fatalf("ParseFilters(%v): expected an error", c)
		}
		if !contains(err.Error(), "--group-by tables") {
			t.Errorf("ParseFilters(%v) error %q should point at --group-by tables", c, err)
		}
	}
}

func TestParseFiltersRejectsEmptyValues(t *testing.T) {
	// A comma-only value previously passed the non-empty check and then
	// produced no values at all, silently running unfiltered.
	for _, in := range []string{"user =", "user = ,", "user = , ,", "db ="} {
		if _, err := ParseFilters([]string{in}); err == nil {
			t.Errorf("ParseFilters(%q): expected an error, got a silent no-op filter", in)
		}
	}
}

func TestParseFiltersErrors(t *testing.T) {
	cases := []struct {
		in      string
		mustSay string
	}{
		{"no_operator_here", "operator"},
		{"bogus = 1", "unknown"},
		{"rows_examined < 5", ">"},     // unsupported op names the supported one
		{"query_time > abc", "number"}, // unparseable numeric
		{"rows_sent > 5", "rows_sent"}, // reportable but not filterable
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
