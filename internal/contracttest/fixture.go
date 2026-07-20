package contracttest

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dbsmedya/gofast/pkg/config"
	"github.com/dbsmedya/gofast/pkg/parser"
	"github.com/dbsmedya/gofast/pkg/storage"
)

// Fixture is a seeded, read-only DuckDB plus its file path.
type Fixture struct {
	Path  string           // for tests that open their own StorageManager
	Store *storage.Storage // read-only handle, closed via t.Cleanup
}

// BuildFixtureDB generates the fixture logs, parses them into a temp DuckDB,
// and returns a read-only handle. Deterministic (bar sliding timestamps).
func BuildFixtureDB(t testing.TB) Fixture {
	t.Helper()
	tmp := t.TempDir()
	logDir := filepath.Join(tmp, "logs")
	if err := WriteFixtureLogs(logDir); err != nil {
		t.Fatalf("write fixture logs: %v", err)
	}
	dbPath := filepath.Join(tmp, "fixture.duckdb")

	rw, err := storage.NewStorage(dbPath, false)
	if err != nil {
		t.Fatalf("open rw: %v", err)
	}
	if err := rw.DropSlowLogIndexes(); err != nil {
		t.Fatalf("drop indexes: %v", err)
	}
	eng := parser.NewEngine(rw, 100, config.DefaultFilePatterns(), 1)
	if _, err := eng.ParseDirectory(context.Background(), logDir); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if err := rw.FinalizeIngestion(context.Background()); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if err := rw.Close(); err != nil {
		t.Fatalf("close rw: %v", err)
	}

	ro, err := storage.NewStorage(dbPath, true)
	if err != nil {
		t.Fatalf("open ro: %v", err)
	}
	t.Cleanup(func() { _ = ro.Close() })
	return Fixture{Path: dbPath, Store: ro}
}
