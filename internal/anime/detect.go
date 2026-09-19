// Package anime provides deterministic, LLM-free detection of anime
// release-name conventions: fansub-group brackets, absolute episode
// numbering, romaji season markers, and English-track markers.
//
// It exists so the anime signal can be computed on the ALWAYS-ON path
// (DHT ingest, the deterministic classifier, the content filter) instead
// of only inside the optional LLM matcher — where, historically, the
// `is_anime` determination lived, was used once for an English-track
// gate, and was then discarded. Callers that must not delete watchable
// anime (the content filter) use Signals.IsAnime / IsKnownFansub to carve
// anime out of destructive drops.
package anime

import (
	"regexp"
	"strings"
)

// Signals is the deterministic read of a release name's anime markers.
// The zero value means "no anime signal found."
type Signals struct {
	// FansubGroup is the normalized name of a KNOWN English-subbing anime
	// group found as a bracketed token (e.g. "[SubsPlease]"). Empty when
	// none matched. Known groups are disjoint from adult studios, so a
	// non-empty value is a high-precision "English-fansubbed anime" signal
	// safe to use even on the NSFW-keyword path.
	FansubGroup string

	// LeadingLatinGroup is the content of a leading [..] bracket when that
	// content is Latin-script (an ASCII group tag such as "[Erai-raws]"),
	// even if the group is not in the known allowlist. Empty for a leading
	// CJK-script bracket (a raw group), no leading bracket, or a name
	// carrying a dotted scene-date token (see sceneDateRe) — there the
	// leading bracket is an adult STUDIO tag, not a fansub tag. Used, in
	// combination with another marker, to recognise new/unlisted fansub
	// groups without rescuing native-script raws.
	LeadingLatinGroup string

	// AbsoluteEpisode is the absolute episode number parsed from the
	// canonical anime shape "Title - NNN" (1-4 digits), 0 when absent.
	// Years (1900-2099) and resolutions are rejected so "Show - 2011" and
	// "Show - 1080p" don't produce a spurious episode.
	AbsoluteEpisode int

	// AbsoluteRange is true when the absolute number is the START of a batch
	// range ("Title - 01 ~ 24", "Title - 01 - 24") or a fractional special
	// ("Title - 05.5"). AbsoluteEpisode still carries the start value so the
	// anime-ness inference (IsAnime) is unchanged, but consumers persisting a
	// single absolute episode must skip these — the start of a batch is not
	// THE episode of the release.
	AbsoluteRange bool

	// SeasonMarker is true for romaji/anime season notations the standard
	// SxxExx parser misses: "2nd Season", "Cour 2", "Part 2",
	// "Final Season".
	SeasonMarker bool

	// EnglishTrack is true when the name advertises an English audio or
	// subtitle track (Dual Audio, Multi-Audio, Eng Sub/Dub, Multiple
	// Subtitle) — the "watchable in English" signal. Computed from the
	// ORIGINAL combined regex so the IsAnime inference is byte-stable;
	// the split Audio/Sub signals below are richer but deliberately do
	// NOT feed IsAnime.
	EnglishTrack bool

	// EnglishAudioSignal is true when the name explicitly advertises an
	// ENGLISH AUDIO track (dub): Dual/Multi-Audio, Eng Dub, Dubbed,
	// (Dub)/[Dub], English Audio. Wider than EnglishTrack's audio half —
	// covers the bare forms the exam found missing — and separate from
	// subtitle advertisements.
	EnglishAudioSignal bool

	// EnglishSubSignal is true when the name explicitly advertises
	// English (or multi) SUBTITLES: Eng Subs, Multi-Subs, Multiple
	// Subtitle, Subbed, (Sub)/[EngSub], softsubs.
	EnglishSubSignal bool
}

// IsAnime reports whether the name carries a strong anime signal. Used to
// carve anime out of the non-Latin-script and non-English-language drops.
// English-track alone does NOT qualify (a Western "Dual Audio" release is
// not anime); it only counts alongside a leading group bracket — the
// fansub shape.
func (s Signals) IsAnime() bool {
	if s.FansubGroup != "" {
		return true
	}
	if s.LeadingLatinGroup != "" && (s.AbsoluteEpisode > 0 || s.SeasonMarker || s.EnglishTrack) {
		return true
	}
	return false
}

