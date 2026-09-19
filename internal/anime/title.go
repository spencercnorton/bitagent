package anime

import (
	"regexp"
	"strings"
)

// langAlt is the language alternation used inside technicalTokenRe, in BOTH
// its short and full forms. The full forms matter because the majority scorer
// works on fields: "[English Dub]" scored 1/2 with only `dub` recognised, fell
// under the majority, and was kept — so CleanTitle returned
// "OVERLORD - The Sacred Kingdom (2024) [English Dub]" and searched TMDB on it.
//
// Every alternative here is matched ANCHORED AT BOTH ENDS (see technicalTokenRe),
// which is what makes the full words safe to add: "English" cannot prefix-match
// "Engage", and a genuine bracket keeps its title because the MAJORITY rule
// still applies — "[English Patient]" scores 1/2 and survives.
const langAlt = `eng?|english|jap?|jpn|japanese|chs|cht|chinese|mandarin|cantonese|` +
	`kor|korean|ita|italian|spa|spanish|fre?|french|ger|german|por|portuguese|` +
	`rus|russian|ara|arabic|tha|thai|vie|vietnamese|ind|indonesian|` +
	`pol|polish|dut|dutch|swe|swedish|dan|danish|nor|norwegian|fin|finnish|` +
	`tur|turkish|hin|hindi|tam|tamil|tel|telugu|ukr|ukrainian`

var (
	// technicalTokenRe recognises a bracketed token as RELEASE METADATA rather
	// than part of the title: resolution, codec, source, audio, container,
	// language/sub advertisement, batch marker, or a bare CRC32.
	//
	// This allowlist is the whole safety mechanism. Anime titles legitimately
	// contain bracketed qualifiers — "Fate/stay night [Unlimited Blade Works]"
	// is a different series from the 2006 "Fate/stay night", and "[Oshi no Ko]"
	// is bracketed in its own right — so truncating at the FIRST bracket
	// silently produces a plausible-but-wrong search title, which is the exact
	// misattachment this file exists to prevent.
	// Anchored at BOTH ends. Start-anchoring alone made short alternatives
	// match ordinary words by prefix — `eng?` matched "Engage", `cr` matched
	// "Crunchyroll", `ind` matched "Indigo", `raw` matched "Rawhide" — so
	// "[Engage Kiss]" was read as metadata and the real title was destroyed.
	technicalTokenRe = regexp.MustCompile(`(?i)^(?:` +
		`\d{3,4}p|\d{3,4}x\d{3,4}|4k|uhd|sd|hd|` + // resolution
		`x26[45]|h\.?26[45]|hevc|avc|xvid|divx|av1|10 ?bit|8 ?bit|hi10p?|` + // codec
		`bd ?rip|bd|blu-?ray|dvd ?rip|dvd|web-?dl|web ?rip|web|hdtv|tv|remux|` + // source
		`cr|amzn|nf|hidive|adn|funi(?:mation)?|disney\+?|abema|baha|b-?global|` + // platform
		`aac|ac3|eac3|flac|dts(?:-?hd)?|opus|mp3|ddp?\d(?:\.\d)?|` + // audio
		`mkv|mp4|avi|ts|m2ts|` + // container
		`multi ?-?subs?|multiple subtitles?|dual ?-?audio|multi ?-?audio|audio|` + // tracks
		`sub(?:bed|s)?|dub(?:bed|s)?|raws?|uncensored|censored|` +
		`batch|complete|repack|v\d|final|` + // release state
		`[0-9a-f]{8}|` + // CRC32
		`(?:` + langAlt + `)(?:[,+/ ]?(?:` + langAlt + `))*` + // language lists
		`|multiple|subtitles?` + // standalone halves of multi-word tags
		`)$`)

	// bracketSpanRe finds each [..] or (..) token with its position.
	bracketSpanRe = regexp.MustCompile(`[\[(][^\])]*[\])]`)
	// fileExtRe strips a trailing container extension.
	fileExtRe = regexp.MustCompile(`(?i)\.(mkv|mp4|avi|ts|m2ts|ogm|wmv|rmvb)$`)
	// versionTailRe strips a trailing "v2"/"v3" release-version marker.
	versionTailRe = regexp.MustCompile(`(?i)\s*v\d+$`)
	// titleJunkTailRe strips separators left dangling at the end.
	titleJunkTailRe = regexp.MustCompile(`[\s._~-]+$`)
	// batchRangeRe detects an episode-range batch ("27-39", "01~24", "(01-12)").
	batchRangeRe = regexp.MustCompile(`(?:^|[\s._(\[])\d{1,4}\s*[-~]\s*\d{1,4}(?:$|[\s._)\]])`)
	// movieMarkerRe detects an explicit film marker.
	movieMarkerRe = regexp.MustCompile(`(?i)(?:^|[\s._\[(-])(?:movie|gekijouban|gekijou-?ban|the movie|film|劇場版)(?:$|[\s._\])-])`)
)

