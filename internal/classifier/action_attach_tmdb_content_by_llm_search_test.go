package classifier

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/spencercnorton/bitagent/internal/animedb"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	classifier_mocks "github.com/spencercnorton/bitagent/internal/classifier/mocks"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/tmdb"
	tmdb_mocks "github.com/spencercnorton/bitagent/internal/tmdb/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// llmChatServer returns the queued responses in order (extract, then rerank).
func llmChatServer(t *testing.T, responses ...string) *httptest.Server {
	t.Helper()
	var i int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&i, 1) - 1
		content := responses[len(responses)-1]
		if int(n) < len(responses) {
			content = responses[n]
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": content}}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newLLMMatchClient(t *testing.T, endpoint string, live bool) *llmmatch.Client {
	cfg := llmmatch.NewDefaultConfig()
	cfg.Enabled = true
	cfg.EnableLive = live
	cfg.Endpoint = endpoint
	cfg.MinTotalSizeBytes = 0
	return llmmatch.NewClient(cfg, nil, llmmatch.NewMetrics(), zap.NewNop().Sugar())
}

// llmMatchScenario configures a full-workflow run of the LLM matcher.
type llmMatchScenario struct {
	live        bool
	observer    MatchDecisionObserver
	torrentName string
	baseTitle   string // BaseTitle the deterministic search sees (returns empty)
	extractJSON string // stage-1 LLM response
	rerankJSON  string // stage-2 LLM response ("" if the run should never reach it)
	llmTitle    string // title the LLM-driven SearchMovie is called with
	candidateID int64
	// localCandidates, when set, is what the mirror returns for the LLM
	// title — the local-first path serves these to the rerank before any
	// TMDB API search.
	localCandidates      []model.Content
	candidateReleaseDate string
}

func runLLMScenario(t *testing.T, s llmMatchScenario) (classification.Result, error) {
	responses := []string{s.extractJSON}
	if s.rerankJSON != "" {
		responses = append(responses, s.rerankJSON)
	}
	srv := llmChatServer(t, responses...)

	search := classifier_mocks.NewLocalSearch(t)
	search.On("ContentBySearch", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(model.Content{}, classification.ErrUnmatched).Maybe()
	search.On("ContentCandidatesBySearch", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(s.localCandidates, nil).Maybe()

	tc := tmdb_mocks.NewClient(t)
	// Deterministic attach_tmdb_content_by_search (parsed BaseTitle) — empty.
	tc.On("SearchMovie", mock.Anything, mock.MatchedBy(func(r tmdb.SearchMovieRequest) bool {
		return r.Query == s.baseTitle
	})).Return(tmdb.SearchMovieResponse{}, nil).Maybe()
	if s.llmTitle != "" {
		releaseDate := s.candidateReleaseDate
		if releaseDate == "" {
			releaseDate = "2020-04-24"
		}
		tc.On("SearchMovie", mock.Anything, mock.MatchedBy(func(r tmdb.SearchMovieRequest) bool {
			return r.Query == s.llmTitle
		})).Return(tmdb.SearchMovieResponse{
			Results: []tmdb.SearchMovieResult{{ID: s.candidateID, Title: s.llmTitle, ReleaseDate: releaseDate}},
		}, nil).Maybe()
	}
	// Catch-alls (defined last, so the specific expectations above win): any
	// other deterministic-search query returns no results.
	tc.On("SearchMovie", mock.Anything, mock.Anything).Return(tmdb.SearchMovieResponse{}, nil).Maybe()
	tc.On("SearchTv", mock.Anything, mock.Anything).Return(tmdb.SearchTvResponse{}, nil).Maybe()
	if s.candidateID != 0 {
		tc.On("MovieDetails", mock.Anything, tmdb.MovieDetailsRequest{
			ID:               s.candidateID,
			AppendToResponse: []string{"alternative_titles", "translations"},
		}).
			Return(tmdb.MovieDetailsResponse{
				ID: s.candidateID, Title: s.llmTitle, ReleaseDate: func() string {
					if s.candidateReleaseDate != "" {
						return s.candidateReleaseDate
					}
					return "2020-04-24"
				}(), OriginalLanguage: "en",
			}, nil).Maybe()
	}

	comp := compiler{
		options: []compilerOption{compilerFeatures(defaultFeatures), celEnvOption},
		dependencies: dependencies{
			search:                search,
			tmdbClient:            tc,
			llmMatch:              newLLMMatchClient(t, srv.URL, s.live),
			matchDecisionObserver: s.observer,
		},
	}
	source, err := yamlSourceProvider{rawSourceProvider: coreSourceProvider{}}.source()
	require.NoError(t, err)
	workflow, err := comp.Compile(source)
	require.NoError(t, err)

	return workflow.Run(context.Background(), "default", Flags{"llm_match_enabled": true}, model.Torrent{
		Name:        s.torrentName,
		Size:        2_000_000_000,
		FilesStatus: model.FilesStatusSingle,
		Extension:   model.NewNullString("mkv"),
	})
}

type recordingMatchDecisionObserver struct {
	observations []MatchDecisionObservation
	sawTrace     bool
	err          error
}

func (o *recordingMatchDecisionObserver) ObserveLLMMatchDecision(
	ctx context.Context,
	observation MatchDecisionObservation,
) error {
	o.sawTrace = llmcapture.ResultTraceFrom(ctx) != nil
	o.observations = append(o.observations, observation)
	return o.err
}

type matchDecisionObserverFunc func(context.Context, MatchDecisionObservation) error

func (f matchDecisionObserverFunc) ObserveLLMMatchDecision(
	ctx context.Context,
	observation MatchDecisionObservation,
) error {
	return f(ctx, observation)
}

// runLLMMatch is the plain non-anime movie scenario used by the base tests.
func runLLMMatch(t *testing.T, live bool) (classification.Result, error) {
	return runLLMScenario(t, llmMatchScenario{
		live:        live,
		torrentName: "Tyler.Rake.2020.1080p.WEBRip.x264.mkv",
		baseTitle:   "Tyler Rake",
		extractJSON: `{"title":"Extraction","year":2020,"type":"movie","season":0,"episode":0,"is_anime":false,"is_pack":false,"is_adult":false}`,
		rerankJSON:  `{"tmdb_id":545609,"confidence":0.95}`,
		llmTitle:    "Extraction",
		candidateID: 545609,
	})
}

func TestLLMMatchLiveAttaches(t *testing.T) {
	t.Parallel()
	result, err := runLLMMatch(t, true)
	require.NoError(t, err)
	require.NotNil(t, result.Content, "live mode should attach the reranked TMDB content")
	assert.Equal(t, "tmdb", result.Content.Source)
	assert.Equal(t, "545609", result.Content.ID)
	assert.Equal(t, model.ContentTypeMovie, result.ContentType.ContentType)
	assert.Contains(t, result.Tags, llmMatchedTagName,
		"live LLM attach must be tagged so it is distinguishable from heuristic tmdb attaches")
}

func TestLLMMatchNativePrivateIsGatedBeforeModelOrSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("native private torrent must not reach the matcher endpoint")
	}))
	t.Cleanup(srv.Close)

	tor := model.Torrent{
		Name:        "Private.Release.2026.1080p.mkv",
		Size:        2_000_000_000,
		Private:     true,
		FilesStatus: model.FilesStatusSingle,
		Extension:   model.NewNullString("mkv"),
	}
	dec, err := (matchRunner{lm: newLLMMatchClient(t, srv.URL, true)}).decide(
		context.Background(), tor, model.NewNullContentType(model.ContentTypeMovie),
	)
	require.NoError(t, err)
	assert.Equal(t, OutcomeGated, dec.Outcome)
}

