package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbsmedya/gofast/internal/digest"
	"github.com/dbsmedya/gofast/pkg/config"
	"github.com/dbsmedya/gofast/pkg/models"
	"github.com/dbsmedya/gofast/pkg/storage"
)

// seedCLIStore builds a small DuckDB file and returns its path.
//
// Distinct EventOffsets are mandatory: FinalizeIngestion dedupes on
// (file_hash, event_offset), so equal offsets collapse the rows into one.
func seedCLIStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cli.duckdb")
	rw, err := storage.NewStorage(path, false)
	if err != nil {
		t.Fatalf("open rw: %v", err)
	}
	if err := rw.DropSlowLogIndexes(); err != nil {
		t.Fatalf("drop indexes: %v", err)
	}
	now := time.Now()
	entries := []*models.SlowLogEntry{
		{FingerprintID: "AAAA", SanitizedSQL: "SELECT * FROM orders WHERE id = ?",
			SampleSQL: "SELECT * FROM orders WHERE id = 1", User: "app", Host: "h1", DB: "shop",
			QueryTimeSec: 1, LockTimeSec: 0.1, RowsSent: 10, RowsExamined: 100,
			TS: now, Tables: []string{"orders"}, EventOffset: 1, EventHash: "e1"},
		{FingerprintID: "BBBB", SanitizedSQL: "UPDATE inventory SET n = ?",
			SampleSQL: "UPDATE inventory SET n = 5", User: "batch", Host: "h2", DB: "ops",
			QueryTimeSec: 5, LockTimeSec: 0.5, RowsSent: 0, RowsExamined: 500,
			TS: now, Tables: []string{"inventory"}, EventOffset: 2, EventHash: "e2"},
	}
	if err := rw.InsertSlowLogBatch(context.Background(), entries, "testfile"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := rw.FinalizeIngestion(context.Background()); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if err := rw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return path
}

func testConfig(path string) *config.Config {
	cfg := &config.Config{}
	cfg.DuckDB.Path = path
	return cfg
}

func TestRunDigestJSONIsPureJSON(t *testing.T) {
	cfg := testConfig(seedCLIStore(t))
	var buf bytes.Buffer
	if err := runDigest(cfg, digestDefaults(), true, &buf); err != nil {
		t.Fatalf("runDigest: %v", err)
	}

	// The whole of stdout must parse; any banner or preamble breaks `| jq .`.
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not pure JSON: %v\n%s", err, buf.String())
	}
	for _, key := range []string{"group_by", "order_by", "limit", "total", "classes"} {
		if _, ok := got[key]; !ok {
			t.Errorf("JSON missing %q: %s", key, buf.String())
		}
	}
}