// isTechnicalBracket reports whether a bracketed token's CONTENT is release
// metadata rather than part of the title.
//
// Three tiers, in order: a known fansub group; the WHOLE content matching a
// technical token (which is how multi-word tags like "10 bit" and "Dual Audio"
// are caught, since splitting would destroy them); otherwise a MAJORITY of
// fields — counting adjacent pairs too — must be technical.
func isTechnicalBracket(tok string) bool {
	inner := strings.TrimSpace(strings.Trim(tok, "[]()"))
	if inner == "" {
		return true
	}
	if KnownGroup("["+inner+"]") != "" {
		return true
	}
	// Whole-content check FIRST. Several technical tags are multi-word
	// ("10 bit", "Dual Audio", "Blu Ray", "WEB DL"), and splitting on spaces
	// destroys them before they can match — "[10 bit]" would score 0/2 and be
	// kept as part of the title, putting the noise into the live TMDB search.
	if technicalTokenRe.MatchString(inner) {
		return true
	}
	fields := strings.FieldsFunc(inner, func(r rune) bool {
		return r == ' ' || r == '_' || r == '.' || r == ','
	})
	if len(fields) == 0 {
		return true
	}
	// MAJORITY, not "the first field" and not "any field".
	//
	// "First field only" misses real metadata whose leading token is a broadcast
	// station we would otherwise have to enumerate — "(AT-X 1280x720 x264 AAC)",
	// "(BS11 1280x720 x264 AAC)". "Any field" would re-break the safety
	// property, since one incidental match ("Complete" in a title, say) would
	// condemn the whole bracket.
	//
	// Requiring more than half keeps station-prefixed metadata technical (3 of 4
	// match) while leaving genuine qualifiers alone: "(Endless Eight)",
	// "[Indigo League]", "[Unlimited Blade Works]", "(Director's Cut)" all score
	// zero.
	// Score each field, and also each ADJACENT PAIR, so a multi-word tag
	// embedded in a longer bracket still counts — "[1080p 10 bit HEVC]" would
	// otherwise score 2/4 (1080p, HEVC) and fall under the majority.
	counted := make([]bool, len(fields))
	for i, f := range fields {
		if technicalTokenRe.MatchString(f) {
			counted[i] = true
		}
	}
	for i := 0; i+1 < len(fields); i++ {
		if counted[i] && counted[i+1] {
			continue
		}
		pair := fields[i] + " " + fields[i+1]
		if technicalTokenRe.MatchString(pair) || technicalTokenRe.MatchString(fields[i]+fields[i+1]) {
			counted[i], counted[i+1] = true, true
		}
	}
	technical := 0
	for _, c := range counted {
		if c {
			technical++
		}
	}
	return technical*2 > len(fields)
}

