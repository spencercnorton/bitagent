package llmmatch

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type captureProbe struct {
	err      error
	requests []llmcapture.Request
}

func (*captureProbe) Enabled() bool { return true }

func (*captureProbe) RecordHTTPResult(_ context.Context, key []byte, result llmcapture.HTTPResult) (llmcapture.ResultReceipt, error) {
	digest := sha256.Sum256(result.Body)
	return llmcapture.ResultReceipt{CaptureKey: key, ResponseSHA256: digest[:], FirstObservation: true, StatusCode: result.StatusCode, ErrorClass: result.ErrorClass}, nil
}

func (*captureProbe) RecordMatchDecision(context.Context, llmcapture.ResultReceipt, []byte, llmcapture.MatchDecision) error {
	return nil
}

func (p *captureProbe) Capture(
	_ context.Context,
	req llmcapture.Request,
) (llmcapture.Outcome, error) {
	p.requests = append(p.requests, req)
	return llmcapture.OutcomeRecorded, p.err
}

func matcherClientWithCapture(
	endpoint string,
	capture llmcapture.Capturer,
) *Client {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.Endpoint = endpoint
	cfg.MinTotalSizeBytes = 0
	return NewClientWithCapture(
		cfg,
		nil,
		NewMetrics(),
		zap.NewNop().Sugar(),
		capture,
	)
}

func TestExtractCaptureIsExactAndCacheMissOnly(t *testing.T) {
	srv, calls := chatServer(
		t,
		`{"title":"Dune","year":2021,"type":"movie","is_anime":false,"is_pack":false,"is_adult":false}`,
	)
	probe := &captureProbe{}
	client := matcherClientWithCapture(srv.URL, probe)
	torrent := mediaTorrent("Dune.2021.1080p.mkv")

	_, err := client.Extract(context.Background(), torrent)
	require.NoError(t, err)
	_, err = client.Extract(context.Background(), torrent)
	require.NoError(t, err)
	require.Len(t, probe.requests, 1)
	assert.Equal(t, int32(1), atomic.LoadInt32(calls))

	req := probe.requests[0]
	assert.Equal(t, llmcapture.TaskMatcherExtract, req.Task)
	assert.Equal(t, ExtractPrompt(), req.SystemPrompt)
	var taskInput struct {
		ReleaseName string   `json:"release_name"`
		FilePaths   []string `json:"file_paths"`
	}
	require.NoError(t, json.Unmarshal(req.TaskInputJSON, &taskInput))
	assert.Equal(t, torrent.Name, taskInput.ReleaseName)
	assert.Empty(t, taskInput.FilePaths)
	var modelInput chatRequest
	require.NoError(t, json.Unmarshal(req.ModelInputJSON, &modelInput))
	assert.Equal(t, client.cfg.Model, modelInput.Model)
	assert.Equal(t, ExtractPrompt(), modelInput.Messages[0].Content)
	assert.Equal(
		t,
		ExtractInput(torrent.Name, nil)+"\n\n/no_think",
		modelInput.Messages[1].Content,
	)
	assert.NotContains(t, string(req.TaskInputJSON), "openrouter_provider")
}

