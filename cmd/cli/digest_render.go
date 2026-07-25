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
