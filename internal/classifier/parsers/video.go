package parsers

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/hedhyw/rex/pkg/dialect"
	"github.com/hedhyw/rex/pkg/rex"
	"github.com/spencercnorton/bitagent/internal/anime"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/keywords"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/regex"
)

// siteNoisePrefixRegex matches leading tracker/site junk that release names are
// frequently prefixed with, e.g. "www.Torrenting.com - ", "www.UIndex.org    -    ".
// The title parser is anchored to the start of the name, so a domain prefix
// poisons BaseTitle (and the downstream TMDB Levenshtein match) with garbage
// tokens. A real title never starts with a bare "<domain>.<tld> -" run — the
// required trailing dash keeps this precise (a film literally named
// "Something.com" has no " - " after the TLD, so it is left untouched).
var siteNoisePrefixRegex = regexp.MustCompile(
	`(?i)^\s*(?:www\.)?[a-z0-9][a-z0-9-]*\.(?:com|net|org|info|biz|me|tv|cc|to|io|xyz|club|se|eu|nz|link|app|us|uk|co)\b[\s._]*-+[\s._]*`,
)

// stripSiteNoisePrefix removes up to a few stacked leading site prefixes.
func stripSiteNoisePrefix(name string) string {
	for i := 0; i < 3; i++ {
		loc := siteNoisePrefixRegex.FindStringIndex(name)
		if loc == nil || loc[0] != 0 {
			break
		}
		name = name[loc[1]:]
	}
	return name
}

// ParseOptions gates parse behaviors that must stay byte-identical to legacy
// output until measured on the eval harness (shadow n>=500, binomial LB>=0.99
// per the roadmap enable rule).
type ParseOptions struct {
	// NoiseV2 enables the second-generation noise stripping and the
	// leading-year rescue (roadmap WI2.3): fullwidth CJK site-tag spans,
	// www.-prefixed site prefixes on any TLD, extra known torrent-site TLDs,
	// and adopting a leading bare year ("2019.Title.1080p...") as the release
	// year when the remainder carries none of its own.
	NoiseV2 bool
}

// cjkSpanRegex matches fullwidth-bracket site/decoration tags
// ("【高清影视之家发布 www.BBQDDQ.com】") anywhere in the name. These are
// pure decoration around CJK-market releases whose real scene name follows;
// the fullwidth brackets are never part of a title.
var cjkSpanRegex = regexp.MustCompile(`【[^】]*】`)

// siteNoisePrefixV2WWWRegex extends prefix stripping to www.-prefixed hosts on
// ANY TLD — "www." followed by a dotted host and a separator is unambiguously a
// site tag regardless of TLD ("www.1TamilMV.rsvp - ..."). The separator is a
// dash, or a run of two or more spaces: "www.Torrenting.org       For All
// Mankind S01E09" carries no dash, and left unstripped it poisoned the base
// title of every such release (8 gold rows across 6 shows on the 2026-09-22
// benchmark). A single space or a dot is still not enough — "www.Example.com
// Title" and "www.Site.org.Title" are left untouched, preserving the v1
// regex's precision argument.
var siteNoisePrefixV2WWWRegex = regexp.MustCompile(
	`(?i)^\s*www\.[a-z0-9][a-z0-9-]*(?:\.[a-z0-9-]+)*\.[a-z]{2,10}\b(?:[\s._]*-+[\s._]*|\s{2,})`,
)

// siteNoisePrefixV2TLDsRegex covers non-www prefixes on TLDs observed in the
// residual forensics (2026-07-07 9K sample: 6.7% of names carry a site prefix)
// that the v1 allowlist misses.
var siteNoisePrefixV2TLDsRegex = regexp.MustCompile(
	`(?i)^\s*[a-z0-9][a-z0-9-]*\.(?:rsvp|pics|la|pl|vip|red|win|pro|site|live|online|top|fun|icu|cyou|ws|ru|in|ph|ai|gg|cx|sh|st|mov|fyi|lol|day|wtf|autos|skin)\b[\s._]*-+[\s._]*`,
)

