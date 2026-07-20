package parser

import (
	"testing"
)

// ---------------------------------------------------------------------------
// extractDBFromQuery
// ---------------------------------------------------------------------------

func TestExtractDBFromQuery(t *testing.T) {
	tests := []struct {
		name   string
		sql    string
		wantDB string
	}{
		{"use with semicolon", "use my_database; SELECT 1", "my_database"},
		{"use without semicolon", "use my_database SELECT 1", "my_database"},
		{"use uppercase", "USE MyDatabase; SELECT 1", "MyDatabase"},
		{"use with backticks", "use `quoted_db`; SELECT 1", "quoted_db"},
		{"use with leading whitespace", "  use analytics; SELECT 1", "analytics"},
		{"no use statement", "SELECT * FROM users", ""},
		{"use at non-start position", "SELECT 1; use other_db;", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractDBFromQuery(tt.sql)
			if got != tt.wantDB {
				t.Errorf("extractDBFromQuery(%q) = %q, want %q", tt.sql, got, tt.wantDB)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// stripUseStatement
// ---------------------------------------------------------------------------

func TestStripUseStatement(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want string
	}{
		{"strips use with semicolon", "use mydb; SELECT * FROM t", "SELECT * FROM t"},
		{"strips use uppercase", "USE mydb; SELECT 1", "SELECT 1"},
		{"strips use with backticks", "use `mydb`; SELECT 1", "SELECT 1"},
		{"no use statement unchanged", "SELECT * FROM users", "SELECT * FROM users"},
		{"use not at start unchanged", "SELECT 1; use other;", "SELECT 1; use other;"},
		{"only use statement", "use mydb;", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripUseStatement(tt.sql)
			if got != tt.want {
				t.Errorf("stripUseStatement(%q) = %q, want %q", tt.sql, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// tokenize
// ---------------------------------------------------------------------------

func TestTokenize(t *testing.T) {
	tokens := tokenize("SELECT * FROM `my_db`.`my_table` WHERE col = 'value'")
	// Should produce: SELECT, FROM, my_db, ., my_table, WHERE, col
	identCount := 0
	dotCount := 0
	for _, tk := range tokens {
		switch tk.kind {
		case tkIdent:
			identCount++
		case tkDot:
			dotCount++
		}
	}
	if identCount < 5 {
		t.Errorf("expected at least 5 identifiers, got %d", identCount)
	}
	if dotCount != 1 {
		t.Errorf("expected 1 dot, got %d", dotCount)
	}
}

func TestTokenizeSkipsStringLiterals(t *testing.T) {
	tokens := tokenize("SELECT * FROM users WHERE name = 'FROM orders'")
	for _, tk := range tokens {
		if tk.kind == tkIdent && tk.val == "orders" {
			t.Error("tokenizer should not produce 'orders' from inside a string literal")
		}
	}
}

func TestTokenizeEscapedQuotes(t *testing.T) {
	tokens := tokenize(`SELECT * FROM users WHERE name = 'it\'s a test'`)
	for _, tk := range tokens {
		if tk.kind == tkIdent && tk.val == "s" {
			t.Error("tokenizer leaked identifier from escaped quote")
		}
	}
}

// ---------------------------------------------------------------------------
// extractTables
// ---------------------------------------------------------------------------

func TestExtractTables(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		db   string
		want []string
	}{
		{
			name: "CTE excluded bare name",
			sql:  "WITH recent AS (SELECT * FROM base) SELECT * FROM recent JOIN orders ON 1=1",
			db:   "",
			want: []string{"base", "orders"},
		},
		{
			name: "multiple CTEs",
			sql:  "WITH a AS (SELECT 1 FROM t1), b AS (SELECT 1 FROM t2) SELECT * FROM a JOIN b JOIN real_table",
			db:   "",
			want: []string{"t1", "t2", "real_table"},
		},
		{
			name: "schema-qualified not treated as CTE",
			sql:  "WITH recent AS (SELECT 1) SELECT * FROM archive.recent",
			db:   "",
			want: []string{"archive.recent"},
		},
		{
			name: "CTE name reused after semicolon",
			sql:  "WITH recent AS (SELECT 1 FROM x) SELECT * FROM recent; SELECT * FROM recent",
			db:   "",
			want: []string{"x", "recent"},
		},
		{
			name: "qualified db.table",
			sql:  "SELECT * FROM bmt.mid WHERE id = 1",
			db:   "bmt",
			want: []string{"bmt.mid"},
		},
		{
			name: "unqualified table gets db context",
			sql:  "SELECT * FROM users WHERE id = 1",
			db:   "analytics",
			want: []string{"analytics.users"},
		},
		{
			name: "no db context keeps bare table",
			sql:  "SELECT * FROM users",
			db:   "",
			want: []string{"users"},
		},
		{
			name: "trailing paren not captured",
			sql:  "SELECT * FROM bmt.mid) AS t",
			db:   "bmt",
			want: []string{"bmt.mid"},
		},
		{
			name: "subquery in WHERE clause",
			sql:  "SELECT * FROM bmt.mid WHERE id IN (SELECT mid FROM bmt.other)",
			db:   "bmt",
			want: []string{"bmt.mid", "bmt.other"},
		},
		{
			name: "subquery after FROM is skipped",
			sql:  "SELECT a FROM (SELECT * FROM bmt.mid) AS sub JOIN bmt.ref ON 1=1",
			db:   "bmt",
			want: []string{"bmt.mid", "bmt.ref"},
		},
		{
			name: "INSERT INTO with column list",
			sql:  "INSERT INTO bmt.mid (col1, col2) VALUES (1, 2)",
			db:   "bmt",
			want: []string{"bmt.mid"},
		},
		{
			name: "INSERT INTO SELECT FROM",
			sql:  "INSERT INTO archive SELECT * FROM events",
			db:   "bmt",
			want: []string{"bmt.archive", "bmt.events"},
		},
		{
			name: "UPDATE",
			sql:  "UPDATE bmt.mid SET col = 1",
			db:   "bmt",
			want: []string{"bmt.mid"},
		},
		{
			name: "DELETE FROM",
			sql:  "DELETE FROM old_records WHERE ts < NOW()",
			db:   "archive",
			want: []string{"archive.old_records"},
		},
		{
			name: "aliases do not produce AS as table",
			sql:  "SELECT * FROM users AS u JOIN orders AS o ON u.id = o.user_id",
			db:   "shop",
			want: []string{"shop.users", "shop.orders"},
		},
		{
			name: "DUAL is filtered",
			sql:  "SELECT 1 FROM DUAL",
			db:   "mydb",
			want: nil,
		},
		{
			name: "NULL is filtered",
			sql:  "SELECT NULL FROM DUAL",
			db:   "mydb",
			want: nil,
		},
		{
			name: "function call after FROM not a table",
			sql:  "SELECT FROM_UNIXTIME(ts) FROM events",
			db:   "logs",
			want: []string{"logs.events"},
		},
		{
			name: "backtick-quoted identifiers",
			sql:  "SELECT * FROM `my_table` JOIN `other_db`.`tbl` ON 1=1",
			db:   "db1",
			want: []string{"db1.my_table", "other_db.tbl"},
		},
		{
			name: "deduplication",
			sql:  "SELECT * FROM users u1 JOIN users u2 ON u1.id = u2.parent_id",
			db:   "app",
			want: []string{"app.users"},
		},
		{
			name: "multiple JOINs",
			sql:  "SELECT * FROM orders o JOIN customers c ON o.cid = c.id LEFT JOIN products p ON o.pid = p.id",
			db:   "shop",
			want: []string{"shop.orders", "shop.customers", "shop.products"},
		},
		{
			name: "use statement stripped before extraction",
			sql:  "use mydb; SELECT * FROM orders",
			db:   "mydb",
			want: []string{"mydb.orders"},
		},
		{
			name: "no tables in SELECT 1",
			sql:  "SELECT 1",
			db:   "test",
			want: nil,
		},
		{
			name: "mixed qualified and unqualified",
			sql:  "SELECT * FROM db1.users JOIN orders ON users.id = orders.uid",
			db:   "db2",
			want: []string{"db1.users", "db2.orders"},
		},
		{
			name: "string literal containing FROM keyword ignored",
			sql:  "SELECT * FROM users WHERE name = 'FROM orders'",
			db:   "app",
			want: []string{"app.users"},
		},
		{
			name: "case insensitive keywords, case-preserved identifiers",
			sql:  "select * from USERS join ORDERS on 1=1",
			db:   "APP",
			want: []string{"APP.USERS", "APP.ORDERS"},
		},
		{
			name: "preserves qualified db.table case",
			sql:  "SELECT * FROM Sales.CustomerOrders WHERE id = 1",
			db:   "Sales",
			want: []string{"Sales.CustomerOrders"},
		},
		{
			name: "preserves unqualified table case with db context",
			sql:  "SELECT * FROM OrderItems",
			db:   "ShopDB",
			want: []string{"ShopDB.OrderItems"},
		},
		{
			name: "preserves bare table case with no db context",
			sql:  "SELECT * FROM MyTable",
			db:   "",
			want: []string{"MyTable"},
		},
		{
			name: "case-sensitive dedup keeps distinct casings",
			sql:  "SELECT * FROM Users u JOIN users l ON u.id = l.pid",
			db:   "",
			want: []string{"Users", "users"},
		},
		{
			name: "backtick identifiers preserve case",
			sql:  "SELECT * FROM `OtherDB`.`MyTbl` WHERE 1=1",
			db:   "db1",
			want: []string{"OtherDB.MyTbl"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractTables(tt.sql, tt.db)

			if len(got) == 0 && len(tt.want) == 0 {
				return
			}

			if len(got) != len(tt.want) {
				t.Fatalf("extractTables(%q, %q)\n  got  %v\n  want %v", tt.sql, tt.db, got, tt.want)
			}
			for i, g := range got {
				if g != tt.want[i] {
					t.Errorf("extractTables(%q, %q)[%d] = %q, want %q", tt.sql, tt.db, i, g, tt.want[i])
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

func BenchmarkExtractTables(b *testing.B) {
	sql := "SELECT o.id, c.name, p.title FROM orders o JOIN customers c ON o.cid = c.id LEFT JOIN products p ON o.pid = p.id WHERE o.ts > '2024-01-01' AND c.country IN (SELECT country FROM regions)"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		extractTables(sql, "shop")
	}
}

func BenchmarkTokenize(b *testing.B) {
	sql := "SELECT o.id, c.name, p.title FROM orders o JOIN customers c ON o.cid = c.id LEFT JOIN products p ON o.pid = p.id WHERE o.ts > '2024-01-01' AND c.country IN (SELECT country FROM regions)"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tokenize(sql)
	}
}
