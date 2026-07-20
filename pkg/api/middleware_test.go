package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dbsmedya/gofast/internal/contracttest"
	"github.com/dbsmedya/gofast/pkg/storage"

	"github.com/gin-gonic/gin"
)

// leaseProvider is a trivial fixed-store provider (tests never parse).
type leaseProvider struct{ s *storage.Storage }

func (p leaseProvider) WithStore(fn func(*storage.Storage) error) error {
	if p.s == nil {
		return ErrStoreUnavailable
	}
	return fn(p.s)
}

func newTestRouter(t *testing.T, opts Options) http.Handler {
	fx := contracttest.BuildFixtureDB(t)
	return NewRouter(leaseProvider{fx.Store}, opts)
}

// bodyHasError decodes w.Body as JSON and compares body["error"] to code.
func bodyHasError(t *testing.T, w *httptest.ResponseRecorder, code string) bool {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v (body=%s)", err, w.Body.String())
	}
	got, _ := body["error"].(string)
	return got == code
}

func TestHealthNoAuth(t *testing.T) {
	r := newTestRouter(t, Options{Token: "test-token"})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("health: want 200, got %d", w.Code)
	}
}

func TestSQLQueriesRequiresToken(t *testing.T) {
	r := newTestRouter(t, Options{Token: "test-token"})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/sql/queries", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no-token queries: want 401, got %d", w.Code)
	}
	if !bodyHasError(t, w, "invalid_token") {
		t.Fatalf("want error=invalid_token, body=%s", w.Body.String())
	}
}

func TestDisableAuthSkipsBearer(t *testing.T) {
	r := newTestRouter(t, Options{DisableAuth: true})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/sql/queries", nil))
	if w.Code == http.StatusUnauthorized {
		t.Fatalf("DisableAuth: want no 401, got 401")
	}
}

func TestRegisterMachineRoutesOntoBareEngine(t *testing.T) {
	fx := contracttest.BuildFixtureDB(t)
	provider := leaseProvider{fx.Store}

	gin.SetMode(gin.TestMode)
	e := gin.New()
	RegisterMachineRoutes(e, provider, Options{Token: "test-token"})

	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("health on bare engine: want 200, got %d", w.Code)
	}

	w2 := httptest.NewRecorder()
	e.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/api/v1/sql/queries", nil))
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("sql/queries on bare engine: want 401, got %d", w2.Code)
	}
}
