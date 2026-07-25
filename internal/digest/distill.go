package digest

import "strings"

// Distill renders pt-query-digest's "Item" column: the leading SQL verb
// followed by the tables the query touches, e.g. "SELECT orders".
//
// It is always the distilled representative query, including for non-fingerprint
// groupings — under --group-by user the Query ID column already shows the
// username, so repeating it here would waste the column.
func Distill(sanitizedSQL string, tables []string) string {
	parts := make([]string, 0, len(tables)+1)

	if verb := leadingVerb(sanitizedSQL); verb != "" {
		parts = append(parts, verb)
	}
	for _, t := range tables {
		if clean := strings.Trim(strings.TrimSpace(t), `"`+"`"); clean != "" {
			parts = append(parts, clean)
		}
	}
	return strings.Join(parts, " ")
}

// leadingVerb returns the first whitespace-delimited token, uppercased.
func leadingVerb(sql string) string {
	for field := range strings.FieldsSeq(sql) {
		return strings.ToUpper(field)
	}
	return ""
}
