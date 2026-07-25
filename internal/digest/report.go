package digest

// Attribute keys used in Class.Attributes and in the JSON output.
const (
	AttrKeyExecTime     = "exec_time"
	AttrKeyLockTime     = "lock_time"
	AttrKeyRowsSent     = "rows_sent"
	AttrKeyRowsExamined = "rows_examined"
)

// Stats is one attribute's statistics within a class, matching
// pt-query-digest's per-query detail columns.
type Stats struct {
	Pct    float64 `json:"pct"`
	Total  float64 `json:"total"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
	Avg    float64 `json:"avg"`
	P95    float64 `json:"p95"`
	Stddev float64 `json:"stddev"`
	Median float64 `json:"median"`
}

// Totals are the population-wide sums over the FILTERED set. Every Stats.Pct is
// computed against these, so filters change percentages by design.
type Totals struct {
	Calls        int64   `json:"calls"`
	ExecTimeSec  float64 `json:"exec_time_sec"`
	LockTimeSec  float64 `json:"lock_time_sec"`
	RowsSent     float64 `json:"rows_sent"`
	RowsExamined float64 `json:"rows_examined"`
}

// Class is one grouped query class — one row of the profile table plus its
// detail section.
type Class struct {
	Rank            int              `json:"rank"`
	ID              string           `json:"id"`
	Item            string           `json:"item"`
	Calls           int64            `json:"calls"`
	ResponseTimePct float64          `json:"response_time_pct"`
	RPerCall        float64          `json:"r_per_call"`
	VM              float64          `json:"v_m"`
	SampleSQL       string           `json:"sample_sql"`
	Attributes      map[string]Stats `json:"attributes"`
}

// Report is a complete digest run.
type Report struct {
	GroupBy string  `json:"group_by"`
	OrderBy string  `json:"order_by"`
	Limit   int     `json:"limit"`
	Total   Totals  `json:"total"`
	Classes []Class `json:"classes"`
}
