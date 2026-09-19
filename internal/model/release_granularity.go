package model

import "regexp"

// ReleaseGranularity classifies how much of a TV series a single release
// carries. It is derived, not parsed: the parser stores WHICH episodes a
// release claims (Episodes); this enum states the SHAPE of that claim so
// consumers (Torznab ranking, pack handling, junk heuristics) can distinguish
// a double-episode file from a half-season pack without re-deriving it from
// JSONB on every query.
// ENUM(episode, multi_episode, partial_season, season, multi_season, complete_series)
type ReleaseGranularity string

func (r ReleaseGranularity) Label() string {
	return r.String()
}

func (r ReleaseGranularity) IsNil() bool {
	return r == ""
}

// completeSeriesRegex detects an explicit whole-series claim in a release
// name. Deliberately narrow: bare "complete" also marks single-season packs
// ("S05.COMPLETE") and bare "batch" collides with titles ("The Bad Batch"),
// so only the unambiguous phrasings and the bracketed anime [Batch] tag
// count. The (^|[^a-z]) anchor (case-folded under (?i)) rejects a preceding
// letter so "Incomplete.Series" / "Miniseries.Complete" do not match — RE2
// has no lookbehind.
var completeSeriesRegex = regexp.MustCompile(
	`(?i)((^|[^a-z])(complete[ ._-]?series|full[ ._-]?series|series[ ._-]?complete)|\[ ?batch ?\])`,
)

// maxMultiEpisode is the episode count above which a single-season,
// multi-episode release is labelled a partial-season pack rather than a
// multi-episode file. Scene multi-episode files are 2-3 episodes (S01E01E02,
// double specials); anything larger is a pack of individual episodes.
// ponytail: files_count could refine this split but counts non-video files
// (nfo/srt), so a fixed threshold is the honest deterministic rule.
const maxMultiEpisode = 3

// DeriveReleaseGranularity computes the granularity label for a release from
// its content type, parsed episodes and name. Non-TV content and TV releases
// with no episode information and no explicit series claim yield an invalid
// (NULL) granularity — unknown stays unknown.
//
// This function is the SINGLE source of truth: the processor stamps it at
// classify time, and the one-off `granularity-backfill` command applies the
// same function to rows that predate migration 00037 (which adds the column
// but deliberately does not backfill — see the migration comment).
func DeriveReleaseGranularity(
	contentType NullContentType,
	episodes Episodes,
	name string,
) NullReleaseGranularity {
	if !contentType.Valid || contentType.ContentType != ContentTypeTvShow {
		return NullReleaseGranularity{}
	}

	completeSeries := completeSeriesRegex.MatchString(name)

	switch {
	case len(episodes) == 0:
		if completeSeries {
			return NewNullReleaseGranularity(ReleaseGranularityCompleteSeries)
		}

		return NullReleaseGranularity{}
	case len(episodes) >= 2:
		// an explicit series claim wins over bare multi-season markers
		// ("S01-S10 Complete Series"); without it, multiple season keys are a
		// multi-season pack regardless of per-season detail.
		if completeSeries {
			return NewNullReleaseGranularity(ReleaseGranularityCompleteSeries)
		}

		return NewNullReleaseGranularity(ReleaseGranularityMultiSeason)
	}

	var episodeCount int
	for _, eps := range episodes {
		episodeCount = len(eps)
	}

	switch {
	case episodeCount == 0:
		return NewNullReleaseGranularity(ReleaseGranularitySeason)
	case episodeCount == 1:
		return NewNullReleaseGranularity(ReleaseGranularityEpisode)
	case episodeCount <= maxMultiEpisode:
		return NewNullReleaseGranularity(ReleaseGranularityMultiEpisode)
	default:
		return NewNullReleaseGranularity(ReleaseGranularityPartialSeason)
	}
}
