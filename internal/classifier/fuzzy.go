package classifier

import (
	"math"
	"strings"
	"unicode"

	"github.com/spencercnorton/bitagent/internal/titlenorm"
)

// The canonical title normalizer lives in internal/titlenorm (extracted from
// this file so non-classifier packages — the family registry builder first —
// key titles identically without importing the classifier). These delegating
// wrappers keep the classifier's internal call sites and tests unchanged.

func normalizeTitleForMatch(s string) string { return titlenorm.NormalizeTitleForMatch(s) }

func kanaToRomaji(s string) string { return titlenorm.KanaToRomaji(s) }

func nativeMatchKey(s string) string { return titlenorm.NativeMatchKey(s) }

// tokenSetRatio returns the Jaccard similarity (intersection/union) of the
// word-token sets of normalised strings a and b, in [0, 1]. This is equivalent
// to a Jaccard over type sets (duplicate tokens are deduplicated).
// foldTitlePunct makes the fuzzy scorer punctuation-insensitive — which
// titlenorm.FamilyKey's comment has long assumed it was. NormalizeTitleForMatch
// keeps mid-title punctuation, but fuzzyFindBestMatch compares tokens by exact
// equality in its first-token precision gate and in tokenSetRatio, and it runs
// the gate BEFORE the Levenshtein distance that would have absorbed a comma.
// So "Diners, Drive-Ins and Dives" normalised to first token "diners," and was
// discarded against a release's "diners"; the 2026-09-22 benchmark measured
// 322 of 3,344 gold rows lost this way (docs/project/benchmarks.md).
//
// Apostrophes are deleted ("Grey's" -> "Greys", "'97" -> "97"); every other
// punctuation or symbol rune becomes a word break ("Drive-Ins" -> "Drive Ins",
// "Dexter:" -> "Dexter"). '&' is left for NormalizeTitleForMatch to expand to
// "and". Folding runs on the raw string BEFORE normalisation, so both sides of
// a comparison get identical article and roman-numeral handling: "X-Men" and
// "X Men" now normalise alike instead of to "x-men" and "10 men".
func foldTitlePunct(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\'' || r == '\u2019' || r == '\u2018' || r == '`':
			// dropped: an apostrophe joins, it does not separate
		case r == '&':
			b.WriteRune(r)
		case unicode.IsPunct(r) || unicode.IsSymbol(r):
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func tokenSetRatio(a, b string) float64 {
	aToks := tokenSet(a)
	bToks := tokenSet(b)

	if len(aToks) == 0 && len(bToks) == 0 {
		return 1.0
	}
	if len(aToks) == 0 || len(bToks) == 0 {
		return 0.0
	}

	intersect := 0
	for tok := range aToks {
		if bToks[tok] {
			intersect++
		}
	}

	union := len(aToks) + len(bToks) - intersect
	return float64(intersect) / float64(union)
}

// tokenSet splits a normalised string into its word tokens and returns them
// as a set (map[string]bool).
func tokenSet(s string) map[string]bool {
	fields := strings.Fields(s)
	set := make(map[string]bool, len(fields))
	for _, f := range fields {
		set[f] = true
	}
	return set
}

// fuzzyLevThreshold returns the length-scaled Levenshtein acceptance threshold:
//
//	min(levenshteinThreshold, ceil(0.15 * len(normalisedQuery)))
//
// This matches the existing constant for short titles and scales up for longer
// ones, improving recall on verbose titles without loosening precision on short ones.
func fuzzyLevThreshold(normQuery string) int {
	scaled := int(math.Ceil(0.15 * float64(len(normQuery))))
	if scaled < levenshteinThreshold {
		return scaled
	}
	return levenshteinThreshold
}

// firstToken returns the first whitespace-separated token of s, or "" if s is
// empty. Used in the precision gate.
func firstToken(s string) string {
	if s == "" {
		return ""
	}
	idx := strings.IndexByte(s, ' ')
	if idx == -1 {
		return s
	}
	return s[:idx]
}

// yearProximityPenalty returns a score penalty for a candidate whose release
// year differs from the query year. Exact match = 0, +-1 year = 1, anything
// else = a large constant (100) that makes exact-year matches strongly preferred
// when comparing two otherwise equal candidates. A zero queryYear or
// candidateYear means no penalty (year unknown -- treat as equal).
func yearProximityPenalty(queryYear, candidateYear int) int {
	if queryYear == 0 || candidateYear == 0 {
		return 0
	}
	diff := queryYear - candidateYear
	if diff < 0 {
		diff = -diff
	}
	switch diff {
	case 0:
		return 0
	case 1:
		return 1
	default:
		return 100
	}
}
