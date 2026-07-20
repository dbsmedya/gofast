package main

import (
	"net/http/httptest"
	"testing"

	"github.com/dbsmedya/gofast/internal/contracttest"
	"github.com/dbsmedya/gofast/pkg/config"
)

func TestBuildServeRouter_HealthAndAuth(t *testing.T) {
	fx := contracttest.BuildFixtureDB(t)

	cfg := &config.Config{}
	cfg.DuckDB.Path = fx.Path // buildServeRouter opens its OWN read-only handle;
	// DuckDB allows multiple RO handles on the same file.

	handler, cleanup, err := buildServeRouter(cfg, "test-token", false)
	if err != nil {
		t.Fatalf("buildServeRouter: %v", err)
	}
	defer func() {
		if cerr := cleanup(); cerr != nil {
			t.Errorf("cleanup: %v", cerr)
		}
	}()

	t.Run("health returns 200", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/health", nil)
		handler.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("sql queries without token returns 401", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/v1/sql/queries", nil)
		handler.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}
