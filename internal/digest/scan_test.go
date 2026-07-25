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
