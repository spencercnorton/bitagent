package model

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/hedhyw/rex/pkg/dialect"
	"github.com/hedhyw/rex/pkg/rex"
	"github.com/spencercnorton/bitagent/internal/keywords"
)

// episodeNumRegex extracts each E<number> from a concatenated run such as
// "E02E03" captured by episodeConcatToken.
var episodeNumRegex = regexp.MustCompile(`(?i)e(\d{1,4})`)

// numberRunRegex extracts bare digit runs from a matched range/list value.
var numberRunRegex = regexp.MustCompile(`\d{1,4}`)

// rangeToken builds the number/range/list sub-pattern shared by the season and
// episode tokens. The number factory defines what a single number may look
// like: episodes are 1-4 digits (Pokémon S20E048, One Piece E1000, daily soaps
// S39E206 — the pre-v0.46.0 2-digit cap silently stored E048 as episode 4);
// seasons are 1-2 digits OR a 4-digit year (year-numbered daily-show seasons,
// "S2023E145"). A bare 3-4 digit non-year run after S ("s1080p", "s2160p") is
// NOT a season — RE2 has no lookbehind, so the year-shape-first alternation is
// what keeps those names parsing byte-identically to the 2-digit cap
// (season 10 / 21 + junk rest).
func rangeToken(runes string, number func() dialect.Token) dialect.Token {
	return rex.Group.Define(
		rex.Group.Define(number()),
		rex.Group.Composite(
			rex.Group.Define(
				rex.Chars.Whitespace().Repeat().ZeroOrOne(),
				rex.Chars.Single('-'),
				rex.Chars.Whitespace().Repeat().ZeroOrOne(),
				rex.Group.NonCaptured(
					rex.Chars.Runes(runes).Repeat().ZeroOrOne(),
					rex.Chars.Whitespace().Repeat().ZeroOrOne(),
				).Repeat().ZeroOrOne(),
				rex.Group.Define(number()),
			).NonCaptured(),
			rex.Group.Define(
				rex.Chars.Whitespace().Repeat().ZeroOrOne(),
				rex.Chars.Single(','),
				rex.Chars.Whitespace().Repeat().ZeroOrOne(),
				rex.Group.NonCaptured(
					rex.Chars.Runes(runes).Repeat().ZeroOrOne(),
					rex.Chars.Whitespace().Repeat().ZeroOrOne(),
				).Repeat().ZeroOrOne(),
				rex.Group.Define(number()),
				rex.Chars.Whitespace().Repeat().ZeroOrOne(),
			).NonCaptured().Repeat().OneOrMore(),
		).NonCaptured().Repeat().ZeroOrOne(),
	)
}

// seasonNumberToken: 1-2 digits, or a 4-digit year-numbered season. Year-shape
// listed FIRST so "S2023E145" binds the full year; non-year 4-digit runs fall
// through to the 2-digit branch and keep legacy behavior ("s1080p" -> S10).
func seasonNumberToken() dialect.Token {
	return rex.Group.Composite(
		rex.Group.NonCaptured(
			rex.Group.Composite(
				rex.Common.Text("18"), rex.Common.Text("19"), rex.Common.Text("20"),
			).NonCaptured(),
			rex.Chars.Digits().Repeat().Exactly(2),
		),
		rex.Chars.Digits().Repeat().Between(1, maxSeasonDigits),
	).NonCaptured()
}

func episodeNumberToken() dialect.Token {
	return rex.Chars.Digits().Repeat().Between(1, maxEpisodeDigits)
}

const (
	maxSeasonDigits  = 2
	maxEpisodeDigits = 4
	// maxRangeSpan bounds how many keys a single episode/season range may
	// expand into. With episodes now up to 4 digits, an adversarial DHT name
	// like "S01E01-E9999" would otherwise materialise ~10k map entries per
	// row. No legitimate single contiguous range approaches this.
	maxRangeSpan = 1000
)

// clampRangeEnd bounds a parsed range so an implausible or adversarial end
// cannot expand into an unbounded number of keys. Returns start when the span
// is negative or exceeds maxRangeSpan, degrading to the single start value.
func clampRangeEnd(start, end int64) int64 {
	if end < start || end-start > maxRangeSpan {
		return start
	}
	return end
}

