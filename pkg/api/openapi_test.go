package api

// Conformance oracle for Task 1.6: pkg/api/openapi.yaml is the canonical
// machine-tier OpenAPI document (also embedded and served at
// GET /api/v1/openapi.json). These tests drive REAL responses (via the
// pkg/api router, over the seeded contracttest fixture) through
// kin-openapi's response validator. If ValidateResponse fails, the SPEC is
// wrong -- the responses it checks against are frozen/contract-fixed
// elsewhere (see sqlrag_test.go / middleware_test.go goldens). Fix
// openapi.yaml, never the handler or the golden.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
)

// loadSpecDoc loads and validates the canonical pkg/api/openapi.yaml.
func loadSpecDoc(t *testing.T) *openapi3.T {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatalf("LoadFromFile(openapi.yaml): %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("doc.Validate: %v", err)
	}
	return doc
}

// TestOpenAPISpecValid asserts openapi.yaml is a well-formed OpenAPI 3.0.3
// document whose info.version equals the frozen ContractVersion.
func TestOpenAPISpecValid(t *testing.T) {
	doc := loadSpecDoc(t)
	if doc.OpenAPI != "3.0.3" {
		t.Fatalf("openapi = %q, want 3.0.3", doc.OpenAPI)
	}
	if doc.Info.Version != ContractVersion {
		t.Fatalf("info.version = %q, want %q (ContractVersion)", doc.Info.Version, ContractVersion)
	}
}

// validateAgainstSpec resolves req's route in docRouter, drives it through r,
// asserts wantStatus, and validates the recorded response against the spec.
func validateAgainstSpec(t *testing.T, docRouter routers.Router, r http.Handler, req *http.Request, wantStatus int) {
	t.Helper()

	route, pathParams, err := docRouter.FindRoute(req)
	if err != nil {
		t.Fatalf("FindRoute(%s %s): %v", req.Method, req.URL.Path, err)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != wantStatus {
		t.Fatalf("%s %s: want status %d, got %d, body=%s", req.Method, req.URL.Path, wantStatus, w.Code, w.Body.String())
	}

	requestValidationInput := &openapi3filter.RequestValidationInput{
		Request:    req,
		PathParams: pathParams,
		Route:      route,
	}
	responseValidationInput := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: requestValidationInput,
		Status:                 w.Code,
		Header:                 w.Header(),
	}
	responseValidationInput.SetBodyBytes(w.Body.Bytes())

	if err := openapi3filter.ValidateResponse(context.Background(), responseValidationInput); err != nil {
		t.Fatalf("%s %s: response does not conform to openapi.yaml: %v", req.Method, req.URL.Path, err)
	}
}

// TestOpenAPIConformanceHealth validates GET /api/v1/health's real response
// against the spec's HealthResponse schema.
func TestOpenAPIConformanceHealth(t *testing.T) {
	doc := loadSpecDoc(t)
	docRouter, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatalf("gorillamux.NewRouter: %v", err)
	}
	r := newTestRouter(t, Options{Token: "test-token"})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	validateAgainstSpec(t, docRouter, r, req, http.StatusOK)
}

// TestOpenAPIConformanceSQLDatabases validates GET /api/v1/sql/databases'
// real (authenticated) response against the spec's DatabasesResponse schema.
func TestOpenAPIConformanceSQLDatabases(t *testing.T) {
	doc := loadSpecDoc(t)
	docRouter, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatalf("gorillamux.NewRouter: %v", err)
	}
	r := newTestRouter(t, Options{Token: "test-token"})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sql/databases", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	validateAgainstSpec(t, docRouter, r, req, http.StatusOK)
}

// TestOpenAPIConformanceSQLQueries validates GET /api/v1/sql/queries' real
// (authenticated, default-window) response against the spec's
// QueriesResponse schema.
func TestOpenAPIConformanceSQLQueries(t *testing.T) {
	doc := loadSpecDoc(t)
	docRouter, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatalf("gorillamux.NewRouter: %v", err)
	}
	r := newTestRouter(t, Options{Token: "test-token"})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sql/queries", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	validateAgainstSpec(t, docRouter, r, req, http.StatusOK)
}

// TestOpenAPIConformanceSQLExecute validates POST /api/v1/sql/execute's real
// (authenticated) response against the spec's ExecuteResponse schema.
func TestOpenAPIConformanceSQLExecute(t *testing.T) {
	doc := loadSpecDoc(t)
	docRouter, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatalf("gorillamux.NewRouter: %v", err)
	}
	r := newTestRouter(t, Options{Token: "test-token"})

	body := []byte(`{"query":"SELECT 1 AS id","timeout_ms":5000}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sql/execute", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	validateAgainstSpec(t, docRouter, r, req, http.StatusOK)
}

// TestOpenAPIJSONServed asserts GET /api/v1/openapi.json is unauthenticated,
// returns 200 application/json, and parses to a valid OpenAPI document whose
// info.version equals ContractVersion -- the same content as openapi.yaml,
// converted to JSON.
func TestOpenAPIJSONServed(t *testing.T) {
	r := newTestRouter(t, Options{Token: "test-token"})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/openapi.json", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/openapi.json: want 200, got %d, body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("GET /api/v1/openapi.json: Content-Type = %q, want application/json; charset=utf-8", ct)
	}

	var probe map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &probe); err != nil {
		t.Fatalf("served openapi.json is not valid JSON: %v", err)
	}

	served, err := openapi3.NewLoader().LoadFromData(w.Body.Bytes())
	if err != nil {
		t.Fatalf("served openapi.json does not parse as an OpenAPI document: %v", err)
	}
	if err := served.Validate(context.Background()); err != nil {
		t.Fatalf("served openapi.json fails openapi3 validation: %v", err)
	}
	if served.Info.Version != ContractVersion {
		t.Fatalf("served openapi.json info.version = %q, want %q (ContractVersion)", served.Info.Version, ContractVersion)
	}
}
