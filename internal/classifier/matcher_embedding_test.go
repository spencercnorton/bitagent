package classifier

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
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

type workflowEmbeddingCapture struct {
	mu              sync.Mutex
	requests        []llmcapture.Request
	decisions       []llmcapture.MatchDecision
	decisionReceipt llmcapture.ResultReceipt
	decisionErr     error
}

func (*workflowEmbeddingCapture) Enabled() bool { return true }
func (p *workflowEmbeddingCapture) Capture(_ context.Context, request llmcapture.Request) (llmcapture.Outcome, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, request)
	return llmcapture.OutcomeRecorded, nil
}
func (*workflowEmbeddingCapture) RecheckEmbeddingRequest(context.Context, []byte, []byte) error {
	return nil
}
func (*workflowEmbeddingCapture) RecordHTTPResult(_ context.Context, key []byte, result llmcapture.HTTPResult) (llmcapture.ResultReceipt, error) {
	digest := sha256.Sum256(result.Body)
	return llmcapture.ResultReceipt{CaptureKey: key, ResponseSHA256: digest[:], FirstObservation: true, StatusCode: result.StatusCode, ErrorClass: result.ErrorClass}, nil
}
func (p *workflowEmbeddingCapture) RecordMatchDecision(_ context.Context, receipt llmcapture.ResultReceipt, _ []byte, decision llmcapture.MatchDecision) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.decisions = append(p.decisions, decision)
	p.decisionReceipt = receipt
	return p.decisionErr
}

