package llmmatch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/llmprovider"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestLegacyMatcherBackendPreservesFullRequestBytes(t *testing.T) {
	for _, backend := range []llmprovider.ChatBackend{"", llmprovider.ChatBackendOpenAI} {
		cfg := NewDefaultConfig()
		cfg.ChatBackend, cfg.Model = backend, "synthetic-model"
		raw, err := EvaluationConfiguredChatRequestJSON(cfg, "policy", "input", 120)
		require.NoError(t, err)
		require.Equal(t, `{"model":"synthetic-model","messages":[{"role":"system","content":"policy"},{"role":"user","content":"input\n\n/no_think"}],"response_format":{"type":"json_object"},"max_completion_tokens":120}`, string(raw))
		cfg.Enabled, cfg.Endpoint, cfg.Model, cfg.OpenrouterProvider = true, "https://openrouter.ai/api/v1/chat/completions", "openai/synthetic-model", "azure/us"
		raw, err = EvaluationConfiguredChatRequestJSON(cfg, "policy", "input", 120)
		require.NoError(t, err)
		require.Equal(t, `{"model":"openai/synthetic-model","messages":[{"role":"system","content":"policy"},{"role":"user","content":"input\n\n/no_think"}],"response_format":{"type":"json_object"},"max_completion_tokens":120,"provider":{"order":["azure/us"],"only":["azure/us"],"allow_fallbacks":false,"require_parameters":true,"data_collection":"deny","zdr":true}}`, string(raw))
		cfg.Endpoint, cfg.Model, cfg.APIKey, cfg.OpenrouterProvider, cfg.OpenaiDataSharing = llmprovider.OpenAIChatEndpoint, llmprovider.OpenAIDataSharingModel, "synthetic-key", "", true
		raw, err = EvaluationConfiguredChatRequestJSON(cfg, "policy", "input", 120)
		require.NoError(t, err)
		require.Equal(t, `{"model":"gpt-5.6-sol","messages":[{"role":"system","content":"policy"},{"role":"user","content":"input"}],"response_format":{"type":"json_object"},"max_completion_tokens":120,"reasoning_effort":"none","store":false}`, string(raw))
	}
}

