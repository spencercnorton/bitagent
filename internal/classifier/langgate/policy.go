package langgate

import (
	"strings"
	"unicode"

	"github.com/spencercnorton/bitagent/internal/titlenorm"
)

// RuZhTitle reports whether a release name is Russian (a Cyrillic word of three or more letters) or Chinese (Han
// characters with fewer than two Japanese kana). Such releases are excluded immediately, whatever the film's language.
func RuZhTitle(name string) bool {
	run, kana, han := 0, 0, false
	for _, r := range name {
		if r >= 0x0400 && r <= 0x04FF {
			run++
			if run >= 3 {
				return true
			}
		} else {
			run = 0
		}
		if (r >= 0x3400 && r <= 0x4DBF) || (r >= 0x4E00 && r <= 0x9FFF) || (r >= 0xF900 && r <= 0xFAFF) {
			han = true
		}
		if r >= 0x3040 && r <= 0x30FF {
			kana++
		}
	}
	return han && kana < 2
}

// foreignStrong: function words (articles, contractions, prepositions) of the
// major release-title languages that are not English words. foreignWeak: the
// same, but each is also an English word ("no", "do", "die", ...), so a weak
// word alone proves nothing. Both hold titlenorm-folded forms.
var foreignStrong = setOf(
	"ao", "aos", "com", "da", "das", "meu", "minha", "na", "nas", "para", "pela", "pelas", "pelo", "pelos", "sem", "seus", "suas", "um", "uma", "umas", "uns",
	"con", "del", "el", "las", "los", "por", "una", "unas", "unos",
	"des", "du", "et", "les", "une",
	"degli", "dei", "della", "delle", "gli", "lo",
	"das", "dem", "den", "einem", "einen", "einer", "und",
	"een", "het",
)

var foreignWeak = setOf("de", "der", "die", "do", "ein", "eine", "em", "en", "il", "la", "le", "no", "un")

// titleTokens is the set of non-numeric tokens of a title in canonical
// titlenorm family-key form.
func titleTokens(title string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, tok := range strings.Fields(titlenorm.FamilyKey(title)) {
		numeric := true
		for _, r := range tok {
			if !unicode.IsDigit(r) {
				numeric = false
				break
			}
		}
		if !numeric {
			out[tok] = struct{}{}
		}
	}
	return out
}

// hasDiacritic reports whether title holds a non-ASCII letter (an accented
// Latin letter or a letter of another script).
func hasDiacritic(title string) bool {
	for _, r := range title {
		if r > 0x7F && unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

// LocalizedTitle reports whether a release was apparently issued under a
// localized, non-English title for an English-original work ("Um Maluco no
// Golfe 2" for Happy Gilmore 2). Such a release is unwanted under the
// English-audio policy even when it carries the English track: the policy
// keeps English dubs of foreign works, not foreign-market editions of
// English ones.
//
// releaseTitle is the parsed title from the release name; canonicalTitle is
// the matched work's English catalogue title, originalTitle its original
// title, and originalLanguage its ISO language code. The answer is false
// whenever the evidence is incomplete: the work is not English-original, no
// catalogue title is known, the release title shares any non-numeric token
// with the catalogue or original title, or the release title carries no
// positive foreign-language evidence (foreign function words or accented
// letters). A title mismatch alone never suffices: English alternate titles
// with no token in common stay kept, as do foreign-original works whatever
// title the release uses.
func LocalizedTitle(releaseTitle, canonicalTitle, originalTitle, originalLanguage string) bool {
	if !strings.EqualFold(originalLanguage, "en") {
		return false
	}
	release := titleTokens(releaseTitle)
	if len(release) < 2 {
		return false
	}
	referenced := false
	for _, ref := range []map[string]struct{}{titleTokens(canonicalTitle), titleTokens(originalTitle)} {
		if len(ref) == 0 {
			continue
		}
		referenced = true
		for tok := range release {
			if _, ok := ref[tok]; ok {
				return false
			}
		}
	}
	if !referenced {
		return false
	}
	if hasDiacritic(releaseTitle) {
		return true
	}
	// Marker words are counted with a sentinel first token so the canonical
	// normalizer keeps a leading foreign article ("Le Fou ...") instead of
	// stripping it as it does for title matching.
	markers := titleTokens("zzq " + releaseTitle)
	delete(markers, "zzq")
	strong, total := 0, 0
	for tok := range markers {
		if _, ok := foreignStrong[tok]; ok {
			strong++
			total++
		} else if _, ok := foreignWeak[tok]; ok {
			total++
		}
	}
	return strong >= 1 && total >= 2
}
