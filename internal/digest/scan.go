package digest

import "math/big"

// toFloat coerces a driver value to float64.
//
// The SQL wraps every aggregate in CAST(... AS DOUBLE) so values normally
// arrive as float64, but this stays defensive: DuckDB returns *big.Int for
// SUM over UBIGINT columns, uint64 for MIN/MAX over them, int64 for COUNT(*),
// and nil for stddev_samp/var_samp over a single-row group. A nil (SQL NULL)
// becomes 0, which is what the spec requires for the single-row case.
func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int64:
		return float64(n)
	case int32:
		return float64(n)
	case int:
		return float64(n)
	case uint64:
		return float64(n)
	case uint32:
		return float64(n)
	case *big.Int:
		if n == nil {
			return 0
		}
		f, _ := new(big.Float).SetInt(n).Float64()
		return f
	default:
		return 0
	}
}

// toInt64 coerces a driver value to int64, for counts.
func toInt64(v any) int64 { return int64(toFloat(v)) }

// toStr coerces a driver value to string; SQL NULL becomes "".
func toStr(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []byte:
		return string(s)
	default:
		return ""
	}
}

// toStrs coerces a driver value to a string slice. Storage.Query already
// converts DuckDB list columns to []string.
func toStrs(v any) []string {
	switch a := v.(type) {
	case []string:
		return a
	case []any:
		out := make([]string, 0, len(a))
		for _, item := range a {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// pct returns part/whole as a percentage, guarding a zero denominator (an
// all-zero attribute such as rows_sent over write-only traffic).
func pct(part, whole float64) float64 {
	if whole == 0 {
		return 0
	}
	return part / whole * 100
}
