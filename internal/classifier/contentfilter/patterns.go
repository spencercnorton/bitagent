package contentfilter

import (
	"regexp"
	"strings"
)

// nsfwKeywords is the conservative-side list — false positives here
// cost less than false negatives. Tokens are matched as whole words
// (\b boundaries) on the lower-cased title to avoid matching
// substrings of innocuous words ("scene" doesn't trigger "sex"
// because of the boundary; but "sex." or "sex," does).
//
// Adding a token: prefer the most specific spelling. Common torrent
// porn-tag formats are "XXX", "[18+]", studio names that always
// indicate adult, and explicit body-part terms.
var nsfwKeywords = []string{
	"xxx",
	"porn",
	"hentai",
	"jav",
	"camrip-xxx",
	"brazzers",
	"naughtyamerica",
	"realitykings",
	"bangbros",
	"onlyfans",
	"manyvids",
	"clips4sale",
	"javhd",
	"uncen",
	"uncensored.adult",
	"adult.movie",
	"18+",
	"r18",
	"erotic",
	"nubilefilms",
	"vixen",
	"blacked",
	"tushy",
	"deeper",
	"slayed",
	"milfy",
}

// nsfwTitlePattern is built once at init. Word-boundary handling has
// to differentiate tokens ending in a word-character (use `\b`) from
// tokens ending in non-word punctuation like `+` (Go's `\b` rule
// won't match because the boundary requires word→non-word transition,
// and `+` is already non-word). For those we use an explicit
// `(?:$|[^A-Za-z0-9])` lookahead-equivalent.
var nsfwTitlePattern = func() *regexp.Regexp {
	parts := make([]string, 0, len(nsfwKeywords))
	for _, k := range nsfwKeywords {
		quoted := regexp.QuoteMeta(k)
		last := k[len(k)-1]
		// Word-character (a–z, A–Z, 0–9, _) → standard \b suffix.
		isWordEnd := (last >= 'a' && last <= 'z') ||
			(last >= 'A' && last <= 'Z') ||
			(last >= '0' && last <= '9') ||
			last == '_'
		var trailing string
		if isWordEnd {
			trailing = `\b`
		} else {
			// Non-word ending (e.g. "18+", "r18+"): match end of
			// string OR a separator char. Go's regexp doesn't
			// support lookahead, so we use a non-capturing group
			// with an alternation.
			trailing = `(?:$|[^A-Za-z0-9])`
		}
		parts = append(parts, `\b`+quoted+trailing)
	}
	// (?i) — case-insensitive. Each alternative carries its own
	// boundary suffix per the rule above.
	pat := `(?i)(?:` + strings.Join(parts, "|") + `)`
	return regexp.MustCompile(pat)
}()

// titleMatchesNSFW reports true iff `title` contains any of the
// keyword tokens (as a whole word).
func titleMatchesNSFW(title string) bool {
	return nsfwTitlePattern.MatchString(title)
}

// nsfwContentTypes is the set of CEL classifier content_type values
// that should always be dropped when DropNSFW=true. The classifier
// emits some of these via TMDB content-rating, others via local
// regex.
var nsfwContentTypeSet = map[string]struct{}{
	"xxx":   {},
	"adult": {},
	"porn":  {},
}

// isNSFWContentType returns true iff a classifier-set content_type
// indicates adult content. Empty string returns false (unknown is
// not NSFW).
func isNSFWContentType(ct string) bool {
	_, ok := nsfwContentTypeSet[strings.ToLower(strings.TrimSpace(ct))]
	return ok
}

// musicAudioExtensions enumerates the file extensions we treat as
// "real audio." Used to test whether a music torrent has any non-mp3
// audio (in which case we keep it) or is mp3-only (drop).
//
// Note this list excludes mp3 by design — the test is "are there
// audio files OTHER than mp3?"
var nonMP3MusicExtensions = map[string]struct{}{
	"flac": {},
	"alac": {},
	"m4a":  {},
	"wav":  {},
	"ape":  {},
	"opus": {},
	"ogg":  {},
	"wv":   {},   // WavPack
	"dsf":  {},   // DSD
}

// hasNonMP3Audio reports true iff any extension in `exts` is in
// nonMP3MusicExtensions. Used to differentiate "pure mp3" from
// "mixed format" or "lossless" music torrents.
//
// `exts` are lowercase strings without leading dots.
func hasNonMP3Audio(exts []string) bool {
	for _, e := range exts {
		if _, ok := nonMP3MusicExtensions[e]; ok {
			return true
		}
	}
	return false
}

// isMP3 reports true iff `ext` is exactly "mp3" (case-insensitive,
// no leading dot).
func isMP3(ext string) bool {
	return strings.EqualFold(ext, "mp3")
}

// hasMP3 reports true iff any extension in `exts` is mp3.
func hasMP3(exts []string) bool {
	for _, e := range exts {
		if isMP3(e) {
			return true
		}
	}
	return false
}

// extInBlocklist returns true iff ext (lowercase, no leading dot)
// appears in `blocked`. Linear scan because lists are short.
func extInBlocklist(ext string, blocked []string) bool {
	low := strings.ToLower(strings.TrimSpace(ext))
	for _, b := range blocked {
		if strings.EqualFold(b, low) {
			return true
		}
	}
	return false
}
