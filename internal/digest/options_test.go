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
		{"query_time:sum", AttrQueryTime, AggSum}, // case-insensitive attribute
		{"Query_time:SUM", AttrQueryTime, AggSum}, // case-insensitive aggregate
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
