package contracttest

import (
	"bytes"
	"encoding/json"
	"regexp"
)

// rfc3339Pattern matches RFC3339-ish timestamp strings (with or without
// fractional seconds), e.g. "2024-01-15T10:00:00Z" or
// "2024-01-15T10:00:00.123456Z" or "2024-01-15T10:00:00+00:00".
var rfc3339Pattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$`)

// Normalize decodes a JSON response body and blanks the parts that are
// expected to vary from run to run, so the remaining shape can be diffed
// against a golden file.
//
// It always blanks:
//   - any object key named "request_id" (recursively, any nesting level)
//   - any string value that looks like an RFC3339 timestamp (recursively,
//     any nesting level, regardless of key name)
//
// When blankTopLevelExecTime is true, it additionally blanks the top-level
// "execution_time_ms" key (used only for the /sql/execute golden, whose
// value is wall-clock and therefore nondeterministic). It never blanks
// execution_time_ms nested inside other structures (e.g. /sql/queries
// record objects), because those values are deterministic aggregations and
// regressions in them must fail the golden comparison.
//
// The result is re-encoded with object keys sorted (Go's encoding/json
// sorts map[string]interface{} keys on Marshal) and 2-space indentation,
// for a stable, readable diff.
func Normalize(body []byte, blankTopLevelExecTime bool) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()

	var root interface{}
	if err := dec.Decode(&root); err != nil {
		return nil, err
	}

	normalized := normalizeValue(root, true, blankTopLevelExecTime)

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(normalized); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func normalizeValue(v interface{}, topLevel bool, blankTopLevelExecTime bool) interface{} {
	switch val := v.(type) {
	case map[string]interface{}:
		for k, cv := range val {
			switch {
			case k == "request_id":
				val[k] = ""
			case topLevel && blankTopLevelExecTime && k == "execution_time_ms":
				val[k] = json.Number("0")
			default:
				val[k] = normalizeValue(cv, false, blankTopLevelExecTime)
			}
		}
		return val
	case []interface{}:
		for i, item := range val {
			val[i] = normalizeValue(item, false, blankTopLevelExecTime)
		}
		return val
	case string:
		if rfc3339Pattern.MatchString(val) {
			return ""
		}
		return val
	default:
		return val
	}
}
