package classifier

import (
	"fmt"

	"github.com/spencercnorton/bitagent/internal/animedb"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/tmdb"
	"go.uber.org/fx"
)

type Params struct {
	fx.In
	Config     Config
	TmdbConfig tmdb.Config
	Search     lazy.Lazy[search.Search]
	TmdbClient lazy.Lazy[tmdb.Client]
	// LlmMatch is optional — provided by llmmatchfx. `optional:"true"` so
	// the classifier still builds if the module is absent (e.g. tests).
	LlmMatch *llmmatch.Client `optional:"true"`
	// MatchDecisionObserver is optional. The capture-backed implementation is
	// wired by the application when decision-ledger persistence is enabled.
	MatchDecisionObserver MatchDecisionObserver `optional:"true"`
	// AnimeResolver is optional — provided by animedbfx. When absent (e.g.
	// tests without the module) the factory substitutes a seed-only resolver so
	// the curated anime aliases still resolve.
	AnimeResolver *animedb.Resolver `optional:"true"`
}

type Result struct {
	fx.Out
	Compiler lazy.Lazy[Compiler]
	Source   lazy.Lazy[Source]
	Runner   lazy.Lazy[Runner]
}

func New(params Params) Result {
	lc := lazy.New(func() (Compiler, error) {
		s, err := params.Search.Get()
		if err != nil {
			return nil, err
		}

		tmdbClient, err := params.TmdbClient.Get()
		if err != nil {
			return nil, err
		}

		// The anime backbone must always resolve the curated seed set, even when
		// the animedbfx module (and its DB-backed resolver) is absent.
		animeResolver := params.AnimeResolver
		if animeResolver == nil {
			animeResolver = animedb.NewSeedResolver()
		}

		return compiler{
			options: []compilerOption{
				compilerFeatures(defaultFeatures),
				celEnvOption,
			},
			dependencies: dependencies{
				search: localSearchSemaphore{
					search: localSearch{
						Search:            s,
						altTitleMatch:     params.Config.AltTitleMatch,
						fuzzyMatchEnabled: params.Config.FuzzyMatchEnabled,
					},
					semaphore: make(chan struct{}, 1),
				},
				tmdbClient:            tmdbClient,
				llmMatch:              params.LlmMatch,
				matchDecisionObserver: params.MatchDecisionObserver,
				animeResolver:         animeResolver,
				fuzzyMatchEnabled:     params.Config.FuzzyMatchEnabled,
				altTitleMatch:         params.Config.AltTitleMatch,
				singleEpisodeMaxBytes: params.Config.SingleEpisodeMaxBytes,
				parseNoiseV2:          params.Config.ParseNoiseV2,
			},
		}, nil
	})
	lsrc := lazy.New[Source](func() (Source, error) {
		src, err := newSourceProvider(params.Config, params.TmdbConfig).source()
		if err != nil {
			return Source{}, err
		}

		if _, ok := src.Workflows[params.Config.Workflow]; !ok {
			return Source{}, fmt.Errorf("default workflow '%s' not found", params.Config.Workflow)
		}

		return src, nil
	})

	return Result{
		Compiler: lc,
		Source:   lsrc,
		Runner: lazy.New(func() (Runner, error) {
			src, err := lsrc.Get()
			if err != nil {
				return nil, err
			}
			c, err := lc.Get()
			if err != nil {
				return nil, err
			}
			r, err := c.Compile(src)
			if err != nil {
				return nil, err
			}

			return runnerSemaphore{
				runner:    r,
				semaphore: make(chan struct{}, params.Config.Concurrency),
			}, nil
		}),
	}
}
