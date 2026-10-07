// Package catalogueguard owns shared native protection predicates.
package catalogueguard

import (
	"fmt"
	"strings"
)

const tagSeparators = ":/-_"

// TagWhitespace is the Unicode White_Space set used by strings.TrimSpace.
// SQL readers use the same set so case/whitespace handling does not diverge.
const TagWhitespace = "\t\n\v\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000"

func qualified(name, prefix string) bool {
	return name == prefix || (strings.HasPrefix(name, prefix) && len(name) > len(prefix) && strings.ContainsRune(tagSeparators, rune(name[len(prefix)])))
}

// ProtectedTag recognises recorded override/privacy tags and their qualified
// forms. Wanted protects maintenance/removal but may remain eligible for ordinary
// public matching; callers choose that policy explicitly.
func ProtectedTag(name string, includeWanted bool) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, prefix := range []string{"manual", "reference", "bitgrab", "wanted"} {
		if prefix == "wanted" && !includeWanted {
			continue
		}
		if qualified(name, prefix) {
			return true
		}
	}
	return false
}

// PrivacyTag distinguishes bitgrab privacy from ordinary public authority and
// wanted tags. It is the only tag family that revokes a restored bloom exception.
func PrivacyTag(name string) bool {
	return qualified(strings.ToLower(strings.TrimSpace(name)), "bitgrab")
}

// PrivacyTagSQL emits the same privacy predicate for a single-snapshot SQL
// reader. Arguments are trusted column/placeholder expressions, never input.
// Supply TagWhitespace for the whitespace placeholder.
func PrivacyTagSQL(column, whitespace string) string {
	normal := fmt.Sprintf("lower(btrim(%s,%s))", column, whitespace)
	return fmt.Sprintf("(%s='bitgrab' or (left(%s,7)='bitgrab' and substring(%s from 8 for 1) in(':','/','-','_')))", normal, normal, normal)
}