func stripSiteNoisePrefixV2(name string) string {
	name = strings.TrimSpace(cjkSpanRegex.ReplaceAllString(name, " "))

	for i := 0; i < 3; i++ {
		stripped := stripSiteNoisePrefix(name)

		if loc := siteNoisePrefixV2WWWRegex.FindStringIndex(stripped); loc != nil {
			stripped = stripped[loc[1]:]
		}

		if loc := siteNoisePrefixV2TLDsRegex.FindStringIndex(stripped); loc != nil {
			stripped = stripped[loc[1]:]
		}

		if stripped == name {
			break
		}

		name = stripped
	}

	return name
}

// leadingYearRegex captures a bare year opening the name ("2019.The.Irishman
// ...") — a form the title/year cascade cannot parse because titleTokens must
// consume at least one token before yearTokens may match, so the year poisons
// BaseTitle instead of anchoring the cut.
var leadingYearRegex = regexp.MustCompile(`^((?:19|20)\d{2})[\s._-]+(\S.*)$`)

// techCutRegex marks the first unambiguous technical token; in the v2
// leading-year rescue it bounds the title when the remainder has no year or
// episode anchor of its own.
var techCutRegex = regexp.MustCompile(
	`(?i)[\s._-](?:2160p|1080p|720p|480p|576p|4k|uhd|bluray|blu-ray|bdrip|brrip|web-?dl|web-?rip|hdtv|dvdrip|dvd-?r|hdrip|camrip|telesync|x264|x265|h\.?264|h\.?265|hevc|xvid|divx|av1|remux|proper|repack|internal|limited)\b`,
)

// rescueLeadingYear attempts the v2 leading-year parse. It returns ok=false
// when the name does not open with a bare year, when the remainder carries a
// year of its own (the legacy cascade already handles "2046.2004..." style
// year-titled films correctly), or when no clean title can be carved from the
// remainder. The returned rest is the technical tail after the title, for
// attribute/language inference.
func rescueLeadingYear(name string) (string, model.Year, string, bool) {
	m := leadingYearRegex.FindStringSubmatch(name)
	if m == nil {
		return "", 0, "", false
	}

	yearVal, _ := strconv.ParseUint(m[1], 10, 16)
	remainder := m[2]

	if titleYearRegex.MatchString(remainder) {
		return "", 0, "", false
	}

	titlePart, rest := remainder, ""
	if loc := techCutRegex.FindStringIndex(remainder); loc != nil {
		titlePart, rest = remainder[:loc[0]], remainder[loc[0]:]
	}

	title := cleanTitle(titlePart)
	if title == "" {
		return "", 0, "", false
	}

	return title, model.Year(yearVal), rest, true
}

var titleTokens = []dialect.Token{
	rex.Group.Define(
		rex.Group.Composite(
			rex.Group.NonCaptured(
				regex.AnyWordChar().Repeat().OneOrMore(),
				rex.Group.NonCaptured(
					rex.Chars.Single('-'), regex.AnyWordChar().Repeat().OneOrMore(),
				).Repeat().ZeroOrMore(),
			),
			regex.AnyNonWordChar().Repeat().OneOrMore(),
		).NonCaptured().Repeat().OneOrMore(),
		rex.Group.Composite(
			regex.AnyNonWordChar().Repeat().OneOrMore(),
			rex.Chars.End(),
		).NonCaptured(),
	),
}

var titleRegex = rex.New(
	rex.Chars.Begin(),
	rex.Group.NonCaptured(titleTokens...),
).MustCompile()

