package digest

import (
	"context"
	"fmt"

	"github.com/dbsmedya/gofast/pkg/models"
)

// Querier is the slice of storage.Storage this package needs. Taking an
// interface keeps Run testable and documents that the digest only ever reads.
type Querier interface {
	Query(ctx context.Context, query string, args ...interface{}) (*models.ReportResult, error)
}

// Run executes the digest in four passes: population totals, a cheap ranking
// pass that narrows to o.Limit groups, then the full statistics matrix and the
// representative-row selection for just those groups. Splitting the work this
// way keeps percentile and standard deviation computation off every group in a
// large store.
func Run(ctx context.Context, q Querier, o Options) (*Report, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}

	rep := &Report{
		GroupBy: string(o.GroupBy),
		OrderBy: o.OrderBy.String(),
		Limit:   o.Limit,
		Classes: []Class{}, // never nil: JSON must render [] rather than null
	}

	// Pass 1 — population totals, feeding every percentage.
	totals, err := runGlobals(ctx, q, o)
	if err != nil {
		return nil, err
	}
	rep.Total = totals

	// Pass 2 — ranking.
	keys, err := runRanking(ctx, q, o)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return rep, nil
	}

	// Pass 3 — statistics for the ranked keys. The totals from pass 1 are
	// threaded in because every Stats.Pct is relative to them.
	statsByKey, err := runStats(ctx, q, o, keys, rep.Total)
	if err != nil {
		return nil, err
	}

	// Pass 4 — one representative row per group, for Item and sample_sql.
	repByKey, err := runRepresentative(ctx, q, o, keys)
	if err != nil {
		return nil, err
	}

	for i, key := range keys {
		cls, ok := statsByKey[key]
		if !ok {
			// Pass 3 lost a key that pass 2 produced. With COALESCE on both
			// sides this should be impossible; surface it rather than emit a
			// silently short report.
			return nil, fmt.Errorf("internal: group %q ranked but has no statistics", key)
		}
		cls.Rank = i + 1
		if r, ok := repByKey[key]; ok {
			cls.SampleSQL = r.sampleSQL
			cls.Item = Distill(r.sanitizedSQL, r.tables)
		}
		rep.Classes = append(rep.Classes, cls)
	}
	return rep, nil
}

// representative is one group's chosen row.
type representative struct {
	sampleSQL    string
	sanitizedSQL string
	tables       []string
}

// runRepresentative returns the representative row per group key.
func runRepresentative(ctx context.Context, q Querier, o Options, keys []string) (map[string]representative, error) {
	sql, args := buildRepresentativeSQL(o, keys)
	res, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("digest representative rows: %w", err)
	}
	out := make(map[string]representative, len(res.Rows))
	for _, row := range res.Rows {
		if len(row) < repWidth {
			return nil, fmt.Errorf("internal: representative row has %d columns, want %d", len(row), repWidth)
		}
		out[toStr(row[colRepGroup])] = representative{
			sampleSQL:    toStr(row[colRepSampleSQL]),
			sanitizedSQL: toStr(row[colRepSanitizedSQL]),
			tables:       toStrs(row[colRepTables]),
		}
	}
	return out, nil
}

func runGlobals(ctx context.Context, q Querier, o Options) (Totals, error) {
	sql, args := buildGlobalsSQL(o.Filter)
	res, err := q.Query(ctx, sql, args...)
	if err != nil {
		return Totals{}, fmt.Errorf("digest totals: %w", err)
	}
	if len(res.Rows) == 0 {
		return Totals{}, nil
	}
	r := res.Rows[0]
	return Totals{
		Calls:        toInt64(r[0]),
		ExecTimeSec:  toFloat(r[1]),
		LockTimeSec:  toFloat(r[2]),
		RowsSent:     toFloat(r[3]),
		RowsExamined: toFloat(r[4]),
	}, nil
}

// runRanking returns the ordered group keys, worst first.
func runRanking(ctx context.Context, q Querier, o Options) ([]string, error) {
	sql, args := buildRankSQL(o)
	res, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("digest ranking: %w", err)
	}
	keys := make([]string, 0, len(res.Rows))
	for _, row := range res.Rows {
		keys = append(keys, toStr(row[colRankGroup]))
	}
	return keys, nil
}

// runStats returns one Class per group key, without Rank (Run assigns it from
// the ranking order). totals supplies the denominators for every percentage.
func runStats(ctx context.Context, q Querier, o Options, keys []string, totals Totals) (map[string]Class, error) {
	sql, args := buildStatsSQL(o, keys)
	res, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("digest statistics: %w", err)
	}

	out := make(map[string]Class, len(res.Rows))
	for _, row := range res.Rows {
		if len(row) < statsWidth {
			return nil, fmt.Errorf("internal: statistics row has %d columns, want %d", len(row), statsWidth)
		}

		calls := toInt64(row[colStatsCalls])
		exec := readStats(row, colStatsQueryTime, totals.ExecTimeSec)

		// SampleSQL and Item are filled in by Run from pass 4.
		cls := Class{
			ID:    toStr(row[colStatsGroup]),
			Calls: calls,
			Attributes: map[string]Stats{
				AttrKeyExecTime:     exec,
				AttrKeyLockTime:     readStats(row, colStatsLockTime, totals.LockTimeSec),
				AttrKeyRowsSent:     readStats(row, colStatsRowsSent, totals.RowsSent),
				AttrKeyRowsExamined: readStats(row, colStatsRowsExamined, totals.RowsExamined),
			},
		}
		cls.ResponseTimePct = exec.Pct
		cls.RPerCall = exec.Avg
		// V/M is the variance-to-mean ratio of response time: a high value
		// marks an erratic query. NULL var_samp (single-row group) is already
		// coerced to 0 by toFloat.
		if exec.Avg != 0 {
			cls.VM = toFloat(row[colStatsVarSamp]) / exec.Avg
		}
		out[cls.ID] = cls
	}
	return out, nil
}

// readStats pulls one attribute's seven statistics starting at base.
func readStats(row []interface{}, base int, populationTotal float64) Stats {
	total := toFloat(row[base])
	return Stats{
		Total:  total,
		Min:    toFloat(row[base+1]),
		Max:    toFloat(row[base+2]),
		Avg:    toFloat(row[base+3]),
		P95:    toFloat(row[base+4]),
		Stddev: toFloat(row[base+5]),
		Median: toFloat(row[base+6]),
		Pct:    pct(total, populationTotal),
	}
}
