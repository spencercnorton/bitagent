// Package langgate decides from a release name and its file paths alone
// whether a torrent's audio is English, using scene and P2P naming conventions:
// a language-of-audio tag with no English-track tag means non-English, an
// English-track tag means English, and subtitle-only tags never decide audio.
//
// It is a port of a rule set that was developed and measured in Python. Evidence
// reproduces that module's decisions, reason strings and order of checks exactly;
// langgate_test.go proves it against the Python decisions for every flag
// combination.
//
// Go's regexp (RE2) has no lookaround, and its \b, \w, \d, \s and (?i) differ from
// Python's str patterns. Rules that are plain alternations stay regular
// expressions, compiled through rx so they keep Python's meaning. Rules that need
// lookaround, Unicode word boundaries or match positions are scanned by hand.
package langgate

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Classes returned by Evidence.
const (
	English      = "english"
	NonEnglish   = "non_english"
	Undetermined = "undetermined"
)

// Python-flavoured character classes -------------------------------------------

// spaceClass is Python's \s for str patterns. Go's \s is ASCII-only, and
// unicode.IsSpace lacks the information separators U+001C..U+001F.
const spaceClass = `\x{9}-\x{d}\x{1c}-\x{20}\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

var (
	pyEscapes = strings.NewReplacer(`\s`, spaceClass, `\d`, digitClass())
	// Python's re.I also folds U+0130 and U+0131 into the letter i; Go's (?i) does not.
	// Everything else the patterns use (k with U+212A, s with U+017F, Cyrillic) folds the same.
	pyFoldI = strings.NewReplacer("i", "[iıİ]", "I", "[iıİ]")
)

// rx compiles a Python-flavoured pattern. \s and \d get their Unicode meaning and,
// when ignoreCase is set (Python's re.I), the letter i and the class [A-Z] also
// match U+0130 and U+0131. Patterns must not contain i inside a character class.
// A lookahead (?=X) is written as a consuming (?:X): that only changes what is
// consumed, not whether a match exists, and rx is used for existence tests only.
func rx(pattern string, ignoreCase bool) *regexp.Regexp {
	p := pyEscapes.Replace(pattern)
	if ignoreCase {
		p = "(?i)" + pyFoldI.Replace(strings.ReplaceAll(p, "[A-Z]", "[A-Zıİ]"))
	}
	return regexp.MustCompile(p)
}

// Go's Unicode tables are version 15.0; the Python 3.14 that produced the
// reference decisions is 16.0. These are the code points 16.0 added to \w and \d,
// so word boundaries and digit runs agree with the reference for every code point.
var (
	newWord = &unicode.RangeTable{
		R16: []unicode.Range16{{Lo: 0x1C89, Hi: 0x1C8A, Stride: 1}, {Lo: 0xA7CB, Hi: 0xA7CD, Stride: 1}, {Lo: 0xA7DA, Hi: 0xA7DC, Stride: 1}},
		R32: []unicode.Range32{
			{Lo: 0x105C0, Hi: 0x105F3, Stride: 1}, {Lo: 0x10D40, Hi: 0x10D65, Stride: 1}, {Lo: 0x10D6F, Hi: 0x10D85, Stride: 1},
			{Lo: 0x10EC2, Hi: 0x10EC4, Stride: 1}, {Lo: 0x11380, Hi: 0x11389, Stride: 1}, {Lo: 0x1138B, Hi: 0x1138B, Stride: 1},
			{Lo: 0x1138E, Hi: 0x1138E, Stride: 1}, {Lo: 0x11390, Hi: 0x113B5, Stride: 1}, {Lo: 0x113B7, Hi: 0x113B7, Stride: 1},
			{Lo: 0x113D1, Hi: 0x113D1, Stride: 1}, {Lo: 0x113D3, Hi: 0x113D3, Stride: 1}, {Lo: 0x116D0, Hi: 0x116E3, Stride: 1},
			{Lo: 0x11BC0, Hi: 0x11BE0, Stride: 1}, {Lo: 0x11BF0, Hi: 0x11BF9, Stride: 1}, {Lo: 0x13460, Hi: 0x143FA, Stride: 1},
			{Lo: 0x16100, Hi: 0x1611D, Stride: 1}, {Lo: 0x16130, Hi: 0x16139, Stride: 1}, {Lo: 0x16D40, Hi: 0x16D6C, Stride: 1},
			{Lo: 0x16D70, Hi: 0x16D79, Stride: 1}, {Lo: 0x18CFF, Hi: 0x18CFF, Stride: 1}, {Lo: 0x1CCF0, Hi: 0x1CCF9, Stride: 1},
			{Lo: 0x1E5D0, Hi: 0x1E5ED, Stride: 1}, {Lo: 0x1E5F0, Hi: 0x1E5FA, Stride: 1}, {Lo: 0x2EBF0, Hi: 0x2EE5D, Stride: 1},
		},
	}
	newDigit = &unicode.RangeTable{
		R32: []unicode.Range32{
			{Lo: 0x10D40, Hi: 0x10D49, Stride: 1}, {Lo: 0x116D0, Hi: 0x116E3, Stride: 1}, {Lo: 0x11BF0, Hi: 0x11BF9, Stride: 1},
			{Lo: 0x16130, Hi: 0x16139, Stride: 1}, {Lo: 0x16D70, Hi: 0x16D79, Stride: 1}, {Lo: 0x1CCF0, Hi: 0x1CCF9, Stride: 1},
			{Lo: 0x1E5F1, Hi: 0x1E5FA, Stride: 1},
		},
	}
)

// digitClass is Python's \d as a Go character class: the Nd category plus the
// digits Unicode 16.0 added.
func digitClass() string {
	var b strings.Builder
	b.WriteString(`[\p{Nd}`)
	for _, r := range newDigit.R32 {
		fmt.Fprintf(&b, `\x{%X}-\x{%X}`, r.Lo, r.Hi)
	}
	b.WriteString(`]`)
	return b.String()
}

// isWord is Python's \w for str patterns.
func isWord(r rune) bool {
	if r < utf8.RuneSelf {
		return r == '_' || '0' <= r && r <= '9' || 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z'
	}
	return unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.Is(newWord, r)
}

// isDigit is Python's \d for str patterns (Unicode decimal digits, not just 0-9).
func isDigit(r rune) bool { return unicode.IsDigit(r) || unicode.Is(newDigit, r) }

// isSpace is Python's \s for str patterns.
func isSpace(r rune) bool { return unicode.IsSpace(r) || 0x1C <= r && r <= 0x1F }

// isSep is the SEP/END character class: whitespace or one of ._-[](){}+,/|.
func isSep(r rune) bool {
	switch r {
	case '.', '_', '-', '[', ']', '(', ')', '{', '}', '+', ',', '/', '|':
		return true
	}
	return isSpace(r)
}

// canon maps r to the lower-case ASCII letter it equals under Python's re.I, or
// to r itself: A-Z, U+212A (Kelvin sign) as k, U+017F (long s) as s, U+0130 and
// U+0131 as i.
func canon(r rune) rune {
	switch {
	case 'A' <= r && r <= 'Z':
		return r + 'a' - 'A'
	case r == 0x212A:
		return 'k'
	case r == 0x17F:
		return 's'
	case r == 0x130 || r == 0x131:
		return 'i'
	}
	return r
}

// foldPrefix reports whether text[i:] starts with lit, a lower-case ASCII
// literal, under Python's re.I, and returns the offset just after the match.
func foldPrefix(text string, i int, lit string) (int, bool) {
	for k := 0; k < len(lit); k++ {
		if i >= len(text) {
			return 0, false
		}
		r, w := utf8.DecodeRuneInString(text[i:])
		if canon(r) != rune(lit[k]) {
			return 0, false
		}
		i += w
	}
	return i, true
}

// wordEnds is Python's \b at offset e right after a word character.
func wordEnds(text string, e int) bool {
	if e >= len(text) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(text[e:])
	return !isWord(r)
}

// Rule data --------------------------------------------------------------------

var audioLang = []string{
	"JAPANESE", "KOREAN", "CHINESE", "MANDARIN", "CANTONESE", "FRENCH", "TRUEFRENCH", "GERMAN", "ITALIAN", "SPANISH",
	"CASTELLANO", "LATINO", "PORTUGUESE", "DUBLADO", "RUSSIAN", "UKRAINIAN", "POLISH", "LEKTOR", "DUTCH", "FLEMISH",
	"SWEDISH", "NORWEGIAN", "DANISH", "FINNISH", "ICELANDIC", "TURKISH", "HINDI", "TAMIL", "TELUGU", "MALAYALAM",
	"KANNADA", "BENGALI", "PUNJABI", "MARATHI", "THAI", "VIETNAMESE", "INDONESIAN", "MALAY", "TAGALOG", "FILIPINO",
	"ARABIC", "PERSIAN", "FARSI", "HEBREW", "CZECH", "SLOVAK", "HUNGARIAN", "GREEK", "ROMANIAN", "BULGARIAN",
	"SERBIAN", "CROATIAN", "SLOVENIAN", "ESTONIAN", "LATVIAN", "LITHUANIAN", "UKR", "RUS", "iTA", "SPA", "FRE", "GER",
	"JPN", "KOR", "CHI", "POR", "POL", "TUR", "HIN",
}

// langTags holds each audio-language tag in the two spellings scene releases use:
// KOREAN, and the one with lower-case i's (iTALiAN, iTA). Matching is
// case-sensitive on purpose.
var langTags = func() map[string]struct{} {
	m := make(map[string]struct{}, 2*len(audioLang))
	for _, w := range audioLang {
		up := strings.ToUpper(w)
		m[up] = struct{}{}
		m[strings.ReplaceAll(up, "I", "i")] = struct{}{}
	}
	return m
}()

// SEP and END of the tag rules: a separator character, or the string edge.
const sepChars = `[\s._\-\[\]\(\)\{\}+,/|]`

var englishAlts = []string{
	// DL is a dual-language tag only after GERMAN/DEUTSCH/GER: the DL of WEB-DL
	// must not count as an English track ("FRENCH.WEB-DL" is French audio).
	`(?:(?:GERMAN|DEUTSCH|GER)[\s._\-]DL|DUAL[\s._\-]?LANGUAGE|MULTI(?:[\s._\-]?AUDIO)?|MULTi|VF2|VFF[\s._\-]?VO)`,
	`ENG(?:LISH)?`,
	`(?:ENG|EN)[\s._\-]?(?:DUB|AUDIO)`,
	`DUBBED`,
	`ORIGINAL`,
	`MULTI[\s._\-]?(?:AUDIO)?[\s._\-]?ENG`,
	`ENG[\s._\-]?(?:\+|&|AND)[\s._\-]?[A-Z]{2,3}`,
	`[A-Z]{2,3}[\s._\-]?(?:\+|&)[\s._\-]?ENG`,
}

var (
	// An English-track tag (ENG, English, German.DL, MULTi incl. ENG, "Eng+Rus" ...).
	engRE = rx(`(?:^|`+sepChars+`)(?:`+strings.Join(englishAlts, "|")+`)(?:$|`+sepChars+`)`, true)
	// A dual-audio label: two audio tracks, English unless the listed languages
	// name two non-English tracks ("Dual Audio [Hindi - Korean]").
	dualRE = rx(`(?:^|`+sepChars+`)DUAL(?:[\s._\-]?AUDIO)?(?:$|`+sepChars+`)`, true)
	// "EN" as a bare token: uppercase only, because French "en" is lower-case.
	enTokenRE = rx(`(?:^|[_.\s\-\[\(])EN(?:$|[_.\s\-\]\)])`, false)
	// "EN" as a track code next to another track code: [EN-FR-TR], CZ.EN, EN+2xBG, (RU, EN).
	enTrackRE = rx(`(?:(?:^|[\[\(\s._\-+,/])(?:[A-Z]{2}|\d+x[A-Z]{2})[\s._\-+,/]+EN|(?:^|[\[\(\s._\-+,/])EN[\s._\-+,/]+(?:[A-Z]{2}|\d+x[A-Z]{2}))(?:$|[\]\)\s._\-+,/])`, false)
	// Subtitle-only markers in other scripts; they never decide audio.
	subsOnlyNativeRE = rx(`(?:中字|中文字幕|中英字幕|中英双字|中英雙字|简中|繁中|字幕|KOR[\s._\-]?SUBS?|한글자막|субтитр|SUBRIP)`, true)

	// A word in Cyrillic, not one homoglyph (Beyoncе).
	cyrillicRE = rx(`[\x{0400}-\x{04FF}]{3,}`, false)
	cjkRE      = rx(`[\x{3040}-\x{30FF}\x{3400}-\x{9FFF}\x{AC00}-\x{D7AF}]`, false)

	// 【HardX】, 【2ChicksSameTime】 on Chinese forum reposts.
	bracketStudioRE = rx(`[【\[][A-Za-z][A-Za-z0-9]{3,}[】\]]`, false)
	// The Cyrillic form of a subtitle marker ("субт..."), anchored at the start of its input.
	subtitleCyrRE = rx(`^субт`, true)
)

// Literal rules ------------------------------------------------------------------

// engNativeMarkers are English-audio markers in other scripts. One followed by 字
// (as in 字幕, subtitles) never counts as audio.
var engNativeMarkers = []string{"英语", "英語", "英文", "英配", "原声", "原聲", "国英", "國英", "英粤", "英粵", "英音", "Оригинал", "оригинал"}

// nativeAudioMarkers name the audio in another script: 国语 = Mandarin,
// Дубляж/MVO = Russian dub or voice-over.
var nativeAudioMarkers = []string{"国语", "國語", "粤语", "粵語", "日语", "日語", "韩语", "韓語", "普通话", "普通話", "俄语", "Дубляж", "дубляж",
	"Многоголос", "многоголос", "Одноголос", "одноголос", "MVO", "DVO", "AVO"}

// westernCJKMarkers: 欧美 歐美 好莱坞 好萊塢 美剧 美劇 英剧 英劇 (Western, Hollywood, American or British series).
var westernCJKMarkers = []string{"\u6b27\u7f8e", "\u6b50\u7f8e", "\u597d\u83b1\u575e", "\u597d\u840a\u5862", "\u7f8e\u5267", "\u7f8e\u5287", "\u82f1\u5267", "\u82f1\u5287"}

// westernAdultNames are western adult studios and sites, whose releases are English
// audio even under a Chinese forum prefix. " ?" is an optional space, as in the
// reference pattern; matching ignores case.
var westernAdultNames = []string{
	"brazzers", "naughty ?america", "reality ?kings", "bang ?bros", "darkx", "blacked", "tushy", "vixen", "deeper", "public ?agent", "fake ?taxi", "fake ?hub",
	"milfs ?like ?it ?big", "team ?skeet", "mofos", "digital ?playground", "evil ?angel", "jules ?jordan", "porn ?pros", "nubiles", "x-art", "passion-hd",
	"tiny4k", "cum4k", "exotic4k", "lubed", "holed", "spy ?fam", "family ?strokes", "bratty ?sis", "my ?friends ?hot ?mom", "dirty ?wives ?club",
	"big ?wet ?asses", "big ?tits ?at ?work", "teens ?like ?it ?big", "hussie ?pass", `private\.com`, "21 ?sextury", "dorcel", `kink\.com`,
}

// westernAdultByFirst holds every spelling of those names, optional spaces
// expanded, indexed by first character.
var westernAdultByFirst = func() (byFirst [utf8.RuneSelf][]string) {
	for _, n := range westernAdultNames {
		for _, lit := range expandOptionalSpaces(strings.ReplaceAll(n, `\.`, ".")) {
			byFirst[lit[0]] = append(byFirst[lit[0]], lit)
		}
	}
	return byFirst
}()

// expandOptionalSpaces returns every spelling of p in which each " ?" is a space or nothing.
func expandOptionalSpaces(p string) []string {
	i := strings.Index(p, " ?")
	if i < 0 {
		return []string{p}
	}
	var out []string
	for _, tail := range expandOptionalSpaces(p[i+2:]) {
		out = append(out, p[:i]+" "+tail, p[:i]+tail)
	}
	return out
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// nativeAudioStudios are Russian dub studios; their name anywhere in a release
// name (any case, no word boundaries) names non-English audio.
var nativeAudioStudios = []string{"lostfilm", "hdrezka", "newstudio", "alexfilm", "baibako", "jaskier"}

// hasNativeAudio reports whether text names non-English audio in another
// script or names a Russian dub studio.
func hasNativeAudio(text string) bool {
	if containsAny(text, nativeAudioMarkers) {
		return true
	}
	for i := 0; i < len(text); {
		r, w := utf8.DecodeRuneInString(text[i:])
		if c := canon(r); c == 'l' || c == 'h' || c == 'n' || c == 'a' || c == 'b' || c == 'j' {
			for _, lit := range nativeAudioStudios {
				if rune(lit[0]) == c {
					if _, ok := foldPrefix(text, i, lit); ok {
						return true
					}
				}
			}
		}
		i += w
	}
	return false
}

// hasEngNative reports whether text holds an engNativeMarkers entry that is not
// followed by 字.
func hasEngNative(text string) bool {
	for _, m := range engNativeMarkers {
		for off := 0; ; {
			i := strings.Index(text[off:], m)
			if i < 0 {
				break
			}
			if !strings.HasPrefix(text[off+i+len(m):], "字") {
				return true
			}
			off += i + 1
		}
	}
	return false
}

// hasWesternAdult reports whether text contains a western adult name, ignoring
// case, with no word boundaries: the reference matches inside longer words too.
func hasWesternAdult(text string) bool {
	for i := 0; i < len(text); {
		r, w := utf8.DecodeRuneInString(text[i:])
		if c := canon(r); c < utf8.RuneSelf {
			for _, lit := range westernAdultByFirst[c] {
				if _, ok := foldPrefix(text, i, lit); ok {
					return true
				}
			}
		}
		i += w
	}
	return false
}

// forumRun is the longest run of characters before an @ in a reposting prefix.
const forumRun = 24

var (
	videoExt    = setOf("mkv", "mp4", "avi", "m4v", "mov", "wmv", "ts", "m2ts", "mpg", "mpeg", "webm", "flv", "rmvb", "vob")
	nonVideoExt = setOf("mp3", "flac", "m4a", "m4b", "ogg", "opus", "wav", "ape", "aac", "wma", "alac", "dsf", "pdf", "epub", "mobi",
		"azw3", "djvu", "fb2", "cbr", "cbz", "txt", "doc", "docx", "exe", "msi", "dmg", "pkg", "apk", "iso", "bin", "rar", "zip", "7z")
	subtitleExt = setOf("srt", "ass", "ssa", "sub", "idx", "vtt", "sup")
)

func setOf(exts ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(exts))
	for _, e := range exts {
		m[e] = struct{}{}
	}
	return m
}

// hasExt reports whether p ends in "." plus one of exts, ignoring case. Python's
// $ also matches just before one trailing newline, so that is allowed too.
func hasExt(p string, exts map[string]struct{}) bool {
	p = strings.TrimSuffix(p, "\n")
	dot := strings.LastIndexByte(p, '.')
	if dot < 0 {
		return false
	}
	var buf [8]byte // the longest extension has four letters
	n := 0
	for _, r := range p[dot+1:] {
		c := canon(r)
		if c >= utf8.RuneSelf || n == len(buf) {
			return false
		}
		buf[n] = byte(c)
		n++
	}
	_, ok := exts[string(buf[:n])]
	return ok
}

// videoLike is true unless the listed files are mostly non-video (music, books,
// software): those are the type policy's job.
func videoLike(name string, files []string) bool {
	if len(files) == 0 {
		files = []string{name}
	}
	nonVideo := false
	for _, f := range files {
		if hasExt(f, videoExt) {
			return true
		}
		if hasExt(f, nonVideoExt) {
			nonVideo = true
		}
	}
	return !nonVideo
}

// lowerLangTags are lower-case tracker tags (rus, ukr), matched case-sensitively:
// Moon.Knight.S01E01.rus.LostFilm.
var lowerLangTags = setOf("rus", "ukr")

// dualLangTags: beside DUAL only, the full language names (more than three
// letters), matched in any case, since a dual-audio label lists its tracks.
var dualLangTags = func() map[string]struct{} {
	m := make(map[string]struct{}, len(audioLang))
	for _, w := range audioLang {
		if len(w) > 3 {
			m[strings.ToUpper(w)] = struct{}{}
		}
	}
	return m
}()

// scanTokens calls fn for each maximal run between separators (or the string
// edges), in order, until fn returns false. start and end bound the token.
func scanTokens(text string, fn func(tok string, start, end int) bool) {
	start := 0
	for i := 0; ; {
		var r rune
		w := 0
		if i < len(text) {
			r, w = utf8.DecodeRuneInString(text[i:])
		}
		if i == len(text) || isSep(r) {
			if !fn(text[start:i], start, i) {
				return
			}
			start = i + w
		}
		if i == len(text) {
			return
		}
		i += w
	}
}

// isSub reports whether the token at [start, end] names a subtitle language:
// followed by a subtitle marker ("Rus subs") or preceded by one ("sub UKR").
func isSub(text string, start, end int) bool {
	return subAfter(text, end) || subBefore(text, start)
}

// audioLangTag returns the first audio-language tag in text that does not name a
// subtitle language, else the first LEKTOR/DUBBING tag. The scene convention:
// the tag names the audio; SUBBED means English subtitles over it, still
// non-English audio. In the reference the case-sensitive tags are scanned before
// the lower-case rus/ukr tags, each in position order, so two passes are made.
func audioLangTag(text string) (string, bool) {
	for _, set := range []map[string]struct{}{langTags, lowerLangTags} {
		tag, ok := "", false
		scanTokens(text, func(tok string, start, end int) bool {
			if _, in := set[tok]; in && !isSub(text, start, end) {
				tag, ok = strings.ToUpper(tok), true
				return false
			}
			return true
		})
		if ok {
			return tag, true
		}
	}
	if m, ok := lektor(text); ok {
		return strings.ToUpper(strings.Trim(m, " ._-[]()")), true
	}
	return "", false
}

// foldUpperToken upper-cases tok under Python's re.I folding of ASCII letters.
// ok is false when a character has no ASCII fold, so tok cannot equal an ASCII
// alternative.
func foldUpperToken(tok string) (string, bool) {
	var b []byte
	for _, r := range tok {
		c := canon(r)
		if c >= utf8.RuneSelf {
			return "", false
		}
		if 'a' <= c && c <= 'z' {
			c += 'A' - 'a'
		}
		b = append(b, byte(c))
	}
	return string(b), true
}

// dualForeignCount counts the distinct full language names in text that do not
// name subtitle languages, for the dual-audio rule. Distinctness follows the
// reference: the matched text upper-cased ("Hindi" and "HINDI" are one).
func dualForeignCount(text string) int {
	seen := make(map[string]struct{})
	scanTokens(text, func(tok string, start, end int) bool {
		if up, ok := foldUpperToken(tok); ok {
			if _, in := dualLangTags[up]; in && !isSub(text, start, end) {
				seen[strings.ToUpper(tok)] = struct{}{}
			}
		}
		return true
	})
	return len(seen)
}

var subForms = []string{"", "s", "bed", "title", "titles"}

// subAfter reports whether a subtitle marker starts at offset i, optionally after
// one separator: sub, subs, subbed, subtitle(s) ending at a word boundary, or "субт".
func subAfter(text string, i int) bool {
	if i < len(text) {
		r, w := utf8.DecodeRuneInString(text[i:])
		if isSpace(r) || r == '.' || r == '_' || r == '-' {
			i += w
		}
	}
	if e, ok := foldPrefix(text, i, "sub"); ok {
		for _, f := range subForms {
			if e2, ok := foldPrefix(text, e, f); ok && wordEnds(text, e2) {
				return true
			}
		}
	}
	return subtitleCyrRE.MatchString(text[i:])
}

var subBeforeForms = []string{"subtitles", "subtitle", "subs", "sub"}

// subBefore reports whether a subtitle marker ends just before the token at
// start, allowing separators between them: "sub UKR" and "SUB ITA" name the
// subtitle language. The marker is sub, subs, subtitle(s) or a Cyrillic
// "субт..." word, matched case-insensitively at any position, as the reference's
// search does.
func subBefore(text string, start int) bool {
	if start == 0 {
		return false
	}
	q := start
	for q > 0 {
		r, size := utf8.DecodeLastRuneInString(text[:q])
		if isSpace(r) || r == '.' || r == '_' || r == '-' {
			q -= size
		} else {
			break
		}
	}
	// The Latin forms are at most nine letters ending exactly at q.
	for s, n := q, 0; s > 0 && n < 10; n++ {
		_, size := utf8.DecodeLastRuneInString(text[:s])
		s -= size
		for _, lit := range subBeforeForms {
			if e, ok := foldPrefix(text, s, lit); ok && e == q {
				return true
			}
		}
	}
	// The Cyrillic form is "субт" plus word characters, within the word run
	// ending at q.
	ws := q
	for ws > 0 {
		r, size := utf8.DecodeLastRuneInString(text[:ws])
		if !isWord(r) {
			break
		}
		ws -= size
	}
	for s := ws; s < q; {
		if cyrFoldPrefix(text, s, "субт") {
			return true
		}
		_, size := utf8.DecodeRuneInString(text[s:])
		s += size
	}
	return false
}

// cyrFoldPrefix reports whether text[i:] starts with lit under Cyrillic simple
// case folding (the reference's re.I on Cyrillic letters).
func cyrFoldPrefix(text string, i int, lit string) bool {
	for _, want := range lit {
		if i >= len(text) {
			return false
		}
		r, w := utf8.DecodeRuneInString(text[i:])
		if unicode.ToLower(r) != want {
			return false
		}
		i += w
	}
	return true
}

// lektor finds the first "SEP (LEKTOR|DUBBING) [sep]? (PL|POL)? END" match, case-
// insensitively, and returns its text including the leading separator. Optional
// parts are taken in the order a backtracking engine prefers them.
func lektor(text string) (string, bool) {
	for w := 0; w < len(text); {
		_, size := utf8.DecodeRuneInString(text[w:])
		s := w // start of the match: the separator before the word, or 0 at the string start
		if w > 0 {
			p, ps := utf8.DecodeLastRuneInString(text[:w])
			if !isSep(p) {
				w += size
				continue
			}
			s = w - ps
		}
		we, ok := foldPrefix(text, w, "lektor")
		if !ok {
			we, ok = foldPrefix(text, w, "dubbing")
		}
		if ok {
			if e, ok := lektorTail(text, we); ok {
				return text[s:e], true
			}
		}
		w += size
	}
	return "", false
}

var lektorForms = []string{"pl", "pol", ""}

// lektorTail matches "[\s._\-]? (PL|POL)? END" at offset we and returns the end.
func lektorTail(text string, we int) (int, bool) {
	for _, takeSep := range [2]bool{true, false} {
		pos := we
		if takeSep {
			if pos >= len(text) {
				continue
			}
			r, w := utf8.DecodeRuneInString(text[pos:])
			if !(isSpace(r) || r == '.' || r == '_' || r == '-') {
				continue
			}
			pos += w
		}
		for _, f := range lektorForms {
			if e, ok := foldPrefix(text, pos, f); ok && sepOrEnd(text, e) {
				return e, true
			}
		}
	}
	return 0, false
}

// sepOrEnd is the END lookahead: a separator character or the string end.
func sepOrEnd(text string, e int) bool {
	if e >= len(text) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(text[e:])
	return isSep(r)
}

// JAV product codes (SSIS-001, ABP-123) --------------------------------------------

// javNotCode are release-format words that can precede a hyphen and a number
// (WEB-1080, HD-720) without being a studio code.
var javNotCode = setOf("DVDRIP", "WEBRIP", "BDRIP", "HDRIP", "BRRIP", "HDTV", "XVID", "DIVX", "HEVC", "AVC", "AAC", "DTS", "AC3", "DDP",
	"ATMOS", "FLAC", "WEB", "AMZN", "DSNP", "HMAX", "REMUX", "UHD", "HDR", "SDR", "NF", "HD", "FHD", "SD", "CD", "DVD", "PART", "EP", "VOL",
	"DISC", "CAM")

var javResolutions = []string{"480", "576", "720", "1080", "2160", "4320"}

// javCode reports whether text holds a studio code: 2-6 capital letters, a hyphen
// and 3-4 digits, not part of a longer alphanumeric run, not a resolution, bitrate
// or release-format word. Every candidate is anchored on a hyphen.
func javCode(text string) bool {
	for h := 0; h < len(text); h++ {
		if text[h] != '-' {
			continue
		}
		s := h
		for s > 0 && 'A' <= text[s-1] && text[s-1] <= 'Z' {
			s--
		}
		if n := h - s; n < 2 || n > 6 {
			continue
		}
		if s > 0 && isASCIIAlnum(text[s-1]) {
			continue
		}
		if _, no := javNotCode[text[s:h]]; no {
			continue
		}
		rest := text[h+1:]
		if javResolution(rest) {
			continue
		}
		// \d{3,4} is greedy but backtracks from four digits to three.
		var ends [5]int
		k, pos := 0, h+1
		for k < 4 && pos < len(text) {
			r, w := utf8.DecodeRuneInString(text[pos:])
			if !isDigit(r) {
				break
			}
			pos += w
			k++
			ends[k] = pos
		}
		for n := k; n >= 3; n-- {
			if javTail(text, ends[n]) {
				return true
			}
		}
	}
	return false
}

func isASCIIAlnum(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

// javResolution is the lookahead (?:480|576|720|1080|2160|4320)(?:\D|$).
func javResolution(rest string) bool {
	for _, n := range javResolutions {
		if strings.HasPrefix(rest, n) {
			after := rest[len(n):]
			if after == "" {
				return true
			}
			if r, _ := utf8.DecodeRuneInString(after); !isDigit(r) {
				return true
			}
		}
	}
	return false
}

// javTail is (?![0-9]|kbps|k\b|kb)(?:[^0-9]|$) at offset e, after the digits.
func javTail(text string, e int) bool {
	if e >= len(text) {
		return true
	}
	c := text[e]
	if '0' <= c && c <= '9' || strings.HasPrefix(text[e:], "kb") {
		return false
	}
	return !(c == 'k' && wordEnds(text, e+1))
}

// Native-script title rules ------------------------------------------------------

// forumPrefix returns the length of a leading "xxx@yyy@" reposting prefix, matching
// (?:[^@\s]{0,24}@)+, or 0. Each group is at most 24 non-space characters then @.
func forumPrefix(text string) int {
	n := 0
	for {
		i, count := n, 0
		for i < len(text) {
			r, w := utf8.DecodeRuneInString(text[i:])
			if r == '@' {
				break
			}
			count++
			if isSpace(r) || count > forumRun {
				return n
			}
			i += w
		}
		if i >= len(text) {
			return n
		}
		n = i + 1
	}
}

var (
	titleResolutions = []string{"480", "576", "720", "1080", "2160"}
	titleWords       = []string{"bluray", "blu-ray", "web", "webrip", "web-dl", "webdl", "hdtv", "dvdrip", "bdrip", "brrip", "hdrip",
		"dlrip", "remux", "x264", "x265", "h264", "h265", "hevc", "xvid"}
)

// yearAt is (?:19|20)\d{2} at offset i, with \d any Unicode decimal digit.
func yearAt(text string, i int) bool {
	if !(strings.HasPrefix(text[i:], "19") || strings.HasPrefix(text[i:], "20")) {
		return false
	}
	i += 2
	for k := 0; k < 2; k++ {
		if i >= len(text) {
			return false
		}
		r, w := utf8.DecodeRuneInString(text[i:])
		if !isDigit(r) {
			return false
		}
		i += w
	}
	return true
}

// titleEndAt reports whether the title ends at offset i (rune r): a year, optionally
// after ( or [; a resolution; or a release-format word, the last two on word
// boundaries.
func titleEndAt(text string, i int, r rune) bool {
	if r == '(' || r == '[' {
		if yearAt(text, i+1) {
			return true
		}
	}
	if yearAt(text, i) {
		return true
	}
	if !isWord(r) { // both remaining alternatives start with a word character
		return false
	}
	if i > 0 {
		if p, _ := utf8.DecodeLastRuneInString(text[:i]); isWord(p) {
			return false
		}
	}
	for _, n := range titleResolutions {
		if strings.HasPrefix(text[i:], n) {
			if e := i + len(n); e < len(text) {
				p, w := utf8.DecodeRuneInString(text[e:])
				if c := canon(p); (c == 'p' || c == 'i') && wordEnds(text, e+w) {
					return true
				}
			}
		}
	}
	for _, w := range titleWords {
		if e, ok := foldPrefix(text, i, w); ok && wordEnds(text, e) {
			return true
		}
	}
	return false
}

// latinTitle is true for two or more Latin words (3+ letters) in the title zone,
// before the year or first release token: Death Note.2017.
func latinTitle(text string) bool {
	zone := text
	for i := 0; i < len(text); {
		r, w := utf8.DecodeRuneInString(text[i:])
		if titleEndAt(text, i, r) {
			zone = text[:i]
			break
		}
		i += w
	}
	// Words are runs of [A-Za-z'] split between a lower-case and an upper-case
	// letter (SeventhSon -> Seventh Son); any other character, native script
	// included, ends a run. A run counts when, past leading apostrophes, it has
	// three or more characters. Bytes suffice: only ASCII belongs to a word.
	words, run, started, prevLower := 0, 0, false, false
	flush := func() {
		if started && run >= 3 {
			words++
		}
		run, started = 0, false
	}
	for i := 0; i < len(zone); i++ {
		c := zone[i]
		upper, lower := 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z'
		switch {
		case upper || lower || c == '\'':
			if upper && prevLower {
				flush()
			}
			if upper || lower {
				started = true
			}
			if started {
				run++
			}
		default:
			flush()
		}
		prevLower = lower
	}
	flush()
	return words >= 2
}

// Evidence reports whether name (and, optionally, the torrent's file paths) shows
// English audio. class is English, NonEnglish or Undetermined; reason is a short
// code: english_track_tag, dual_audio_without_english,
// audio_language_tag:<TAG>, native_audio_tag, jav_code,
// native_script_no_subtitle_marker, latin_title_alongside_native_script,
// western_origin_marker, subtitle_files_present, not_video, or "" when nothing
// decides.
//
// useScript turns on the rules for names written in Cyrillic, CJK, Hangul or
// kana; useJAV turns on the JAV product-code rule. Files are only used to tell
// video from music, books and software, and to spot subtitle files.
func Evidence(name string, files []string, useScript, useJAV bool) (class, reason string) {
	if !videoLike(name, files) {
		return Undetermined, "not_video"
	}
	if engRE.MatchString(name) || hasEngNative(name) || enTrackRE.MatchString(name) || enTokenRE.MatchString(name) || hasWesternAdult(name) {
		return English, "english_track_tag"
	}
	tag, hasTag := audioLangTag(name)
	if !hasTag && dualRE.MatchString(name) {
		// Two audio tracks: English unless the label names two non-English
		// tracks. One named dub beside DUAL is the original track plus a dub.
		if dualForeignCount(name) >= 2 {
			return NonEnglish, "dual_audio_without_english"
		}
		return English, "english_track_tag"
	}
	if hasTag {
		return NonEnglish, "audio_language_tag:" + tag
	}
	if hasNativeAudio(name) { // 国语 = Mandarin audio, Дубляж/MVO = Russian dub or voice-over
		return NonEnglish, "native_audio_tag"
	}
	if useJAV && javCode(name) && !subsOnlyNativeRE.MatchString(name) {
		return NonEnglish, "jav_code"
	}
	if useScript && (cyrillicRE.MatchString(name) || cjkRE.MatchString(name)) && !subsOnlyNativeRE.MatchString(name) {
		// The reference also tests the western adult studios here and would return
		// english/western_adult_studio, but that test is unreachable: the same
		// pattern on the same text already returned english_track_tag above.
		prefix := forumPrefix(name)
		rest := name[prefix:]
		if containsAny(name, westernCJKMarkers) {
			return Undetermined, "western_origin_marker" // 欧美 = Western, 好莱坞 = Hollywood
		}
		if latinTitle(rest) || (prefix > 0 && bracketStudioRE.MatchString(rest)) {
			return Undetermined, "latin_title_alongside_native_script" // e.g. Death Note.2017, Private.Lessons.1981
		}
		for _, f := range files {
			if hasExt(f, subtitleExt) {
				return Undetermined, "subtitle_files_present" // original audio is usually kept
			}
		}
		return NonEnglish, "native_script_no_subtitle_marker"
	}
	return Undetermined, ""
}
