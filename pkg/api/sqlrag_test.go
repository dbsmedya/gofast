package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/dbsmedya/gofast/internal/contracttest"
)

// doQueriesRequest issues an authenticated GET against target through r and
// returns the raw response body, asserting wantStatus.
func doQueriesRequest(t *testing.T, r http.Handler, target string, wantStatus int) []byte {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != wantStatus {
		t.Fatalf("GET %s: want %d, got %d, body=%s", target, wantStatus, w.Code, w.Body.String())
	}
	return w.Body.Bytes()
}

// assertMatchesGolden normalizes body (blankTopLevelExecTime=false, so the
// per-record execution_time_ms aggregation values ARE asserted) and compares
// it byte-for-byte against the golden at testdata/goldens/<goldenName>. It
// never writes/regenerates the golden -- only reads it. Golden equality here
// pins the parameterized query's response for whichever
// WHERE/cursor/known_hashes binding path the target exercises.
func assertMatchesGolden(t *testing.T, body []byte, goldenName string) {
	t.Helper()
	got, err := contracttest.Normalize(body, false)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	want, err := os.ReadFile("testdata/goldens/" + goldenName)
	if err != nil {
		t.Fatalf("read golden %s: %v", goldenName, err)
	}
	if string(got) != string(want) {
		t.Fatalf("golden mismatch for %s:\n--- want ---\n%s\n--- got ---\n%s", goldenName, want, got)
	}
}

// TestQueriesGoldenDefault drives GET /api/v1/sql/queries through the
// pkg/api router (the parameterized handler) and asserts the response matches
// the golden at testdata/goldens/sql_queries_default.json. This pins the
// endpoint's response shape/content independent of how the query is built.
//
// This test never regenerates the golden; it only reads it.
func TestQueriesGoldenDefault(t *testing.T) {
	r := newTestRouter(t, Options{Token: "test-token"})
	body := doQueriesRequest(t, r, "/api/v1/sql/queries", http.StatusOK)
	assertMatchesGolden(t, body, "sql_queries_default.json")
}

// TestQueriesGoldenDatabaseFilter exercises the `db = ?` bound-arg branch
// (baseWhere's conditional clause) against the existing
// sql_queries_db_shop.json golden.
func TestQueriesGoldenDatabaseFilter(t *testing.T) {
	r := newTestRouter(t, Options{Token: "test-token"})
	body := doQueriesRequest(t, r, "/api/v1/sql/queries?database=shop", http.StatusOK)
	assertMatchesGolden(t, body, "sql_queries_db_shop.json")
}

// TestQueriesGoldenMinExecutionMS exercises the `min_execution_ms` bound arg
// (the `WHERE execution_time_ms >= ?` clause in both countSQL and dataSQL)
// against the existing sql_queries_min_exec.json golden. Confirmed
// "min_execution_ms" is the exact query-param name the handler reads
// (queriesHandler: c.DefaultQuery("min_execution_ms", "0")).
func TestQueriesGoldenMinExecutionMS(t *testing.T) {
	r := newTestRouter(t, Options{Token: "test-token"})
	body := doQueriesRequest(t, r, "/api/v1/sql/queries?min_execution_ms=1000", http.StatusOK)
	assertMatchesGolden(t, body, "sql_queries_min_exec.json")
}

