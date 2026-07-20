package storage

import (
	"context"
	"strings"
	"time"

	"github.com/dbsmedya/gofast/pkg/models"
)

// allowedOrderColumns is the shared allow-list for individual-record ORDER BY
// columns. ORDER BY column/direction are SQL identifiers, not values, so they
// can never be bound as `?` placeholders — the injection is closed instead by
// only ever interpolating a literal drawn from this fixed allow-list.
var allowedOrderColumns = map[string]bool{
	"id": true, "fingerprint_id": true, "sanitized_sql": true,
	"sample_sql": true, "user": true, "host": true, "db": true,
	"query_time_sec": true, "lock_time_sec": true, "rows_sent": true,
	"rows_examined": true, "ts": true, "created_at": true,
}

// validateOrder returns an (orderBy, orderDir) pair safe to interpolate
// directly into an ORDER BY clause: orderBy falls back to "id" when it is not
// in allowedOrderColumns, and orderDir falls back to "asc" unless it is
// exactly "desc".
func validateOrder(orderBy, orderDir string) (string, string) {
	if !allowedOrderColumns[orderBy] {
		orderBy = "id"
	}
	if orderDir != "asc" && orderDir != "desc" {
		orderDir = "asc"
	}
	return orderBy, orderDir
}

// ListRecords returns individual slow-log rows (preview columns), ordered by
// an allow-listed column/dir, paginated. orderBy outside the allow-list falls
// back to "id". LIMIT/OFFSET are bound as `?` ints (never string-interpolated).
func (s *Storage) ListRecords(ctx context.Context, orderBy, orderDir string, limit, offset int) (*models.ReportResult, error) {
	orderBy, orderDir = validateOrder(orderBy, orderDir)

	q := `
		SELECT
			id, fingerprint_id,
			SUBSTRING(sanitized_sql, 1, 200) as sanitized_sql_preview,
			SUBSTRING(sample_sql, 1, 200) as sample_sql_preview,
			user, host, db, query_time_sec, lock_time_sec,
			rows_sent, rows_examined, ts, created_at, source_host
		FROM slow_logs_with_source
		ORDER BY ` + orderBy + ` ` + orderDir + `
		LIMIT ? OFFSET ?
	`
	return s.Query(ctx, q, limit, offset)
}

// GetRecordByID returns the single full row for a numeric id, or Count==0 if
// absent. `WHERE id = ?` binds id as an int64 — no string value can ever
// reach this clause.
func (s *Storage) GetRecordByID(ctx context.Context, id int64) (*models.ReportResult, error) {
	const q = `
		SELECT
			id, fingerprint_id, sanitized_sql, sample_sql,
			user, host, db, query_time_sec, lock_time_sec,
			rows_sent, rows_examined, ts, created_at, tables, source_host
		FROM slow_logs_with_source
		WHERE id = ?
	`
	return s.Query(ctx, q, id)
}

// RecordFilter holds validated record filters (nil pointer = filter absent).
type RecordFilter struct {
	Users, Hosts, DBs, Tables, SourceHosts []string
	FingerprintID                          string
	QueryTimeGT, QueryTimeLT               *float64
	LockTimeGT, LockTimeLT                 *float64
	RowsExaminedGT                         *int64
	TSFrom, TSTo                           *time.Time
	FilePath                               string
}

