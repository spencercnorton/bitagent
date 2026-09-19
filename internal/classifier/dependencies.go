package classifier

import (
	"github.com/spencercnorton/bitagent/internal/animedb"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/tmdb"
)

type dependencies struct {
	search     LocalSearch
	tmdbClient tmdb.Client
	// llmMatch is the two-stage LLM fallback matcher. May be nil (tests,
	// or when the module is absent); the action guards with lm.Enabled().
	llmMatch *llmmatch.Client
	// matchDecisionObserver optionally records the final online matcher policy
	// verdict. It is invoked before any live attach/tag mutation.
	matchDecisionObserver MatchDecisionObserver
	// animeResolver drives deterministic romaji/AKA resolution from the
	// anime-titles backbone. Never nil in production (factory falls back to a
	// seed-only resolver); may be nil in tests that build dependencies directly,
	// where the matcher simply skips alias resolution.
	animeResolver *animedb.Resolver
	// fuzzyMatchEnabled mirrors Config.FuzzyMatchEnabled. Carried on
	// dependencies so that executionContext methods (tmdbSearchMovie,
	// tmdbSearchTVShow) can read it without a separate config field on
	// executionContext.
	fuzzyMatchEnabled bool
	// altTitleMatch mirrors Config.AltTitleMatch. Carried here so the LLM
	// matcher's identity gate honours the same operator switch that governs
	// alt-title use on the deterministic path.
	altTitleMatch bool
	// singleEpisodeMaxBytes mirrors Config.SingleEpisodeMaxBytes. The
	// parse_video_content action uses it for the season-pack size backstop.
	singleEpisodeMaxBytes int64
	// parseNoiseV2 mirrors Config.ParseNoiseV2 (WI2.3 noise stripping).
	parseNoiseV2 bool
}