func TestRunDigestJSONEmptyResultStillParses(t *testing.T) {
	cfg := testConfig(seedCLIStore(t))
	o := digestDefaults()
	f, err := digestFilterFor("db = nonexistent")
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	o.Filter = f

	var buf bytes.Buffer
	if err := runDigest(cfg, o, true, &buf); err != nil {
		t.Fatalf("runDigest: %v", err)
	}
	var got struct {
		Classes []any `json:"classes"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("empty result is not valid JSON: %v\n%s", err, buf.String())
	}
	if len(got.Classes) != 0 {
		t.Errorf("classes = %d, want 0", len(got.Classes))
	}
	if !strings.Contains(buf.String(), `"classes": []`) &&
		!strings.Contains(buf.String(), `"classes":[]`) {
		t.Errorf("classes must marshal as [], not null: %s", buf.String())
	}
}

func TestRunDigestMarkdownDefault(t *testing.T) {
	cfg := testConfig(seedCLIStore(t))
	var buf bytes.Buffer
	if err := runDigest(cfg, digestDefaults(), false, &buf); err != nil {
		t.Fatalf("runDigest: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "## Profile") {
		t.Errorf("expected a markdown profile table:\n%s", out)
	}
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("markdown mode must not emit JSON:\n%s", out)
	}
}

func TestRunDigestMissingStoreErrorsWithoutPartialOutput(t *testing.T) {
	cfg := testConfig(filepath.Join(t.TempDir(), "does-not-exist.duckdb"))
	var buf bytes.Buffer
	err := runDigest(cfg, digestDefaults(), true, &buf)
	if err == nil {
		t.Fatal("expected an error for a missing store")
	}
	if buf.Len() != 0 {
		t.Errorf("no output may be written on failure, got: %s", buf.String())
	}
}

func TestBuildDigestOptionsWiresEveryFlag(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.Local)
	f := digestFlags{
		groupBy: "user",
		orderBy: "Lock_time:max",
		limit:   5,
		filters: []string{"db = shop", "query_time > 2"},
		since:   "7d",
		until:   "2026-07-25",
	}
	o, err := buildDigestOptions(f, now)
	if err != nil {
		t.Fatalf("buildDigestOptions: %v", err)
	}
	if string(o.GroupBy) != "user" {
		t.Errorf("GroupBy = %q", o.GroupBy)
	}
	if o.OrderBy.String() != "Lock_time:max" {
		t.Errorf("OrderBy = %q", o.OrderBy.String())
	}
	if o.Limit != 5 {
		t.Errorf("Limit = %d", o.Limit)
	}
	if len(o.Filter.DBs) != 1 || o.Filter.QueryTimeGT == nil {
		t.Errorf("filters not applied: %+v", o.Filter)
	}
	if o.Filter.TSFrom == nil || o.Filter.TSTo == nil {
		t.Errorf("time bounds not applied: %+v", o.Filter)
	}
}

func TestBuildDigestOptionsSurfacesParseErrors(t *testing.T) {
	now := time.Now()
	cases := []digestFlags{
		{groupBy: "bogus", orderBy: "Query_time:sum", limit: 20},
		{groupBy: "fingerprint", orderBy: "nope", limit: 20},
		{groupBy: "fingerprint", orderBy: "Query_time:sum", limit: 0},
		{groupBy: "fingerprint", orderBy: "Query_time:sum", limit: 20, filters: []string{"bad"}},
		{groupBy: "fingerprint", orderBy: "Query_time:sum", limit: 20, since: "not-a-time"},
	}
	for i, f := range cases {
		if _, err := buildDigestOptions(f, now); err == nil {
			t.Errorf("case %d (%+v): expected an error", i, f)
		}
	}
}

func TestDigestCmdIsRegisteredWithSilencedUsage(t *testing.T) {
	cmd := digestCmd()
	if cmd.Use != "digest" {
		t.Errorf("Use = %q, want digest", cmd.Use)
	}
	// Usage on stdout would corrupt `--json | jq .`.
	if !cmd.SilenceUsage {
		t.Error("SilenceUsage must be true so errors never print usage to stdout")
	}
	for _, name := range []string{"group-by", "order-by", "limit", "filter", "since", "until", "json"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("missing flag --%s", name)
		}
	}
}

func TestRootCmdRegistersDigest(t *testing.T) {
	// Registration itself must be tested: a command that is never added to the
	// root is unreachable no matter how well its internals work.
	var found bool
	for _, c := range newRootCmd().Commands() {
		if c.Name() == "digest" {
			found = true
		}
	}
	if !found {
		t.Fatal("digest is not registered on the root command")
	}
}

func TestRootCmdPreservesExistingSubcommands(t *testing.T) {
	// Extracting newRootCmd() out of main() must not drop anything.
	want := map[string]bool{"parse": false, "stats": false, "query": false,
		"serve": false, "version": false, "digest": false}
	for _, c := range newRootCmd().Commands() {
		if _, ok := want[c.Name()]; ok {
			want[c.Name()] = true
		}
	}
	for name, present := range want {
		if !present {
			t.Errorf("subcommand %q is missing from the root command", name)
		}
	}
}

func TestDigestEndToEndThroughRootCommand(t *testing.T) {
	// Exercises the real command tree: flag parsing, PersistentPreRunE config
	// loading, and output routing -- none of which calling runDigest directly
	// would cover.
	path := seedCLIStore(t)
	var stdout, stderr bytes.Buffer

	root := newRootCmd()
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"digest", "--duck-db-path", path, "--json", "--limit", "1"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v (stderr: %s)", err, stderr.String())
	}

	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not pure JSON: %v\n%s", err, stdout.String())
	}
	if got["group_by"] != "fingerprint" {
		t.Errorf("group_by = %v", got["group_by"])
	}
}

func TestDigestEndToEndInvalidFlagKeepsStdoutClean(t *testing.T) {
	path := seedCLIStore(t)
	var stdout, stderr bytes.Buffer

	root := newRootCmd()
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"digest", "--duck-db-path", path, "--json", "--order-by", "bogus"})

	if err := root.Execute(); err == nil {
		t.Fatal("expected an error for an invalid --order-by")
	}
	// Usage and diagnostics must never land on stdout in --json mode.
	if stdout.Len() != 0 {
		t.Errorf("stdout must stay empty on error, got: %s", stdout.String())
	}
}

// digestDefaults and digestFilterFor are small test helpers.
func digestDefaults() digest.Options { return digest.DefaultOptions() }

func digestFilterFor(expr string) (storage.RecordFilter, error) {
	return digest.ParseFilters([]string{expr})
}