var yearTokens = []dialect.Token{
	rex.Group.NonCaptured(rex.Common.NotClass(rex.Chars.WordCharacter()).Repeat().ZeroOrMore()),
	rex.Group.Define(
		rex.Group.Composite(
			rex.Common.Text("18"), rex.Common.Text("19"), rex.Common.Text("20"),
		).NonCaptured(),
		rex.Chars.Digits().Repeat().Exactly(2),
	),
	rex.Group.Composite(
		rex.Common.NotClass(rex.Chars.WordCharacter()),
		rex.Chars.End(),
	).NonCaptured(),
}

var titleYearRegex = rex.New(
	rex.Chars.Begin(),
	rex.Group.NonCaptured(rex.Group.NonCaptured(titleTokens...), rex.Group.NonCaptured(yearTokens...)),
).MustCompile()

var titleEpisodesRegex = rex.New(
	rex.Chars.Begin(),
	rex.Group.NonCaptured(
		rex.Group.NonCaptured(titleTokens...),
		model.EpisodesToken,
	),
).MustCompile()

var multiRegex = keywords.MustNewRegexFromKeywords("multi", "dual")

var separatorToken = rex.Chars.Runes(" ._")

// dashSeparatorRegex collapses dash-slug separators to spaces so
// "the-raid-redemption" becomes "the raid redemption" and "Spider-Man" becomes
// "Spider Man" — both of which TMDB resolves correctly. titlePartRegex only
// treats " ._" as separators, so residual dashes are normalised here.
var dashSeparatorRegex = regexp.MustCompile(`-+`)
var spaceSqueezeRegex = regexp.MustCompile(`\s+`)

// yearRangeRegex collapses a broadcast/production YEAR RANGE to its first
// (anchor) year. "Peep Show (2003-2015)" -> "Peep Show 2003". Without this the
// dash-joined run "2003-2015" is swallowed as a single title token, poisoning
// BaseTitle with "(2003" and latching the year onto the SECOND year. Open-ended
// ranges ("2005-present"/"2005-?") collapse the same way. The leading/trailing
// parens are consumed so the anchor year stands bare and is picked up normally
// by yearTokens.
var yearRangeRegex = regexp.MustCompile(
	`(?i)\(?((?:18|19|20)\d{2})\s*[-–—]\s*(?:(?:18|19|20)\d{2}|present|now|ongoing|\?{1,4})\)?`,
)

func collapseYearRange(name string) string {
	return yearRangeRegex.ReplaceAllString(name, "$1")
}

// moviePackRegex matches signals that a release is a multi-film collection
// rather than a single movie.  When combined with a year-range and no episode
// tokens the release is a pack and must NOT be attached to one movie.
var moviePackRegex = regexp.MustCompile(
	`(?i)(trilogy|dilog(?:y|ies)|quadrilogy|anthology|filmography|\bcollection\b|complete\s+(?:movies|films|collection)|\b\d+[- ]?movies?\b)`,
)

// episodeTokenRegex detects SxxExx / "Season N" / "S01-S09"-style markers that
// indicate a TV series pack (which SHOULD be resolved by the year-range logic).
var episodeTokenRegex = regexp.MustCompile(
	`(?i)(?:S\d{1,2}(?:E\d{1,2})?|season\s+\d+)`,
)

// isLikelyMoviePack reports true when name looks like a multi-film collection
// rather than a TV series pack. A TV series pack carries SxxExx/season markers
// so we return false for those — they must still resolve via year-range.
func isLikelyMoviePack(name string) bool {
	if episodeTokenRegex.MatchString(name) {
		return false
	}
	return moviePackRegex.MatchString(name)
}

var titlePartRegex = rex.New(
	separatorToken.Repeat().ZeroOrOne(),
	rex.Group.Define(regex.WordToken()),
	separatorToken.Repeat().ZeroOrOne(),
).MustCompile()

