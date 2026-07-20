package api

// Handlers for the read-only machine SQL API (/api/v1/sql/*).
//
// Two safety-relevant properties of the query handler:
//
//  1. Store access is wrapped in the StoreProvider lease (WithStore) instead
//     of a bare GetStore()/nil-check, so a concurrent parse/swap can't
//     invalidate the handle mid-query.
//  2. The WHERE/cursor/known_hashes construction is parameterized (bound `?`
//     args via store.Query) instead of fmt.Sprintf'd directly into the SQL
//     text, so the `database`, `known_hashes`, and cursor values can no
//     longer be used to inject SQL. LIMIT stays a formatted int literal
//     (never user text, so it is not an injection vector).
//
// The since/until timestamps are bound as time.Time values (not strings) so
// the DuckDB driver sends a real TIMESTAMP parameter rather than relying on
// implicit string->TIMESTAMP coercion in the `ts >= ?` comparison.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dbsmedya/gofast/pkg/storage"

	"github.com/gin-gonic/gin"
)

type sqlQueriesCursor struct {
	ExecutionTimeMS float64 `json:"execution_time_ms"`
	ID              string  `json:"id"`
}

var (
	validDatabaseNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	validHashPattern         = regexp.MustCompile(`^[a-f0-9]{16}$`)
	validCursorIDPattern     = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

func toFloat64(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint64:
		return float64(n), true
	case uint32:
		return float64(n), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func toInt64(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case int32:
		return int64(n), true
	case uint:
		return int64(n), true
	case uint64:
		return int64(n), true
	case uint32:
		return int64(n), true
	case float64:
		return int64(n), true
	case float32:
		return int64(n), true
	case string:
		i, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		return i, err == nil
	default:
		return 0, false
	}
}

func toStringSlice(v interface{}) []string {
	switch t := v.(type) {
	case nil:
		return []string{}
	case []string:
		return t
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		var parsed []string
		if err := json.Unmarshal([]byte(t), &parsed); err == nil {
			return parsed
		}
		return []string{}
	case []byte:
		var parsed []string
		if err := json.Unmarshal(t, &parsed); err == nil {
			return parsed
		}
		return []string{}
	default:
		return []string{}
	}
}

func parseKnownHashes(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	seen := make(map[string]bool)
	hashes := make([]string, 0)
	for _, item := range strings.Split(raw, ",") {
		hash := strings.ToLower(strings.TrimSpace(item))
		if hash == "" {
			continue
		}
		if !validHashPattern.MatchString(hash) {
			return nil, fmt.Errorf("invalid known_hashes")
		}
		if !seen[hash] {
			seen[hash] = true
			hashes = append(hashes, hash)
		}
	}
	return hashes, nil
}

func parseCursor(raw string) (*sqlQueriesCursor, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		decoded, err = base64.URLEncoding.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid cursor")
		}
	}

	var cursor sqlQueriesCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil {
		return nil, fmt.Errorf("invalid cursor")
	}
	if cursor.ID == "" || !validCursorIDPattern.MatchString(cursor.ID) {
		return nil, fmt.Errorf("invalid cursor")
	}
	return &cursor, nil
}

func asRFC3339UTC(v interface{}) string {
	switch t := v.(type) {
	case time.Time:
		return t.UTC().Format(time.RFC3339)
	case string:
		parsed, err := time.Parse(time.RFC3339Nano, t)
		if err != nil {
			return t
		}
		return parsed.UTC().Format(time.RFC3339)
	default:
		return ""
	}
}

// queriesHandler serves GET /api/v1/sql/queries: an aggregated, paginated
// view of slow-query "fingerprints" over a time window. Store access goes
// through the StoreProvider lease and the WHERE/cursor/known_hashes
// construction is parameterized (bound args) instead of Sprintf'd into the
// SQL text.
// databasesHandler serves GET /api/v1/sql/databases: a static list of available
// databases; store access goes through the StoreProvider lease.
func databasesHandler(provider StoreProvider) gin.HandlerFunc {
	return func(c *gin.Context) {
		err := provider.WithStore(func(store *storage.Storage) error {
			databases, e := listDatabases(c.Request.Context(), store)
			if e != nil {
				contractError(c, http.StatusInternalServerError, "internal_error", "Failed to list databases.")
				return nil
			}
			c.JSON(http.StatusOK, gin.H{"databases": databases})
			return nil
		})
		if errors.Is(err, ErrStoreUnavailable) {
			contractError(c, http.StatusServiceUnavailable, "db_unavailable", "Database is unavailable.")
			return
		}
	}
}

