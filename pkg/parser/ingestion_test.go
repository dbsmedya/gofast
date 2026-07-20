package parser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbsmedya/gofast/pkg/storage"
)

// minimalSlowLog is a valid MySQL slow log with two complete entries.
const minimalSlowLog = `# Time: 2024-01-15T10:00:00.000000Z
# User@Host: root[root] @ localhost []  Id: 1
# Query_time: 1.000000  Lock_time: 0.000000  Rows_sent: 1  Rows_examined: 1
use testdb;
SELECT 1;
# Time: 2024-01-15T10:00:01.000000Z
# User@Host: root[root] @ localhost []  Id: 1
# Query_time: 2.000000  Lock_time: 0.000000  Rows_sent: 2  Rows_examined: 2
SELECT 2 FROM users;
`

const thirdEntry = `# Time: 2024-01-15T10:00:02.000000Z
# User@Host: root[root] @ localhost []  Id: 1
# Query_time: 3.000000  Lock_time: 0.000000  Rows_sent: 3  Rows_examined: 3
SELECT 3 FROM orders;
`

func newTestEngine(t *testing.T) (*Engine, *storage.Storage, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.duckdb")
	store, err := storage.NewStorage(dbPath, false)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	eng := NewEngine(store, 100, nil, 1)
	return eng, store, dir
}

func writeSlowLog(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func countRows(t *testing.T, store *storage.Storage) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM slow_logs`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestParseThenSkip(t *testing.T) {
	eng, store, dir := newTestEngine(t)
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSlowLog(t, logDir, "mysql-slow.log", minimalSlowLog)
	ctx := context.Background()

	r1, err := eng.ParseDirectory(ctx, logDir)
	if err != nil {
		t.Fatalf("first parse: %v", err)
	}
	if r1.FilesProcessed != 1 || r1.FilesSkipped != 0 {
		t.Fatalf("first: processed=%d skipped=%d failed=%d err=%v", r1.FilesProcessed, r1.FilesSkipped, r1.FilesFailed, r1.Errors)
	}
	if r1.EntriesStored < 1 {
		t.Fatalf("expected entries stored, got %d", r1.EntriesStored)
	}
	n1 := countRows(t, store)

	// Second run over unchanged file → skip
	r2, err := eng.ParseDirectory(ctx, logDir)
	if err != nil {
		t.Fatalf("second parse: %v", err)
	}
	if r2.FilesSkipped != 1 || r2.FilesProcessed != 0 {
		t.Fatalf("second: processed=%d skipped=%d failed=%d errors=%v", r2.FilesProcessed, r2.FilesSkipped, r2.FilesFailed, r2.Errors)
	}
	if countRows(t, store) != n1 {
		t.Fatalf("row count changed after skip: %d → %d", n1, countRows(t, store))
	}

	// Third run still skip [R2-1]
	r3, err := eng.ParseDirectory(ctx, logDir)
	if err != nil {
		t.Fatalf("third parse: %v", err)
	}
	if r3.FilesSkipped != 1 {
		t.Fatalf("third: want files_skipped=1, got %d (processed=%d)", r3.FilesSkipped, r3.FilesProcessed)
	}
}

func TestParseAppendIncremental(t *testing.T) {
	eng, store, dir := newTestEngine(t)
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := writeSlowLog(t, logDir, "app-slow.log", minimalSlowLog)
	ctx := context.Background()

	if _, err := eng.ParseDirectory(ctx, logDir); err != nil {
		t.Fatalf("first: %v", err)
	}
	n1 := countRows(t, store)
	if n1 < 1 {
		t.Fatalf("expected rows after first parse")
	}

	// Append a third entry
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(thirdEntry); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	r2, err := eng.ParseDirectory(ctx, logDir)
	if err != nil {
		t.Fatalf("incremental: %v", err)
	}
	if r2.FilesProcessed != 1 {
		t.Fatalf("incremental want processed=1, got processed=%d skipped=%d failed=%d err=%v",
			r2.FilesProcessed, r2.FilesSkipped, r2.FilesFailed, r2.Errors)
	}
	n2 := countRows(t, store)
	if n2 <= n1 {
		t.Fatalf("expected more rows after append: %d → %d", n1, n2)
	}

	if err := store.FinalizeIngestion(ctx); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	// No duplicate (file_hash, event_offset)
	var dups int
	err = store.DB().QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT file_hash, event_offset, COUNT(*) c
			FROM slow_logs WHERE event_offset IS NOT NULL
			GROUP BY 1, 2 HAVING COUNT(*) > 1
		)`).Scan(&dups)
	if err != nil {
		t.Fatalf("dup query: %v", err)
	}
	if dups != 0 {
		t.Fatalf("found %d duplicate (file_hash, event_offset) groups", dups)
	}
}

