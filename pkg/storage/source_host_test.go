package storage

import (
	"context"
	"testing"
)

// TestSourceHostRegex verifies the basename + extension-stripping regex used by
// the slow_logs_with_source view produces the expected hostname from a file path.
func TestSourceHostRegex(t *testing.T) {
	s, err := NewStorage("", false) // in-memory DuckDB
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	defer s.Close()

	// Mirror EXACTLY the expression used in the view (createTables).
	const q = `
		SELECT path,
		       regexp_replace(
		         regexp_replace(path, '^.*[/\\]', ''),
		         '(\.(log|slow|gz))+$', '', 'i') AS host
		FROM (VALUES
		    ('/var/log/mysql/prod-db-01.slow.log'),
		    ('mysql-slow.log'),
		    ('/data/host.log.gz'),
		    ('plainhost'),
		    ('C:\\logs\\winhost.slow.log'),
		    ('/v/slow.log'),
		    ('/srv/db.prod.example.com.slow.log'),
		    ('/x/UPPER.LOG')
		) AS t(path)`

	want := map[string]string{
		"/var/log/mysql/prod-db-01.slow.log": "prod-db-01",
		"mysql-slow.log":                     "mysql-slow",
		"/data/host.log.gz":                  "host",
		"plainhost":                          "plainhost",
		`C:\logs\winhost.slow.log`:           "winhost",
		"/v/slow.log":                        "slow",
		"/srv/db.prod.example.com.slow.log":  "db.prod.example.com",
		"/x/UPPER.LOG":                       "UPPER",
	}

	rows, err := s.db.QueryContext(context.Background(), q)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	seen := 0
	for rows.Next() {
		var path, host string
		if err := rows.Scan(&path, &host); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen++
		if w, ok := want[path]; ok && host != w {
			t.Errorf("transform(%q) = %q, want %q", path, host, w)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if seen != len(want) {
		t.Errorf("got %d rows, want %d", seen, len(want))
	}
}

// TestSlowLogsWithSourceView verifies the view derives source_host per row via the
// files join, yields NULL when no file row matches, and supports distinct-list
// aggregation per fingerprint (the shape /sql/queries uses).
func TestSlowLogsWithSourceView(t *testing.T) {
	s, err := NewStorage("", false) // in-memory DuckDB (schema + view created)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	// Two ingested files on two different DB servers.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO files (hash, file_path, file_size) VALUES
		 ('h1', '/var/log/mysql/db-01.slow.log', 10),
		 ('h2', '/var/log/mysql/db-02.slow.log', 20)`); err != nil {
		t.Fatalf("insert files: %v", err)
	}

	// Same fingerprint 'fp1' seen on both servers; 'fp2' only on h1; 'fp3' has no
	// matching files row (orphan file_hash) -> source_host must be NULL.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO slow_logs (file_hash, fingerprint_id, sanitized_sql, sample_sql, ts) VALUES
		 ('h1', 'fp1', 'select ?', 'select 1', NOW()),
		 ('h2', 'fp1', 'select ?', 'select 2', NOW()),
		 ('h1', 'fp2', 'select ? from t', 'select 1 from t', NOW()),
		 ('hX', 'fp3', 'select ? from u', 'select 1 from u', NOW())`); err != nil {
		t.Fatalf("insert slow_logs: %v", err)
	}

	// Scalar source_host per row.
	var orphan interface{}
	if err := s.db.QueryRowContext(ctx,
		`SELECT source_host FROM slow_logs_with_source WHERE fingerprint_id = 'fp3'`).Scan(&orphan); err != nil {
		t.Fatalf("scan orphan: %v", err)
	}
	if orphan != nil {
		t.Errorf("orphan source_host = %v, want NULL", orphan)
	}

	var h1host string
	if err := s.db.QueryRowContext(ctx,
		`SELECT source_host FROM slow_logs_with_source WHERE fingerprint_id = 'fp2'`).Scan(&h1host); err != nil {
		t.Fatalf("scan fp2: %v", err)
	}
	if h1host != "db-01" {
		t.Errorf("fp2 source_host = %q, want %q", h1host, "db-01")
	}

	// Distinct-list aggregation for fp1 (the /sql/queries shape). DuckDB returns a
	// VARCHAR[]; the v2 driver surfaces it as []any of strings.
	rows, err := s.db.QueryContext(ctx, `
		SELECT fingerprint_id,
		       list_sort(list(DISTINCT source_host) FILTER (WHERE source_host IS NOT NULL AND source_host <> '')) AS source_hosts
		FROM slow_logs_with_source
		GROUP BY fingerprint_id
		ORDER BY fingerprint_id`)
	if err != nil {
		t.Fatalf("aggregate query: %v", err)
	}
	defer rows.Close()

	got := map[string][]string{}
	for rows.Next() {
		var fp string
		var hosts interface{}
		if err := rows.Scan(&fp, &hosts); err != nil {
			t.Fatalf("scan agg: %v", err)
		}
		var list []string
		if arr, ok := hosts.([]any); ok {
			for _, v := range arr {
				if str, ok := v.(string); ok {
					list = append(list, str)
				}
			}
		}
		got[fp] = list
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	if want := []string{"db-01", "db-02"}; !equalStrs(got["fp1"], want) {
		t.Errorf("fp1 source_hosts = %v, want %v", got["fp1"], want)
	}
	if want := []string{"db-01"}; !equalStrs(got["fp2"], want) {
		t.Errorf("fp2 source_hosts = %v, want %v", got["fp2"], want)
	}
	if len(got["fp3"]) != 0 {
		t.Errorf("fp3 source_hosts = %v, want empty", got["fp3"])
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
