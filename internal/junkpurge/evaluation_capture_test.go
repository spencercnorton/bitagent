package junkpurge

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/stretchr/testify/require"
)

type junkCaptureFake struct {
	requests []llmcapture.Request
	outcome  llmcapture.Outcome
	err      error
}

func (f *junkCaptureFake) Enabled() bool { return true }

func (f *junkCaptureFake) Capture(
	_ context.Context,
	request llmcapture.Request,
) (llmcapture.Outcome, error) {
	f.requests = append(f.requests, request)
	return f.outcome, f.err
}

func TestBuildBatchCaptureRequestsUsesExactPersistedBody(t *testing.T) {
	run := batchRun{
		Model:           "gpt-test",
		ProviderBaseURL: "https://api.openai.com/v1",
		Endpoint:        "/v1/chat/completions",
		PromptVersion:   "prompt-v1",
		SystemPrompt:    "system",
		MaxTokens:       17,
		ReasoningEffort: "low",
	}
	items := []batchItem{{
		CustomID:    "case-1",
		InfoHash:    make([]byte, 20),
		TorrentName: "Some.Show.S01E01",
	}}
	body, err := json.Marshal(
		buildChatRequestWithPolicy(
			run.Model,
			items[0].TorrentName,
			run.SystemPrompt,
			run.MaxTokens,
			run.ReasoningEffort,
		),
	)
	require.NoError(t, err)
	payload := append(
		[]byte(`{"custom_id":"case-1","method":"POST","url":"/v1/chat/completions","body":`),
		body...,
	)
	payload = append(payload, []byte("}\n")...)
	attempt := batchAttempt{Payload: payload, ItemCount: 1}

	requests, err := buildBatchCaptureRequests(run, attempt, items)
	require.NoError(t, err)
	require.Len(t, requests, 1)
	request := requests[0]
	require.Equal(t, llmcapture.TaskJunkPurge, request.Task)
	require.Equal(t, "https://api.openai.com/v1/chat/completions", request.Endpoint)
	require.JSONEq(t, string(body), string(request.ModelInputJSON))
	require.JSONEq(
		t,
		`{"torrent_name":"Some.Show.S01E01"}`,
		string(request.TaskInputJSON),
	)
	require.NotEmpty(t, request.GroupKey)
	require.Equal(t, junkBatchCaptureContractID, request.ContractID)
}

func TestBuildBatchCaptureRequestsRejectsPayloadMismatch(t *testing.T) {
	run := batchRun{
		Model:           "gpt-test",
		ProviderBaseURL: "https://api.openai.com/v1",
		Endpoint:        "/v1/chat/completions",
		PromptVersion:   "prompt-v1",
		SystemPrompt:    "system",
	}
	items := []batchItem{{
		CustomID:    "case-1",
		InfoHash:    make([]byte, 20),
		TorrentName: "Some.Show",
	}}
	attempt := batchAttempt{
		ItemCount: 1,
		Payload: []byte(
			`{"custom_id":"wrong","method":"POST","url":"/v1/chat/completions","body":{"model":"gpt-test"}}` + "\n",
		),
	}

	_, err := buildBatchCaptureRequests(run, attempt, items)
	require.ErrorContains(t, err, "does not match")
}

func TestHostedBatchEndpointRejectsUnsafeEndpoint(t *testing.T) {
	_, err := hostedBatchEndpoint(
		"https://api.openai.com/v1",
		"//attacker.example/chat",
	)
	require.Error(t, err)
}