func TestEmptyFileSkip(t *testing.T) {
	eng, _, dir := newTestEngine(t)
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSlowLog(t, logDir, "empty-slow.log", "")
	ctx := context.Background()

	r1, err := eng.ParseDirectory(ctx, logDir)
	if err != nil {
		t.Fatalf("first empty: %v", err)
	}
	if r1.FilesProcessed != 1 {
		t.Fatalf("empty first: processed=%d failed=%d err=%v", r1.FilesProcessed, r1.FilesFailed, r1.Errors)
	}

	r2, err := eng.ParseDirectory(ctx, logDir)
	if err != nil {
		t.Fatalf("second empty: %v", err)
	}
	if r2.FilesSkipped != 1 {
		t.Fatalf("empty second: want skip, got processed=%d skipped=%d failed=%d", r2.FilesProcessed, r2.FilesSkipped, r2.FilesFailed)
	}
}

func TestDedupeNullSafety(t *testing.T) {
	_, store, _ := newTestEngine(t)
	ctx := context.Background()
	// Insert a row with NULL event_offset
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO slow_logs (file_hash, fingerprint_id, sanitized_sql, sample_sql, ts)
		VALUES ('abcdabcdabcdabcd', 'fp', 'select ?', 'select 1', NOW())
	`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := store.DedupeSlowLogs(ctx); err != nil {
		t.Fatalf("dedupe: %v", err)
	}
	var n int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM slow_logs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("NULL-offset row should survive dedupe, got count=%d", n)
	}
}

func TestDedupeKeepsMaxID(t *testing.T) {
	_, store, _ := newTestEngine(t)
	ctx := context.Background()
	// Two rows same (file_hash, event_offset); later id should win
	for i := 0; i < 2; i++ {
		_, err := store.DB().ExecContext(ctx, `
			INSERT INTO slow_logs (file_hash, fingerprint_id, sanitized_sql, sample_sql, ts, event_offset, event_hash)
			VALUES ('abcdabcdabcdabcd', 'fp', 'select ?', ?, NOW(), 100, ?)
		`, "select "+string(rune('a'+i)), "hash"+string(rune('a'+i)))
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := store.DedupeSlowLogs(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	var sample string
	if err := store.DB().QueryRow(`SELECT COUNT(*), MAX(sample_sql) FROM slow_logs WHERE event_offset = 100`).Scan(&n, &sample); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want 1 survivor, got %d", n)
	}
	if sample != "select b" {
		t.Fatalf("want MAX(id) survivor sample 'select b', got %q", sample)
	}
}

func TestRotationKeepsBothGenerations(t *testing.T) {
	eng, store, dir := newTestEngine(t)
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := writeSlowLog(t, logDir, "rot-slow.log", minimalSlowLog)
	ctx := context.Background()

	if _, err := eng.ParseDirectory(ctx, logDir); err != nil {
		t.Fatalf("first: %v", err)
	}
	n1 := countRows(t, store)

	// Replace content with same size-ish but different prefix (new generation)
	// Write completely different content
	alt := strings.ReplaceAll(minimalSlowLog, "SELECT 1", "SELECT 99")
	if err := os.WriteFile(path, []byte(alt), 0o644); err != nil {
		t.Fatal(err)
	}
	// Bump mtime
	_ = os.Chtimes(path, time.Now(), time.Now())

	if _, err := eng.ParseDirectory(ctx, logDir); err != nil {
		t.Fatalf("after replace: %v", err)
	}
	var gens int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM files`).Scan(&gens); err != nil {
		t.Fatal(err)
	}
	if gens < 2 {
		t.Fatalf("want >=2 generations after replace, got %d", gens)
	}
	n2 := countRows(t, store)
	if n2 <= n1 {
		t.Fatalf("expected more rows across generations: %d → %d", n1, n2)
	}
}

