package namepolicy

import (
	"fmt"
	"regexp"
	"strings"
)

var sqlColumn = regexp.MustCompile(`^[a-z_][a-z0-9_]*\.[a-z_][a-z0-9_]*$`)

func runeClass(ranges []RuneRange) string {
	var b strings.Builder
	b.WriteByte('[')
	for _, span := range ranges {
		b.WriteRune(span.Lo)
		if span.Hi != span.Lo {
			b.WriteByte('-')
			b.WriteRune(span.Hi)
		}
	}
	b.WriteByte(']')
	return b.String()
}

// AllowsSQL is equivalent to Evaluate for actual UTF-8 PostgreSQL release
// names and hashes. Its arguments are trusted qualified column names, never
// request values. A disabled policy leaves existing query text and plans alone.
func (p *Policy) AllowsSQL(nameColumn, hashColumn string) (string, []any) {
	if !p.Enabled() {
		return "TRUE", nil
	}
	if !sqlColumn.MatchString(nameColumn) || !sqlColumn.MatchString(hashColumn) {
		panic("name policy: untrusted SQL column")
	}
	spec := p.Specification()
	name := "(" + nameColumn + ` COLLATE "C")`
	args := []any{spec.Whitespace, runeClass(append(spec.Han, spec.Cyrillic...))}
	patterns := make(map[string]string, len(spec.AdultTerms))
	for _, term := range spec.AdultTerms {
		patterns[term.Name] = term.Pattern
	}
	pairs := make([]string, 0, len(spec.ExplicitAdultPairs))
	for _, pair := range spec.ExplicitAdultPairs {
		pairs = append(pairs, "("+name+" ~ ? AND "+name+" ~ ?)")
		args = append(args, patterns[pair[0]], patterns[pair[1]])
	}
	strong := make([]string, 0, len(spec.AdultTerms))
	for _, term := range spec.AdultTerms {
		if term.Strong {
			strong = append(strong, name+" ~ ?")
			args = append(args, term.Pattern)
		}
	}
	counts := make([]string, 0, len(spec.AdultTerms))
	for _, term := range spec.AdultTerms {
		counts = append(counts, "CASE WHEN "+name+" ~ ? THEN 1 ELSE 0 END")
		args = append(args, term.Pattern)
	}
	// Only scalar name predicates are in CASE. The independent serving joins
	// remain outside it. Missing/script cases avoid every composite-token regex;
	// ordinary names with no strong anchor avoid the distinct-term tally.
	sql := "CASE WHEN COALESCE(btrim(" + nameColumn + ",?), '')='' THEN FALSE WHEN " + name + " ~ ? THEN FALSE"
	if len(pairs) > 0 {
		sql += " WHEN (" + strings.Join(pairs, " OR ") + ") THEN FALSE"
	}
	sql += " WHEN (" +
		strings.Join(strong, " OR ") + ") THEN (" + strings.Join(counts, " + ") + ") < " +
		fmt.Sprint(spec.MinimumDistinctAdultTerms) + " ELSE TRUE END"
	if excluded := p.ExcludedHashes(); len(excluded) > 0 {
		marks := make([]string, len(excluded))
		for i, hash := range excluded {
			// Counts use the existing ToSQL path. Explicit hex decoding keeps
			// prepared queries and that rendered SQL bytea-equivalent, without
			// GORM expanding a naked byte slice into separate integer values.
			marks[i] = "decode(?,'hex')"
			args = append(args, hash.String())
		}
		sql = "(" + sql + ") AND " + hashColumn + " NOT IN (" + strings.Join(marks, ",") + ")"
	}
	return sql, args
}
