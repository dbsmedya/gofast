package storage_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/dbsmedya/gofast/pkg/models"
	"github.com/dbsmedya/gofast/pkg/storage"
)

// binaryLiteralSQL mirrors what MySQL writes to the slow log when a query
// embeds a _binary'...' literal — here a Java serialized object passed into an
// IN clause. \xac\xed is the Java serialization magic number and 0xAC is an
// invalid UTF-8 start byte, so this string is not valid UTF-8.
const binaryLiteralSQL = "select id from orders where id in " +
	"(_binary'\xac\xed\x00\x05sr\x00\x17java.lang.StringBuilder')"

func newTestStore(t *testing.T) *storage.Storage {
	t.Helper()
	s, err := storage.NewStorage(filepath.Join(t.TempDir(), "t.duckdb"), false)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func entry(sampleSQL string) *models.SlowLogEntry {
	return &models.SlowLogEntry{
		FingerprintID: "0123456789abcdef",
		SanitizedSQL:  sampleSQL,
		SampleSQL:     sampleSQL,
		User:          "app",
		Host:          "10.0.0.1",
		DB:            "shop",
		TS:            time.Unix(0, 0).UTC(),
		CreatedAt:     time.Unix(0, 0).UTC(),
		Tables:        []string{"orders"},
		EventHash:     "fedcba9876543210",
	}
}

// TestInsertSlowLogBatchAcceptsInvalidUTF8 is the regression proof for the
// import-aborting bug: DuckDB VARCHAR rejects invalid UTF-8 at appender flush,
// so a single _binary literal used to fail the whole batch.
func TestInsertSlowLogBatchAcceptsInvalidUTF8(t *testing.T) {
	s := newTestStore(t)
	err := s.InsertSlowLogBatch(context.Background(),
		[]*models.SlowLogEntry{entry(binaryLiteralSQL)}, "abc123")
	if err != nil {
		t.Fatalf("InsertSlowLogBatch with invalid UTF-8: %v", err)
	}
}

// TestInsertSlowLogBatchPreservesSiblingRows pins the blast radius: the DuckDB
// appender invalidates the whole batch on a bad flush, so one non-UTF-8 row used
// to take every good row in the same batch (up to batch_size, default 1000)
// down with it.
func TestInsertSlowLogBatchPreservesSiblingRows(t *testing.T) {
	s := newTestStore(t)
	batch := []*models.SlowLogEntry{
		entry("select 1"),
		entry(binaryLiteralSQL),
		entry("select 2"),
	}
	if err := s.InsertSlowLogBatch(context.Background(), batch, "abc123"); err != nil {
		t.Fatalf("InsertSlowLogBatch: %v", err)
	}
	res, err := s.FilterRecords(context.Background(), storage.RecordFilter{}, "id", "asc", 100, 0)
	if err != nil {
		t.Fatalf("FilterRecords: %v", err)
	}
	if res.Count != 3 {
		t.Fatalf("stored %d rows; want 3 (good rows must survive a poison sibling)", res.Count)
	}
}

// TestInsertSlowLogBatchPreservesValidMultibyte guards the fix itself: coercion
// must only touch invalid bytes. Legitimate multibyte UTF-8 (Turkish here) has
// to round-trip untouched — mangling it would be a silent data-corruption bug
// traded for the crash. This passes both before and after the fix by design.
func TestInsertSlowLogBatchPreservesValidMultibyte(t *testing.T) {
	s := newTestStore(t)
	const turkish = "select * from müşteri where şehir = 'İstanbul' and ürün = 'çiçek'"
	if err := s.InsertSlowLogBatch(context.Background(),
		[]*models.SlowLogEntry{entry(turkish)}, "abc123"); err != nil {
		t.Fatalf("InsertSlowLogBatch: %v", err)
	}
	res, err := s.FilterRecords(context.Background(), storage.RecordFilter{}, "id", "asc", 100, 0)
	if err != nil {
		t.Fatalf("FilterRecords: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("got %d rows; want 1", len(res.Rows))
	}
	got, ok := res.Rows[0][3].(string) // sample_sql_preview
	if !ok {
		t.Fatalf("sample_sql_preview is %T; want string", res.Rows[0][3])
	}
	if got != turkish {
		t.Fatalf("valid UTF-8 was altered:\n got: %q\nwant: %q", got, turkish)
	}
}
