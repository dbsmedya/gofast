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