// clampEpisodeRangeEnd additionally rejects a resolution-shaped end: in
// "S01E106 - 1080p" the dash-range branch reads "1080" as a range end (RE2
// cannot look ahead to the trailing 'p'), which would store ~975 bogus
// episodes. The guard is skipped when the raw range text carries an explicit
// e/E marker before the end number ("E779-E1080") — that marker proves an
// episode, and the only e/E the range token can consume is that marker.
// ponytail: a word-boundary redesign of the token is the durable fix — it also
// stops "s1080p" parsing as season 10 — but changes enough name classes to
// need its own MR and eval.
func clampEpisodeRangeEnd(start, end int64, rawRange string) int64 {
	end = clampRangeEnd(start, end)
	if strings.ContainsAny(rawRange, "eE") {
		return end
	}
	switch end {
	case 480, 576, 720, 1080, 2160:
		if end-start > 300 {
			return start
		}
	}
	return end
}

var seasonToken = rex.Group.Define(
	rex.Group.Composite(
		keywords.MustNewRexTokensFromKeywords("season", "s")...,
	).NonCaptured(),
	rex.Chars.Whitespace().Repeat().ZeroOrOne(),
	rangeToken("sS", seasonNumberToken),
	rex.Chars.Whitespace().Repeat().ZeroOrOne(),
).NonCaptured()

var episodeToken = rex.Group.Define(
	rex.Group.Composite(
		keywords.MustNewRexTokensFromKeywords("episode", "ep", "e")...,
	).NonCaptured(),
	rex.Chars.Whitespace().Repeat().ZeroOrOne(),
	rangeToken("eE", episodeNumberToken),
).NonCaptured()

// episodeConcatToken captures a run of additional concatenated episode tokens
// — the "E02[E03...]" in S01E01E02 — that immediately follow the primary
// episode with no separator. Without it the parser stops after the first
// episode and a double/triple-episode release is stored as a single episode.
// The whole run is captured as one group and split on the e/E delimiter by
// EpisodesMatchToEpisodes. Optional, so single episodes and season-only names
// are unaffected. It is appended LAST in episodesRegularTokens so existing
// capture-group indices are preserved.
var episodeConcatToken = rex.Group.Define(
	rex.Group.NonCaptured(
		rex.Chars.Runes("eE"),
		rex.Chars.Digits().Repeat().Between(1, maxEpisodeDigits),
	).Repeat().OneOrMore(),
).Repeat().ZeroOrOne()

var episodesRegularTokens = rex.Group.Define(
	seasonToken,
	episodeToken.Repeat().ZeroOrOne(),
	episodeConcatToken,
).NonCaptured()

// episodesXFormatTokens matches the NNxNN / S?NNxNN episode notation where
// x or X separates season from episode (e.g. 2x09, S02x20, S17X02, 01x06).
// The optional leading S/s is consumed non-capturing so existing capture-group
// indices for season and episode are preserved.  The composite in EpisodesToken
// places this alternative FIRST so that "S02x20" is matched here rather than
// being truncated to "S02" by the regular season-keyword path.
var episodesXFormatTokens = rex.Group.Define(
	rex.Group.Define(
		rex.Chars.Runes("sS").Repeat().ZeroOrOne(), // optional S prefix, non-capturing
		rex.Group.Define(rex.Chars.Digits().Repeat().Between(1, maxSeasonDigits)),
		rex.Chars.Runes("xX"),
		rex.Group.Define(rex.Chars.Digits().Repeat().Between(1, maxEpisodeDigits)),
	).NonCaptured(),
	rex.Group.Define(
		rex.Chars.Whitespace().Repeat().ZeroOrOne(),
		rex.Chars.Single('-'),
		rex.Chars.Whitespace().Repeat().ZeroOrOne(),
		rex.Group.Define(rex.Chars.Digits().Repeat().Between(1, maxEpisodeDigits)),
	).NonCaptured().Repeat().ZeroOrOne(),
).NonCaptured()

// EpisodesToken is the combined parser for all recognised episode notations.
// x-format (episodesXFormatTokens) is listed FIRST in the composite so that
// "S02x20" is matched as season 2 episode 20 rather than being truncated to
// the season-only "S02" by the regular keyword path.
//
// Capture group layout (relative to the start of this token, i.e. the slice
// passed to EpisodesMatchToEpisodes):
//
//	[0] outer composite (always non-empty on a successful match)
//
// x-format alternative (matched when [1] != ""):
//
//	[1] season number (1–2 digits)
//	[2] episode number (1–2 digits)
//	[3] range-end episode (1–2 digits; empty when not a range)
//
// regular alternative (matched when [1] == "" and [4] != ""):
//
//	[4]  season range value (whole text, used for list split)
//	[5]  season start number
//	[6]  season range end number  (empty when not a range)
//	[7]  season list item number  (empty when not a list)
//	[8]  episode range value (whole text, used for list split; empty when season-only)
//	[9]  episode start number
//	[10] episode range end number (empty when not a range)
//	[11] episode list item number (empty when not a list)
//	[12] concatenated additional-episode run (the "E02[E03...]" in S01E01E02;
//	     empty when not a concatenated multi-episode)
var EpisodesToken = rex.Group.Composite(
	episodesXFormatTokens,
	episodesRegularTokens,
)