// IsBatch reports whether a fansub release is an episode batch rather than a
// single episode or a film. Batches must type as tv_show, not movie.
func IsBatch(name string, s Signals) bool {
	if s.AbsoluteRange {
		return true
	}
	lower := strings.ToLower(name)
	if strings.Contains(lower, "[batch]") || strings.Contains(lower, "(batch)") ||
		strings.Contains(lower, "complete series") || strings.Contains(lower, "complete season") {
		return true
	}
	return batchRangeRe.MatchString(name)
}

// IsExplicitMovie reports whether the name carries an explicit film marker.
// Without one, an ambiguous bracket-only fansub release is assumed to be TV:
// fansub output is overwhelmingly episodic, and guessing `movie` routes TMDB
// matching down the film path where it can attach an unrelated film.
func IsExplicitMovie(name string) bool { return movieMarkerRe.MatchString(name) }

// CleanTitle recovers the series/film title from a fansub-style release name,
// or returns "" when it cannot do so confidently.
//
// It exists because the generic title parser cannot handle the fansub shape:
// for "[SubsPlease] One Piece - 1077 (480p) [3FC90F00].mkv" it yields the
// literal "[SubsPlease ]One Piece 1077 (480p) [3FC90F00 ]mkv". Feeding that to
// the TMDB search is worse than useless — with fuzzy and alt-title matching
// enabled it is a misattachment surface, and it costs a live API call per
// release to produce nothing.
//
// The contract is deliberately asymmetric: a confident clean title, or nothing.
// Callers MUST treat "" as "do not search", never as "search with the raw
// name". Rescuing a torrent from deletion is the goal; attaching it to the
// wrong work is a worse outcome than leaving it unattached.
func CleanTitle(name string, s Signals) string {
	if name == "" {
		return ""
	}
	t := name

	// Strip leading brackets ONLY while they are recognisably technical or a
	// known group. A leading bracket that is neither may be the title itself
	// ("[Oshi no Ko]"), so stripping stops there.
	for {
		loc := bracketSpanRe.FindStringIndex(t)
		if loc == nil || strings.TrimSpace(t[:loc[0]]) != "" {
			break
		}
		if !isTechnicalBracket(t[loc[0]:loc[1]]) {
			break
		}
		t = t[loc[1]:]
	}

	// Underscore-separated releases ("[Coalgirls]_Soul_Eater_27-39_(...)").
	// Only when there is no space at all, so real titles keep their spacing.
	if !strings.Contains(t, " ") && strings.Contains(t, "_") {
		t = strings.ReplaceAll(t, "_", " ")
	}

	// Cut at the absolute-episode marker when the detector found one. Anchor on
	// the same " - NNN" shape the detector matched so the two never disagree.
	if s.AbsoluteEpisode > 0 {
		if m := absoluteEpRe.FindStringIndex(t); m != nil {
			t = t[:m[0]]
		}
	}

	// Cut at the first TECHNICAL bracket. A non-technical bracket is kept —
	// "Fate/stay night [Unlimited Blade Works]" must not become the 2006 series.
	for _, loc := range bracketSpanRe.FindAllStringIndex(t, -1) {
		if isTechnicalBracket(t[loc[0]:loc[1]]) {
			t = t[:loc[0]]
			break
		}
	}

	t = fileExtRe.ReplaceAllString(t, "")
	t = versionTailRe.ReplaceAllString(t, "")
	// Drop a trailing batch range ("Soul Eater 27-39") — it is not the title.
	t = batchRangeRe.ReplaceAllString(t, "")
	t = titleJunkTailRe.ReplaceAllString(t, "")
	t = strings.Join(strings.Fields(t), " ")

	// Confidence floor. A 1-2 character residue, or one with no letters, is
	// noise rather than a title and must not reach the TMDB search.
	if len([]rune(t)) < 3 || strings.IndexFunc(t, isLetterRune) < 0 {
		return ""
	}
	return t
}

func isLetterRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r > 127
}
