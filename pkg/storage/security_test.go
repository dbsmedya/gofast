package storage

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// isDisabledErr reports whether err is DuckDB's "filesystem/network access is
// disabled by configuration" permission error — the signal that the hardening
// applied in NewStorage (duckdbSecurityOpts) rejected the operation.
func isDisabledErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "disabled by configuration") ||
		strings.Contains(msg, "Permission Error")
}

// TestSecurityBlocksExternalFileAccess is the core regression guard: even with a
// read-WRITE store, SQL cannot read local files (read_csv), write them
// (COPY ... TO), attach other databases, or install extensions. access_mode=
// read_only does NOT close this path, so the check must hold independent of it.
func TestSecurityBlocksExternalFileAccess(t *testing.T) {
	s, err := NewStorage("", false) // in-memory, read-write
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	cases := []struct {
		name string
		sql  string
	}{
		{"read_csv", `SELECT * FROM read_csv('/etc/passwd')`},
		{"copy_to", `COPY (SELECT 1 AS x) TO '/tmp/gofast_exfil_test.csv'`},
		{"attach", `ATTACH '/tmp/gofast_attack.duckdb' AS other`},
		{"install_httpfs", `INSTALL httpfs`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.db.ExecContext(ctx, tc.sql); !isDisabledErr(err) {
				t.Fatalf("%s: want a 'disabled by configuration' permission error, got: %v", tc.name, err)
			}
		})
	}
}

// TestSecurityConfigurationLocked verifies lock_configuration=true is in force:
// SQL cannot re-enable external access to undo the sandbox.
func TestSecurityConfigurationLocked(t *testing.T) {
	s, err := NewStorage("", false)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	defer s.Close()

	_, err = s.db.ExecContext(context.Background(), "SET enable_external_access = true")
	if err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("want a 'configuration has been locked' error, got: %v", err)
	}
}

// TestSecurityReadOnlyFileDBBlocksFileAccess mirrors the real stats/query/serve
// path: a file-backed database created read-write (schema init must still
// succeed under the hardening), then reopened read-only — where read_csv must
// still be blocked.
func TestSecurityReadOnlyFileDBBlocksFileAccess(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.duckdb")

	rw, err := NewStorage(dbPath, false) // creates file + schema under hardening
	if err != nil {
		t.Fatalf("NewStorage (read-write): %v", err)
	}
	if err := rw.Close(); err != nil {
		t.Fatalf("Close (read-write): %v", err)
	}

	ro, err := NewStorage(dbPath, true) // the read-only consumer path
	if err != nil {
		t.Fatalf("NewStorage (read-only): %v", err)
	}
	defer ro.Close()

	if _, err := ro.db.ExecContext(context.Background(), `SELECT * FROM read_csv('/etc/passwd')`); !isDisabledErr(err) {
		t.Fatalf("read-only read_csv: want a 'disabled by configuration' error, got: %v", err)
	}
}

// TestSecurityAllowsNormalQueries confirms the hardening does not disturb
// ordinary in-engine SQL (no filesystem/network needed).
func TestSecurityAllowsNormalQueries(t *testing.T) {
	s, err := NewStorage("", false)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	defer s.Close()

	var n int
	if err := s.db.QueryRowContext(context.Background(), "SELECT 21 + 21").Scan(&n); err != nil {
		t.Fatalf("normal query failed: %v", err)
	}
	if n != 42 {
		t.Fatalf("SELECT 21 + 21 = %d, want 42", n)
	}
}
