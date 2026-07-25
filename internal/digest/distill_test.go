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