var trimTitleRegex = rex.New(
	rex.Chars.Begin(),
	rex.Group.Composite(
		rex.Group.NonCaptured(
			rex.Chars.Single('['),
			rex.Common.NotClass(rex.Chars.Single(']')).Repeat().OneOrMore(),
			rex.Chars.Single(']'),
		),
		rex.Group.NonCaptured(
			rex.Chars.Single('【'),
			rex.Common.NotClass(rex.Chars.Single('】')).Repeat().OneOrMore(),
			rex.Chars.Single('】'),
		),
	).NonCaptured().Repeat().ZeroOrOne(),
	regex.AnyNonWordChar().Repeat().ZeroOrMore(),
	rex.Group.Define(
		regex.WordToken(),
		rex.Group.NonCaptured(
			rex.Chars.Any(),
			regex.WordToken(),
		).Repeat().ZeroOrMore(),
	),
	regex.AnyNonWordChar().Repeat().ZeroOrMore(),
	rex.Chars.End(),
).MustCompile()

func cleanTitle(title string) string {
	title = titlePartRegex.ReplaceAllStringFunc(title, func(s string) string {
		partMatch := titlePartRegex.FindStringSubmatch(s)
		if partMatch == nil {
			return ""
		}

		return partMatch[1] + " "
	})
	title = trimTitleRegex.ReplaceAllString(title, "$1")
	title = dashSeparatorRegex.ReplaceAllString(title, " ")
	title = strings.TrimSpace(spaceSqueezeRegex.ReplaceAllString(title, " "))

	return title
}

func parseTitleYear(input string) (string, model.Year, string, error) {
	if match := titleYearRegex.FindStringSubmatch(input); match != nil {
		yearMatch, _ := strconv.ParseUint(match[2], 10, 16)
		title := cleanTitle(match[1])

		if title != "" {
			return title, model.Year(yearMatch), input[len(match[0]):], nil
		}
	}

	return "", 0, "", classification.ErrUnmatched
}

func parseTitle(input string) (title string, rest string, err error) {
	if match := titleRegex.FindStringSubmatch(input); match != nil {
		title = cleanTitle(match[1])
		if title != "" {
			return title, input[len(match[0]):], nil
		}
	}

	return "", "", classification.ErrUnmatched
}

func parseTitleYearEpisodes(input string) (string, model.Year, model.Episodes, string, error) {
	if match := titleEpisodesRegex.FindStringSubmatch(input); match != nil {
		title := match[1]
		year := model.Year(0)

		if t, y, _, err := parseTitleYear(title); err == nil {
			title = t
			year = y
		} else {
			title = cleanTitle(title)
		}

		episodes := model.EpisodesMatchToEpisodes(match[2:])

		return title, year, episodes, input[len(match[0]):], nil
	}

	return "", 0, nil, "", classification.ErrUnmatched
}

func ParseTitleYearEpisodes(
	contentType model.NullContentType,
	input string,
) (string, model.Year, model.Episodes, string, error) {
	if !contentType.Valid || contentType.ContentType == model.ContentTypeTvShow {
		if title, year, episodes, rest, err := parseTitleYearEpisodes(input); err == nil {
			return title, year, episodes, rest, nil
		}
	}

	if title, year, rest, err := parseTitleYear(input); err == nil {
		return title, year, nil, rest, nil
	}

	if title, rest, err := parseTitle(input); err == nil {
		return title, 0, nil, rest, nil
	}

	return "", 0, nil, "", classification.ErrUnmatched
}

func ParseVideoContent(torrent model.Torrent, result classification.Result) (classification.ContentAttributes, error) {
	return ParseVideoContentWithOptions(torrent, result, ParseOptions{})
}

