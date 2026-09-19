package classifier

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	classifier_mocks "github.com/spencercnorton/bitagent/internal/classifier/mocks"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/tmdb"
	tmdb_mocks "github.com/spencercnorton/bitagent/internal/tmdb/mocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type matcherCaptureProbe struct {
	requests []llmcapture.Request
}

func (*matcherCaptureProbe) Enabled() bool { return true }

func (*matcherCaptureProbe) RecordHTTPResult(_ context.Context, key []byte, result llmcapture.HTTPResult) (llmcapture.ResultReceipt, error) {
	digest := sha256.Sum256(result.Body)
	return llmcapture.ResultReceipt{CaptureKey: key, ResponseSHA256: digest[:], FirstObservation: true, StatusCode: result.StatusCode, ErrorClass: result.ErrorClass}, nil
}

func (*matcherCaptureProbe) RecordMatchDecision(context.Context, llmcapture.ResultReceipt, []byte, llmcapture.MatchDecision) error {
	return nil
}

func (p *matcherCaptureProbe) Capture(
	_ context.Context,
	req llmcapture.Request,
) (llmcapture.Outcome, error) {
	p.requests = append(p.requests, req)
	return llmcapture.OutcomeRecorded, nil
}

func TestMatcherRerankCaptureFreezesSourceEvidenceOnBothCandidatePaths(
	t *testing.T,
) {
	for _, candidateSource := range []llmcapture.CandidateSource{
		llmcapture.CandidateSourceLocal,
		llmcapture.CandidateSourceAPI,
	} {
		t.Run(string(candidateSource), func(t *testing.T) {
			srv := llmChatServer(
				t,
				`{"title":"Dune","year":2021,"type":"movie","season":0,"episode":0,"is_anime":false,"english":"unknown","is_pack":false,"is_adult":false}`,
				`{"tmdb_id":438631,"confidence":0.99}`,
			)
			probe := &matcherCaptureProbe{}
			cfg := llmmatch.NewDefaultConfig()
			cfg.Enabled = true
			cfg.Endpoint = srv.URL
			cfg.MinTotalSizeBytes = 0
			cfg.RequireSourceTitle = true
			client := llmmatch.NewClientWithCapture(
				cfg,
				nil,
				llmmatch.NewMetrics(),
				zap.NewNop().Sugar(),
				probe,
			)

			catalogue := model.Content{
				Type: model.ContentTypeMovie, Source: model.SourceTmdb,
				ID: "438631", Title: "Dune", ReleaseYear: 2021,
				Attributes: []model.ContentAttribute{{
					Key: model.AltTitleAttributePrefix + "es:test", Value: "Duna",
				}, {
					Key:   model.AltTitleAttributePrefix + "audit:test",
					Value: "Catalogue Alias Never Sent",
				}},
			}
			search := classifier_mocks.NewLocalSearch(t)
			if candidateSource == llmcapture.CandidateSourceLocal {
				search.On(
					"ContentCandidatesBySearch",
					mock.Anything, model.ContentTypeMovie, "Dune", model.Year(2021),
					mock.Anything,
				).Return([]model.Content{catalogue}, nil).Once()
			} else {
				search.On(
					"ContentCandidatesBySearch",
					mock.Anything, model.ContentTypeMovie, "Dune", model.Year(2021),
					mock.Anything,
				).Return([]model.Content{}, nil).Once()
				search.On(
					"ContentByID",
					mock.Anything,
					model.ContentRef{
						Type: model.ContentTypeMovie, Source: "tmdb", ID: "438631",
					},
				).Return(catalogue, nil).Once()
			}

			tmdbClient := tmdb_mocks.NewClient(t)
			if candidateSource == llmcapture.CandidateSourceAPI {
				tmdbClient.On(
					"SearchMovie", mock.Anything, mock.Anything,
				).Return(tmdb.SearchMovieResponse{Results: []tmdb.SearchMovieResult{{
					ID: 438631, Title: "Dune", ReleaseDate: "2021-10-22",
				}}}, nil).Once()
			}

			decision, err := (matchRunner{
				search:        search,
				tmdb:          tmdbClient,
				lm:            client,
				parsedTitle:   "Duna",
				altTitleMatch: true,
			}).decide(context.Background(), model.Torrent{
				Name:        "Duna.2021.1080p.mkv",
				Size:        2_000_000_000,
				FilesStatus: model.FilesStatusSingle,
				Extension:   model.NewNullString("mkv"),
			}, model.NewNullContentType(model.ContentTypeMovie))
			require.NoError(t, err)
			require.Equal(t, OutcomeMatched, decision.Outcome)
			require.Equal(t, int64(438631), decision.MatchedID)

			var rerankRequest *llmcapture.Request
			for i := range probe.requests {
				if probe.requests[i].Task == llmcapture.TaskMatcherRerank {
					rerankRequest = &probe.requests[i]
					break
				}
			}
			require.NotNil(t, rerankRequest)
			require.Equal(t, candidateSource, rerankRequest.CandidateSource)
			var captured struct {
				ParsedTitle string `json:"parsed_title"`
				Candidates  []struct {
					AltTitles []string `json:"alt_titles"`
				} `json:"candidates"`
			}
			require.NoError(
				t,
				json.Unmarshal(rerankRequest.TaskInputJSON, &captured),
			)
			require.Equal(t, "Duna", captured.ParsedTitle)
			require.Len(t, captured.Candidates, 1)
			require.ElementsMatch(
				t,
				[]string{"Duna", "Catalogue Alias Never Sent"},
				captured.Candidates[0].AltTitles,
			)
			require.NotContains(
				t,
				string(rerankRequest.ModelInputJSON),
				"Catalogue Alias Never Sent",
			)
			require.NotContains(
				t,
				string(rerankRequest.ModelInputJSON),
				`"alt_titles"`,
			)
			require.NotContains(
				t,
				string(rerankRequest.ModelInputJSON),
				`"parsed_title"`,
			)
		})
	}
}