// TestQueriesGoldenPagination exercises the 3 cursor bound args
// (`execution_time_ms < ? OR (execution_time_ms = ? AND id > ?)`) across a
// two-page walk, against the existing sql_queries_limit1_p1.json /
// sql_queries_limit1_p2.json goldens. p2's cursor is extracted from p1's
// live response (not hardcoded), matching how a real client would paginate.
func TestQueriesGoldenPagination(t *testing.T) {
	r := newTestRouter(t, Options{Token: "test-token"})

	body1 := doQueriesRequest(t, r, "/api/v1/sql/queries?limit=1", http.StatusOK)
	assertMatchesGolden(t, body1, "sql_queries_limit1_p1.json")

	var page1 struct {
		NextCursor *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(body1, &page1); err != nil {
		t.Fatalf("unmarshal page1: %v", err)
	}
	if page1.NextCursor == nil || *page1.NextCursor == "" {
		t.Fatalf("expected non-empty next_cursor in page1 response: %s", body1)
	}

	target2 := "/api/v1/sql/queries?limit=1&cursor=" + url.QueryEscape(*page1.NextCursor)
	body2 := doQueriesRequest(t, r, target2, http.StatusOK)
	assertMatchesGolden(t, body2, "sql_queries_limit1_p2.json")
}

// TestQueriesKnownHashesExcludesFingerprint exercises the `known_hashes`
// bound-arg branch (`NOT IN (?, ...)` over lower(substr(sha256(text),1,16)))
// -- the one binding path with no existing Phase-0 golden. It computes the
// hash the SAME way the query does (lower(hex(sha256(text)))[:16], on the
// "text" field, which is the aggregated `sanitized_sql`), passes it as
// known_hashes, and asserts that fingerprint is excluded: total_count drops
// by exactly 1 vs. the (golden-verified) default response.
func TestQueriesKnownHashesExcludesFingerprint(t *testing.T) {
	r := newTestRouter(t, Options{Token: "test-token"})

	defaultBody := doQueriesRequest(t, r, "/api/v1/sql/queries", http.StatusOK)
	var defaultResp struct {
		Data []struct {
			Text string `json:"text"`
		} `json:"data"`
		TotalCount int `json:"total_count"`
	}
	if err := json.Unmarshal(defaultBody, &defaultResp); err != nil {
		t.Fatalf("unmarshal default response: %v", err)
	}
	if len(defaultResp.Data) == 0 {
		t.Fatalf("expected at least one record in default response: %s", defaultBody)
	}

	targetText := defaultResp.Data[0].Text
	sum := sha256.Sum256([]byte(targetText))
	hash := hex.EncodeToString(sum[:])[:16] // hex.EncodeToString is already lowercase

	filteredBody := doQueriesRequest(t, r, "/api/v1/sql/queries?known_hashes="+hash, http.StatusOK)
	var filteredResp struct {
		Data []struct {
			Text string `json:"text"`
		} `json:"data"`
		TotalCount int `json:"total_count"`
	}
	if err := json.Unmarshal(filteredBody, &filteredResp); err != nil {
		t.Fatalf("unmarshal filtered response: %v", err)
	}

	if filteredResp.TotalCount != defaultResp.TotalCount-1 {
		t.Fatalf("known_hashes=%s: total_count = %d, want %d (default %d minus the excluded fingerprint)",
			hash, filteredResp.TotalCount, defaultResp.TotalCount-1, defaultResp.TotalCount)
	}
	for _, rec := range filteredResp.Data {
		if rec.Text == targetText {
			t.Fatalf("known_hashes=%s: excluded fingerprint %q still present in filtered data: %s", hash, targetText, filteredBody)
		}
	}
}

// TestQueriesDatabaseParamNotInjectable asserts that a malformed `database`
// query param (attempting SQL injection) is rejected by
// validDatabaseNamePattern with 400 before ever reaching the parameterized
// query -- i.e. the database param is non-injectable both by construction
// (bound as ?) and by validation (rejected up front).
func TestQueriesDatabaseParamNotInjectable(t *testing.T) {
	r := newTestRouter(t, Options{Token: "test-token"})
	// httptest.NewRequest builds a raw "METHOD target HTTP/1.0" request line
	// and splits it on literal spaces, so an unescaped space in the payload
	// (as in the brief's literal `?database=shop' OR '1'='1`) breaks its own
	// request-line parsing before the request ever reaches our router --
	// panic: "malformed HTTP version". url.QueryEscape encodes the same
	// payload (space -> "+", quotes -> %27) without changing what
	// c.Query("database") decodes back to, so the probe still exercises the
	// exact injection string against validDatabaseNamePattern.
	target := "/api/v1/sql/queries?database=" + url.QueryEscape(`shop' OR '1'='1`)
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest { // caught by validDatabaseNamePattern
		t.Fatalf("want 400 for malformed database, got %d", w.Code)
	}
	if !bodyHasError(t, w, "invalid_param") {
		t.Fatalf("want error=invalid_param, body=%s", w.Body.String())
	}
}

// TestDatabasesGolden drives GET /api/v1/sql/databases through the pkg/api
// router (databasesHandler) and asserts the response matches the golden at
// testdata/goldens/sql_databases.json.
//
// This test never regenerates the golden; it only reads it.
func TestDatabasesGolden(t *testing.T) {
	r := newTestRouter(t, Options{Token: "test-token"})
	body := doQueriesRequest(t, r, "/api/v1/sql/databases", http.StatusOK)
	assertMatchesGolden(t, body, "sql_databases.json")
}

// postExecute issues an authenticated POST /api/v1/sql/execute with the given
// query and a fixed 5s timeout_ms, returning the raw recorder.
func postExecute(t *testing.T, r http.Handler, query string) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(map[string]interface{}{
		"query":      query,
		"timeout_ms": 5000,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sql/execute", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestExecuteGoldenSelect drives POST /api/v1/sql/execute through the pkg/api
// router (executeHandler) and asserts the response matches the golden at
// testdata/goldens/sql_execute_select.json. blankTopLevelExecTime is true
// here -- and ONLY here among the sql goldens -- because this endpoint's
// top-level execution_time_ms is wall-clock (time.Since(start)), not a
// deterministic aggregation.
//
// This test never regenerates the golden; it only reads it.
func TestExecuteGoldenSelect(t *testing.T) {
	r := newTestRouter(t, Options{Token: "test-token"})
	w := postExecute(t, r, "SELECT 1 AS id")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d, body=%s", w.Code, w.Body.String())
	}
	got, err := contracttest.Normalize(w.Body.Bytes(), true)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	want, err := os.ReadFile("testdata/goldens/sql_execute_select.json")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("golden mismatch for sql_execute_select.json:\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

// TestExecuteRejectsWrites asserts that non-SELECT statements are rejected
// with 422 unsafe_query by validateReadOnlySelectQuery, never reaching the
// store.
func TestExecuteRejectsWrites(t *testing.T) {
	r := newTestRouter(t, Options{Token: "test-token"}) // DisableSQLExecute false => enabled
	for _, q := range []string{"INSERT INTO x VALUES (1)", "UPDATE x SET a=1", "DELETE FROM x", "DROP TABLE x"} {
		w := postExecute(t, r, q)
		if w.Code != http.StatusUnprocessableEntity || !bodyHasError(t, w, "unsafe_query") {
			t.Fatalf("query %q: want 422 unsafe_query, got %d %s", q, w.Code, w.Body.String())
		}
	}
}

// TestExecuteRejectsStacked asserts that a stacked-statement payload
// (a SELECT followed by a `;` and a second statement) is rejected -- either
// at body-parse/empty-query validation (400) or by validateReadOnlySelectQuery
// (422), never executed against the store.
func TestExecuteRejectsStacked(t *testing.T) {
	r := newTestRouter(t, Options{Token: "test-token"})
	w := postExecute(t, r, "SELECT 1; DROP TABLE slow_logs")
	if w.Code != http.StatusBadRequest && w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("stacked statements: want 400 or 422, got %d %s", w.Code, w.Body.String())
	}
}

// TestExecuteDisabledReturns404 asserts that with Options.DisableSQLExecute
// true, the /sql/execute route is not registered at all (404), not merely
// rejecting requests once registered.
func TestExecuteDisabledReturns404(t *testing.T) {
	fx := contracttest.BuildFixtureDB(t)
	r := NewRouter(leaseProvider{fx.Store}, Options{Token: "test-token", DisableSQLExecute: true})
	w := postExecute(t, r, "SELECT 1 AS id")
	if w.Code != http.StatusNotFound {
		t.Fatalf("DisableSQLExecute: want 404, got %d %s", w.Code, w.Body.String())
	}
}