func TestIdentityHashStable(t *testing.T) {
	h1 := identityHash("/tmp/a", 1)
	h2 := identityHash("/tmp/a", 1)
	h3 := identityHash("/tmp/a", 2)
	if len(h1) != 16 {
		t.Fatalf("want 16 hex chars, got %d %q", len(h1), h1)
	}
	if h1 != h2 {
		t.Fatal("not stable")
	}
	if h1 == h3 {
		t.Fatal("generation must change identity")
	}
}

func TestCanonicalPathFallback(t *testing.T) {
	// Non-existent path under temp still Abs's
	p, err := CanonicalPath(filepath.Join(t.TempDir(), "nope-slow.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(p) {
		t.Fatalf("want abs path, got %q", p)
	}
}

func TestWorkersMatchSequential(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Several files so the pool has work to do
	for i := 0; i < 4; i++ {
		writeSlowLog(t, logDir, fmt.Sprintf("host%d-slow.log", i), minimalSlowLog)
	}

	run := func(workers int) int {
		dbPath := filepath.Join(dir, fmt.Sprintf("w%d.duckdb", workers))
		store, err := storage.NewStorage(dbPath, false)
		if err != nil {
			t.Fatalf("storage: %v", err)
		}
		defer store.Close()
		eng := NewEngine(store, 50, nil, workers)
		r, err := eng.ParseDirectory(context.Background(), logDir)
		if err != nil {
			t.Fatalf("workers=%d: %v", workers, err)
		}
		if r.FilesFailed > 0 {
			t.Fatalf("workers=%d failures: %v", workers, r.Errors)
		}
		if err := store.FinalizeIngestion(context.Background()); err != nil {
			t.Fatalf("finalize: %v", err)
		}
		return countRows(t, store)
	}

	n1 := run(1)
	n4 := run(4)
	if n1 != n4 {
		t.Fatalf("EntriesStored mismatch workers=1 (%d) vs workers=4 (%d)", n1, n4)
	}
	if n1 == 0 {
		t.Fatal("expected stored rows")
	}
}

// TestCanonicalSymlinkDedupeNoDeadlock ensures a matched symlink + matched target
// for the same file cannot double-register identityHash and hang the pool.
func TestCanonicalSymlinkDedupeNoDeadlock(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := writeSlowLog(t, logDir, "mysql-slow.log.1", minimalSlowLog)
	link := filepath.Join(logDir, "mysql-slow.log")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	store, err := storage.NewStorage(filepath.Join(dir, "t.duckdb"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	eng := NewEngine(store, 50, nil, 4)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	done := make(chan struct{})
	var result *ParseResult
	var parseErr error
	go func() {
		defer close(done)
		result, parseErr = eng.ParseDirectory(ctx, logDir)
	}()

	select {
	case <-done:
		// ok
	case <-ctx.Done():
		t.Fatal("ParseDirectory hung (likely identityHash double-registration deadlock)")
	}
	if parseErr != nil {
		t.Fatalf("parse: %v", parseErr)
	}
	// Exactly one logical file ingested (symlink + target collapsed)
	if result.FilesProcessed != 1 {
		t.Fatalf("want files_processed=1 after symlink dedupe, got processed=%d skipped=%d failed=%d",
			result.FilesProcessed, result.FilesSkipped, result.FilesFailed)
	}
	if countRows(t, store) < 1 {
		t.Fatal("expected rows stored")
	}
}

// TestWriterFailureAcks ensures injected commit failure negatively acks registered
// files with the real error (no hang, error surfaces in Errors).
func TestWriterFailureAcks(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		writeSlowLog(t, logDir, fmt.Sprintf("f%d-slow.log", i), minimalSlowLog)
	}
	store, err := storage.NewStorage(filepath.Join(dir, "t.duckdb"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	eng := NewEngine(store, 10, nil, 4)
	eng.testCommitErr = fmt.Errorf("injected writer failure: disk full")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := eng.ParseDirectory(ctx, logDir)
	// Writer failure should surface (return err and/or FilesFailed)
	if err == nil && result.FilesFailed == 0 {
		t.Fatal("expected failure from injected commit error")
	}
	found := false
	for _, msg := range result.Errors {
		if strings.Contains(msg, "injected writer failure") {
			found = true
			break
		}
	}
	if err != nil && strings.Contains(err.Error(), "injected writer failure") {
		found = true
	}
	if !found {
		t.Fatalf("real failure reason missing from result; err=%v errors=%v", err, result.Errors)
	}
}

// TestCancelMidRun ensures cancellation does not deadlock and returns ctx.Err().
func TestCancelMidRun(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Larger batch of files so cancel can fire mid-run
	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString(minimalSlowLog)
	}
	big := b.String()
	for i := 0; i < 8; i++ {
		writeSlowLog(t, logDir, fmt.Sprintf("big%d-slow.log", i), big)
	}
	store, err := storage.NewStorage(filepath.Join(dir, "t.duckdb"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	eng := NewEngine(store, 5, nil, 4)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel shortly after start
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()

	done := make(chan struct{})
	var parseErr error
	go func() {
		defer close(done)
		_, parseErr = eng.ParseDirectory(ctx, logDir)
	}()

	select {
	case <-done:
		// finished without hang
	case <-time.After(15 * time.Second):
		t.Fatal("cancelled ParseDirectory hung")
	}
	if parseErr == nil {
		// May complete before cancel on very fast machines — still must not hang
		t.Log("parse finished before cancel took effect (ok if no hang)")
		return
	}
	if parseErr != context.Canceled && !strings.Contains(parseErr.Error(), "canceled") {
		t.Fatalf("want canceled error, got %v", parseErr)
	}
}

// TestPerFileErrorInPool: one unreadable file fails; others still process.
func TestPerFileErrorInPool(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSlowLog(t, logDir, "good-a-slow.log", minimalSlowLog)
	writeSlowLog(t, logDir, "good-b-slow.log", minimalSlowLog)
	bad := filepath.Join(logDir, "bad-slow.log")
	if err := os.WriteFile(bad, []byte(minimalSlowLog), 0o000); err != nil {
		t.Fatal(err)
	}
	// Ensure unreadable (best-effort on platforms that honor mode)
	_ = os.Chmod(bad, 0o000)
	t.Cleanup(func() { _ = os.Chmod(bad, 0o644) })

	store, err := storage.NewStorage(filepath.Join(dir, "t.duckdb"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	eng := NewEngine(store, 50, nil, 4)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := eng.ParseDirectory(ctx, logDir)
	if err != nil {
		// directory-level hard fail unexpected if only one file is bad
		t.Logf("parse err (may be ok): %v", err)
	}
	if result.FilesProcessed < 1 {
		// On some CI environments root may still open 000 files — skip then
		if result.FilesFailed == 0 {
			t.Skip("could not create unreadable file on this platform")
		}
	}
	if result.FilesFailed < 1 && result.FilesProcessed < 2 {
		// Either failed the bad file or processed everything if open succeeded
		if _, openErr := os.Open(bad); openErr == nil {
			t.Skip("platform allows opening mode-000 files")
		}
		t.Fatalf("want at least one failure or both goods processed; got %+v", result)
	}
	if result.FilesProcessed >= 1 && countRows(t, store) < 1 {
		t.Fatal("good files should still store rows")
	}
}