func queriesHandler(provider StoreProvider) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit := 200
		if rawLimit := strings.TrimSpace(c.DefaultQuery("limit", "200")); rawLimit != "" {
			parsedLimit, err := strconv.Atoi(rawLimit)
			if err != nil || parsedLimit < 1 || parsedLimit > 1000 {
				contractError(c, http.StatusBadRequest, "invalid_param", "Invalid limit parameter.")
				return
			}
			limit = parsedLimit
		}

		now := time.Now().UTC()
		since := now.AddDate(0, 0, -15)
		until := now

		if rawSince := strings.TrimSpace(c.Query("since")); rawSince != "" {
			parsedSince, err := time.Parse(time.RFC3339, rawSince)
			if err != nil {
				contractError(c, http.StatusBadRequest, "invalid_param", "Invalid since parameter.")
				return
			}
			since = parsedSince.UTC()
		}

		if rawUntil := strings.TrimSpace(c.Query("until")); rawUntil != "" {
			parsedUntil, err := time.Parse(time.RFC3339, rawUntil)
			if err != nil {
				contractError(c, http.StatusBadRequest, "invalid_param", "Invalid until parameter.")
				return
			}
			until = parsedUntil.UTC()
		}

		if since.After(until) {
			contractError(c, http.StatusBadRequest, "invalid_param", "since must be before or equal to until.")
			return
		}

		minExecutionMS := 0.0
		if rawMin := strings.TrimSpace(c.DefaultQuery("min_execution_ms", "0")); rawMin != "" {
			parsedMin, err := strconv.ParseFloat(rawMin, 64)
			if err != nil || parsedMin < 0 {
				contractError(c, http.StatusBadRequest, "invalid_param", "Invalid min_execution_ms parameter.")
				return
			}
			minExecutionMS = parsedMin
		}

		database := strings.TrimSpace(c.Query("database"))
		if database != "" && !validDatabaseNamePattern.MatchString(database) {
			contractError(c, http.StatusBadRequest, "invalid_param", "Invalid database parameter.")
			return
		}

		knownHashes, err := parseKnownHashes(c.Query("known_hashes"))
		if err != nil {
			contractError(c, http.StatusBadRequest, "invalid_param", "Invalid known_hashes parameter.")
			return
		}

		cursor, err := parseCursor(c.Query("cursor"))
		if err != nil {
			contractError(c, http.StatusBadRequest, "invalid_param", "Invalid cursor parameter.")
			return
		}

		// baseWhere/baseArgs: `since`/`until` are bound as time.Time values (not
		// strings) so the DuckDB driver sends a real TIMESTAMP parameter for the
		// `ts >= ?` / `ts <= ?` comparisons.
		baseWhere := []string{"ts >= ?", "ts <= ?"}
		baseArgs := []interface{}{since, until}
		if database != "" {
			baseWhere = append(baseWhere, "db = ?")
			baseArgs = append(baseArgs, database)
		}
		baseWhereSQL := strings.Join(baseWhere, " AND ")

		hashClause, hashArgs := "", []interface{}{}
		if len(knownHashes) > 0 {
			ph := make([]string, len(knownHashes))
			for i, h := range knownHashes {
				ph[i] = "?"
				hashArgs = append(hashArgs, h)
			}
			hashClause = "AND lower(substr(sha256(text), 1, 16)) NOT IN (" + strings.Join(ph, ",") + ")"
		}

		// count query args: base + minExec + hash
		countArgs := append(append([]interface{}{}, baseArgs...), minExecutionMS)
		countArgs = append(countArgs, hashArgs...)
		// data query args: base + minExec + hash + (cursor?) ; LIMIT n+1 stays an
		// int literal (safe, not user text).
		dataArgs := append([]interface{}{}, countArgs...)
		cursorClause := ""
		if cursor != nil {
			cursorClause = "AND (execution_time_ms < ? OR (execution_time_ms = ? AND id > ?))"
			dataArgs = append(dataArgs, cursor.ExecutionTimeMS, cursor.ExecutionTimeMS, cursor.ID)
		}

		countSQL := fmt.Sprintf(`
			WITH aggregated AS (
				SELECT
					fingerprint_id AS id,
					COALESCE(arg_max(sanitized_sql, ts), '') AS text,
					COALESCE(SUM(query_time_sec), 0) * 1000.0 AS execution_time_ms
				FROM slow_logs
				WHERE %s
				GROUP BY fingerprint_id
			)
			SELECT COUNT(*) AS total_count
			FROM aggregated
			WHERE execution_time_ms >= ?
			%s
		`, baseWhereSQL, hashClause)

		dataSQL := fmt.Sprintf(`
			WITH aggregated AS (
				SELECT
					fingerprint_id AS id,
					COALESCE(arg_max(sanitized_sql, ts), '') AS text,
					COALESCE(arg_max(sample_sql, ts), '') AS raw_query,
					COALESCE(arg_max(db, ts), '') AS source,
					COALESCE(SUM(query_time_sec), 0) * 1000.0 AS execution_time_ms,
					COUNT(*) AS calls,
					COALESCE(arg_max(tables, ts), []::VARCHAR[]) AS tables,
					MAX(ts) AS latest_ts,
					arg_max(user, ts) AS user,
					arg_max(host, ts) AS host,
					arg_max(rows_sent, ts) AS rows_sent,
					arg_max(rows_examined, ts) AS rows_examined,
					arg_max(lock_time_sec, ts) AS lock_time_sec,
					COALESCE(
						list_sort(list(DISTINCT source_host) FILTER (WHERE source_host IS NOT NULL AND source_host <> '')),
						[]::VARCHAR[]
					) AS source_hosts
				FROM slow_logs_with_source
				WHERE %s
				GROUP BY fingerprint_id
			)
			SELECT
				id, text, raw_query, source, execution_time_ms, calls, tables, latest_ts, user, host, rows_sent, rows_examined, lock_time_sec, source_hosts
			FROM aggregated
			WHERE execution_time_ms >= ?
			%s
			%s
			ORDER BY execution_time_ms DESC, id ASC
			LIMIT %d
		`, baseWhereSQL, hashClause, cursorClause, limit+1)

		err = provider.WithStore(func(store *storage.Storage) error {
			totalCountResult, qerr := store.Query(c.Request.Context(), countSQL, countArgs...)
			if qerr != nil {
				contractError(c, http.StatusInternalServerError, "internal_error", "Failed to count query records.")
				return nil
			}
			totalCount := 0
			if len(totalCountResult.Rows) > 0 && len(totalCountResult.Rows[0]) > 0 {
				if countValue, ok := toInt64(totalCountResult.Rows[0][0]); ok {
					totalCount = int(countValue)
				}
			}

			queryResult, qerr := store.Query(c.Request.Context(), dataSQL, dataArgs...)
			if qerr != nil {
				contractError(c, http.StatusInternalServerError, "internal_error", "Failed to fetch query records.")
				return nil
			}

			data := make([]gin.H, 0, len(queryResult.Rows))
			for _, row := range queryResult.Rows {
				if len(row) < 14 {
					continue
				}

				executionTimeMS, _ := toFloat64(row[4])
				calls, _ := toInt64(row[5])

				record := gin.H{
					"id":                fmt.Sprintf("%v", row[0]),
					"text":              fmt.Sprintf("%v", row[1]),
					"raw_query":         fmt.Sprintf("%v", row[2]),
					"source":            fmt.Sprintf("%v", row[3]),
					"execution_time_ms": executionTimeMS,
					"calls":             calls,
					"tables":            toStringSlice(row[6]),
					"latest_ts":         asRFC3339UTC(row[7]),
					"user":              row[8],
					"host":              row[9],
					"rows_sent":         row[10],
					"rows_examined":     row[11],
					"lock_time_sec":     row[12],
					"source_hosts":      toStringSlice(row[13]),
				}
				data = append(data, record)
			}

			hasMore := false
			if len(data) > limit {
				hasMore = true
				data = data[:limit]
			}

			var nextCursor interface{}
			if hasMore && len(data) > 0 {
				last := data[len(data)-1]
				next := sqlQueriesCursor{
					ExecutionTimeMS: 0,
					ID:              fmt.Sprintf("%v", last["id"]),
				}
				if f, ok := toFloat64(last["execution_time_ms"]); ok {
					next.ExecutionTimeMS = f
				}
				encoded, merr := json.Marshal(next)
				if merr == nil {
					nextCursor = base64.StdEncoding.EncodeToString(encoded)
				}
			}

			c.JSON(http.StatusOK, gin.H{
				"data":        data,
				"next_cursor": nextCursor,
				"has_more":    hasMore,
				"total_count": totalCount,
			})
			return nil
		})
		if errors.Is(err, ErrStoreUnavailable) {
			contractError(c, http.StatusServiceUnavailable, "db_unavailable", "Database is unavailable.")
			return
		}
	}
}

