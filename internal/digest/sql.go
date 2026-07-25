package digest

import (
	"fmt"
	"strings"

	"github.com/dbsmedya/gofast/pkg/storage"
)

// view is the enrichment view every pass reads; it adds source_host to
// slow_logs via the files join.
const view = "slow_logs_with_source"

// unnestFrom is the FROM clause used when grouping by tables. A query touching
// N tables contributes one row to each of its N groups.
const unnestFrom = view + ", UNNEST(tables) AS u(t)"

// Pass-2 (ranking) result layout.
const (
	colRankGroup = 0
	colRankCalls = 1
	colRankValue = 2
)

// Stats column layout for the pass-3 result. run.go indexes by position, so
// these constants and the SELECT list in buildStatsSQL must change together.
const (
	statsPerAttr = 7 // sum, min, max, avg, p95, stddev, median

	colStatsGroup        = 0
	colStatsCalls        = 1
	colStatsQueryTime    = 2                                   // .. +6
	colStatsVarSamp      = colStatsQueryTime + statsPerAttr    // 9
	colStatsLockTime     = colStatsVarSamp + 1                 // 10 .. +6
	colStatsRowsSent     = colStatsLockTime + statsPerAttr     // 17 .. +6
	colStatsRowsExamined = colStatsRowsSent + statsPerAttr     // 24 .. +6
	statsWidth           = colStatsRowsExamined + statsPerAttr // 31
)

// Pass-4 (representative row) result layout.
const (
	colRepGroup        = 0
	colRepSampleSQL    = 1
	colRepSanitizedSQL = 2
	colRepTables       = 3
	repWidth           = 4
)

// groupExpr renders the grouping expression for o.
//
// COALESCE is not cosmetic: user, host, db and source_host are all nullable
// (source_host comes from a LEFT JOIN), and a NULL key cannot survive pass 3's
// `IN (?)` lookup because NULL IN (NULL) evaluates to NULL, not true. Verified
// against DuckDB: the uncoalesced form silently drops the group. The empty
// string it produces renders as "(none)".
func groupExpr(o Options) string {
	return fmt.Sprintf("COALESCE(%s, '')", o.GroupBy.column())
}

// fromClause picks the plain view or the UNNEST fan-out.
func fromClause(o Options) string {
	if o.GroupBy.isTables() {
		return unnestFrom
	}
	return view
}

// aggExpr renders the ordering aggregate. "cnt" ignores the attribute and
// counts rows, matching pt-query-digest.
//
// Both halves come from the allow-lists in options.go, so this interpolation
// only ever emits a fixed literal.
func aggExpr(o OrderSpec) string {
	if o.Agg == AggCnt {
		return "COUNT(*)"
	}
	return fmt.Sprintf("%s(%s)", strings.ToUpper(string(o.Agg)), o.Attr.column())
}

// buildGlobalsSQL is pass 1: population-wide totals feeding every Stats.Pct.
//
// It deliberately reads the plain view even when grouping by tables, so the
// denominators stay the real totals rather than fan-out-inflated ones.
func buildGlobalsSQL(f storage.RecordFilter) (string, []any) {
	where, args := f.BuildWhere()
	q := `
		SELECT CAST(COUNT(*) AS DOUBLE),
		       CAST(SUM(query_time_sec) AS DOUBLE),
		       CAST(SUM(lock_time_sec) AS DOUBLE),
		       CAST(SUM(rows_sent) AS DOUBLE),
		       CAST(SUM(rows_examined) AS DOUBLE)
		FROM ` + view + `
		` + where
	return q, args
}

// buildRankSQL is pass 2: the cheap ranking pass. It computes only the ordering
// aggregate — no percentiles — so narrowing to the top N stays fast on large
// stores.
func buildRankSQL(o Options) (string, []any) {
	where, args := o.Filter.BuildWhere()
	grp := groupExpr(o)
	q := fmt.Sprintf(`
		SELECT %s AS grp,
		       CAST(COUNT(*) AS DOUBLE) AS calls,
		       CAST(%s AS DOUBLE) AS rank_val
		FROM %s
		%s
		GROUP BY grp
		ORDER BY rank_val DESC, grp ASC
		LIMIT ?`, grp, aggExpr(o.OrderBy), fromClause(o), where)
	args = append(args, o.Limit)
	return q, args
}