// IsAnimeIndependentOfAbsolute reports anime-ness established WITHOUT the
// absolute-episode marker: a known fansub group, or an unlisted Latin group
// tag alongside a season or English-track marker. Consumers persisting the
// absolute number gate on this rather than IsAnime, whose unlisted-group path
// is satisfied BY the absolute number — a circular gate that would stamp any
// "[LatinTag] Anything - NN" ("[Brazzers] Scene - 12", "[Multi] Thing - 12").
func (s Signals) IsAnimeIndependentOfAbsolute() bool {
	if s.FansubGroup != "" {
		return true
	}
	return s.LeadingLatinGroup != "" && (s.SeasonMarker || s.EnglishTrack)
}

// IsKnownFansub reports whether a KNOWN anime fansub group was found.
// This is the porn-safe predicate: because the allowlist is disjoint from
// adult studios, it can gate the NSFW-keyword drop without risking that a
// studio-tagged adult release is rescued.
func (s Signals) IsKnownFansub() bool { return s.FansubGroup != "" }

// fansubGroupNames is a curated allowlist of English-releasing anime groups —
// the single source of truth, in canonical display casing. knownFansubGroups
// (normalized, for detection) and canonicalFansubName are derived from it so
// they can never drift. Matched only against bracketed tokens, the fansub
// convention, so a title merely containing the word can't false-match.
//
// Membership rules — a listed group's bracket alone makes IsAnime and the
// porn-safe IsKnownFansub true, and (unless bucketed below) manufactures an
// english_audio=sub verdict, so:
//   - keep it disjoint from adult studios (IsKnownFansub gates the NSFW drop);
//   - only list groups whose releases are English-watchable, or bucket them in
//     rawFansubGroups / dubFansubGroups;
//   - groups that sub in OTHER languages (CHS/CHT, Spanish, Hungarian…) stay
//     OFF the list entirely, however many rows they lead — see
//     TestNonEnglishSubGroupsStayOffTheAllowlist for the census exclusions.
var fansubGroupNames = []string{
	"SubsPlease", "Erai-raws", "HorribleSubs", "Judas", "ASW", "EMBER",
	"Anime Time", "Ohys-Raws", "Commie", "GJM", "Doki", "Chihiro", "Cleo",
	"Kaleido", "Kaleido-subs", "SSA", "Kametsu", "Beatrice-Raws", "Yameii",
	"VARYG", "NanDesuKa", "DKB", "URANIME", "ToonsHub", "Sokudo", "Aergia",
	"Lycoris", "Crymore", "Nep_Blanc", "Coalgirls", "VCB-Studio", "MTBB",
	"no-raws", "One Pace", "Arid", "smol",
	// 2026-08 prod census refresh (top unlisted brackets over is_anime rows).
	// English-sub / English-inclusive groups, verified via their own release
	// markers (Dual-Audio, Multi-Subs, Eng-Sub) or nyaa's English-translated
	// category (Dynamis One, Raze, VON, SubsMix):
	"AnimeRG", "BlackRabbit", "Breeze", "DragsterPS", "Dynamis One",
	"KawaSubs", "Kosaka", "LostYears", "NeoLX", "Raze", "Subeteka",
	"SubsMix", "Trix", "VON", "neoDESU", "neoHEVC",
	// English-dub rippers (bucketed in dubFansubGroups):
	"TRC", "Golumpa",
	// Raw-capture groups, no subs (bucketed in rawFansubGroups):
	"NanakoRaws", "AsukaRaws", "New-raws", "shincaps",
}

// knownFansubGroups is the normalized (separator-stripped, lower-cased) set
// used for bracket-token matching, derived from fansubGroupNames.
var knownFansubGroups = func() map[string]struct{} {
	m := make(map[string]struct{}, len(fansubGroupNames))
	for _, g := range fansubGroupNames {
		m[normGroup(g)] = struct{}{}
	}
	return m
}()

// canonicalFansubName maps a normalized group key back to its canonical
// display casing (e.g. "subsplease" -> "SubsPlease", "erairaws" ->
// "Erai-raws"), derived from fansubGroupNames so it can never drift from the
// allowlist. Used by KnownGroup to emit a stable, human-readable group name.
var canonicalFansubName = func() map[string]string {
	m := make(map[string]string, len(fansubGroupNames))
	for _, g := range fansubGroupNames {
		m[normGroup(g)] = g
	}
	return m
}()