// tokenizeSQL and validateReadOnlySelectQuery gate what queries executeHandler
// is allowed to run (read-only single SELECT). They are security-critical:
// change them only with matching test coverage.
func tokenizeSQL(query string) ([]string, error) {
	tokens := make([]string, 0, 32)
	var current strings.Builder
	inSingle := false
	inDouble := false
	inBacktick := false
	inLineComment := false
	inBlockComment := false

	flush := func() {
		if current.Len() == 0 {
			return
		}
		tokens = append(tokens, strings.ToLower(current.String()))
		current.Reset()
	}

	for i := 0; i < len(query); i++ {
		ch := query[i]
		next := byte(0)
		if i+1 < len(query) {
			next = query[i+1]
		}

		if inLineComment {
			if ch == '\n' {
				inLineComment = false
			}
			continue
		}
		if inBlockComment {
			if ch == '*' && next == '/' {
				inBlockComment = false
				i++
			}
			continue
		}
		if inSingle {
			if ch == '\'' && (i == 0 || query[i-1] != '\\') {
				inSingle = false
			}
			continue
		}
		if inDouble {
			if ch == '"' && (i == 0 || query[i-1] != '\\') {
				inDouble = false
			}
			continue
		}
		if inBacktick {
			if ch == '`' {
				inBacktick = false
			}
			continue
		}

		if ch == '-' && next == '-' {
			flush()
			inLineComment = true
			i++
			continue
		}
		if ch == '#' {
			flush()
			inLineComment = true
			continue
		}
		if ch == '/' && next == '*' {
			flush()
			inBlockComment = true
			i++
			continue
		}
		if ch == '\'' {
			flush()
			inSingle = true
			continue
		}
		if ch == '"' {
			flush()
			inDouble = true
			continue
		}
		if ch == '`' {
			flush()
			inBacktick = true
			continue
		}
		if ch == ';' {
			return nil, fmt.Errorf("multiple statements are not allowed")
		}

		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '.' || ch == '$' {
			current.WriteByte(ch)
			continue
		}
		flush()
	}
	flush()

	if inSingle || inDouble || inBacktick || inBlockComment {
		return nil, fmt.Errorf("unterminated SQL input")
	}
	return tokens, nil
}

