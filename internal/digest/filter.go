package digest

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/dbsmedya/gofast/pkg/storage"
)

// filterKeyList is the human-facing list used in error messages.
var filterKeyList = []string{
	"fingerprint", "user", "host", "db", "source_host", "table",
	"query_time", "lock_time", "rows_examined",
}

// ParseFilters turns repeated "KEY OP VALUE" strings into a RecordFilter.
// Filters are AND-combined. user/host/db/source_host accumulate into IN-lists
// (repeat the flag or use a comma list) and mean "either"; numeric keys
// overwrite, because RecordFilter holds a single bound per comparison.
//
// `table` is restricted to ONE value. BuildWhere() renders each table value as
// its own list_contains predicate and joins predicates with AND
// (pkg/storage/records.go:104), so a comma list would mean "touches both" while
// the identical syntax on `user` means "either". Shipping two opposite meanings
// for one syntax is worse than rejecting the ambiguous case.
//
// Every value ends up as a bound `?` via RecordFilter.BuildWhere(), so no user
// input reaches SQL text.
func ParseFilters(exprs []string) (storage.RecordFilter, error) {
	var f storage.RecordFilter
	for _, raw := range exprs {
		key, op, val, err := splitFilter(raw)
		if err != nil {
			return f, err
		}
		if err := applyFilter(&f, key, op, val, raw); err != nil {
			return f, err
		}
	}
	if len(f.Tables) > 1 {
		return f, fmt.Errorf(
			"--filter table accepts a single value (got %d); "+
				"use --group-by tables for a per-table breakdown", len(f.Tables))
	}
	return f, nil
}

// splitFilter separates "KEY OP VALUE". The two-character operators are not
// supported, so a single scan for the first operator character is enough.
func splitFilter(raw string) (key, op, val string, err error) {
	s := strings.TrimSpace(raw)
	idx := strings.IndexAny(s, "=<>")
	if idx < 0 {
		return "", "", "", fmt.Errorf(
			"invalid --filter %q: missing an operator (expected KEY = VALUE, KEY > VALUE, or KEY < VALUE)", raw)
	}
	key = strings.ToLower(strings.TrimSpace(s[:idx]))
	op = string(s[idx])
	val = strings.TrimSpace(s[idx+1:])
	if key == "" {
		return "", "", "", fmt.Errorf("invalid --filter %q: empty key", raw)
	}
	if val == "" {
		return "", "", "", fmt.Errorf("invalid --filter %q: empty value", raw)
	}
	return key, op, val, nil
}

// splitList expands a comma list into trimmed, non-empty values.
func splitList(val string) []string {
	parts := strings.Split(val, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func applyFilter(f *storage.RecordFilter, key, op, val, raw string) error {
	// String keys: equality only, accumulating into IN-lists.
	stringTargets := map[string]*[]string{
		"user":        &f.Users,
		"host":        &f.Hosts,
		"db":          &f.DBs,
		"source_host": &f.SourceHosts,
		"table":       &f.Tables,
		"tables":      &f.Tables, // alias, matching --group-by
	}
	if target, ok := stringTargets[key]; ok {
		if op != "=" {
			return fmt.Errorf("--filter %q: %s supports only '='", raw, key)
		}
		vals := splitList(val)
		if len(vals) == 0 {
			// "user = ," trims to nothing; without this the filter would
			// silently vanish and the command would run unfiltered.
			return fmt.Errorf("--filter %q: no values given for %s", raw, key)
		}
		*target = append(*target, vals...)
		return nil
	}

	if key == "fingerprint" {
		if op != "=" {
			return fmt.Errorf("--filter %q: fingerprint supports only '='", raw)
		}
		f.FingerprintID = val
		return nil
	}

	num := func() (float64, error) {
		v, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return 0, fmt.Errorf("--filter %q: %q is not a number", raw, val)
		}
		return v, nil
	}

	switch key {
	case "query_time":
		v, err := num()
		if err != nil {
			return err
		}
		switch op {
		case ">":
			f.QueryTimeGT = &v
		case "<":
			f.QueryTimeLT = &v
		default:
			return fmt.Errorf("--filter %q: query_time supports only '>' and '<'", raw)
		}
	case "lock_time":
		v, err := num()
		if err != nil {
			return err
		}
		switch op {
		case ">":
			f.LockTimeGT = &v
		case "<":
			f.LockTimeLT = &v
		default:
			return fmt.Errorf("--filter %q: lock_time supports only '>' and '<'", raw)
		}
	case "rows_examined":
		if op != ">" {
			return fmt.Errorf("--filter %q: rows_examined supports only '>'", raw)
		}
		v, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return fmt.Errorf("--filter %q: %q is not a whole number", raw, val)
		}
		f.RowsExaminedGT = &v
	case "rows_sent":
		// Reported but not filterable: RecordFilter has no RowsSent field and
		// adding one would change pkg/, which this feature does not do.
		return fmt.Errorf("--filter %q: rows_sent is reported but cannot be filtered; valid keys: %s",
			raw, strings.Join(filterKeyList, ", "))
	default:
		return fmt.Errorf("--filter %q: unknown key %q; valid keys: %s",
			raw, key, strings.Join(filterKeyList, ", "))
	}
	return nil
}

