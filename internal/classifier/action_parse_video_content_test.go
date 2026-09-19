package classifier

import (
	"context"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	classifier_mocks "github.com/spencercnorton/bitagent/internal/classifier/mocks"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/tmdb"
	tmdb_mocks "github.com/spencercnorton/bitagent/internal/tmdb/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

const (
	testGiB = 1024 * 1024 * 1024
)

// newBackstopTestClassifier builds a Compiler wired with the supplied size
// ceiling and stub search/TMDB mocks that return unmatched for all queries.
func newBackstopTestClassifier(t *testing.T, ceiling int64) (Compiler, *classifier_mocks.LocalSearch, *tmdb_mocks.Client) {
	t.Helper()
	search := classifier_mocks.NewLocalSearch(t)
	tmdbClient := tmdb_mocks.NewClient(t)
	c := compiler{
		options: []compilerOption{
			compilerFeatures(defaultFeatures),
			celEnvOption,
		},
		dependencies: dependencies{
			search:                search,
			tmdbClient:            tmdbClient,
			singleEpisodeMaxBytes: ceiling,
		},
	}
	return c, search, tmdbClient
}

// runBackstopTest compiles the full core source (so parse_video_content runs
// in its usual workflow context) and returns the episode map for torrent t.
func runBackstopTest(
	t *testing.T,
	ceiling int64,
	torrent model.Torrent,
) model.Episodes {
	t.Helper()

	c, search, tmdbClient := newBackstopTestClassifier(t, ceiling)

	// Stub out all search / TMDB calls so they return "unmatched" safely.
	anyCtx := mock.MatchedBy(func(ctx any) bool {
		_, ok := ctx.(context.Context)
		return ok
	})
	search.On("ContentBySearch", anyCtx, mock.Anything, mock.Anything, mock.Anything).
		Return(model.Content{}, classification.ErrUnmatched).Maybe()
	// Use Maybe() so tests that don't reach TMDB don't fail on unexpected-call
	// assertions; return empty structs so the mock type-assertion doesn't panic.
	tmdbClient.On("SearchTv", anyCtx, mock.Anything).
		Return(tmdb.SearchTvResponse{}, nil).Maybe()
	tmdbClient.On("SearchMovie", anyCtx, mock.Anything).
		Return(tmdb.SearchMovieResponse{}, nil).Maybe()
	tmdbClient.On("TvDetails", anyCtx, mock.Anything).
		Return(tmdb.TvDetailsResponse{}, nil).Maybe()
	tmdbClient.On("MovieDetails", anyCtx, mock.Anything).
		Return(tmdb.MovieDetailsResponse{}, nil).Maybe()
	tmdbClient.On("FindByID", anyCtx, mock.Anything).
		Return(tmdb.FindByIDResponse{}, nil).Maybe()

	source, err := yamlSourceProvider{rawSourceProvider: coreSourceProvider{}}.source()
	if err != nil {
		t.Fatal("failed to load core source:", err)
	}

	runner, err := c.Compile(source)
	if err != nil {
		t.Fatal("failed to compile:", err)
	}

	result, err := runner.Run(context.Background(), "default", Flags{}, torrent)
	if err != nil {
		t.Logf("run error (may be expected): %v", err)
	}

	return result.Episodes
}

// TestSeasonPackSizeBackstop verifies the season-pack size backstop behaviour
// inside the parse_video_content action running in the full core workflow.
func TestSeasonPackSizeBackstop(t *testing.T) {
	t.Parallel()

	const ceiling = int64(4 * testGiB)

	cases := []struct {
		name         string
		torrentName  string
		size         uint
		ceiling      int64
		wantNilEps   bool // true → expect nil/empty after backstop
		wantEpisodes model.Episodes
	}{
		{
			// Small torrent → season-only is demoted (episodes cleared).
			name:        "small season-only is demoted",
			torrentName: "ShowName.S02.HDTV.mkv",
			size:        uint(300 * 1024 * 1024), // 300 MiB
			ceiling:     ceiling,
			wantNilEps:  true,
		},
		{
			// Large genuine pack → backstop does NOT fire.
			name:         "large season pack is kept",
			torrentName:  "ShowName.S02.720p.BluRay.mkv",
			size:         uint(25 * testGiB), // 25 GiB
			ceiling:      ceiling,
			wantEpisodes: model.Episodes{2: {}},
		},
		{
			// Exactly at the ceiling → backstop fires (≤ ceiling).
			name:        "at ceiling is demoted",
			torrentName: "ShowName.S03.1080p.mkv",
			size:        uint(ceiling),
			ceiling:     ceiling,
			wantNilEps:  true,
		},
		{
			// One byte above ceiling → backstop does NOT fire.
			name:         "one byte above ceiling is kept",
			torrentName:  "ShowName.S03.1080p.mkv",
			size:         uint(ceiling) + 1,
			ceiling:      ceiling,
			wantEpisodes: model.Episodes{3: {}},
		},
		{
			// Specific episode present → backstop must not touch it.
			name:         "episode-specific parse is untouched",
			torrentName:  "ShowName.S02E05.1080p.mkv",
			size:         uint(500 * 1024 * 1024), // 500 MiB
			ceiling:      ceiling,
			wantEpisodes: model.Episodes{2: {5: {}}},
		},
		{
			// Backstop disabled (ceiling == 0) → season-only kept as-is.
			name:         "backstop disabled keeps season-only",
			torrentName:  "ShowName.S02.720p.mkv",
			size:         uint(300 * 1024 * 1024),
			ceiling:      0,
			wantEpisodes: model.Episodes{2: {}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Pre-set ContentType via hint so the core workflow's
			// parse_video_content condition ("result.contentType in
			// [movie, tv_show]") is satisfied regardless of file-size
			// heuristics (which require real Files entries to work).
			eps := runBackstopTest(t, tc.ceiling, model.Torrent{
				Name:        tc.torrentName,
				Size:        tc.size,
				FilesStatus: model.FilesStatusSingle,
				Hint: model.TorrentHint{
					ContentType: model.ContentTypeTvShow,
				},
			})
			if tc.wantNilEps {
				assert.True(t, len(eps) == 0, "expected empty/nil episodes, got %v", eps)
			} else {
				assert.Equal(t, tc.wantEpisodes, eps)
			}
		})
	}
}

// TestIsSeasonOnlyPack validates the helper directly for edge cases.
func TestIsSeasonOnlyPack(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		episodes model.Episodes
		want     bool
	}{
		{"nil episodes", nil, false},
		{"empty episodes", model.Episodes{}, false},
		{"season only single", model.Episodes{1: {}}, true},
		{"season only multi", model.Episodes{1: {}, 2: {}}, true},
		{"has episode numbers", model.Episodes{1: {5: {}}}, false},
		{"mixed: one season-only one with episodes", model.Episodes{1: {}, 2: {5: {}}}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, isSeasonOnlyPack(tc.episodes))
		})
	}
}