var (
	// bracketTokenRe extracts the content of every [..] token.
	bracketTokenRe = regexp.MustCompile(`\[([^\]]+)\]`)
	// leadingBracketRe captures a leading [..] token (after optional space).
	leadingBracketRe = regexp.MustCompile(`^\s*\[([^\]]+)\]`)
	// absoluteEpRe captures "Title - NNN" where NNN is 1-4 digits, allowing
	// an optional version suffix (v2) and requiring a boundary after.
	absoluteEpRe = regexp.MustCompile(`(?:^|\s)-\s(\d{1,4})(?:v\d)?(?:\s|$|[\[(.])`)
	// absoluteRangeTailRe, applied at the end of the captured number, detects
	// a batch-range tail ("01 ~ 24", "01 - 24") or a fractional special
	// ("05.5") — shapes where the captured number is a range start, not the
	// release's episode.
	absoluteRangeTailRe = regexp.MustCompile(`^(?:v\d)?\s*(?:[~-]\s*\d|\.\d)`)
	// dateTailRe rejects a DD.MM.YYYY / DD.MM.YY tail, the shape adult
	// studio releases use ("[IKnowThatGirl] Performer (Title - 16.01.2017)").
	// Without this the leading day parses as an absolute episode, which —
	// combined with the leading Latin bracket — makes Signals.IsAnime true
	// and stamps 243 adult releases into Torznab's anime category 5070.
	dateTailRe = regexp.MustCompile(`^\.\d{1,2}\.\d{2,4}\b`)
	// sceneDateRe matches a dotted numeric date token anywhere in the name
	// (DD.MM.YYYY / DD.MM.YY / MM.DD.YY — order-agnostic), the adult-scene
	// release convention: "[BrazzersExxtra] Performer - Title Part 1
	// (19.01.2020)", "[FirstAnalQuest] Performer - 433 (13.02.2017)",
	// "[ATKGirlFriends] Performer - Hawaii Part 2 [07.29.19]". dateTailRe
	// only guards a date DIRECTLY after the absolute number, so these shapes
	// still reached IsAnime via "Part N" season markers or a scene number
	// before a parenthesized date (345 live rows measured on the 2026-08-24
	// prod census, every one an adult studio). A scene-dated name's leading
	// bracket is a studio tag, not a fansub tag, so Detect withholds
	// LeadingLatinGroup entirely. Real anime never dots a D.M.Y date into
	// the name; JP-raw YYYY.MM.DD airdates ("2023.01.05") don't match — the
	// leading \b cannot sit between two digits.
	sceneDateRe = regexp.MustCompile(`\b\d{1,2}\.\d{1,2}\.\d{2,4}\b`)
	// englishTrackRe matches English audio/sub-track advertisements.
	englishTrackRe = regexp.MustCompile(`(?i)(dual[ ._-]?audio|multi[ ._-]?audio|eng(?:lish)?[ ._-]?(?:dub|sub)s?|multi[ ._-]?subs?|multiple[ ._-]?subtitle|\[dual\])`)
	// englishAudioRe / englishSubRe are the SPLIT halves feeding the
	// english_audio column. Audio adds the bare forms the combined regex
	// missed (Dubbed, (Dub), English Audio); bare "dual"/"raw" stay out —
	// "Dual" is a movie title and fansub groups embed "Raws".
	englishAudioAnchoredRe = regexp.MustCompile(`(?i)(dual[ ._-]?audio|multi[ ._-]?audio|eng(?:lish)?[ ._-]?dub(?:bed)?s?|[\[(]dub[\])]|english[ ._-]?audio|\[dual\])`)
	englishAudioBareRe     = regexp.MustCompile(`(?i)\bdubbed\b`)
	englishSubAnchoredRe   = regexp.MustCompile(`(?i)(eng(?:lish)?[ ._-]?subs?|multi[ ._-]?subs?|multiple[ ._-]?subtitle|[\[(](?:eng)?subs?[\])]|soft[ ._-]?subs?)`)
	englishSubBareRe       = regexp.MustCompile(`(?i)\bsubbed\b`)
	// nonEnglishTrackRe suppresses the BARE dubbed/subbed forms when the
	// adjacent language token names a non-English dub/sub ("Hindi Dubbed",
	// "Spanish Subbed") — a common real release class the bare forms would
	// otherwise mislabel as English. The anchored forms above are immune
	// ("English Dubbed" carries its own language token); "multi" stays OUT
	// of this list, matching the existing Dual/Multi=English convention
	// until the LLM phase revisits it.
	nonEnglishTrackRe = regexp.MustCompile(`(?i)(hindi|tamil|telugu|kannada|malayalam|bengali|french|german|spanish|castellano|italian|portuguese|russian|polish|arabic|latino)[ ._-]{0,2}(dub|sub)(?:bed)?s?\b`)
	// rawFansubGroups release UNSUBBED transport streams or encode-only
	// remuxes, so membership of the allowlist says nothing about an English
	// track. Excluded from the group-implies-English-subs inference below.
	rawFansubGroups = map[string]struct{}{
		normGroup("Ohys-Raws"): {}, normGroup("Beatrice-Raws"): {},
		normGroup("no-raws"): {}, normGroup("VCB-Studio"): {},
		normGroup("NanakoRaws"): {}, normGroup("AsukaRaws"): {},
		normGroup("New-raws"): {}, normGroup("shincaps"): {},
	}
	// dubFansubGroups release English DUBS rather than subs (TRC and Golumpa
	// rip the CR/Funimation dub streams; their "FuniDub"/"CR-Dub" tags carry
	// no bare token the audio regexes recognise, so the bucket is what turns
	// their unmarked names into a dub verdict).
	dubFansubGroups = map[string]struct{}{
		normGroup("Yameii"): {}, normGroup("TRC"): {}, normGroup("Golumpa"): {},
	}
	// seasonMarkerRe matches romaji/anime season notations SxxExx misses.
	seasonMarkerRe = regexp.MustCompile(`(?i)(\b\d(?:st|nd|rd|th)\ season\b|\bcour\ ?\d\b|\bpart\ \d\b|\bfinal\ season\b)`)
	// asciiLetterRe requires at least one ASCII letter (a group tag, not a
	// pure-numeric "[1080p]"-style tag — those carry no group identity).
	asciiLetterRe = regexp.MustCompile(`[A-Za-z]`)
)

