package llmstage

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/llmprovider"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestLegacyTypeBackendPreservesFullRequestBytes(t *testing.T) {
	const prefix = `{"model":"synthetic-model","messages":[{"role":"system","content":"You classify torrents by media type from the title and file list. Reply ONLY with compact JSON: {\"category\":\"movie|tv|music|audiobook|book|unknown\",\"confidence\":0.0-1.0}. Confidence is your self-assessment in [0,1]. Use \"unknown\" when unsure — do not guess."},{"role":"user","content":"prompt_version: synthetic-v1\ntitle: Synthetic.2031.mkv\ntotal_size_bytes: 1073741824\nfile_count: 0\nfiles:\n"}],"response_format":{"type":"json_object"},"max_completion_tokens":64`
	torrent := model.Torrent{Name: "Synthetic.2031.mkv", Size: 1073741824}
	for _, backend := range []llmprovider.ChatBackend{"", llmprovider.ChatBackendOpenAI} {
		cfg := NewDefaultConfig()
		cfg.ChatBackend, cfg.Model, cfg.PromptVersion = backend, "synthetic-model", "synthetic-v1"
		require.Equal(t, prefix+`}`, string(buildBoundedRequestBody(cfg, torrent)))
		cfg.OpenrouterProvider = "azure/us"
		require.Equal(t, prefix+`,"provider":{"order":["azure/us"],"only":["azure/us"],"allow_fallbacks":false,"require_parameters":true,"data_collection":"deny","zdr":true}}`, string(buildBoundedRequestBody(cfg, torrent)))
		cfg.OpenrouterProvider, cfg.OpenaiDataSharing = "", true
		require.Equal(t, prefix+`,"reasoning_effort":"none","store":false}`, string(buildBoundedRequestBody(cfg, torrent)))
	}
}

func newOllamaStage(t *testing.T, handler http.HandlerFunc) *Stage {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cfg := NewDefaultConfig()
	cfg.Enabled, cfg.ChatBackend, cfg.Endpoint, cfg.APIKey = true, llmprovider.ChatBackendOllama, srv.URL+"/v1/chat/completions", "synthetic-local-only"
	return NewStage(cfg, fakeInner{err: classification.ErrUnmatched}, fakePrivacy{}, NewMetrics(), zap.NewNop().Sugar(), Admission{Budget: &testBudget{}, Capture: &testAudit{}})
}

func TestOllamaTypeNativeRequestCaptureShadowAndCache(t *testing.T) {
	var wire [][]byte
	s := newOllamaStage(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/chat/completions", r.URL.Path)
		require.Equal(t, "Bearer synthetic-local-only", r.Header.Get("Authorization"))
		raw, _ := io.ReadAll(r.Body)
		wire = append(wire, raw)
		respondWith(w, "movie", .99)
	})
	audit := s.admission.Capture.(*testAudit)
	budget := s.admission.Budget.(*testBudget)
	for i := 0; i < 2; i++ {
		res, err := s.Run(context.Background(), "", nil, baseTorrent())
		require.ErrorIs(t, err, classification.ErrUnmatched)
		require.False(t, res.ContentType.Valid, "new dialect does not enable live classification")
		require.Nil(t, res.Content)
	}
	require.Len(t, wire, 1)
	require.Len(t, audit.requests, 1)
	require.Len(t, audit.results, 1)
	require.Len(t, audit.decisions, 2)
	require.EqualValues(t, 1, budget.used.Load())
	require.Equal(t, wire[0], []byte(audit.requests[0].ModelInputJSON))
	require.Equal(t, "classifier-type-v4-ollama-chat", audit.requests[0].ContractID)
	var task, body map[string]any
	require.NoError(t, json.Unmarshal(audit.requests[0].TaskInputJSON, &task))
	require.Equal(t, "ollama", task["chat_backend"])
	require.NoError(t, json.Unmarshal(wire[0], &body))
	require.EqualValues(t, 64, body["max_tokens"])
	require.Equal(t, "none", body["reasoning_effort"])
	for _, key := range []string{"max_completion_tokens", "provider", "store", "temperature", "think"} {
		require.NotContains(t, body, key)
	}
	s.cfg.ChatBackend = llmprovider.ChatBackendOpenAI
	_, _ = s.Run(context.Background(), "", nil, baseTorrent())
	require.Len(t, wire, 2, "full request cache binds the explicit dialect")
	require.EqualValues(t, 2, budget.used.Load())
	require.Contains(t, string(wire[1]), `"max_completion_tokens":64`)
	require.NotContains(t, string(wire[1]), `"max_tokens"`)
	require.Equal(t, "classifier-type-v1", audit.requests[1].ContractID)
}

func TestOllamaTypeKeepsAdmissionGuards(t *testing.T) {
	for _, tc := range []string{"unknown_backend", "paid_host", "provider_pin", "data_sharing", "missing_key", "native_private", "evidence_private", "evidence_error", "zero_budget", "capture_error", "live_low_confidence"} {
		t.Run(tc, func(t *testing.T) {
			var calls atomic.Int32
			s := newOllamaStage(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); respondWith(w, "movie", .1) })
			audit := s.admission.Capture.(*testAudit)
			torrent := baseTorrent()
			switch tc {
			case "unknown_backend":
				s.cfg.ChatBackend = "unknown"
			case "paid_host":
				s.cfg.Endpoint = "https://API.OPENAI.COM./v1/chat/completions"
			case "provider_pin":
				s.cfg.OpenrouterProvider = "azure/us"
			case "data_sharing":
				s.cfg.OpenaiDataSharing = true
			case "missing_key":
				s.cfg.APIKey = ""
			case "native_private":
				torrent.Private = true
			case "evidence_private":
				s.privacy = fakePrivacy{isPriv: true}
			case "evidence_error":
				s.privacy = fakePrivacy{err: errFake}
			case "zero_budget":
				s.cfg.DailyCallLimit = 0
			case "capture_error":
				audit.captureErr = errFake
			case "live_low_confidence":
				s.cfg.EnableLive = true
			}
			res, err := s.Run(context.Background(), "", nil, torrent)
			require.ErrorIs(t, err, classification.ErrUnmatched)
			require.False(t, res.ContentType.Valid)
			require.Nil(t, res.Content)
			if tc == "live_low_confidence" {
				require.EqualValues(t, 1, calls.Load())
			} else {
				require.Zero(t, calls.Load())
			}
			if tc != "capture_error" && tc != "live_low_confidence" {
				require.Empty(t, audit.requests)
				require.Zero(t, s.admission.Budget.(*testBudget).used.Load())
			}
		})
	}
}

func TestOllamaTypeRedirectIsCapturedWithoutFollowing(t *testing.T) {
	var sinkCalls atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sinkCalls.Add(1); respondWith(w, "movie", .99) }))
	defer sink.Close()
	s := newOllamaStage(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL+"/v1/chat/completions", http.StatusTemporaryRedirect)
	})
	res, err := s.Run(context.Background(), "", nil, baseTorrent())
	require.ErrorIs(t, err, classification.ErrUnmatched)
	require.False(t, res.ContentType.Valid)
	require.Zero(t, sinkCalls.Load())
	audit := s.admission.Capture.(*testAudit)
	require.Len(t, audit.results, 1)
	require.Equal(t, http.StatusTemporaryRedirect, audit.results[0].StatusCode)
	require.Equal(t, "http_status", audit.results[0].ErrorClass)
	require.Empty(t, audit.decisions)
}