func TestOllamaMatcherNativeRequestsAndCaptureShareExactBytes(t *testing.T) {
	var dispatched [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		require.Equal(t, "/v1/chat/completions", req.URL.Path)
		require.Empty(t, req.Header.Get("Authorization"))
		raw, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		dispatched = append(dispatched, raw)
		content := `{"title":"Glass Acacia","year":2031,"type":"movie","is_anime":false,"is_pack":false,"is_adult":false}`
		if len(dispatched) == 2 {
			content = `{"tmdb_id":7300101,"confidence":0.95}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": content}}}})
	}))
	defer srv.Close()
	cfg := NewDefaultConfig()
	cfg.Enabled, cfg.ChatBackend, cfg.Endpoint = true, llmprovider.ChatBackendOllama, srv.URL+"/v1/chat/completions"
	probe := &resultCaptureProbe{first: true}
	c := NewClientWithCapture(cfg, &privacyProbe{}, NewMetrics(), zap.NewNop().Sugar(), probe)
	torrent := mediaTorrent("Glass.Acacia.2031.1080p.mkv")
	ext, err := c.Extract(context.Background(), torrent)
	require.NoError(t, err)
	id, _, err := c.RerankForMediaType(context.Background(), torrent, ext, "Glass Acacia", false, []Candidate{{ID: 7300101, Title: "Glass Acacia", Year: 2031}}, llmcapture.CandidateSourceLocal)
	require.NoError(t, err)
	require.EqualValues(t, 7300101, id)
	require.Len(t, dispatched, 2)
	require.Len(t, probe.requests, 2)
	require.Len(t, probe.results, 2)
	for i, req := range probe.requests {
		var body map[string]any
		require.NoError(t, json.Unmarshal(dispatched[i], &body))
		require.EqualValues(t, []int{120, 60}[i], body["max_tokens"])
		require.Equal(t, "none", body["reasoning_effort"])
		for _, key := range []string{"max_completion_tokens", "provider", "store", "temperature", "think"} {
			require.NotContains(t, body, key)
		}
		require.Equal(t, dispatched[i], []byte(req.ModelInputJSON))
		require.True(t, strings.HasSuffix(req.ContractID, "-ollama-chat-v1"))
		var task map[string]any
		require.NoError(t, json.Unmarshal(req.TaskInputJSON, &task))
		require.Equal(t, "ollama", task["chat_backend"])
		expected, err := EvaluationConfiguredChatRequestJSON(cfg, req.SystemPrompt, strings.TrimSuffix(body["messages"].([]any)[1].(map[string]any)["content"].(string), "\n\n/no_think"), []int{120, 60}[i])
		require.NoError(t, err)
		require.Equal(t, expected, dispatched[i])
	}
	_, err = c.Extract(context.Background(), torrent)
	require.NoError(t, err)
	require.Len(t, dispatched, 2, "native same-dialect extraction cache is preserved")
}

func TestOllamaBatchAndSingleExtractionCachesBindBackend(t *testing.T) {
	var dispatched [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		dispatched = append(dispatched, raw)
		items := make([]map[string]any, 8)
		for i := range items {
			items[i] = map[string]any{"id": i + 1, "title": "Glass Acacia", "year": 2031, "type": "movie", "is_anime": false, "is_pack": false, "is_adult": false}
		}
		batchResponse, _ := json.Marshal(map[string]any{"items": items})
		content := string(batchResponse)
		if len(dispatched) > 1 {
			content = `{"title":"Glass Acacia","year":2031,"type":"movie","is_anime":false,"is_pack":false,"is_adult":false}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": content}}}})
	}))
	defer srv.Close()
	cfg := NewDefaultConfig()
	cfg.Enabled, cfg.ChatBackend, cfg.Endpoint = true, llmprovider.ChatBackendOllama, srv.URL+"/v1/chat/completions"
	c := NewClient(cfg, &privacyProbe{}, NewMetrics(), zap.NewNop().Sugar())
	torrent := mediaTorrent("Glass.Acacia.2031.1080p.mkv")
	torrents := []model.Torrent{torrent}
	for i := 1; i < 8; i++ {
		torrents = append(torrents, mediaTorrent(fmt.Sprintf("Synthetic.Movie.%d.2031.mkv", i)))
	}
	stats := c.ExtractMany(context.Background(), torrents, 8)
	require.Equal(t, 8, stats.OK)
	require.Equal(t, 1, stats.Batches)
	_, err := c.Extract(context.Background(), torrent)
	require.NoError(t, err)
	require.Len(t, dispatched, 2, "a group result cannot warm the single request contract")
	var batch map[string]any
	require.NoError(t, json.Unmarshal(dispatched[0], &batch))
	require.EqualValues(t, 1140, batch["max_tokens"])
	require.Equal(t, "none", batch["reasoning_effort"])
	require.NotContains(t, batch, "max_completion_tokens")
	c.cfg.ChatBackend = llmprovider.ChatBackendOpenAI
	_, err = c.Extract(context.Background(), torrent)
	require.NoError(t, err)
	require.Len(t, dispatched, 3, "a different dialect cannot reuse the single request contract")
	require.Contains(t, string(dispatched[2]), `"max_completion_tokens":120`)
}

func TestOllamaRerankCacheBindsBackend(t *testing.T) {
	srv, calls := chatServer(t, `{"tmdb_id":7300101,"confidence":0.95}`)
	cfg := NewDefaultConfig()
	cfg.Enabled, cfg.Endpoint, cfg.ChatBackend = true, srv.URL+"/v1/chat/completions", llmprovider.ChatBackendOllama
	c := NewClient(cfg, &privacyProbe{}, NewMetrics(), zap.NewNop().Sugar())
	torrent := mediaTorrent("Glass.Acacia.2031.mkv")
	ext := Extraction{Title: "Glass Acacia", Year: 2031, Type: "movie"}
	candidates := []Candidate{{ID: 7300101, Title: "Glass Acacia", Year: 2031}}
	_, _, err := c.Rerank(context.Background(), torrent, ext, candidates)
	require.NoError(t, err)
	c.cfg.ChatBackend = llmprovider.ChatBackendOpenAI
	_, _, err = c.Rerank(context.Background(), torrent, ext, candidates)
	require.NoError(t, err)
	require.EqualValues(t, 2, *calls)
}