// statFragment renders the seven statistics for one attribute column. Every one
// is cast to DOUBLE so Go always receives a float64 (SUM over a UBIGINT column
// otherwise arrives as *big.Int).
func statFragment(col string) string {
	return fmt.Sprintf(`CAST(SUM(%[1]s) AS DOUBLE),
		       CAST(MIN(%[1]s) AS DOUBLE),
		       CAST(MAX(%[1]s) AS DOUBLE),
		       CAST(AVG(%[1]s) AS DOUBLE),
		       CAST(quantile_cont(%[1]s, 0.95) AS DOUBLE),
		       CAST(stddev_samp(%[1]s) AS DOUBLE),
		       CAST(quantile_cont(%[1]s, 0.5) AS DOUBLE)`, col)
}

// keyedWhere appends a bound `grp IN (...)` predicate to the filter's WHERE
// clause, shared by passes 3 and 4.
func keyedWhere(o Options, keys []string) (string, []any) {
	where, args := o.Filter.BuildWhere()
	ph := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	cond := fmt.Sprintf("%s IN (%s)", groupExpr(o), ph)
	if where == "" {
		where = "WHERE " + cond
	} else {
		where += " AND " + cond
	}
	for _, k := range keys {
		args = append(args, k)
	}
	return where, args
}

// buildStatsSQL is pass 3: the full statistics matrix, restricted to the group
// keys pass 2 returned. keys are bound as placeholders.
//
// It carries no representative columns. Three independent ANY_VALUE calls have
// no guarantee of agreeing on a row, which would let Item be distilled from one
// row's sanitized_sql and another row's tables; pass 4 selects one row
// explicitly instead.
func buildStatsSQL(o Options, keys []string) (string, []any) {
	where, args := keyedWhere(o, keys)
	q := fmt.Sprintf(`
		SELECT %s AS grp,
		       CAST(COUNT(*) AS DOUBLE),
		       %s,
		       CAST(var_samp(query_time_sec) AS DOUBLE),
		       %s,
		       %s,
		       %s
		FROM %s
		%s
		GROUP BY grp`,
		groupExpr(o),
		statFragment("query_time_sec"),
		statFragment("lock_time_sec"),
		statFragment("rows_sent"),
		statFragment("rows_examined"),
		fromClause(o),
		where)
	return q, args
}

// buildRepresentativeSQL is pass 4: one representative row per group.
//
// The rule is: within each group rank fingerprints by call count (ties broken on
// fingerprint_id), take the winner — the query the group runs most often — and
// from its rows take the slowest. All three fields share one arg_max ordering
// expression, so they provably come from the same row. Verified against DuckDB
// for both VARCHAR and VARCHAR[].
//
// For --group-by fingerprint the ranking step is a no-op, so this degenerates to
// "the slowest instance of this query".
func buildRepresentativeSQL(o Options, keys []string) (string, []any) {
	where, args := keyedWhere(o, keys)
	q := fmt.Sprintf(`
		WITH per_fp AS (
		  SELECT %s AS grp,
		         fingerprint_id,
		         COUNT(*) AS fp_calls,
		         arg_max(sample_sql, query_time_sec)    AS sample_sql,
		         arg_max(sanitized_sql, query_time_sec) AS sanitized_sql,
		         arg_max(tables, query_time_sec)        AS tbls
		  FROM %s
		  %s
		  GROUP BY grp, fingerprint_id
		),
		ranked AS (
		  SELECT *, ROW_NUMBER() OVER (
		             PARTITION BY grp
		             ORDER BY fp_calls DESC, fingerprint_id ASC) AS rn
		  FROM per_fp
		)
		SELECT grp, sample_sql, sanitized_sql, tbls FROM ranked WHERE rn = 1`,
		groupExpr(o), fromClause(o), where)
	return q, args
}