// resolutionHeights are values that can appear after " - " but are a
// resolution, not an absolute episode.
var resolutionHeights = map[int]struct{}{
	360: {}, 480: {}, 540: {}, 576: {}, 720: {}, 1080: {}, 1440: {}, 2160: {}, 4320: {},
}

// normGroup lower-cases and strips separators so "Erai-raws", "erai raws"
// and "erairaws" all compare equal.
func normGroup(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.NewReplacer("-", "", "_", "", " ", "", ".", "").Replace(s)
}

// isLatinScript reports whether s is entirely ASCII and carries at least
// one ASCII letter — i.e. a Latin-script group tag, not a CJK raw group.
func isLatinScript(s string) bool {
	for _, r := range s {
		if r > 127 {
			return false
		}
	}
	return asciiLetterRe.MatchString(s)
}

// Detect reads the anime markers out of a release name.
func Detect(name string) Signals {
	var s Signals

	// Known fansub group in any bracketed token.
	for _, m := range bracketTokenRe.FindAllStringSubmatch(name, -1) {
		if _, ok := knownFansubGroups[normGroup(m[1])]; ok {
			s.FansubGroup = normGroup(m[1])
			break
		}
	}

	// Leading Latin-script bracket group (a possibly-unlisted fansub tag).
	// A dotted scene-date token marks the bracket as an adult studio tag
	// instead, so no unlisted-group signal is emitted for it.
	if m := leadingBracketRe.FindStringSubmatch(name); m != nil {
		if inner := strings.TrimSpace(m[1]); isLatinScript(inner) && !sceneDateRe.MatchString(name) {
			s.LeadingLatinGroup = inner
		}
	}

	// Absolute episode: "Title - NNN", rejecting years and resolutions.
	if m := absoluteEpRe.FindStringSubmatchIndex(name); m != nil {
		numStr := name[m[2]:m[3]]
		if n := atoi(numStr); n > 0 {
			_, isRes := resolutionHeights[n]
			isYear := n >= 1900 && n <= 2099
			if !isRes && !isYear && !dateTailRe.MatchString(name[m[3]:]) {
				s.AbsoluteEpisode = n
				s.AbsoluteRange = absoluteRangeTailRe.MatchString(name[m[3]:])
			}
		}
	}

	s.SeasonMarker = seasonMarkerRe.MatchString(name)
	s.EnglishTrack = englishTrackRe.MatchString(name)
	nonEnglish := nonEnglishTrackRe.MatchString(name)
	s.EnglishAudioSignal = englishAudioAnchoredRe.MatchString(name) ||
		(englishAudioBareRe.MatchString(name) && !nonEnglish)
	s.EnglishSubSignal = englishSubAnchoredRe.MatchString(name) ||
		(englishSubBareRe.MatchString(name) && !nonEnglish)

	// A curated subbing group IS the English-track evidence: [SubsPlease],
	// [Erai-raws] and friends exist to publish English-subtitled releases and
	// almost never advertise it in the name. Applied only as a FALLBACK: any
	// explicit advertisement in the name wins, in EITHER direction. "[Judas] …
	// [Dual-Audio]" stays a pure audio signal, and "[SubsPlease] … French
	// Dubbed" keeps no English signal at all — without the guard the group
	// would manufacture one and ForeignAudioOnly would then keep a French dub.
	// The guard is explicitForeignTrack, which unions every foreign-track token
	// this package knows — audio ("Dublado", "Lektor PL", "VFF", "Dublaj"),
	// "<language> Dub/Sub" compounds, and standalone subtitle tokens
	// ("VOSTFR", "SweSub", "Napisy PL"). Three rounds of review each found one
	// more family the guard missed, so it now unions the lot rather than
	// naming them case by case. Without this fallback, 43,784 of the
	// 53,945 anime rows carrying no english_audio verdict are simply unknown
	// while their group name already answers the question. Raw/encode groups
	// are excluded; Yameii dubs.
	if s.FansubGroup != "" && !s.EnglishAudioSignal && !s.EnglishSubSignal &&
		!explicitForeignTrack(name) {
		if _, raw := rawFansubGroups[s.FansubGroup]; !raw {
			if _, dub := dubFansubGroups[s.FansubGroup]; dub {
				s.EnglishAudioSignal = true
			} else {
				s.EnglishSubSignal = true
			}
		}
	}
	return s
}