func TestInvalidMatcherBackendStopsBeforeCaptureBudgetAndHTTP(t *testing.T) {
	for _, backend := range []llmprovider.ChatBackend{"unknown", llmprovider.ChatBackendOllama} {
		cfg := NewDefaultConfig()
		cfg.Enabled, cfg.ChatBackend, cfg.Endpoint = true, backend, "https://API.OPENAI.COM./v1/chat/completions"
		probe := &resultCaptureProbe{first: true}
		allowance := &capBudget{}
		allowance.left.Store(3)
		c := NewClientWithBudget(cfg, &privacyProbe{}, NewMetrics(), zap.NewNop().Sugar(), probe, allowance)
		c.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("invalid backend dispatched"); return nil, nil })
		_, err := c.Extract(context.Background(), mediaTorrent("Glass.Acacia.2031.mkv"))
		require.Error(t, err)
		require.Empty(t, probe.requests)
		require.EqualValues(t, 3, allowance.left.Load())
	}
}

func TestOllamaExtractManyWithCaptureKeepsIndependentExactRequests(t *testing.T) {
	var wire [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		wire = append(wire, raw)
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": `{"title":"Synthetic","year":2031,"type":"movie","is_anime":false,"is_pack":false,"is_adult":false}`}}}})
	}))
	defer srv.Close()
	cfg := NewDefaultConfig()
	cfg.Enabled, cfg.ChatBackend, cfg.Endpoint = true, llmprovider.ChatBackendOllama, srv.URL+"/v1/chat/completions"
	audit := &resultCaptureProbe{first: true}
	c := NewClientWithCapture(cfg, &privacyProbe{}, NewMetrics(), zap.NewNop().Sugar(), audit)
	torrents := make([]model.Torrent, 8)
	for i := range torrents {
		torrents[i] = mediaTorrent(fmt.Sprintf("Synthetic.Movie.%d.2031.mkv", i))
	}
	stats := c.ExtractMany(context.Background(), torrents, 8)
	require.Equal(t, 8, stats.OK)
	require.Equal(t, 8, stats.Singles)
	require.Zero(t, stats.Batches, "capture preserves independently replayable single requests")
	require.Len(t, audit.requests, 8)
	require.Len(t, audit.results, 8)
	for i, captured := range audit.requests {
		require.Equal(t, wire[i], []byte(captured.ModelInputJSON))
		require.Equal(t, "llmmatch-chat-extract-v1-ollama-chat-v1", captured.ContractID)
		var task map[string]any
		require.NoError(t, json.Unmarshal(captured.TaskInputJSON, &task))
		require.Equal(t, "ollama", task["chat_backend"])
		require.Equal(t, torrents[i].Name, task["release_name"])
		require.Contains(t, string(wire[i]), `"max_tokens":120,"reasoning_effort":"none"`)
	}
}

func TestOllamaMatcherRefusesOrdinaryAndLongRedirects(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for _, batch := range []bool{false, true} {
			t.Run(fmt.Sprintf("status_%d_batch_%t", status, batch), func(t *testing.T) {
				var sinkCalls atomic.Int32
				sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					sinkCalls.Add(1)
					_, _ = w.Write([]byte(`{"choices":[]}`))
				}))
				defer sink.Close()
				source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, sink.URL+"/v1/chat/completions", status)
				}))
				defer source.Close()
				cfg := NewDefaultConfig()
				cfg.Enabled, cfg.ChatBackend, cfg.Endpoint = true, llmprovider.ChatBackendOllama, source.URL+"/v1/chat/completions"
				audit := &resultCaptureProbe{first: true}
				c := NewClientWithCapture(cfg, &privacyProbe{}, NewMetrics(), zap.NewNop().Sugar(), audit)
				if batch {
					// This primitive uses the long client; ExtractMany capture mode
					// deliberately delegates to independently captured singles.
					c.capture = nil
					_, err := c.callBatchExtract(context.Background(), []model.Torrent{mediaTorrent("Synthetic.2031.mkv")})
					require.ErrorContains(t, err, fmt.Sprint(status))
				} else {
					_, err := c.Extract(context.Background(), mediaTorrent("Synthetic.2031.mkv"))
					require.ErrorContains(t, err, fmt.Sprint(status))
					require.Len(t, audit.results, 1)
					require.Equal(t, status, audit.results[0].StatusCode)
				}
				require.Zero(t, sinkCalls.Load(), "redirect cannot bypass the validated route")
			})
		}
	}
}
