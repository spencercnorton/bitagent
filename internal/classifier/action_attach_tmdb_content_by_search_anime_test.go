package classifier

import (
	"context"
	"errors"
	"testing"

	"github.com/spencercnorton/bitagent/internal/animedb"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/tmdb"
)

// recordingTMDB captures the query the deterministic path actually sends and
// then fails, so the test can assert on the QUERY without a full TMDB fixture.
type recordingTMDB struct {
	tmdb.Client
	tvQuery string
}

var errStopAfterQuery = errors.New("stop after query")

func (r *recordingTMDB) SearchTv(_ context.Context, req tmdb.SearchTvRequest) (tmdb.SearchTvResponse, error) {
	r.tvQuery = req.Query
	return tmdb.SearchTvResponse{}, errStopAfterQuery
}

func resolverWith(aliases ...animedb.Alias) *animedb.Resolver {
	r := animedb.NewSeedResolver()
	r.Swap(aliases)
	return r
}

func runAttachBySearch(t *testing.T, name, baseTitle string,
	res *animedb.Resolver, client tmdb.Client) (classification.Result, error) {
	t.Helper()
	a, err := attachTmdbContentBySearchAction{}.compileAction(compilerContext{
		source: attachTmdbContentBySearchName,
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return a.run(executionContext{
		Context: context.Background(),
		dependencies: dependencies{
			animeResolver: res,
			tmdbClient:    client,
		},
		torrent: model.Torrent{Name: name},
		result: classification.Result{
			ContentAttributes: classification.ContentAttributes{
				BaseTitle:   model.NewNullString(baseTitle),
				ContentType: model.NewNullContentType(model.ContentTypeTvShow),
			},
		},
	})
}

// A romaji base title must reach TMDB as the alias' catalogued display title —
// the romaji-hostile TMDB search is what the backbone exists to avoid.
func TestAttachBySearch_ResolverRewritesQuery(t *testing.T) {
	res := resolverWith(animedb.Alias{
		Normalized: animedb.Normalize("Sousou no Frieren"),
		Display:    "Frieren: Beyond Journey's End",
		TMDBType:   model.ContentTypeTvShow,
		TMDBID:     209867,
	})
	probe := &recordingTMDB{}
	_, _ = runAttachBySearch(t, "[SubsPlease] Sousou no Frieren - 12.mkv", "Sousou no Frieren", res, probe)
	if probe.tvQuery != "Frieren: Beyond Journey's End" {
		t.Fatalf("query = %q, want the alias display title", probe.tvQuery)
	}
}

// The adult gate previously existed only on the LLM path, so on the free path
// a known adult title had no gate at all.
func TestAttachBySearch_AdultAliasIsRejected(t *testing.T) {
	res := resolverWith(animedb.Alias{
		Normalized: animedb.Normalize("Some Adult Anime"),
		Display:    "Some Adult Anime",
		Adult:      true,
	})
	probe := &recordingTMDB{}
	_, err := runAttachBySearch(t, "Some Adult Anime - 03.mkv", "Some Adult Anime", res, probe)
	if !errors.Is(err, classification.ErrUnmatched) {
		t.Fatalf("err = %v, want ErrUnmatched", err)
	}
	if probe.tvQuery != "" {
		t.Fatalf("adult alias reached TMDB with query %q", probe.tvQuery)
	}
}

// A data-built alias (DirectAttach=false) must only GUIDE the search, never
// attach — that is the trust contract Alias declares.
func TestAttachBySearch_DataAliasDoesNotDirectAttach(t *testing.T) {
	res := resolverWith(animedb.Alias{
		Normalized:   animedb.Normalize("Kimetsu no Yaiba"),
		Display:      "Demon Slayer",
		TMDBType:     model.ContentTypeTvShow,
		TMDBID:       85937,
		DirectAttach: false,
	})
	probe := &recordingTMDB{}
	_, err := runAttachBySearch(t, "Kimetsu no Yaiba - 05.mkv", "Kimetsu no Yaiba", res, probe)
	if probe.tvQuery != "Demon Slayer" {
		t.Fatalf("data alias should guide the search; query = %q", probe.tvQuery)
	}
	if !errors.Is(err, errStopAfterQuery) {
		t.Fatalf("data alias must not attach without a search; err = %v", err)
	}
}

// Without a resolver the action must behave exactly as before.
func TestAttachBySearch_NilResolverUsesBaseTitle(t *testing.T) {
	probe := &recordingTMDB{}
	_, _ = runAttachBySearch(t, "Some.Show.S01E01", "Some Show", nil, probe)
	if probe.tvQuery != "Some Show" {
		t.Fatalf("query = %q, want %q", probe.tvQuery, "Some Show")
	}
}