func TestOpenRouterCaptureFreezesProviderPinAsAuditEvidence(t *testing.T) {
	const (
		endpoint = "https://openrouter.ai/api/v1/chat/completions"
		provider = "azure/us"
	)
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.Endpoint = endpoint
	cfg.Model = "openai/gpt-5.4-nano"
	cfg.OpenrouterProvider = provider
	probe := &captureProbe{}
	client := NewClientWithCapture(
		cfg,
		nil,
		NewMetrics(),
		zap.NewNop().Sugar(),
		probe,
	)
	torrent := mediaTorrent("Dune.2021.1080p.mkv")

	for _, tc := range []struct {
		task       llmcapture.Task
		source     llmcapture.CandidateSource
		contractID string
		system     string
		user       string
		maxTokens  int
		taskInput  map[string]any
	}{
		{
			task:       llmcapture.TaskMatcherExtract,
			contractID: "llmmatch-chat-extract-v1",
			system:     ExtractPrompt(),
			user:       ExtractInput(torrent.Name, nil),
			maxTokens:  120,
			taskInput: map[string]any{
				"release_name": torrent.Name,
				"file_paths":   []string{},
				"admission": map[string]any{
					"native_private":  false,
					"size_bytes":      torrent.Size,
					"file_count":      len(torrent.Files),
					"media_plausible": true,
				},
			},
		},
		{
			task:       llmcapture.TaskMatcherRerank,
			source:     llmcapture.CandidateSourceLocal,
			contractID: "llmmatch-chat-rerank-v2",
			system:     RerankPrompt(),
			user: RerankInput(
				torrent.Name,
				Extraction{Title: "Dune", Year: 2021, Type: "movie"},
				[]Candidate{{ID: 438631, Title: "Dune", Year: 2021}},
			),
			maxTokens: 60,
			taskInput: map[string]any{
				"release_name": torrent.Name,
				"parsed_title": "Dune",
				"extraction": Extraction{
					Title: "Dune", Year: 2021, Type: "movie",
				},
				"effective_media_type": "movie",
				"candidates": []rerankCaptureCandidate{{
					ID: 438631, Title: "Dune", Year: 2021,
				}},
			},
		},
	} {
		require.NoError(t, client.captureRequest(
			context.Background(),
			torrent,
			tc.task,
			tc.source,
			tc.contractID,
			[]byte("group"),
			tc.system,
			tc.user,
			tc.maxTokens,
			tc.taskInput,
		))
	}

	require.Len(t, probe.requests, 2)
	for index, req := range probe.requests {
		var taskInput map[string]any
		require.NoError(t, json.Unmarshal(req.TaskInputJSON, &taskInput))
		require.Equal(t, provider, taskInput["openrouter_provider"])
		expected, err := EvaluationConfiguredChatRequestJSON(
			cfg,
			[]string{ExtractPrompt(), RerankPrompt()}[index],
			[]string{
				ExtractInput(torrent.Name, nil),
				RerankInput(
					torrent.Name,
					Extraction{Title: "Dune", Year: 2021, Type: "movie"},
					[]Candidate{{ID: 438631, Title: "Dune", Year: 2021}},
				),
			}[index],
			[]int{120, 60}[index],
		)
		require.NoError(t, err)
		require.JSONEq(t, string(expected), string(req.ModelInputJSON))
	}
}

func TestCaptureFailurePreventsMatcherProviderCall(t *testing.T) {
	srv, calls := chatServer(
		t,
		`{"title":"Never Sent","year":2026,"type":"movie","is_anime":false,"is_pack":false,"is_adult":false}`,
	)
	probe := &captureProbe{err: errors.New("capture database down")}
	client := matcherClientWithCapture(srv.URL, probe)

	_, err := client.Extract(
		context.Background(),
		mediaTorrent("Never.Sent.2026.mkv"),
	)
	require.Error(t, err)
	assert.Zero(t, atomic.LoadInt32(calls))
}