func ParseVideoContentWithOptions(
	torrent model.Torrent,
	result classification.Result,
	opts ParseOptions,
) (classification.ContentAttributes, error) {
	stripped := stripSiteNoisePrefix(torrent.Name)
	if opts.NoiseV2 {
		stripped = stripSiteNoisePrefixV2(torrent.Name)
	}

	name := collapseYearRange(stripped)
	title, year, episodes, rest, err := ParseTitleYearEpisodes(result.ContentType, name)

	// v2 leading-year rescue: "2019.The.Irishman.1080p..." — the cascade
	// either fails outright or swallows the leading year into BaseTitle
	// (titleTokens must consume a token before yearTokens may match). Only
	// fires when the normal parse produced no year of its own.
	if opts.NoiseV2 && year.IsNil() {
		if m := leadingYearRegex.FindStringSubmatch(name); m != nil {
			switch {
			case err == nil && len(episodes) > 0:
				// TV parse succeeded but the title carries the year prefix
				// ("2019 Series" S01E01). Move the year out of the title.
				if yearTok := m[1]; strings.HasPrefix(title, yearTok+" ") {
					title = strings.TrimPrefix(title, yearTok+" ")
					yearVal, _ := strconv.ParseUint(yearTok, 10, 16)
					year = model.Year(yearVal)
				}
			default:
				if rescueTitle, rescueYear, rescueRest, ok := rescueLeadingYear(name); ok {
					title, year, rest, err = rescueTitle, rescueYear, rescueRest, nil
					episodes = nil
				}
			}
		}
	}

	if err != nil {
		if !result.ContentType.Valid {
			return classification.ContentAttributes{}, err
		}

		rest = name
	}

	// Movie-pack guard: a multi-film collection (e.g. "The Bourne Collection
	// 2002-2007") must not be force-attached to a single movie. We detect this
	// when there are no episode tokens (not a TV series pack) but the name
	// carries an explicit collection/pack keyword. Clearing both title and year
	// causes the downstream deterministic matcher to produce no match and fall
	// back safely to LLM classification.
	if len(episodes) == 0 && isLikelyMoviePack(torrent.Name) {
		title = ""
		year = 0
	}

	// Anime released by a KNOWN fansub group is video by construction, but its
	// canonical shape ("[Group] Title - NNN [tags].mkv") carries no SxxExx, no
	// full date and no year — so the three cases below all miss and the switch
	// falls through to the zero value, i.e. unknown. Operators commonly delete
	// unknown, so this cohort is destroyed despite being exactly the content
	// the index exists to hold.
	//
	// IsKnownFansub, NOT IsAnime: the fansub allowlist is curated and disjoint
	// from adult studios, whereas IsAnime's unlisted-Latin-group path is
	// explicitly not porn-safe (see detect.go). Measured over 41k production
	// names, IsKnownFansub rescues zero adult releases; IsAnime rescues four.
	animeSignals := anime.Detect(name)
	// Tracks whether the fansub branch below actually decided the type. The
	// title override at the end must key on THIS, not on IsKnownFansub() alone:
	// a known group can also publish a conventional "S01E02" name, which an
	// earlier branch types correctly and whose title must not be rewritten.
	animeBranchTaken := false
	// Set only by the inner switch's DEFAULT case: typed tv_show for SURVIVAL,
	// with no evidence of episode, batch or film. See the title block below.
	animeAmbiguous := false

	ct := model.NullContentType{}

	switch {
	case result.ContentType.Valid:
		ct = model.NullContentType{Valid: true, ContentType: result.ContentType.ContentType}
	case len(episodes) > 0 || result.Date.IsValid():
		ct = model.NullContentType{Valid: true, ContentType: model.ContentTypeTvShow}
	case animeSignals.IsKnownFansub():
		animeBranchTaken = true
		// Default to tv_show and require an EXPLICIT film marker to say movie.
		// Fansub output is overwhelmingly episodic, and a bracket-only release
		// is far more often a batch ("[Group] Title [Batch]", "Title 27-39")
		// than a film. Guessing `movie` routes TMDB matching down the film path
		// where it can attach an unrelated film — a wrong attachment is worse
		// than a coarse one, and both survive the operator's delete rule.
		//
		// Episodes is deliberately left EMPTY: anime absolute numbering is not
		// season/episode, and the processor already persists it separately as
		// torrent_contents.anime_absolute_episode.
		switch {
		case animeSignals.AbsoluteEpisode > 0 || animeSignals.SeasonMarker,
			anime.IsBatch(name, animeSignals):
			ct = model.NullContentType{Valid: true, ContentType: model.ContentTypeTvShow}
		case anime.IsExplicitMovie(name):
			ct = model.NullContentType{Valid: true, ContentType: model.ContentTypeMovie}
		default:
			// NO evidence either way: no episode, no season, no batch, no film
			// marker. Type tv_show so the release survives the operator's
			// contentType delete rule, but mark it ambiguous — the type is a
			// SURVIVAL choice here, not a finding, and must not be used to pick
			// a TMDB search path. Measured on the survivor corpus: 48 of 5,075
			// known-fansub rows (0.95%), and roughly 40% of them are genuinely
			// films (The Boy and the Heron, Drifting Home, Heaven's Feel III)
			// mixed with genuine TV (SAC_2045, Dorohedoro, Night Head 2041).
			ct = model.NullContentType{Valid: true, ContentType: model.ContentTypeTvShow}
			animeAmbiguous = true
		}
	case !year.IsNil():
		ct = model.NullContentType{Valid: true, ContentType: model.ContentTypeMovie}
	}

	if ct.ContentType != model.ContentTypeTvShow {
		episodes = nil

		if year.IsNil() {
			title = ""
			rest = name
		}
	}

	// Set the fansub title AFTER the clearing block above, not inside the
	// switch: an anime film types `movie` with no year, so the block would wipe
	// it again. The generic parser yields a mangled string for this shape
	// ("[SubsPlease ]One Piece 1077 (480p) [3FC90F00 ]mkv"), and BaseTitle is
	// fed straight to the live TMDB search — so a mangled title costs an API
	// call per release and, with fuzzy + alt-title matching on, can MIS-attach.
	// CleanTitle returns "" rather than guess; empty means "do not search",
	// which is strictly better than searching for noise.
	if animeBranchTaken {
		if animeAmbiguous {
			// CLEAR the title rather than clean it. A valid BaseTitle is what
			// admits a torrent to the TMDB search, and the search path is
			// chosen by ContentType — which for this cohort is a survival
			// default, not a finding. A clean title plus a coin-flip type is a
			// misattachment surface with fuzzy and alt-title matching on, so
			// the release is kept but left unattached. This file's own rule:
			// a wrong attachment is worse than no attachment.
			//
			// Clearing is REQUIRED, not merely skipping the override: the
			// generic parser has already put its mangled title here
			// ("[Beatrice-Raws ]Steins;Gate (2014) ONA [BDRip …"), so leaving
			// it alone would search on junk instead of on nothing.
			title = ""
		} else {
			title = anime.CleanTitle(name, animeSignals)
		}
	}

	attrs := classification.ContentAttributes{
		ContentType:   ct,
		BaseTitle:     model.NullString{Valid: title != "", String: title},
		Date:          model.Date{Year: year},
		Episodes:      episodes,
		Languages:     model.InferLanguages(rest),
		LanguageMulti: multiRegex.MatchString(rest),
	}
	attrs.InferVideoAttributes(rest)

	// Anime names its release group in a leading [Group] bracket
	// ("[SubsPlease] Show - 12 …"), which cleanTitle strips from BaseTitle
	// and the trailing "-GROUP" scene extractor in InferVideoAttributes never
	// sees — so anime would otherwise carry an empty ReleaseGroup, leaving
	// Torznab's AttrTeam and the release-group FTS field blank and depriving
	// Sonarr of per-group scoring/dedup. Recover a KNOWN fansub group (the
	// porn-safe curated allowlist) from the (site-prefix stripped) name. Only
	// fill when the scene extractor found nothing, so a genuine trailing
	// "codec-GROUP" is never overridden.
	if !attrs.ReleaseGroup.Valid {
		if group := anime.KnownGroup(name); group != "" {
			attrs.ReleaseGroup = model.NullString{Valid: true, String: group}
		}
	}

	return attrs, nil
}