func validateReadOnlySelectQuery(raw string) error {
	tokens, err := tokenizeSQL(raw)
	if err != nil {
		return err
	}
	if len(tokens) == 0 {
		return fmt.Errorf("empty query")
	}

	first := tokens[0]
	if first != "select" && first != "with" {
		return fmt.Errorf("only select queries are allowed")
	}

	if first == "with" {
		hasSelect := false
		for _, token := range tokens {
			if token == "select" {
				hasSelect = true
				break
			}
		}
		if !hasSelect {
			return fmt.Errorf("with query must contain a select")
		}
	}

	forbidden := map[string]bool{
		"insert": true, "update": true, "delete": true, "drop": true, "alter": true, "create": true,
		"truncate": true, "set": true, "grant": true, "revoke": true, "attach": true, "copy": true,
		"export": true, "import": true, "call": true, "vacuum": true, "analyze": true, "pragma": true,
		"begin": true, "commit": true, "rollback": true, "transaction": true,
	}
	forbiddenSchemas := map[string]bool{
		"information_schema": true,
		"pg_catalog":         true,
		"sqlite_master":      true,
	}
	for _, token := range tokens {
		if forbidden[token] {
			return fmt.Errorf("unsafe statement detected")
		}
		if forbiddenSchemas[token] || strings.HasPrefix(token, "duckdb_") {
			return fmt.Errorf("system schema access is not allowed")
		}
	}
	return nil
}