func TestRerankCaptureFreezesOrderedCandidatesAndSource(t *testing.T) {
	srv, calls := chatServer(t, `{"tmdb_id":2,"confidence":0.9}`)
	probe := &captureProbe{}
	client := matcherClientWithCapture(srv.URL, probe)
	parsedTitle := "Independent Source Alias"
	candidates := []Candidate{
		{ID: 1, Title: "Same", Year: 1980, Overview: "first"},
		{
			ID: 2, Title: "Same", Year: 2020, Overview: "second",
			AltTitles: []string{parsedTitle},
		},
	}
	torrent := mediaTorrent("Same.2020.mkv")
	extraction := Extraction{Title: "Same", Year: 2020, Type: "movie"}
	_, _, err := client.RerankForMediaType(
		context.Background(),
		torrent,
		extraction,
		parsedTitle,
		false,
		candidates,
		llmcapture.CandidateSourceLocal,
	)
	require.NoError(t, err)
	require.Len(t, probe.requests, 1)
	assert.Equal(t, int32(1), atomic.LoadInt32(calls))
	req := probe.requests[0]
	assert.Equal(t, llmcapture.TaskMatcherRerank, req.Task)
	assert.Equal(t, llmcapture.CandidateSourceLocal, req.CandidateSource)
	assert.Equal(t, "llmmatch-chat-rerank-v2", req.ContractID)
	var taskInput struct {
		ParsedTitle        string `json:"parsed_title"`
		EffectiveMediaType string `json:"effective_media_type"`
		Candidates         []struct {
			ID        int64    `json:"tmdb_id"`
			Title     string   `json:"title"`
			Year      int      `json:"year"`
			Overview  string   `json:"overview"`
			AltTitles []string `json:"alt_titles"`
		} `json:"candidates"`
	}
	require.NoError(t, json.Unmarshal(req.TaskInputJSON, &taskInput))
	assert.Equal(t, parsedTitle, taskInput.ParsedTitle)
	assert.Equal(t, "movie", taskInput.EffectiveMediaType)
	require.Len(t, taskInput.Candidates, 2)
	assert.Equal(t, candidates[1].ID, taskInput.Candidates[1].ID)
	assert.Equal(t, candidates[1].Title, taskInput.Candidates[1].Title)
	assert.Equal(t, candidates[1].Year, taskInput.Candidates[1].Year)
	assert.Equal(t, candidates[1].Overview, taskInput.Candidates[1].Overview)
	assert.Equal(t, candidates[1].AltTitles, taskInput.Candidates[1].AltTitles)

	// Parser evidence and catalogue aliases are audit-only. The exact provider
	// body remains the ordinary rerank prompt built from Candidate, whose
	// AltTitles field is deliberately excluded from the wire contract.
	expectedModelInput, err := json.Marshal(client.newChatRequest(
		RerankPrompt(), RerankInput(torrent.Name, extraction, candidates), 60,
	))
	require.NoError(t, err)
	assert.Equal(t, expectedModelInput, []byte(req.ModelInputJSON))
	assert.NotContains(t, string(req.ModelInputJSON), parsedTitle)
	assert.NotContains(t, string(req.ModelInputJSON), `"parsed_title"`)
	assert.NotContains(t, string(req.ModelInputJSON), `"alt_titles"`)
}

func TestRerankCaptureSeparatesActualLocalAndAPICalls(t *testing.T) {
	srv, calls := chatServer(
		t,
		`{"tmdb_id":0,"confidence":0}`,
		`{"tmdb_id":2,"confidence":0.9}`,
	)
	probe := &captureProbe{}
	client := matcherClientWithCapture(srv.URL, probe)
	torrent := mediaTorrent("Same.2020.mkv")
	extraction := Extraction{Title: "Same", Year: 2020, Type: "movie"}

	_, _, err := client.RerankFrom(
		context.Background(),
		torrent,
		extraction,
		false,
		[]Candidate{{ID: 1, Title: "Same", Year: 1980}},
		llmcapture.CandidateSourceLocal,
	)
	require.NoError(t, err)
	_, _, err = client.RerankFrom(
		context.Background(),
		torrent,
		extraction,
		false,
		[]Candidate{{ID: 2, Title: "Same", Year: 2020}},
		llmcapture.CandidateSourceAPI,
	)
	require.NoError(t, err)

	require.Len(t, probe.requests, 2)
	var legacyTaskInput struct {
		ParsedTitle string `json:"parsed_title"`
	}
	require.NoError(
		t,
		json.Unmarshal(probe.requests[0].TaskInputJSON, &legacyTaskInput),
	)
	assert.Empty(t, legacyTaskInput.ParsedTitle)
	assert.Equal(
		t,
		llmcapture.CandidateSourceLocal,
		probe.requests[0].CandidateSource,
	)
	assert.Equal(
		t,
		llmcapture.CandidateSourceAPI,
		probe.requests[1].CandidateSource,
	)
	assert.Equal(t, int32(2), atomic.LoadInt32(calls))
}
