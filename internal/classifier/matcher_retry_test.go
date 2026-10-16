package classifier

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	classifier_mocks "github.com/spencercnorton/bitagent/internal/classifier/mocks"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/tmdb"
	tmdb_mocks "github.com/spencercnorton/bitagent/internal/tmdb/mocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const syntheticExtraction = `{"title":"Amber Signal","year":2025,"type":"movie","season":0,"episode":0,"is_anime":false,"english":"unknown","is_pack":false,"is_adult":false}`

func TestMatcherProviderFailuresRemainErrors(t *testing.T) {
	for _, stage := range []string{"extract", "local_rerank", "api_rerank"} {
		for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
			t.Run(stage+http.StatusText(status), func(t *testing.T) {
				calls := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					if stage != "extract" && calls == 1 {
						_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": syntheticExtraction}}}})
						return
					}
					w.Header().Set("Retry-After", "60")
					http.Error(w, "unavailable", status)
				}))
				defer srv.Close()
				search := classifier_mocks.NewLocalSearch(t)
				catalogue := []model.Content{}
				if stage == "local_rerank" {
					catalogue = []model.Content{{Type: model.ContentTypeMovie, Source: model.SourceTmdb, ID: "42", Title: "Amber Signal", ReleaseYear: 2025}}
				}
				if stage != "extract" {
					search.On("ContentCandidatesBySearch", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(catalogue, nil).Once()
				}
				tc := tmdb_mocks.NewClient(t)
				if stage == "local_rerank" {
					tc.On("SearchMovie", mock.Anything, mock.Anything).Return(tmdb.SearchMovieResponse{}, nil).Maybe()
				}
				if stage == "api_rerank" {
					tc.On("SearchMovie", mock.Anything, mock.Anything).Return(tmdb.SearchMovieResponse{Results: []tmdb.SearchMovieResult{{ID: 42, Title: "Amber Signal", ReleaseDate: "2025-01-01"}}}, nil).Once()
				}
				_, err := (matchRunner{lm: newLLMMatchClient(t, srv.URL, true), search: search, tmdb: tc}).decide(context.Background(), model.Torrent{Name: "Amber.Signal.2025.1080p.mkv", Size: 2_000_000_000, FilesStatus: model.FilesStatusSingle, Extension: model.NewNullString("mkv")}, model.NewNullContentType(model.ContentTypeMovie))
				require.Error(t, err, "provider errors must remain retryable, never clean no-match")
				require.NotErrorIs(t, err, classification.ErrUnmatched)
				if stage == "extract" {
					require.Equal(t, 1, calls)
				} else {
					require.Equal(t, 2, calls)
				}
			})
		}
	}
}

func TestMatcherResolveFailureRemainsRetryable(t *testing.T) {
	for _, cause := range []error{context.DeadlineExceeded, errors.New("catalogue unavailable"), classification.ErrUnmatched} {
		observer := &recordingMatchDecisionObserver{}
		result, err := finishLLMMatch(context.Background(), classification.Result{}, newLLMMatchClient(t, "http://unused.invalid", true), model.Torrent{Name: "Amber.Signal.2025.mkv"}, MatchDecision{Outcome: OutcomeMatched, Confidence: .99, resolve: func() (model.Content, error) { return model.Content{}, cause }}, observer)
		require.ErrorIs(t, err, cause)
		require.Nil(t, result.Content)
		require.Len(t, observer.observations, 1)
		require.False(t, observer.observations[0].WouldAttach)
	}
}

func TestMatcherLocalCatalogueFailureIsNotEmptySearch(t *testing.T) {
	srv := llmChatServer(t, syntheticExtraction)
	search := classifier_mocks.NewLocalSearch(t)
	cause := errors.New("catalogue unavailable")
	search.On("ContentCandidatesBySearch", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return([]model.Content{}, cause).Once()
	dec, err := (matchRunner{lm: newLLMMatchClient(t, srv.URL, true), search: search}).decide(context.Background(), model.Torrent{Name: "Amber.Signal.2025.1080p.mkv", Size: 2_000_000_000, FilesStatus: model.FilesStatusSingle, Extension: model.NewNullString("mkv")}, model.NewNullContentType(model.ContentTypeMovie))
	require.ErrorIs(t, err, cause)
	require.Equal(t, OutcomeRetryable, dec.Outcome)
	require.Len(t, dec.Queries, 1)
	require.Equal(t, "retryable_failure", dec.Queries[0].Status)
	require.Equal(t, "Amber Signal", dec.Queries[0].Title)
}

func TestResolutionMissDoesNotHideAuditFailure(t *testing.T) {
	cause := errors.New("audit unavailable")
	observer := matchDecisionObserverFunc(func(context.Context, MatchDecisionObservation) error { return cause })
	_, err := finishLLMMatch(context.Background(), classification.Result{}, newLLMMatchClient(t, "http://unused.invalid", true), model.Torrent{Name: "Amber.Signal.2025.mkv"}, MatchDecision{Outcome: OutcomeMatched, Confidence: .99, resolve: func() (model.Content, error) { return model.Content{}, classification.ErrUnmatched }}, observer)
	require.ErrorIs(t, err, cause)
	require.NotErrorIs(t, err, classification.ErrUnmatched)
}

func TestQueryTracePreservesAliasesWithoutChangingModelCandidates(t *testing.T) {
	var trace []MatchQueryAttempt
	runner := matchRunner{queryTrace: &trace}
	candidates := []llmmatch.Candidate{{ID: 42, Title: "Amber Signal", Year: 2025, AltTitles: []string{"Signal Amber"}}}
	runner.recordQuery("local", "Signal Amber", 2025, "movie", candidates, nil)
	candidates[0].AltTitles[0] = "changed"
	raw, err := json.Marshal(trace)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"42":["Signal Amber"]`)
	require.NotContains(t, string(raw), "changed")
	modelInput, err := json.Marshal(candidates)
	require.NoError(t, err)
	require.NotContains(t, string(modelInput), "changed")
}
