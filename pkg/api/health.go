package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/dbsmedya/gofast/pkg/storage"

	"github.com/gin-gonic/gin"
)

// listDatabases returns the distinct database names present in the store.
func listDatabases(ctx context.Context, store *storage.Storage) ([]string, error) {
	result, err := store.Query(ctx, `
		SELECT DISTINCT db
		FROM slow_logs
		WHERE db IS NOT NULL AND db != ''
		ORDER BY db
	`)
	if err != nil {
		return nil, err
	}

	databases := make([]string, 0, len(result.Rows))
	for _, row := range result.Rows {
		if len(row) == 0 {
			continue
		}
		if dbName, ok := row[0].(string); ok && strings.TrimSpace(dbName) != "" {
			databases = append(databases, dbName)
		}
	}
	return databases, nil
}

// healthHandler builds the health-check body, reading databases via the
// StoreProvider lease so a concurrent parse/swap can't yield a half-closed
// handle. ErrStoreUnavailable (or any query error) yields an empty databases
// list; the endpoint always answers 200.
func healthHandler(provider StoreProvider) gin.HandlerFunc {
	return func(c *gin.Context) {
		databases := []string{}
		_ = provider.WithStore(func(s *storage.Storage) error {
			if list, err := listDatabases(c.Request.Context(), s); err == nil {
				databases = list
			}
			return nil
		})
		c.JSON(http.StatusOK, gin.H{
			"status":    "ok",
			"version":   ContractVersion,
			"databases": databases,
		})
	}
}
