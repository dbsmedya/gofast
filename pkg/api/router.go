// Package api is the machine-tier REST API: a stable seam between the core
// library and its consumers. It is mounted by `gofast-cli serve` via
// NewRouter, and can be mounted by a downstream product via
// RegisterMachineRoutes onto its own gin engine.
package api

import (
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/dbsmedya/gofast/pkg/storage"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

// ErrStoreUnavailable is returned by a StoreProvider when no store is open
// (e.g. the DB is being rebuilt by a parse). Handlers map it to 503 db_unavailable.
var ErrStoreUnavailable = errors.New("store unavailable")

// ContractVersion is the machine-API REST contract version: what /health and
// the served OpenAPI advertise. NOT the software release (that is the git tag).
// Held constant so contract goldens stay stable.
const ContractVersion = "1.0.0"

// openapiYAML is the canonical, hand-authored machine-tier OpenAPI 3.0.3
// document (pkg/api/openapi.yaml). It is the conformance oracle for
// pkg/api's REST responses -- see openapi_test.go.
//
//go:embed openapi.yaml
var openapiYAML []byte

// openapiJSON is openapiYAML parsed and re-marshaled to JSON exactly once at
// package init, so GET /api/v1/openapi.json serves a pre-computed byte slice
// on every request instead of re-parsing YAML per request. A bad embedded
// spec is a build-time programmer error, so failure to load/marshal panics
// at init rather than being silently swallowed.
var openapiJSON []byte

func init() {
	doc, err := openapi3.NewLoader().LoadFromData(openapiYAML)
	if err != nil {
		panic("pkg/api: failed to parse embedded openapi.yaml: " + err.Error())
	}
	data, err := json.Marshal(doc)
	if err != nil {
		panic("pkg/api: failed to marshal embedded openapi.yaml to JSON: " + err.Error())
	}
	openapiJSON = data
}

// openapiSpecHandler serves the embedded machine-tier OpenAPI document as
// JSON (converted once at init from the canonical openapi.yaml).
func openapiSpecHandler(c *gin.Context) {
	c.Data(http.StatusOK, "application/json; charset=utf-8", openapiJSON)
}

// StoreProvider yields the current read-only store under a read LEASE held for
// the whole callback, so a concurrent parse (which closes/swaps the store) can't
// invalidate the handle mid-query. The machine `serve` path wraps a fixed RO
// store; a downstream RW-swapping store implements the lease over its swap mutex.
type StoreProvider interface {
	// WithStore runs fn while holding the read lease. fn MUST NOT retain the
	// *storage.Storage past its return. Returns ErrStoreUnavailable if no store
	// is open.
	WithStore(fn func(*storage.Storage) error) error
}

// Options configures the machine-tier router.
type Options struct {
	Token             string // bearer token; required unless DisableAuth
	DisableAuth       bool   // DEV ONLY: mount /sql/* with no bearer middleware
	DisableSQLExecute bool   // zero-value false => /sql/execute ENABLED (default)
}

// RegisterMachineRoutes mounts the machine tier onto any gin router (a full
// engine or a group): /health, /api/v1/health, /api/v1/openapi.json,
// /api/v1/sql/{queries,databases,execute}. It attaches the machine
// middleware (request-id, gzip, CORS) to its own route groups so the same
// behavior holds wherever it is mounted. Bearer auth on /sql/* unless
// opts.DisableAuth; /sql/execute registered unless opts.DisableSQLExecute.
// /api/v1/openapi.json is always unauthenticated, alongside /health.
//
// Exported so downstream consumers can mount the machine tier onto their own
// engine.
func RegisterMachineRoutes(r gin.IRouter, provider StoreProvider, opts Options) {
	// Order: request-id, then gzip, then CORS.
	r.Use(func(c *gin.Context) {
		requestID := generateRequestID()
		c.Set("request_id", requestID)
		c.Header("X-Request-Id", requestID)
		c.Next()
	})
	r.Use(gzipMiddleware())

	corsConfig := cors.Config{
		AllowOrigins:     []string{"http://localhost:3000", "http://localhost:5173", "http://localhost:4173", "http://127.0.0.1:5173", "http://127.0.0.1:4173"},
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "HEAD"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Accept", "Authorization", "X-Requested-With"},
		ExposeHeaders:    []string{"Content-Length", "Content-Type", "X-Request-ID"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}
	r.Use(cors.New(corsConfig))

	health := healthHandler(provider)
	r.GET("/health", health)
	r.GET("/api/v1/health", health)
	r.GET("/api/v1/openapi.json", openapiSpecHandler)

	sql := r.Group("/api/v1/sql")
	if !opts.DisableAuth {
		sql.Use(bearerAuthMiddleware(opts.Token))
	}

	sql.GET("/queries", queriesHandler(provider))
	sql.GET("/databases", databasesHandler(provider))
	if !opts.DisableSQLExecute {
		sql.POST("/execute", executeHandler(provider))
	}
}

// NewRouter is the standalone entry point: a fresh *gin.Engine with Recovery()
// plus RegisterMachineRoutes. Used by `gofast-cli serve`. No dashboard routes.
func NewRouter(provider StoreProvider, opts Options) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	e := gin.New()
	e.Use(gin.Recovery())
	RegisterMachineRoutes(e, provider, opts)
	return e
}