var episodesRegex = rex.New(
	rex.Chars.Begin(),
	EpisodesToken,
	rex.Chars.End(),
).MustCompile()

// EpisodesMatchToEpisodes converts a regex sub-match slice into an Episodes
// map. The slice must start at the EpisodesToken group — i.e. call with
// match[1:] from episodesRegex or match[2:] from titleEpisodesRegex.
//
// See the EpisodesToken doc-comment above for the exact group-index layout.
func EpisodesMatchToEpisodes(match []string) Episodes {
	if len(match) < 12 {
		return nil
	}

	episodes := Episodes{}

	if match[1] != "" {
		// x-format: (S?)NNxNN or (S?)NNxNN-NN
		season, _ := strconv.ParseInt(match[1], 10, 16)
		episodeStart, _ := strconv.ParseInt(match[2], 10, 16)
		episodeEnd := episodeStart

		if match[3] != "" {
			episodeEnd, _ = strconv.ParseInt(match[3], 10, 16)
		}

		episodeEnd = clampEpisodeRangeEnd(episodeStart, episodeEnd, "")
		for i := episodeStart; i <= episodeEnd; i++ {
			episodes = episodes.AddEpisode(int(season), int(i))
		}
	} else {
		// regular format: S/Season prefix with optional E/Episode suffix
		seasonStart, _ := strconv.ParseInt(match[5], 10, 16)

		if match[8] == "" {
			// no episodes — season-only or season range/list
			switch {
			case match[6] != "":
				// a season range
				seasonEnd, _ := strconv.ParseInt(match[6], 10, 16)
				seasonEnd = clampRangeEnd(seasonStart, seasonEnd)
				for i := seasonStart; i <= seasonEnd; i++ {
					episodes = episodes.AddSeason(int(i))
				}
			case match[7] != "":
				// a list of seasons — extract digit runs rather than splitting
				// on commas: items keep their s/S prefix ("S2020,S2021"), which
				// ParseInt would silently turn into a bogus season 0.
				for _, season := range numberRunRegex.FindAllString(match[4], -1) {
					seasonIndex, _ := strconv.ParseInt(season, 10, 16)
					episodes = episodes.AddSeason(int(seasonIndex))
				}
			default:
				// or just a single season
				episodes = episodes.AddSeason(int(seasonStart))
			}
		} else {
			// episodes present
			episodeStart, _ := strconv.ParseInt(match[9], 10, 16)

			switch {
			case match[10] != "":
				// an episode range
				episodeEnd, _ := strconv.ParseInt(match[10], 10, 16)
				episodeEnd = clampEpisodeRangeEnd(episodeStart, episodeEnd, match[8])
				for i := episodeStart; i <= episodeEnd; i++ {
					episodes = episodes.AddEpisode(int(seasonStart), int(i))
				}
			case match[11] != "":
				// a list of episodes — digit runs for the same reason as the
				// season list ("E01,E03" items keep their e/E prefix).
				for _, episode := range numberRunRegex.FindAllString(match[8], -1) {
					episodeIndex, _ := strconv.ParseInt(episode, 10, 16)
					episodes = episodes.AddEpisode(int(seasonStart), int(episodeIndex))
				}
			default:
				// a single episode
				episodes = episodes.AddEpisode(int(seasonStart), int(episodeStart))
			}
		}

		// Concatenated additional episodes: the "E02[E03...]" in S01E01E02.
		if len(match) > 12 && match[12] != "" {
			for _, sm := range episodeNumRegex.FindAllStringSubmatch(match[12], -1) {
				episodeNum, _ := strconv.ParseInt(sm[1], 10, 32)
				episodes = episodes.AddEpisode(int(seasonStart), int(episodeNum))
			}
		}
	}

	return episodes
}

func ParseEpisodes(input string) Episodes {
	m := episodesRegex.FindStringSubmatch(input)
	if m == nil {
		return nil
	}
	// EpisodesMatchToEpisodes expects the slice starting at the EpisodesToken
	// group (index 1), not the full match (index 0).
	return EpisodesMatchToEpisodes(m[1:])
}