// executeHandler serves POST /api/v1/sql/execute: runs a single, validated,
// read-only SELECT (or SELECT-containing WITH) supplied by the caller and
// returns its rows. Read-only safety here comes from two layers, NEITHER
// of which is parameterization (the whole query is user text, so it cannot
// be bound as a `?` arg): validateReadOnlySelectQuery statically rejects
// anything but a single SELECT/WITH statement (no writes, no stacked
// statements, no system-schema access), and the StoreProvider lease hands
// the handler a read-only *storage.Storage for the duration of the call.
// Request parsing/validation happens outside WithStore (matching the
// established queriesHandler pattern: reject bad input for free, before
// paying for a lease); only the query execution, truncation, and success
// response are inside the lease.
func executeHandler(provider StoreProvider) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Query     string `json:"query"`
			Database  string `json:"database"`
			TimeoutMS *int   `json:"timeout_ms"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			contractError(c, http.StatusBadRequest, "invalid_param", "Invalid request body.")
			return
		}
		if strings.TrimSpace(req.Query) == "" {
			contractError(c, http.StatusBadRequest, "invalid_param", "query is required.")
			return
		}
		if req.Database != "" && !validDatabaseNamePattern.MatchString(req.Database) {
			contractError(c, http.StatusBadRequest, "invalid_param", "Invalid database parameter.")
			return
		}
		if err := validateReadOnlySelectQuery(req.Query); err != nil {
			contractError(c, http.StatusUnprocessableEntity, "unsafe_query", "Only a single read-only SELECT query is allowed.")
			return
		}

		timeoutMS := 10000
		if req.TimeoutMS != nil {
			if *req.TimeoutMS <= 0 {
				contractError(c, http.StatusBadRequest, "invalid_param", "timeout_ms must be positive.")
				return
			}
			timeoutMS = *req.TimeoutMS
		}
		if timeoutMS > 30000 {
			timeoutMS = 30000
		}

		// Hard row cap for safety; extra row is fetched to detect truncation.
		const rowCap = 10000
		wrappedQuery := fmt.Sprintf("SELECT * FROM (%s) AS q LIMIT %d", strings.TrimSpace(req.Query), rowCap+1)

		err := provider.WithStore(func(store *storage.Storage) error {
			ctx, cancel := context.WithTimeout(c.Request.Context(), time.Duration(timeoutMS)*time.Millisecond)
			defer cancel()

			start := time.Now()
			result, qerr := store.Query(ctx, wrappedQuery)
			if qerr != nil {
				if ctx.Err() == context.DeadlineExceeded {
					contractError(c, http.StatusRequestTimeout, "query_timeout", "Query execution timed out.")
					return nil
				}
				contractError(c, http.StatusInternalServerError, "internal_error", "Failed to execute query.")
				return nil
			}

			truncated := false
			rows := result.Rows
			if len(rows) > rowCap {
				truncated = true
				rows = rows[:rowCap]
			}

			c.JSON(http.StatusOK, gin.H{
				"columns":           result.Columns,
				"rows":              rows,
				"row_count":         len(rows),
				"truncated":         truncated,
				"execution_time_ms": time.Since(start).Milliseconds(),
			})
			return nil
		})
		if errors.Is(err, ErrStoreUnavailable) {
			contractError(c, http.StatusServiceUnavailable, "db_unavailable", "Database is unavailable.")
			return
		}
	}
}
