// Package titlenorm is the ONE canonical title normalizer. It was extracted
// verbatim from internal/classifier (fuzzy.go / romaji.go) so that packages
// outside the classifier — the title-family registry builder first — can key
// titles identically to the matcher without importing the classifier (which
// will itself consume the family registry, so a reverse import would cycle).
//
// Do not fork this logic. The repo historically accumulated ~11 divergent
// title normalizers; every new keying/matching surface must call this package.
package titlenorm

import (
	"strings"
	"unicode"

	"github.com/mozillazg/go-unidecode"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/regex"
)

// romanArabic maps the bounded set of standalone roman numeral tokens (I-XX)
// to their arabic equivalents. This is the full closed set for reasonable
// sequel/episode numbering; we do NOT extend further to avoid false conversions.
var romanArabic = map[string]string{
	"i":     "1",
	"ii":    "2",
	"iii":   "3",
	"iv":    "4",
	"v":     "5",
	"vi":    "6",
	"vii":   "7",
	"viii":  "8",
	"ix":    "9",
	"x":     "10",
	"xi":    "11",
	"xii":   "12",
	"xiii":  "13",
	"xiv":   "14",
	"xv":    "15",
	"xvi":   "16",
	"xvii":  "17",
	"xviii": "18",
	"xix":   "19",
	"xx":    "20",
}

// partNumberWords maps the number words that follow "part" in catalogue
// titles ("Part One") to digits, bounded like romanArabic.
var partNumberWords = map[string]string{
	"one": "1", "two": "2", "three": "3", "four": "4", "five": "5",
	"six": "6", "seven": "7", "eight": "8", "nine": "9", "ten": "10",
}

// isPartNumber reports whether tok can be the number after "part"/"pt":
// digits, a roman numeral, or a number word.
func isPartNumber(tok string) bool {
	if tok == "" {
		return false
	}
	digits := true
	for _, r := range tok {
		if r < '0' || r > '9' {
			digits = false
			break
		}
	}
	if digits {
		return true
	}
	if _, ok := romanArabic[tok]; ok && isPureRoman(tok) {
		return true
	}
	_, ok := partNumberWords[tok]
	return ok
}

// leadingArticles is the set of articles stripped from the start of a
// normalised title. Includes common English and Romance-language articles
// that appear in TMDB titles.
var leadingArticles = map[string]bool{
	"the": true,
	"a":   true,
	"an":  true,
	"le":  true,
	"la":  true,
	"les": true,
	"el":  true,
	"il":  true,
}

// NormalizeTitleForMatch returns a normalised form of s suitable for fuzzy
// comparison and family keying. It applies, in order:
//  1. "&" is expanded to " and " before NormalizeString so the word survives
//     tokenisation (NormalizeString discards bare '&' as a non-word char).
//  2. kana → Hepburn romaji (kana only), then unidecode + NormalizeString.
//  3. Strip a single leading article (the/a/an/le/la/les/el/il), only when
//     at least one more token remains.
//  4. Roman numeral tokens (I-XX) are replaced with arabic digits, ONLY when
//     the token consists entirely of roman numeral characters so that mixed
//     tokens like "se7en" are left untouched.
//  5. A token "part" (or a bare "pt" before a part number) followed by a
//     roman numeral or a number word is normalised so that "Part II",
//     "Part One" and "Part 2" compare equal.
func NormalizeTitleForMatch(s string) string {
	// Step 1: expand '&' to 'and' before NormalizeString swallows it.
	s = strings.ReplaceAll(s, "&", " and ")

	// Step 1b: kana → Hepburn romaji BEFORE unidecode. go-unidecode romanises
	// kana with a non-Hepburn scheme (きょ→"kiyo", じ→"zi", っ→"tsu", drops
	// ー), leaving Japanese titles too far from their romaji alt-titles to
	// match. Transliterating kana to Hepburn first fixes that; the remaining
	// kanji still fall through to unidecode. Gated on kana presence so titles
	// with no kana are byte-for-byte unchanged.
	if contentfilter.ContainsKana(s) {
		s = KanaToRomaji(s)
	}

	// Step 2: unidecode + existing word-token normalisation.
	s = regex.NormalizeString(unidecode.Unidecode(s))

	// Step 3: strip a single leading article when there are additional tokens.
	tokens := strings.Fields(s)
	if len(tokens) > 1 && leadingArticles[tokens[0]] {
		tokens = tokens[1:]
	}

	// Steps 4+5: roman numeral replacement and "Part X" normalisation.
	for i, tok := range tokens {
		lower := strings.ToLower(tok)

		// "Pt. 1" / "Pt II" spell "Part 1" / "Part 2": expand a bare "pt"
		// token only when a part number follows it, so words are untouched.
		if lower == "pt" && i+1 < len(tokens) && isPartNumber(strings.ToLower(tokens[i+1])) {
			tokens[i] = "part"
			lower = "part"
		}

		// Step 5: "part" followed by a roman numeral or a number word token
		// ("Part One" and "Part 1" compare equal).
		if lower == "part" && i+1 < len(tokens) {
			next := strings.ToLower(tokens[i+1])
			if ar, ok := romanArabic[next]; ok && isPureRoman(next) {
				tokens[i+1] = ar
			} else if ar, ok := partNumberWords[next]; ok {
				tokens[i+1] = ar
			}
			continue
		}

		// Step 4: standalone roman numeral token.
		if ar, ok := romanArabic[lower]; ok && isPureRoman(lower) {
			// Safety: "Se7en" becomes "se7en" after NormalizeString; "7" is not
			// in romanArabic so it passes through unchanged. Only pure-roman
			// tokens (all chars in [ivxlcdm]) are substituted.
			tokens[i] = ar
		}
	}

	return strings.Join(tokens, " ")
}

// isPureRoman returns true when every character in s is a valid roman numeral
// character (i, v, x, l, c, d, m -- lowercased). We only call this for tokens
// that are already in the romanArabic map (I-XX), so the cardinality is already
// bounded; this guard prevents partial-word substitutions if the map is ever
// extended carelessly.
func isPureRoman(s string) bool {
	for _, c := range s {
		switch c {
		case 'i', 'v', 'x', 'l', 'c', 'd', 'm':
		default:
			return false
		}
	}
	return true
}

// FamilyKey reduces a title to its family-registry keying form: the
// NormalizeTitleForMatch output with every token stripped to its letters and
// digits (empty tokens dropped). NormalizeTitleForMatch deliberately keeps
// mid-title punctuation ("dexter: new blood"); the fuzzy matcher folds it
// away itself before comparing (classifier.foldTitlePunct — until 2026-09-22
// it did not, and its exact first-token gate rejected "dexter:" against
// "dexter"). Family keying needs exact token
// equality ("dexter" must be a token-prefix of "dexter new blood"), so the
// punctuation goes. This is a derivation of the canonical normalizer, not a
// competing one — every family lookup must key through this function.
func FamilyKey(s string) string {
	fields := strings.Fields(NormalizeTitleForMatch(s))
	out := fields[:0]
	for _, tok := range fields {
		var b strings.Builder
		for _, r := range tok {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				b.WriteRune(r)
			}
		}
		if b.Len() > 0 {
			out = append(out, b.String())
		}
	}
	return strings.Join(out, " ")
}