// KnownGroup returns the canonical display name of a KNOWN anime fansub
// group named in a bracketed token of a release name (e.g. "[SubsPlease]
// Show - 12 …" -> "SubsPlease", "[erai raws] …" -> "Erai-raws"), or "" when
// no allowlisted group is present.
//
// Anime conventionally leads with the group tag — a bracket that the title
// parser strips from the base title and the trailing "-GROUP" scene
// extractor never sees, so without recovering it the fansub group is
// discarded for anime, leaving Torznab's AttrTeam and the release-group FTS
// field empty and costing Sonarr its per-group release scoring and dedup.
//
// Extraction is deliberately restricted to the curated fansubGroupNames
// allowlist (the same single source of truth that drives detection and the
// SQL anime filter). That allowlist is disjoint from adult studios, so a
// match can never surface a studio name as a release group — this predicate
// is porn-safe by construction (like IsKnownFansub). A generic leading
// Latin-script bracket is intentionally NOT treated as a group here: an
// adversarial review found that path mislabels Western encoder tags with a
// Dual/Multi-Audio advertisement ("[Tigole] … Dual-Audio"), out-of-list
// adult studios ("[Brazzers] … - 12"), and format descriptors ("[Multi] …
// - 12"). New groups are onboarded by extending fansubGroupNames, a one-line
// change guarded by TestFansubGroupNames_SingleSourceConsistency.
func KnownGroup(name string) string {
	fg := Detect(name).FansubGroup
	if fg == "" {
		return ""
	}
	if canon, ok := canonicalFansubName[fg]; ok {
		return canon
	}
	// fg is always a normalized allowlist key here; return it defensively.
	return fg
}

// atoi parses a small non-negative integer; returns 0 on any non-digit.
func atoi(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