func TestLLMMatchTitleCompatibilityRejectsUnrelatedCandidates(t *testing.T) {
	for _, tc := range []struct {
		name      string
		extracted string
		candidate string
		mediaType string
		want      bool
	}{
		{"exact", "Extraction", "Extraction", "movie", true},
		{"punctuation", "Mickey: The Story of a Mouse", "Mickey The Story Of A Mouse", "movie", true},
		{"tv subtitle is distinct", "Dr. STONE: New World", "Dr. STONE", "tv", false},
		{"one edit sequel", "Cars 2", "Cars 3", "movie", false},
		{"singular plural identity", "Alien", "Aliens", "movie", false},
		{"tv prefix without delimiter", "Formula 1 Portugal Grand Prix", "Formula 1", "tv", false},
		{"one token tv base", "Mickey: Mouse Clubhouse", "Mickey", "tv", false},
		{"tv spinoff", "Star Trek: The Next Generation", "Star Trek", "tv", false},
		{"tv franchise", "Law & Order: Special Victims Unit", "Law & Order", "tv", false},
		{"movie sequel prefix", "The Matrix Reloaded", "The Matrix", "movie", false},
		{"wrong mickey title", "Mickey: The Story of a Mouse", "Mickey Mouse Clubhouse", "movie", false},
		{"wrong grand tour", "Formula 1 Portugal Grand Prix", "Grand Tours of Scotland's Rivers", "tv", false},
		{"wrong hidden title", "Hidden Zone Asian Edition", "Hidden Garden", "tv", false},
		{"wrong bbc title", "BBC Urbi et Orbi", "United Gangs of America", "tv", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := llmMatchTitleCompatible(
				llmmatch.Extraction{Title: tc.extracted, Type: tc.mediaType},
				llmmatch.Candidate{Title: tc.candidate},
			)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestFinishLLMMatchRejectsInvalidModelConfidence(t *testing.T) {
	for _, confidence := range []float64{
		math.NaN(),
		math.Inf(1),
		-0.1,
		1.1,
	} {
		cfg := llmmatch.NewDefaultConfig()
		cfg.Enabled = true
		cfg.EnableLive = true
		client := llmmatch.NewClient(
			cfg,
			nil,
			llmmatch.NewMetrics(),
			zap.NewNop().Sugar(),
		)
		resolved := false
		_, err := finishLLMMatch(
			context.Background(),
			classification.Result{},
			client,
			model.Torrent{},
			MatchDecision{
				Outcome:    OutcomeMatched,
				Confidence: confidence,
				resolve: func() (model.Content, error) {
					resolved = true
					return model.Content{}, nil
				},
			},
			nil,
		)
		if !errors.Is(err, classification.ErrUnmatched) {
			t.Errorf("confidence=%v: err = %v, want unmatched", confidence, err)
		}
		if resolved {
			t.Errorf("confidence=%v: invalid score reached resolve", confidence)
		}
	}
}

func TestFinishLLMMatchRejectsInvalidConfidenceConfiguration(t *testing.T) {
	for _, minConfidence := range []float64{
		math.NaN(),
		math.Inf(1),
		-0.1,
		0,
		1.1,
	} {
		cfg := llmmatch.NewDefaultConfig()
		cfg.Enabled = true
		cfg.EnableLive = true
		cfg.MinConfidence = minConfidence
		client := llmmatch.NewClient(
			cfg,
			nil,
			llmmatch.NewMetrics(),
			zap.NewNop().Sugar(),
		)
		resolved := false
		_, err := finishLLMMatch(
			context.Background(),
			classification.Result{},
			client,
			model.Torrent{},
			MatchDecision{
				Outcome:    OutcomeMatched,
				Confidence: 0.99,
				resolve: func() (model.Content, error) {
					resolved = true
					return model.Content{}, nil
				},
			},
			nil,
		)
		if !errors.Is(err, classification.ErrUnmatched) {
			t.Errorf("MinConfidence=%v: err = %v, want unmatched", minConfidence, err)
		}
		if resolved {
			t.Errorf("MinConfidence=%v: invalid threshold reached resolve", minConfidence)
		}
	}
}

func TestFinishLLMMatchShadowAndLiveApplySameFinalPolicy(t *testing.T) {
	for _, live := range []bool{false, true} {
		for _, tc := range []struct {
			name         string
			confidence   float64
			resolveErr   error
			resolvedYear model.Year
			wantResolved bool
			wantGate     string
			wouldAttach  bool
		}{
			{name: "confidence", confidence: 0.79, resolvedYear: 1986, wantGate: "confidence"},
			{name: "resolve", confidence: 0.95, resolveErr: errors.New("details unavailable"), wantResolved: true, wantGate: "resolve"},
			{name: "resolved year", confidence: 0.95, resolvedYear: 2026, wantResolved: true, wantGate: "resolved_year"},
			{name: "accepted", confidence: 0.95, resolvedYear: 1986, wantResolved: true, wouldAttach: true},
		} {
			t.Run(tc.name+"/live="+strconv.FormatBool(live), func(t *testing.T) {
				cfg := llmmatch.NewDefaultConfig()
				cfg.Enabled = true
				cfg.EnableLive = live
				cfg.MinConfidence = 0.8
				cfg.RequireSourceTitle = true
				client := llmmatch.NewClient(cfg, nil, llmmatch.NewMetrics(), zap.NewNop().Sugar())
				observer := &recordingMatchDecisionObserver{}
				resolved := false
				torrent := model.Torrent{Name: "Labyrinth.1986.1080p.mkv"}
				candidate := llmmatch.Candidate{
					ID: 13597, Title: "Labyrinth", Year: 1986,
					AltTitles: []string{"Labyrinth (1986)"},
				}
				decision := MatchDecision{
					Outcome:         OutcomeMatched,
					Extract:         llmmatch.Extraction{Title: "Labyrinth", Year: 1986, Type: "movie"},
					ParsedTitle:     "Labyrinth",
					CandidateSource: "api",
					Candidates:      []llmmatch.Candidate{candidate},
					MatchedID:       candidate.ID,
					MatchedTitle:    candidate.Title,
					Confidence:      tc.confidence,
					resolve: func() (model.Content, error) {
						resolved = true
						return model.Content{
							Type: model.ContentTypeMovie, Source: model.SourceTmdb,
							ID: "13597", Title: "Labyrinth", ReleaseYear: tc.resolvedYear,
						}, tc.resolveErr
					},
				}

				result, err := finishLLMMatch(
					context.Background(), classification.Result{}, client,
					torrent, decision, observer,
				)

				assert.Equal(t, tc.wantResolved, resolved)
				require.Len(t, observer.observations, 1)
				observation := observer.observations[0]
				assert.Equal(t, tc.wantGate, observation.GateReason)
				assert.Equal(t, tc.wouldAttach, observation.WouldAttach)
				assert.Equal(t, live, observation.Live)
				assert.Equal(t, 0.8, observation.MinConfidence)
				assert.True(t, observation.RequireSourceTitle)
				assert.Equal(t, int64(13597), observation.MatchedID)
				assert.Equal(t, tc.confidence, observation.Confidence)
				if tc.wantGate != "" {
					assert.Equal(t, OutcomeDeclined, observation.Outcome)
				} else {
					assert.Equal(t, OutcomeMatched, observation.Outcome)
				}

				if live && tc.wouldAttach {
					require.NoError(t, err)
					require.NotNil(t, result.Content)
					assert.Contains(t, result.Tags, llmMatchedTagName)
				} else {
					assert.ErrorIs(t, err, classification.ErrUnmatched)
					assert.Nil(t, result.Content)
					assert.NotContains(t, result.Tags, llmMatchedTagName)
				}
			})
		}
	}
}

func TestFinishLLMMatchObserverErrorFailsClosedBeforeLiveAttach(t *testing.T) {
	cfg := llmmatch.NewDefaultConfig()
	cfg.Enabled = true
	cfg.EnableLive = true
	client := llmmatch.NewClient(cfg, nil, llmmatch.NewMetrics(), zap.NewNop().Sugar())
	wantErr := errors.New("decision ledger unavailable")
	observer := &recordingMatchDecisionObserver{err: wantErr}
	result, err := finishLLMMatch(
		context.Background(),
		classification.Result{},
		client,
		model.Torrent{Name: "Dune.2021.mkv"},
		MatchDecision{
			Outcome:      OutcomeMatched,
			Extract:      llmmatch.Extraction{Title: "Dune", Year: 2021},
			MatchedID:    438631,
			MatchedTitle: "Dune",
			Confidence:   0.99,
			resolve: func() (model.Content, error) {
				return model.Content{
					Type: model.ContentTypeMovie, Source: model.SourceTmdb,
					ID: "438631", Title: "Dune", ReleaseYear: 2021,
				}, nil
			},
		},
		observer,
	)
	assert.ErrorIs(t, err, wantErr)
	assert.Nil(t, result.Content)
	assert.NotContains(t, result.Tags, llmMatchedTagName)
}

func TestObserveLLMMatchDecisionCopiesMutableEvidence(t *testing.T) {
	var infoHash [20]byte
	infoHash[0] = 7
	torrent := model.Torrent{InfoHash: infoHash}
	decision := MatchDecision{Candidates: []llmmatch.Candidate{{
		ID: 1, Title: "Dune", AltTitles: []string{"Dune: Part One"},
	}}}
	observer := matchDecisionObserverFunc(func(_ context.Context, got MatchDecisionObservation) error {
		got.InfoHash[0] = 99
		got.Candidates[0].Title = "mutated"
		got.Candidates[0].AltTitles[0] = "mutated"
		return nil
	})
	require.NoError(t, observeLLMMatchDecision(
		context.Background(), observer, torrent, decision, 0.8, nil, false, false, true,
	))
	assert.Equal(t, byte(7), torrent.InfoHash[0])
	assert.Equal(t, "Dune", decision.Candidates[0].Title)
	assert.Equal(t, "Dune: Part One", decision.Candidates[0].AltTitles[0])
}

func TestLLMMatchShadowDoesNotAttach(t *testing.T) {
	t.Parallel()
	result, err := runLLMMatch(t, false)
	// Shadow: the torrent stays a typed-but-unattached movie.
	assert.Nil(t, result.Content, "shadow mode must not attach")
	assert.Equal(t, model.ContentTypeMovie, result.ContentType.ContentType)
	assert.NotContains(t, result.Tags, llmMatchedTagName, "shadow mode must not tag")
	_ = err
}

func TestLLMMatchShadowObservesExactWouldAttachDecision(t *testing.T) {
	observer := &recordingMatchDecisionObserver{}
	result, err := runLLMScenario(t, llmMatchScenario{
		live:        false,
		observer:    observer,
		torrentName: "Tyler.Rake.2020.1080p.WEBRip.x264.mkv",
		baseTitle:   "Tyler Rake",
		extractJSON: `{"title":"Extraction","year":2020,"type":"movie","season":0,"episode":0,"is_anime":false,"is_pack":false,"is_adult":false}`,
		rerankJSON:  `{"tmdb_id":545609,"confidence":0.95}`,
		llmTitle:    "Extraction",
		candidateID: 545609,
	})

	require.NoError(t, err)
	assert.Nil(t, result.Content)
	assert.NotContains(t, result.Tags, llmMatchedTagName)
	require.Len(t, observer.observations, 1)
	observation := observer.observations[0]
	assert.True(t, observer.sawTrace, "action must share one result trace across provider calls and observation")
	assert.Equal(t, OutcomeMatched, observation.Outcome)
	assert.Equal(t, "Tyler Rake", observation.ParsedTitle)
	assert.Equal(t, "api", observation.CandidateSource)
	assert.Equal(t, int64(545609), observation.MatchedID)
	assert.Equal(t, 0.95, observation.Confidence)
	assert.True(t, observation.Resolved)
	assert.Equal(t, 2020, observation.ResolvedYear)
	assert.True(t, observation.WouldAttach)
	assert.False(t, observation.Live)
}

// A Japanese-only anime raw must never be matched, even in live mode — the
// English gate fires before any TMDB search.
func TestLLMMatchAnimeRawRejected(t *testing.T) {
	t.Parallel()
	result, _ := runLLMScenario(t, llmMatchScenario{
		live:        true,
		torrentName: "Some.Anime.Movie.2020.1080p.RAW.mkv",
		baseTitle:   "Some Anime Movie",
		// is_anime + english:none -> gate rejects; rerank/llmTitle never reached.
		extractJSON: `{"title":"Some Anime Movie","year":2020,"type":"movie","is_anime":true,"english":"none","is_pack":false,"is_adult":false}`,
	})
	assert.Nil(t, result.Content, "japanese-only anime must not be attached")
}

// A local-mirror candidate the rerank accepts attaches directly — no TMDB API
// call at all (no SearchMovie for the LLM title, no MovieDetails; the mock
// would fail the test on an unexpected MovieDetails call).
func TestLLMMatchLocalCandidateAttachesWithoutAPI(t *testing.T) {
	t.Parallel()
	result, err := runLLMScenario(t, llmMatchScenario{
		live:        true,
		torrentName: "Tyler.Rake.2020.1080p.WEBRip.x264.mkv",
		baseTitle:   "Tyler Rake",
		extractJSON: `{"title":"Extraction","year":2020,"type":"movie","is_anime":false,"is_pack":false,"is_adult":false}`,
		rerankJSON:  `{"tmdb_id":545609,"confidence":0.95}`,
		localCandidates: []model.Content{{
			Type: model.ContentTypeMovie, Source: model.SourceTmdb, ID: "545609",
			Title: "Extraction", ReleaseYear: 2020,
			Overview: model.NewNullString("A black market mercenary..."),
		}},
	})
	require.NoError(t, err)
	require.NotNil(t, result.Content, "an accepted local candidate should attach")
	assert.Equal(t, "545609", result.Content.ID)
	assert.Contains(t, result.Tags, llmMatchedTagName)
}

func TestLLMMatchMovieRejectsReleaseNameYearConflict(t *testing.T) {
	t.Parallel()
	result, err := runLLMScenario(t, llmMatchScenario{
		live:                 true,
		torrentName:          "Brown's Requiem.Michael Rooker.Selma Blair.1998.DVDRip",
		baseTitle:            "Brown's Requiem Michael Rooker Selma Blair",
		extractJSON:          `{"title":"Selma","year":2014,"type":"movie","is_anime":false,"is_pack":false,"is_adult":false}`,
		rerankJSON:           `{"tmdb_id":273895,"confidence":0.95}`,
		llmTitle:             "Selma",
		candidateID:          273895,
		candidateReleaseDate: "2014-12-25",
	})
	_ = err
	assert.Nil(t, result.Content, "movie LLM match must not override an explicit conflicting release year")
	assert.NotContains(t, result.Tags, llmMatchedTagName)
}

// When the rerank declines every local candidate, the matcher falls through
// to the TMDB API search and can still attach a fresh result.
func TestLLMMatchLocalDeclineFallsBackToAPI(t *testing.T) {
	t.Parallel()
	srv := llmChatServer(t,
		`{"title":"Extraction","year":2020,"type":"movie","is_anime":false,"is_pack":false,"is_adult":false}`,
		`{"tmdb_id":0,"confidence":0}`,        // rerank declines the local candidate
		`{"tmdb_id":545609,"confidence":0.9}`, // rerank accepts the API candidate
	)

	search := classifier_mocks.NewLocalSearch(t)
	search.On("ContentBySearch", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(model.Content{}, classification.ErrUnmatched).Maybe()
	search.On("ContentCandidatesBySearch", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return([]model.Content{{
			Type: model.ContentTypeMovie, Source: model.SourceTmdb, ID: "999999",
			Title: "Extraction Point", ReleaseYear: 2007,
		}}, nil).Maybe()

	tc := tmdb_mocks.NewClient(t)
	tc.On("SearchMovie", mock.Anything, mock.Anything).Return(tmdb.SearchMovieResponse{
		Results: []tmdb.SearchMovieResult{{ID: 545609, Title: "Extraction", ReleaseDate: "2020-04-24"}},
	}, nil).Maybe()
	tc.On("SearchTv", mock.Anything, mock.Anything).Return(tmdb.SearchTvResponse{}, nil).Maybe()
	tc.On("MovieDetails", mock.Anything, mock.Anything).Return(tmdb.MovieDetailsResponse{
		ID: 545609, Title: "Extraction", ReleaseDate: "2020-04-24", OriginalLanguage: "en",
	}, nil).Maybe()

	comp := compiler{
		options: []compilerOption{compilerFeatures(defaultFeatures), celEnvOption},
		dependencies: dependencies{
			search:     search,
			tmdbClient: tc,
			llmMatch:   newLLMMatchClient(t, srv.URL, true),
		},
	}
	source, err := yamlSourceProvider{rawSourceProvider: coreSourceProvider{}}.source()
	require.NoError(t, err)
	workflow, err := comp.Compile(source)
	require.NoError(t, err)

	result, err := workflow.Run(context.Background(), "default", Flags{"llm_match_enabled": true}, model.Torrent{
		Name:        "Tyler.Rake.2020.1080p.WEBRip.x264.mkv",
		Size:        2_000_000_000,
		FilesStatus: model.FilesStatusSingle,
		Extension:   model.NewNullString("mkv"),
	})
	require.NoError(t, err)
	require.NotNil(t, result.Content, "API fallback should attach after a local decline")
	assert.Equal(t, "545609", result.Content.ID)
}

// A multi-film pack must stay unattached even in live mode — attaching a
// trilogy to its first film is a wrong match (torrent_contents holds ONE
// content row per torrent).
func TestLLMMatchPackRejected(t *testing.T) {
	t.Parallel()
	result, _ := runLLMScenario(t, llmMatchScenario{
		live:        true,
		torrentName: "The.Lord.of.the.Rings.Trilogy.Extended.2001-2003.1080p.BluRay.x264.mkv",
		baseTitle:   "The Lord of the Rings Trilogy Extended",
		// is_pack -> gate rejects; rerank/llmTitle never reached.
		extractJSON: `{"title":"The Lord of the Rings","year":2001,"type":"movie","is_anime":false,"is_pack":true,"is_adult":false}`,
	})
	assert.Nil(t, result.Content, "multi-film packs must not be attached to a single film")
}

// An adult release that slipped past xxx typing must never attach to a
// mainstream title, even when the model still produced one.
func TestLLMMatchAdultRejected(t *testing.T) {
	t.Parallel()
	result, _ := runLLMScenario(t, llmMatchScenario{
		live:        true,
		torrentName: "SomeAdultSite - Performer Name (Scene Title) 17 June 2017.mkv",
		baseTitle:   "Some Adult Site",
		// Defence in depth: is_adult rejects even if the model disobeyed the
		// title-"" instruction and named a real show.
		extractJSON: `{"title":"The Baby-Sitters Club","year":1990,"type":"tv","is_anime":false,"is_pack":false,"is_adult":true}`,
	})
	assert.Nil(t, result.Content, "adult releases must not be matched to mainstream titles")
}

// An anime with an English dub (Dual Audio) passes the gate and matches.
func TestLLMMatchAnimeDubAttaches(t *testing.T) {
	t.Parallel()
	result, err := runLLMScenario(t, llmMatchScenario{
		live:        true,
		torrentName: "Kimetsu.no.Yaiba.2019.1080p.Dual.Audio.mkv",
		baseTitle:   "Kimetsu no Yaiba",
		extractJSON: `{"title":"Demon Slayer","year":2019,"type":"movie","is_anime":true,"english":"dub","is_pack":false,"is_adult":false}`,
		rerankJSON:  `{"tmdb_id":777001,"confidence":0.9}`,
		llmTitle:    "Demon Slayer",
		candidateID: 777001,
	})
	require.NoError(t, err)
	require.NotNil(t, result.Content, "english-dub anime should attach")
	assert.Equal(t, "777001", result.Content.ID)
}

func TestLLMMatchAnimeTVRetriesWithoutSeasonYear(t *testing.T) {
	t.Parallel()
	srv := llmChatServer(t,
		`{"title":"One-Punch Man","year":2025,"type":"tv","season":3,"episode":10,"is_anime":true,"english":"sub","is_pack":false,"is_adult":false}`,
		`{"tmdb_id":63926,"confidence":0.93}`,
	)
	search := classifier_mocks.NewLocalSearch(t)
	search.On("ContentCandidatesBySearch", mock.Anything, model.ContentTypeTvShow, "One-Punch Man", model.Year(2025), mock.Anything).
		Return([]model.Content{}, nil).Once()
	search.On("ContentCandidatesBySearch", mock.Anything, model.ContentTypeTvShow, "One-Punch Man", model.Year(0), mock.Anything).
		Return([]model.Content{}, nil).Once()

	tc := tmdb_mocks.NewClient(t)
	tc.On("SearchTv", mock.Anything, mock.MatchedBy(func(r tmdb.SearchTvRequest) bool {
		return r.Query == "One-Punch Man" && r.FirstAirDateYear == model.Year(2025)
	})).Return(tmdb.SearchTvResponse{}, nil).Once()
	tc.On("SearchTv", mock.Anything, mock.MatchedBy(func(r tmdb.SearchTvRequest) bool {
		return r.Query == "One-Punch Man" && r.FirstAirDateYear == model.Year(0)
	})).Return(tmdb.SearchTvResponse{
		Results: []tmdb.SearchTvResult{{ID: 63926, Name: "One-Punch Man", FirstAirDate: "2015-10-05"}},
	}, nil).Once()

	dec, err := matchRunner{
		search: search,
		tmdb:   tc,
		lm:     newLLMMatchClient(t, srv.URL, true),
	}.decide(context.Background(), model.Torrent{
		Name:        "[Erai-raws] One Punch Man (2025) - 10 [1080p CR WEB-DL AVC AAC][MultiSub].mkv",
		Size:        1_400_000_000,
		FilesStatus: model.FilesStatusSingle,
		Extension:   model.NewNullString("mkv"),
	}, model.NewNullContentType(model.ContentTypeTvShow))

	require.NoError(t, err)
	assert.Equal(t, OutcomeMatched, dec.Outcome)
	assert.Equal(t, int64(63926), dec.MatchedID)
}

func TestLLMMatchAnimeTVBaseTitleRequiresVettedAlias(t *testing.T) {
	t.Parallel()
	srv := llmChatServer(t,
		`{"title":"Dr. STONE: New World","year":2023,"type":"tv","season":3,"episode":0,"is_anime":true,"english":"dub","is_pack":false,"is_adult":false}`,
		`{"tmdb_id":86031,"confidence":0.94}`,
	)
	search := classifier_mocks.NewLocalSearch(t)
	search.On("ContentCandidatesBySearch", mock.Anything, model.ContentTypeTvShow, "Dr. STONE: New World", model.Year(2023), mock.Anything).
		Return([]model.Content{}, nil).Once()
	search.On("ContentCandidatesBySearch", mock.Anything, model.ContentTypeTvShow, "Dr. STONE: New World", model.Year(0), mock.Anything).
		Return([]model.Content{}, nil).Once()
	search.On("ContentCandidatesBySearch", mock.Anything, model.ContentTypeTvShow, "Dr. STONE", model.Year(2023), mock.Anything).
		Return([]model.Content{}, nil).Once()
	search.On("ContentCandidatesBySearch", mock.Anything, model.ContentTypeTvShow, "Dr. STONE", model.Year(0), mock.Anything).
		Return([]model.Content{}, nil).Once()

	tc := tmdb_mocks.NewClient(t)
	tc.On("SearchTv", mock.Anything, mock.MatchedBy(func(r tmdb.SearchTvRequest) bool {
		return r.Query == "Dr. STONE: New World" && r.FirstAirDateYear == model.Year(2023)
	})).Return(tmdb.SearchTvResponse{}, nil).Once()
	tc.On("SearchTv", mock.Anything, mock.MatchedBy(func(r tmdb.SearchTvRequest) bool {
		return r.Query == "Dr. STONE: New World" && r.FirstAirDateYear == model.Year(0)
	})).Return(tmdb.SearchTvResponse{}, nil).Once()
	tc.On("SearchTv", mock.Anything, mock.MatchedBy(func(r tmdb.SearchTvRequest) bool {
		return r.Query == "Dr. STONE" && r.FirstAirDateYear == model.Year(2023)
	})).Return(tmdb.SearchTvResponse{}, nil).Once()
	tc.On("SearchTv", mock.Anything, mock.MatchedBy(func(r tmdb.SearchTvRequest) bool {
		return r.Query == "Dr. STONE" && r.FirstAirDateYear == model.Year(0)
	})).Return(tmdb.SearchTvResponse{
		Results: []tmdb.SearchTvResult{{ID: 86031, Name: "Dr. STONE", FirstAirDate: "2019-07-05"}},
	}, nil).Once()

	dec, err := matchRunner{
		search: search,
		tmdb:   tc,
		lm:     newLLMMatchClient(t, srv.URL, true),
	}.decide(context.Background(), model.Torrent{
		Name:        "[Sokudo] Dr. STONE - New World - S03 v3 [1080p BD AV1][Dual Audio]",
		Size:        9_400_000_000,
		FilesStatus: model.FilesStatusMulti,
		Files: []model.TorrentFile{{
			Path:      "Dr.STONE.New.World.S03E01.mkv",
			Extension: model.NewNullString("mkv"),
		}},
	}, model.NewNullContentType(model.ContentTypeTvShow))

	require.NoError(t, err)
	assert.Equal(t, OutcomeDeclined, dec.Outcome)
	assert.Equal(t, int64(86031), dec.MatchedID)
	assert.Equal(t, "Dr. STONE", dec.MatchedTitle)
	assert.Equal(t, 0.94, dec.Confidence)
	assert.NotEmpty(t, dec.GateReason)
}

// A resolved anime alias attaches its vetted TMDB id directly from the local
// mirror — no TMDB search, no rerank. The LLM mis-extracts the title ("Kiss
// Him, Not Me"); the deterministic anime backbone overrides it via the seed set
// (KiseKoi -> My Dress-Up Darling).
func TestLLMMatchAnimeAliasDirectAttachLocal(t *testing.T) {
	t.Parallel()
	srv := llmChatServer(t,
		`{"title":"Kiss Him, Not Me","year":2026,"type":"tv","season":2,"episode":12,"is_anime":true,"english":"sub","is_pack":false,"is_adult":false}`,
	)
	search := classifier_mocks.NewLocalSearch(t)
	search.On("ContentByID", mock.Anything, model.ContentRef{
		Type: model.ContentTypeTvShow, Source: "tmdb", ID: "123249",
	}).Return(model.Content{
		Type: model.ContentTypeTvShow, Source: model.SourceTmdb, ID: "123249",
		Title: "My Dress-Up Darling", ReleaseYear: model.Year(2022),
	}, nil).Once()

	tc := tmdb_mocks.NewClient(t) // must not be called — direct attach from the mirror

	dec, err := matchRunner{
		search:   search,
		tmdb:     tc,
		lm:       newLLMMatchClient(t, srv.URL, true),
		resolver: animedb.NewSeedResolver(),
	}.decide(context.Background(), model.Torrent{
		Name:        "[Judas] KiseKoi - S02E12.mkv",
		Size:        240_000_000,
		FilesStatus: model.FilesStatusSingle,
		Extension:   model.NewNullString("mkv"),
	}, model.NewNullContentType(model.ContentTypeTvShow))

	require.NoError(t, err)
	assert.Equal(t, OutcomeMatched, dec.Outcome)
	assert.Equal(t, int64(123249), dec.MatchedID)
	assert.Equal(t, "My Dress-Up Darling", dec.MatchedTitle)
	assert.Equal(t, "alias", dec.CandidateSource)
	assert.Equal(t, 1.0, dec.Confidence)
	assert.Equal(t, "My Dress-Up Darling", dec.Extract.Title)
}

// When the alias id is not mirrored locally, the direct attach falls back to a
// deferred TMDB fetch-by-id — still no search, no rerank. decide makes no TMDB
// call (the fetch is inside the resolve closure invoked later on attach).
func TestLLMMatchAnimeAliasDirectAttachAPI(t *testing.T) {
	t.Parallel()
	srv := llmChatServer(t,
		`{"title":"Kiss Him, Not Me","year":2026,"type":"tv","season":2,"episode":12,"is_anime":true,"english":"sub","is_pack":false,"is_adult":false}`,
	)
	search := classifier_mocks.NewLocalSearch(t)
	search.On("ContentByID", mock.Anything, model.ContentRef{
		Type: model.ContentTypeTvShow, Source: "tmdb", ID: "123249",
	}).Return(model.Content{}, classification.ErrUnmatched).Once()

	tc := tmdb_mocks.NewClient(t) // resolve() deferred; decide itself makes no tmdb call

	dec, err := matchRunner{
		search:   search,
		tmdb:     tc,
		lm:       newLLMMatchClient(t, srv.URL, true),
		resolver: animedb.NewSeedResolver(),
	}.decide(context.Background(), model.Torrent{
		Name:        "[Judas] KiseKoi - S02E12.mkv",
		Size:        240_000_000,
		FilesStatus: model.FilesStatusSingle,
		Extension:   model.NewNullString("mkv"),
	}, model.NewNullContentType(model.ContentTypeTvShow))

	require.NoError(t, err)
	assert.Equal(t, OutcomeMatched, dec.Outcome)
	assert.Equal(t, int64(123249), dec.MatchedID)
	assert.Equal(t, "alias", dec.CandidateSource)
	assert.Equal(t, 1.0, dec.Confidence)
}

// A romaji title with a season suffix resolves through the seed contains-net
// (Jidou Hanbaiki -> Reborn as a Vending Machine) even when the LLM extracts
// something else entirely, and direct-attaches its vetted id.
func TestLLMMatchAnimeAliasResolvesRomaji(t *testing.T) {
	t.Parallel()
	srv := llmChatServer(t,
		`{"title":"Wrong Anime","year":2025,"type":"tv","is_anime":true,"english":"dub","is_pack":false,"is_adult":false}`,
	)
	search := classifier_mocks.NewLocalSearch(t)
	search.On("ContentByID", mock.Anything, model.ContentRef{
		Type: model.ContentTypeTvShow, Source: "tmdb", ID: "207564",
	}).Return(model.Content{}, classification.ErrUnmatched).Once()
	tc := tmdb_mocks.NewClient(t)

	dec, err := matchRunner{
		search:   search,
		tmdb:     tc,
		lm:       newLLMMatchClient(t, srv.URL, true),
		resolver: animedb.NewSeedResolver(),
	}.decide(context.Background(), model.Torrent{
		Name:        "[SubsPlease] Jidou Hanbaiki ni Umarekawatta Ore wa Meikyuu wo Samayou S2 - 07 (1080p).mkv",
		Size:        1_400_000_000,
		FilesStatus: model.FilesStatusSingle,
		Extension:   model.NewNullString("mkv"),
	}, model.NewNullContentType(model.ContentTypeTvShow))

	require.NoError(t, err)
	assert.Equal(t, OutcomeMatched, dec.Outcome)
	assert.Equal(t, int64(207564), dec.MatchedID)
	assert.Equal(t, "Reborn as a Vending Machine, I Now Wander the Dungeon", dec.Extract.Title)
	assert.Equal(t, "alias", dec.CandidateSource)
}

func runLLMMatchDataAliasDecision(
	t *testing.T,
	rerankJSON string,
	candidates []tmdb.SearchTvResult,
) MatchDecision {
	t.Helper()
	srv := llmChatServer(t,
		`{"title":"JoJo's Bizarre Adventure","year":0,"type":"tv","season":5,"episode":0,"is_anime":true,"english":"dub","is_pack":false,"is_adult":false}`,
		rerankJSON,
	)
	resolver := animedb.NewSeedResolver()
	resolver.Swap([]animedb.Alias{{
		Normalized: animedb.Normalize("JoJo's Bizarre Adventure"),
		Display:    "JoJo's Bizarre Adventure",
		TMDBType:   model.ContentTypeTvShow,
		TMDBID:     60862,
		Source:     "official",
	}})

	search := classifier_mocks.NewLocalSearch(t)
	search.On(
		"ContentCandidatesBySearch",
		mock.Anything,
		model.ContentTypeTvShow,
		mock.Anything,
		mock.Anything,
		mock.Anything,
	).Return([]model.Content{}, nil).Maybe()
	tc := tmdb_mocks.NewClient(t)
	tc.On("SearchTv", mock.Anything, mock.Anything).Return(tmdb.SearchTvResponse{
		Results: candidates,
	}, nil).Maybe()

	dec, err := matchRunner{
		search:      search,
		tmdb:        tc,
		lm:          newLLMMatchClient(t, srv.URL, true),
		resolver:    resolver,
		parsedTitle: "JoJos Bizarre Adventure",
	}.decide(context.Background(), model.Torrent{
		Name:        "JoJos Bizarre Adventure - S05 - MULTi.1080p.mkv",
		Size:        1_400_000_000,
		FilesStatus: model.FilesStatusSingle,
		Extension:   model.NewNullString("mkv"),
	}, model.NewNullContentType(model.ContentTypeTvShow))

	require.NoError(t, err)
	return dec
}

func TestLLMMatchDataAliasCannotBypassRerank(t *testing.T) {
	t.Parallel()
	dec := runLLMMatchDataAliasDecision(
		t,
		`{"tmdb_id":45790,"confidence":0.98}`,
		[]tmdb.SearchTvResult{
			{ID: 60862, Name: "JoJo's Bizarre Adventure (OVA)", FirstAirDate: "1993-11-19"},
			{ID: 45790, Name: "JoJo's Bizarre Adventure", FirstAirDate: "2012-10-06"},
		},
	)
	assert.Equal(t, OutcomeMatched, dec.Outcome)
	assert.Equal(t, int64(45790), dec.MatchedID)
	assert.Equal(t, "api", dec.CandidateSource)
}

func TestLLMMatchDataAliasWrongCandidateCannotPassTitleGate(t *testing.T) {
	t.Parallel()
	dec := runLLMMatchDataAliasDecision(
		t,
		`{"tmdb_id":60862,"confidence":0.99}`,
		[]tmdb.SearchTvResult{
			{ID: 60862, Name: "JoJo's Bizarre Adventure", FirstAirDate: "1993-11-19"},
			{ID: 45790, Name: "JoJo's Bizarre Adventure", FirstAirDate: "2012-10-06"},
		},
	)
	assert.Equal(t, OutcomeDeclined, dec.Outcome)
	assert.Equal(t, int64(60862), dec.MatchedID)
	assert.Equal(t, "JoJo's Bizarre Adventure", dec.MatchedTitle)
	assert.Equal(t, 0.99, dec.Confidence)
	assert.NotEmpty(t, dec.GateReason)
}

func TestLLMMatchAPIModelDeclinePreservesConfidence(t *testing.T) {
	t.Parallel()
	dec := runLLMMatchDataAliasDecision(
		t,
		`{"tmdb_id":0,"confidence":0.23}`,
		[]tmdb.SearchTvResult{
			{ID: 45790, Name: "JoJo's Bizarre Adventure", FirstAirDate: "2012-10-06"},
		},
	)
	assert.Equal(t, OutcomeDeclined, dec.Outcome)
	assert.Equal(t, "model_declined", dec.GateReason)
	assert.Zero(t, dec.MatchedID)
	assert.Equal(t, 0.23, dec.Confidence)
}

func TestLLMMatchDuplicateCanonicalTitleNeedsIndependentEvidence(t *testing.T) {
	movieCandidates := []llmmatch.Candidate{
		{ID: 1, Title: "The Thing", Year: 1982},
		{ID: 2, Title: "The Thing", Year: 2011},
	}
	assert.True(t, llmMatchCandidateUnambiguous(
		"The.Thing.1982.1080p.mkv", false, movieCandidates[0], movieCandidates,
	))
	assert.False(t, llmMatchCandidateUnambiguous(
		"The.Thing.1080p.mkv", false, movieCandidates[0], movieCandidates,
	))
	assert.False(t, llmMatchCandidateUnambiguous(
		"JoJos.Bizarre.Adventure.S05.mkv",
		true,
		llmmatch.Candidate{ID: 60862, Title: "JoJo's Bizarre Adventure", Year: 1993},
		[]llmmatch.Candidate{
			{ID: 60862, Title: "JoJo's Bizarre Adventure", Year: 1993},
			{ID: 45790, Title: "JoJo's Bizarre Adventure", Year: 2012},
		},
	))
}

// A known adult anime is gated even when the model reports is_adult:false and
// the alias would otherwise resolve — the adult seed entry rejects it.
func TestLLMMatchInterspeciesReviewersOverrideRejected(t *testing.T) {
	t.Parallel()
	srv := llmChatServer(t,
		`{"title":"Interspecies Reviewers","year":2020,"type":"tv","is_anime":true,"english":"sub","is_pack":false,"is_adult":false}`,
	)
	search := classifier_mocks.NewLocalSearch(t)
	tc := tmdb_mocks.NewClient(t)

	dec, err := matchRunner{
		search:   search,
		tmdb:     tc,
		lm:       newLLMMatchClient(t, srv.URL, true),
		resolver: animedb.NewSeedResolver(),
	}.decide(context.Background(), model.Torrent{
		Name:        "[Judas] Ishuzoku Reviewers (Interspecies Reviewers) - (Season 1) [UNCENSORED 1080p][Eng-Subs]",
		Size:        3_800_000_000,
		FilesStatus: model.FilesStatusMulti,
		Files: []model.TorrentFile{{
			Path:      "Ishuzoku.Reviewers.S01E01.mkv",
			Extension: model.NewNullString("mkv"),
		}},
	}, model.NewNullContentType(model.ContentTypeTvShow))

	require.NoError(t, err)
	assert.Equal(t, OutcomeAdult, dec.Outcome)
}

// TestFinishLLMMatchResolvedYearGuard locks the post-resolve year invariant.
// The pre-attach candidate gate cannot see this case: a TMDB search hit with no
// release_date has Year==0, which llmMatchYearCompatible treats as no evidence.
// Only the resolved record carries the real year. Regression anchor for the 32
// same-title wrong-entry attachments found in the 2026-08-04 production census.
func TestFinishLLMMatchResolvedYearGuard(t *testing.T) {
	newClient := func() *llmmatch.Client {
		cfg := llmmatch.NewDefaultConfig()
		cfg.Enabled = true
		cfg.EnableLive = true
		return llmmatch.NewClient(cfg, nil, llmmatch.NewMetrics(), zap.NewNop().Sugar())
	}
	for _, tc := range []struct {
		label        string
		name         string
		isTV         bool
		resolvedYear int
		wantAttached bool
	}{
		{"resolved year contradicts name", "Labyrinth (Henson,1986) 1080p", false, 2026, false},
		{"resolved year matches name", "Labyrinth (Henson,1986) 1080p", false, 1986, true},
		{"off-by-one tolerated", "Labyrinth 1986 1080p", false, 1987, true},
		{"no year in name abstains", "Labyrinth 1080p x264", false, 2026, true},
		{"resolved year unknown abstains", "Labyrinth 1986 1080p", false, 0, true},
		{"tv is exempt", "Shelter S01E02 2014", true, 2016, true},
	} {
		client := newClient()
		resolved := false
		_, err := finishLLMMatch(
			context.Background(),
			classification.Result{},
			client,
			model.Torrent{Name: tc.name},
			MatchDecision{
				Outcome:    OutcomeMatched,
				IsTV:       tc.isTV,
				Confidence: 0.99,
				resolve: func() (model.Content, error) {
					resolved = true
					return model.Content{ReleaseYear: model.Year(tc.resolvedYear)}, nil
				},
			},
			nil,
		)
		if !resolved {
			t.Fatalf("%s: resolve was never called", tc.label)
		}
		attached := err == nil
		if attached != tc.wantAttached {
			t.Errorf("%s: attached = %v (err %v), want %v",
				tc.label, attached, err, tc.wantAttached)
		}
	}
}

// TestNormalizeLLMIdentityTitleFoldsApostrophes locks the one fold the identity
// gate performs beyond titlenorm, using real pairs from 14h of production
// rerank decisions (2026-08-04) that the gate was rejecting.
//
// The negative set is the point: an apostrophe is the ONLY thing folded here.
// Word boundaries must stay significant, because collapsing them merges
// distinct works ("Black Bird"/"Blackbird"), and article handling belongs to
// titlenorm rather than this function.
func TestNormalizeLLMIdentityTitleFoldsApostrophes(t *testing.T) {
	for _, p := range [][2]string{
		{"Jerrys Cousin", "Jerry's Cousin"},
		{"Ill Sleep When Im Dead", "I'll Sleep When I'm Dead"},
		{"Vipers Nest 2", "Viper’s Nest 2"},
		{"Brothers Justice", "Brother's Justice"},
		{"Margos Got Money Troubles", "Margo's Got Money Troubles"},
		{"Lets Stick Together", "Let's Stick Together"},
	} {
		if a, b := normalizeLLMIdentityTitle(p[0]), normalizeLLMIdentityTitle(p[1]); a != b {
			t.Errorf("apostrophe should fold: %q -> %q vs %q -> %q", p[0], a, p[1], b)
		}
	}

	for _, p := range [][2]string{
		// Word boundaries stay significant — the guard against "Black
		// Bird"/"Blackbird"-class merges.
		{"Black Bird", "Blackbird"},
		{"Uma Musume: Pretty Derby", "Umamusume: Pretty Derby"},
		{"KGF: Chapter 2", "K.G.F: Chapter 2"},
		// Distinguishing detail is never folded.
		{"Sinister", "Sinister 2"},
		{"Dirty Deeds", "Dirty"},
		{"Pocket Monsters", "Pokemon Horizons"},
		{"Cars 2", "Cars 3"},
		{"Spider-Man: The Animated Series", "Spider-Man"},
	} {
		if a, b := normalizeLLMIdentityTitle(p[0]), normalizeLLMIdentityTitle(p[1]); a == b {
			t.Errorf("must NOT fold: %q and %q both -> %q", p[0], p[1], a)
		}
	}
}