func TestEmbeddingWorkflowRetainsFinalMatchAuthority(t *testing.T) {
	for _, kind := range []string{"live_movie", "shadow", "low_confidence", "removed_ambiguous_candidate", "audit_failure", "live_tv_tuple"} {
		t.Run(kind, func(t *testing.T) {
			var embeddingCalls, chatCalls atomic.Int32
			confidence := .99
			if kind == "low_confidence" {
				confidence = .01
			}
			mediaType, title, name := "movie", "Sample Film", "Sample.Film.2020.1080p.mkv"
			contentType := model.ContentTypeMovie
			if kind == "live_tv_tuple" {
				mediaType, title, name = "tv", "Sample Show", "Sample.Show.S01E02.1080p.mkv"
				contentType = model.ContentTypeTvShow
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/embeddings" {
					embeddingCalls.Add(1)
					_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[1,0]},{"index":1,"embedding":[1,0]},{"index":2,"embedding":[0.8,0.6]},{"index":3,"embedding":[0,1]}]}`)
					return
				}
				call := chatCalls.Add(1)
				answer := map[string]any{"tmdb_id": 101, "confidence": confidence}
				if call == 1 {
					answer = map[string]any{"title": title, "type": mediaType, "year": 2020, "season": 9, "episode": 99, "is_anime": false, "is_pack": false, "is_adult": false}
				}
				encoded, err := json.Marshal(answer)
				require.NoError(t, err)
				require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": string(encoded)}}}}))
			}))
			defer server.Close()
			probe := &workflowEmbeddingCapture{}
			if kind == "audit_failure" {
				probe.decisionErr = errors.New("final ledger unavailable")
			}
			cfg := llmmatch.NewDefaultConfig()
			cfg.Enabled = true
			cfg.EnableLive = kind != "shadow"
			cfg.Endpoint, cfg.Model = server.URL+"/chat", "synthetic-chat"
			cfg.MinTotalSizeBytes = 0
			cfg.RequireSourceTitle = true
			cfg.Embeddings.Enabled, cfg.Embeddings.Endpoint, cfg.Embeddings.Model = true, server.URL+"/v1/embeddings", "synthetic-embedding"
			cfg.Embeddings.Dimensions, cfg.Embeddings.ShortlistSize = 2, 2
			client := llmmatch.NewClientWithCapture(cfg, nil, llmmatch.NewMetrics(), zap.NewNop().Sugar(), probe)
			contents := []model.Content{
				{Type: contentType, Source: model.SourceTmdb, ID: "101", Title: title, ReleaseYear: 2020},
				{Type: contentType, Source: model.SourceTmdb, ID: "102", Title: title + " Two", ReleaseYear: 2021},
				{Type: contentType, Source: model.SourceTmdb, ID: "103", Title: "Other Title", ReleaseYear: 2010},
			}
			if kind == "removed_ambiguous_candidate" {
				contents[2].Title = title
				contents[2].ReleaseYear = 2020
			}
			search := classifier_mocks.NewLocalSearch(t)
			search.On("ContentBySearch", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(model.Content{}, classification.ErrUnmatched).Maybe()
			search.On("ContentCandidatesBySearch", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(contents, nil).Once()
			tmdbClient := tmdb_mocks.NewClient(t)
			tmdbClient.On("SearchMovie", mock.Anything, mock.Anything).Return(tmdb.SearchMovieResponse{}, nil).Maybe()
			tmdbClient.On("SearchTv", mock.Anything, mock.Anything).Return(tmdb.SearchTvResponse{}, nil).Maybe()
			comp := compiler{options: []compilerOption{compilerFeatures(defaultFeatures), celEnvOption}, dependencies: dependencies{
				search: search, tmdbClient: tmdbClient, llmMatch: client, matchDecisionObserver: NewMatchDecisionObserver(probe, client),
			}}
			source, err := yamlSourceProvider{rawSourceProvider: coreSourceProvider{}}.source()
			require.NoError(t, err)
			workflow, err := comp.Compile(source)
			require.NoError(t, err)
			torrent := model.Torrent{Name: name, Size: 2_000_000_000, FilesStatus: model.FilesStatusSingle, Extension: model.NewNullString("mkv")}
			baseline, err := workflow.Run(context.Background(), "default", Flags{"llm_match_enabled": false}, torrent)
			require.NoError(t, err)
			result, err := workflow.Run(context.Background(), "default", Flags{"llm_match_enabled": true}, torrent)
			if kind == "audit_failure" {
				require.Error(t, err)
				require.Nil(t, result.Content)
				require.NotContains(t, result.Tags, llmMatchedTagName)
			} else {
				require.NoError(t, err)
			}
			require.EqualValues(t, 1, embeddingCalls.Load())
			require.EqualValues(t, 2, chatCalls.Load())
			require.Len(t, probe.decisions, 1)
			decision := probe.decisions[0]
			require.Equal(t, confidence, decision.Confidence)
			switch kind {
			case "live_movie", "live_tv_tuple":
				require.NotNil(t, result.Content)
				require.Equal(t, "101", result.Content.ID)
				require.Contains(t, result.Tags, llmMatchedTagName)
				require.True(t, decision.WouldAttach)
				require.True(t, decision.Live)
				if kind == "live_tv_tuple" {
					require.Equal(t, baseline.Episodes, result.Episodes)
					require.NotEmpty(t, result.Episodes, "source TV tuple must remain authoritative over model's 9x99")
				}
			case "shadow":
				require.Equal(t, baseline, result)
				require.True(t, decision.WouldAttach)
				require.False(t, decision.Live)
			case "low_confidence":
				require.Equal(t, baseline, result)
				require.Equal(t, "confidence", decision.GateReason)
				require.False(t, decision.WouldAttach)
			case "removed_ambiguous_candidate":
				require.Equal(t, baseline, result)
				require.Equal(t, "candidate_ambiguous", decision.GateReason)
				require.False(t, decision.WouldAttach)
			}
			require.Len(t, probe.requests, 3)
			require.Equal(t, llmcapture.TaskMatcherRerank, probe.requests[2].Task)
			key, err := llmcapture.KeyForRequest(probe.requests[2])
			require.NoError(t, err)
			require.Equal(t, key, probe.decisionReceipt.CaptureKey, "final decision must bind actual chat response, never cosine")
		})
	}
}