func TestBuildBatchCaptureRequestsGroupsReleaseVariants(t *testing.T) {
	run := batchRun{
		Model:           "gpt-test",
		ProviderBaseURL: "https://api.openai.com/v1",
		Endpoint:        "/v1/chat/completions",
		PromptVersion:   "prompt-v1",
		SystemPrompt:    "system",
		MaxTokens:       17,
		ReasoningEffort: "low",
	}
	items := []batchItem{
		{
			CustomID:    "case-1",
			InfoHash:    make([]byte, 20),
			TorrentName: "Some.Movie.2024.2160p.BluRay-GROUP",
		},
		{
			CustomID:    "case-2",
			InfoHash:    append(make([]byte, 19), 1),
			TorrentName: "Some Movie (2024) 1080p WEB-DL-OTHER",
		},
	}
	var payload []byte
	for _, item := range items {
		body, err := json.Marshal(buildChatRequestWithPolicy(
			run.Model,
			item.TorrentName,
			run.SystemPrompt,
			run.MaxTokens,
			run.ReasoningEffort,
		))
		require.NoError(t, err)
		line, err := json.Marshal(capturedBatchLine{
			CustomID: item.CustomID,
			Method:   "POST",
			URL:      run.Endpoint,
			Body:     body,
		})
		require.NoError(t, err)
		payload = append(payload, line...)
		payload = append(payload, '\n')
	}
	requests, err := buildBatchCaptureRequests(
		run,
		batchAttempt{Payload: payload, ItemCount: len(items)},
		items,
	)
	require.NoError(t, err)
	require.Equal(t, requests[0].GroupKey, requests[1].GroupKey)
}

func TestCaptureSyncCandidateUsesExactHostedRequest(t *testing.T) {
	capture := &junkCaptureFake{outcome: llmcapture.OutcomeRecorded}
	cfg := NewDefaultConfig()
	cfg.LLMApiStyle = "chat"
	cfg.LLMBaseURL = "https://api.openai.com/v1"
	cfg.LLMModel = "gpt-test"
	worker := &purgeWorker{cfg: cfg, capture: capture}
	item := candidate{
		infoHash: make([]byte, 20),
		name:     "Some.Movie.2024.1080p.WEB-DL-GROUP",
	}

	require.NoError(
		t,
		worker.captureSyncCandidate(context.Background(), item),
	)
	require.Len(t, capture.requests, 1)
	request := capture.requests[0]
	require.Equal(t, llmcapture.TaskJunkPurge, request.Task)
	require.Equal(
		t,
		"https://api.openai.com/v1/chat/completions",
		request.Endpoint,
	)
	expectedBody, err := json.Marshal(buildChatRequest(cfg.LLMModel, item.name))
	require.NoError(t, err)
	require.JSONEq(t, string(expectedBody), string(request.ModelInputJSON))
	require.Equal(t, []byte("some movie"), request.GroupKey)
	require.Equal(
		t,
		llmcapture.SamplingOriginNaturalCapture,
		request.SamplingOrigin,
	)
}

func TestCaptureSyncSafetyTopUpUsesDistinctOriginAndSameExactBody(
	t *testing.T,
) {
	capture := &junkCaptureFake{outcome: llmcapture.OutcomeRecorded}
	cfg := NewDefaultConfig()
	cfg.LLMApiStyle = "chat"
	cfg.LLMBaseURL = "https://api.openai.com/v1"
	cfg.LLMModel = "gpt-test"
	worker := &purgeWorker{cfg: cfg, capture: capture}
	item := candidate{
		infoHash: make([]byte, 20),
		name:     "Definitely.Not.A.Movie.FitGirl.Repack",
	}

	require.NoError(t, worker.captureSyncCandidateWithOrigin(
		context.Background(),
		item,
		llmcapture.SamplingOriginSafetyTopUpCapture,
	))
	require.Len(t, capture.requests, 1)
	request := capture.requests[0]
	require.Equal(
		t,
		llmcapture.SamplingOriginSafetyTopUpCapture,
		request.SamplingOrigin,
	)
	expectedBody, err := json.Marshal(buildChatRequest(cfg.LLMModel, item.name))
	require.NoError(t, err)
	require.JSONEq(t, string(expectedBody), string(request.ModelInputJSON))
}

func TestCaptureSyncCandidateFailsClosedOnUnexpectedOutcome(t *testing.T) {
	capture := &junkCaptureFake{outcome: llmcapture.OutcomeDisabled}
	cfg := NewDefaultConfig()
	cfg.LLMApiStyle = "chat"
	worker := &purgeWorker{cfg: cfg, capture: capture}

	err := worker.captureSyncCandidate(context.Background(), candidate{
		infoHash: make([]byte, 20),
		name:     "Some Movie",
	})
	require.ErrorIs(t, err, llmcapture.ErrCaptureUnavailable)
}
