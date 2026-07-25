package digest

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/dbsmedya/gofast/pkg/models"
	"github.com/dbsmedya/gofast/pkg/storage"
)

// seedStore writes entries into a fresh temp DuckDB and returns a read-only
// handle.
//
// CRITICAL: every entry must carry a DISTINCT EventOffset. FinalizeIngestion
// runs DedupeSlowLogs, which deletes rows sharing (file_hash, event_offset)
// (pkg/storage/duckdb.go:214). Leaving the default 0 on several entries
// silently collapses them to one row and makes every expected value wrong.
func seedStore(t *testing.T, entries []*models.SlowLogEntry) *storage.Storage {
	t.Helper()

	seen := map[int64]bool{}
	for i, e := range entries {
		if seen[e.EventOffset] {
			t.Fatalf("entry %d reuses EventOffset %d; offsets must be distinct or dedupe drops rows",
				i, e.EventOffset)
		}
		seen[e.EventOffset] = true
	}

	path := filepath.Join(t.TempDir(), "digest.duckdb")
	rw, err := storage.NewStorage(path, false)
	if err != nil {
		t.Fatalf("open rw: %v", err)
	}
	if err := rw.DropSlowLogIndexes(); err != nil {
		t.Fatalf("drop indexes: %v", err)
	}
	if err := rw.InsertSlowLogBatch(context.Background(), entries, "testfile"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := rw.FinalizeIngestion(context.Background()); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if err := rw.Close(); err != nil {
		t.Fatalf("close rw: %v", err)
	}

	ro, err := storage.NewStorage(path, true)
	if err != nil {
		t.Fatalf("open ro: %v", err)
	}
	t.Cleanup(func() { _ = ro.Close() })
	return ro
}

// fixtureEntries is the canonical three-row fixture. Its expected aggregates are
// hand-computed in run_test.go:
//
//	AAAA: query_time 1.0 and 3.0  -> sum 4.0, min 1.0, max 3.0, avg 2.0,
//	                                 median 2.0, p95 2.9, stddev sqrt(2),
//	                                 var 2.0, V/M = 2.0/2.0 = 1.0
//	BBBB: query_time 5.0          -> sum 5.0, stddev/var NULL -> 0, V/M 0
//	totals: calls 3, exec 9.0, lock 0.9, rows_sent 40, rows_examined 900
//
// AAAA's representative row is its slowest (the 3.0s one), so Item is
// "SELECT orders customers" and sample_sql is "SELECT * FROM orders WHERE id = 2".
func fixtureEntries(now time.Time) []*models.SlowLogEntry {
	return []*models.SlowLogEntry{
		{
			FingerprintID: "AAAA", SanitizedSQL: "SELECT * FROM orders WHERE id = ?",
			SampleSQL: "SELECT * FROM orders WHERE id = 1",
			User:      "app", Host: "h1", DB: "shop",
			QueryTimeSec: 1.0, LockTimeSec: 0.1, RowsSent: 10, RowsExamined: 100,
			TS: now, Tables: []string{"orders"}, EventOffset: 1, EventHash: "e1",
		},
		{
			FingerprintID: "AAAA", SanitizedSQL: "SELECT * FROM orders WHERE id = ?",
			SampleSQL: "SELECT * FROM orders WHERE id = 2",
			User:      "app", Host: "h1", DB: "shop",
			QueryTimeSec: 3.0, LockTimeSec: 0.3, RowsSent: 30, RowsExamined: 300,
			TS: now, Tables: []string{"orders", "customers"}, EventOffset: 2, EventHash: "e2",
		},
		{
			FingerprintID: "BBBB", SanitizedSQL: "UPDATE inventory SET n = ?",
			SampleSQL: "UPDATE inventory SET n = 5",
			User:      "batch", Host: "h2", DB: "",
			QueryTimeSec: 5.0, LockTimeSec: 0.5, RowsSent: 0, RowsExamined: 500,
			TS: now, Tables: []string{"inventory"}, EventOffset: 3, EventHash: "e3",
		},
	}
}

// approx compares floats with a tolerance, since quantile_cont interpolates
// (p95 over {1.0, 3.0} is 2.8999999999999995, not exactly 2.9).
func approx(t *testing.T, label string, got, want float64) {
	t.Helper()
	const tol = 1e-9
	if diff := got - want; diff > tol || diff < -tol {
		t.Errorf("%s = %v, want %v", label, got, want)
	}
}
