package animedb

import (
	"strings"
	"sync"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// diacriticFolder folds to NFKD and drops combining marks, so romaji macrons
// and accents compare equal ("Tōkyō" -> "Tokyo", "Pokémon" -> "Pokemon").
// transform.Transformer is stateful and NOT safe for concurrent use, so guard
// the shared instance with a mutex — both the one-off build (hundreds of
// thousands of rows) and the per-classify query path call Normalize.
var (
	diacriticFolder = transform.Chain(norm.NFKD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	diacriticMu     sync.Mutex
)

func foldDiacritics(s string) string {
	diacriticMu.Lock()
	defer diacriticMu.Unlock()
	out, _, err := transform.String(diacriticFolder, s)
	if err != nil {
		return s
	}
	return out
}

// Normalize reduces a title to its comparison key: fold Latin diacritics,
// lower-case, and keep only letters and digits (dropping whitespace,
// punctuation, brackets and separators). This collapses the many surface forms
// of a title — "Steins;Gate", "Steins Gate", "STEINS·GATE" all become
// "steinsgate" — for high-precision exact matching. Non-Latin letters
// (kanji/kana) are preserved so Japanese titles remain matchable; only Latin
// diacritics are folded.
func Normalize(s string) string {
	s = foldDiacritics(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case unicode.IsLetter(r):
			b.WriteRune(unicode.ToLower(r))
		case unicode.IsDigit(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// minNormalizedLen is the shortest normalized key we will index or query. A
// one-character key ("k") is far too broad — it would match inside almost any
// name — so such aliases are dropped at build time and skipped at query time.
const minNormalizedLen = 2