// relativeUnits maps pt-query-digest's suffixes to durations.
var relativeUnits = map[byte]time.Duration{
	's': time.Second,
	'm': time.Minute,
	'h': time.Hour,
	'd': 24 * time.Hour,
}

// ParseTimeBound accepts pt-query-digest's relative form (N[shmd], bare integer
// meaning seconds) resolved against now, or an absolute "YYYY-MM-DD" /
// "YYYY-MM-DD HH:MM:SS".
//
// Absolute values are interpreted in the LOCAL zone, because the slow-log
// parser stores timestamps that way: pkg/parser/engine.go passes an empty
// log.Options{}, so percona's parser falls back to time.Local. Using UTC here
// would silently offset every comparison.
func ParseTimeBound(s string, now time.Time) (time.Time, error) {
	v := strings.TrimSpace(s)
	if v == "" {
		return time.Time{}, fmt.Errorf("empty time value")
	}

	// Relative: all digits, optionally followed by one unit suffix.
	unit := time.Second
	digits := v
	if last := v[len(v)-1]; last < '0' || last > '9' {
		u, ok := relativeUnits[last]
		if ok {
			unit = u
			digits = v[:len(v)-1]
		} else {
			digits = "" // force the absolute path below
		}
	}
	if digits != "" {
		if n, err := strconv.ParseInt(digits, 10, 64); err == nil {
			return now.Add(-time.Duration(n) * unit), nil
		}
	}

	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, v, time.Local); err == nil {
			return t, nil
		}
	}

	return time.Time{}, fmt.Errorf(
		"invalid time value %q: expected a relative value like 7d, 90m, 2h, 30s, "+
			"or an absolute \"YYYY-MM-DD\" / \"YYYY-MM-DD HH:MM:SS\" "+
			"(MySQL expressions such as CURRENT_DATE - INTERVAL 7 DAY are not supported)", s)
}

// ApplyTimeBounds parses since/until (either may be empty) onto f as the
// inclusive TSFrom/TSTo bounds.
func ApplyTimeBounds(f *storage.RecordFilter, since, until string, now time.Time) error {
	if strings.TrimSpace(since) != "" {
		t, err := ParseTimeBound(since, now)
		if err != nil {
			return fmt.Errorf("--since: %w", err)
		}
		f.TSFrom = &t
	}
	if strings.TrimSpace(until) != "" {
		t, err := ParseTimeBound(until, now)
		if err != nil {
			return fmt.Errorf("--until: %w", err)
		}
		f.TSTo = &t
	}
	if f.TSFrom != nil && f.TSTo != nil && f.TSFrom.After(*f.TSTo) {
		return fmt.Errorf("--since (%s) is after --until (%s)",
			f.TSFrom.Format(time.RFC3339), f.TSTo.Format(time.RFC3339))
	}
	return nil
}