// BuildWhere renders f as a parameterized "WHERE ..." clause (or "" when
// empty) plus its bound args, in statement order. Exported so downstream
// consumers reuse this one SQLi-safe filter implementation: every value is
// bound as a `?` placeholder instead of being spliced into the SQL text.
func (f RecordFilter) BuildWhere() (string, []interface{}) {
	var conds []string
	var args []interface{}
	inClause := func(col string, vals []string) {
		if len(vals) == 0 {
			return
		}
		ph := make([]string, len(vals))
		for i, v := range vals {
			ph[i] = "?"
			args = append(args, strings.TrimSpace(v))
		}
		conds = append(conds, col+" IN ("+strings.Join(ph, ",")+")")
	}
	inClause("user", f.Users)
	inClause("host", f.Hosts)
	inClause("db", f.DBs)
	inClause("source_host", f.SourceHosts)
	for _, tbl := range f.Tables {
		t := strings.TrimSpace(tbl)
		conds = append(conds, "(list_contains(tables, ?) OR list_contains(tables, ?))")
		args = append(args, `"`+t+`"`, t)
	}
	if f.FingerprintID != "" {
		conds = append(conds, "fingerprint_id = ?")
		args = append(args, f.FingerprintID)
	}
	addF := func(col, op string, v *float64) {
		if v != nil {
			conds = append(conds, col+" "+op+" ?")
			args = append(args, *v)
		}
	}
	addF("query_time_sec", ">", f.QueryTimeGT)
	addF("query_time_sec", "<", f.QueryTimeLT)
	addF("lock_time_sec", ">", f.LockTimeGT)
	addF("lock_time_sec", "<", f.LockTimeLT)
	if f.RowsExaminedGT != nil {
		conds = append(conds, "rows_examined > ?")
		args = append(args, *f.RowsExaminedGT)
	}
	if f.TSFrom != nil {
		conds = append(conds, "ts >= ?")
		args = append(args, *f.TSFrom)
	}
	if f.TSTo != nil {
		conds = append(conds, "ts <= ?")
		args = append(args, *f.TSTo)
	}
	if f.FilePath != "" {
		conds = append(conds, "file_hash = (SELECT hash FROM files WHERE file_path = ? ORDER BY generation DESC LIMIT 1)")
		args = append(args, f.FilePath) // canonicalize at the Phase-2 handler via parser.CanonicalPath
	}
	if len(conds) == 0 {
		return "", args
	}
	return "WHERE " + strings.Join(conds, " AND "), args
}

// FilterRecords returns individual (ungrouped) filtered rows, ordered by an
// allow-listed column/dir, paginated. Grouped aggregations are not provided
// here (a downstream consumer composes BuildWhere with its own templates).
// The WHERE clause is f.BuildWhere()'s parameterized output, and LIMIT/OFFSET
// are bound as `?` ints appended after the WHERE args (never interpolated).
func (s *Storage) FilterRecords(ctx context.Context, f RecordFilter, orderBy, orderDir string, limit, offset int) (*models.ReportResult, error) {
	orderBy, orderDir = validateOrder(orderBy, orderDir)
	whereSQL, args := f.BuildWhere()

	q := `
		SELECT
			id, fingerprint_id,
			SUBSTRING(sanitized_sql, 1, 200) as sanitized_sql_preview,
			SUBSTRING(sample_sql, 1, 200) as sample_sql_preview,
			user, host, db, query_time_sec, lock_time_sec,
			rows_sent, rows_examined, ts, created_at, source_host
		FROM slow_logs_with_source
		` + whereSQL + `
		ORDER BY ` + orderBy + ` ` + orderDir + `
		LIMIT ? OFFSET ?
	`
	args = append(args, limit, offset)
	return s.Query(ctx, q, args...)
}

// FilterOptions returns distinct users/dbs/hosts/tables/source_hosts. These
// queries take no user input (static DISTINCT scans), so no parameterization
// is needed.
func (s *Storage) FilterOptions(ctx context.Context) (users, dbs, hosts, tables, sourceHosts []string, err error) {
	users, err = s.distinctStrings(ctx, `
		SELECT DISTINCT user
		FROM slow_logs
		WHERE user IS NOT NULL AND user != ''
		ORDER BY user
	`)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}

	dbs, err = s.distinctStrings(ctx, `
		SELECT DISTINCT db
		FROM slow_logs
		WHERE db IS NOT NULL AND db != ''
		ORDER BY db
	`)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}

	hosts, err = s.distinctStrings(ctx, `
		SELECT DISTINCT host
		FROM slow_logs
		WHERE host IS NOT NULL AND host != ''
		ORDER BY host
	`)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}

	tables, err = s.distinctStrings(ctx, `
		SELECT DISTINCT table_name
		FROM (
			SELECT UNNEST(tables) as table_name
			FROM slow_logs
			WHERE tables IS NOT NULL
		)
		WHERE table_name IS NOT NULL AND table_name != ''
		ORDER BY table_name
	`)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}

	sourceHosts, err = s.distinctStrings(ctx, `
		SELECT DISTINCT source_host
		FROM slow_logs_with_source
		WHERE source_host IS NOT NULL AND source_host != ''
		ORDER BY source_host
	`)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}

	return users, dbs, hosts, tables, sourceHosts, nil
}

// distinctStrings runs a single-column static query and collects the string
// values of column 0.
func (s *Storage) distinctStrings(ctx context.Context, q string) ([]string, error) {
	res, err := s.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(res.Rows))
	for _, row := range res.Rows {
		if v, ok := row[0].(string); ok {
			out = append(out, v)
		}
	}
	return out, nil
}
