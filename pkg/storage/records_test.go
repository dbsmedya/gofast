package storage_test

// package storage_test (external test package): records_test.go imports
// contracttest for BuildFixtureDB, and contracttest imports storage — so this
// file must live outside package storage to avoid an import cycle.

import (
	"context"
	"testing"

	"github.com/dbsmedya/gofast/internal/contracttest"
	"github.com/dbsmedya/gofast/pkg/storage"
)

// TestFilterRecordsBindsUsers is THE SQLi proof: a classic quote-breakout
// payload in the Users filter must be bound as a literal value (matching
// nothing), never spliced into the SQL text.
func TestFilterRecordsBindsUsers(t *testing.T) {
	s := contracttest.BuildFixtureDB(t).Store
	f := storage.RecordFilter{Users: []string{"x') OR '1'='1"}}
	res, err := s.FilterRecords(context.Background(), f, "id", "asc", 100, 0)
	if err != nil {
		t.Fatalf("FilterRecords: %v", err)
	}
	if res.Count != 0 {
		t.Fatalf("injection returned %d rows; want 0", res.Count)
	}
}

// TestFilterRecordsTableInjection mirrors the Users probe for the Tables
// filter, which uses a different (list_contains) shape in BuildWhere.
func TestFilterRecordsTableInjection(t *testing.T) {
	s := contracttest.BuildFixtureDB(t).Store
	f := storage.RecordFilter{Tables: []string{"orders') OR ('1'='1"}}
	res, err := s.FilterRecords(context.Background(), f, "id", "asc", 100, 0)
	if err != nil {
		t.Fatalf("FilterRecords: %v", err)
	}
	if res.Count != 0 {
		t.Fatalf("table injection returned %d rows; want 0", res.Count)
	}
}

// TestFilterRecordsMatchesLegitimateFilter sanity-checks that a real filter
// value (not an injection) still matches rows, i.e. BuildWhere isn't
// over-escaping legitimate input.
func TestFilterRecordsMatchesLegitimateFilter(t *testing.T) {
	s := contracttest.BuildFixtureDB(t).Store
	f := storage.RecordFilter{Users: []string{"app"}}
	res, err := s.FilterRecords(context.Background(), f, "id", "asc", 100, 0)
	if err != nil {
		t.Fatalf("FilterRecords: %v", err)
	}
	if res.Count == 0 {
		t.Fatalf("legitimate filter matched 0 rows; want > 0")
	}
	for _, row := range res.Rows {
		// user is the 5th selected column (id, fingerprint_id, sanitized_sql_preview,
		// sample_sql_preview, user, ...) — see FilterRecords' SELECT list.
		if u, ok := row[4].(string); !ok || u != "app" {
			t.Fatalf("row user = %v, want %q", row[4], "app")
		}
	}
}

func TestGetRecordByIDNoInjection(t *testing.T) {
	s := contracttest.BuildFixtureDB(t).Store
	res, err := s.GetRecordByID(context.Background(), 1)
	if err != nil || res.Count != 1 {
		t.Fatalf("GetRecordByID(1): count=%d err=%v", res.Count, err)
	}
}

// TestGetRecordByIDAbsent proves a huge/absent id returns Count==0 (not an
// error, not another row). id is always a bound int64, so there is no id
// value that can escape the WHERE clause.
func TestGetRecordByIDAbsent(t *testing.T) {
	s := contracttest.BuildFixtureDB(t).Store
	res, err := s.GetRecordByID(context.Background(), 9_000_000_000)
	if err != nil {
		t.Fatalf("GetRecordByID(huge): %v", err)
	}
	if res.Count != 0 {
		t.Fatalf("GetRecordByID(huge) count = %d, want 0", res.Count)
	}
}

func TestListRecords(t *testing.T) {
	s := contracttest.BuildFixtureDB(t).Store
	ctx := context.Background()

	res, err := s.ListRecords(ctx, "id", "asc", 100, 0)
	if err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	if res.Count == 0 {
		t.Fatalf("ListRecords returned 0 rows; fixture should have rows")
	}

	// Limit is respected.
	limited, err := s.ListRecords(ctx, "id", "asc", 1, 0)
	if err != nil {
		t.Fatalf("ListRecords limited: %v", err)
	}
	if limited.Count != 1 {
		t.Fatalf("ListRecords limit=1 returned %d rows, want 1", limited.Count)
	}

	// limit/offset are plain Go `int` parameters — there is no string to
	// inject through them; the type system rules it out before the SQL layer
	// ever sees a value (they are also bound as `?` args, not interpolated).

	// A non-allow-listed orderBy falls back to "id" — no error, no injection.
	fallback, err := s.ListRecords(ctx, "id; DROP TABLE slow_logs; --", "asc", 100, 0)
	if err != nil {
		t.Fatalf("ListRecords with malicious orderBy: %v", err)
	}
	if fallback.Count != res.Count {
		t.Fatalf("fallback orderBy count = %d, want %d (same as valid 'id' order)", fallback.Count, res.Count)
	}
}

func TestFilterOptions(t *testing.T) {
	s := contracttest.BuildFixtureDB(t).Store
	users, dbs, hosts, tables, sourceHosts, err := s.FilterOptions(context.Background())
	if err != nil {
		t.Fatalf("FilterOptions: %v", err)
	}

	assertContains(t, "users", users, "app", "reporting", "etl")
	assertContains(t, "dbs", dbs, "shop", "analytics")
	if len(dbs) != 2 {
		t.Fatalf("dbs = %v, want exactly 2 (shop, analytics)", dbs)
	}
	assertContains(t, "hosts", hosts, "web1", "web2", "batch1")
	// Table names come out db-qualified because every fixture query is
	// preceded by a `use <db>;` that sets the session context (WriteFixtureLogs).
	assertContains(t, "tables", tables, "shop.orders", "shop.invoices", "analytics.events")
	if len(sourceHosts) == 0 {
		t.Fatalf("sourceHosts is empty; want derived hostnames from ingested filenames")
	}
}

func assertContains(t *testing.T, label string, got []string, want ...string) {
	t.Helper()
	set := make(map[string]bool, len(got))
	for _, g := range got {
		set[g] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("%s = %v, missing %q", label, got, w)
		}
	}
}
