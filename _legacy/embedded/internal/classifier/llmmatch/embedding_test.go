package llmmatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type embeddingCaptureProbe struct {
	mu                                sync.Mutex
	requests                          []llmcapture.Request
	results                           []llmcapture.HTTPResult
	keys                              [][]byte
	captureErr, resultErr, recheckErr error
	duplicate                         bool
	captureDuplicate                  bool
	rechecks                          int
}

func (*embeddingCaptureProbe) Enabled() bool { return true }
func (p *embeddingCaptureProbe) Capture(_ context.Context, request llmcapture.Request) (llmcapture.Outcome, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, request)
	if p.captureDuplicate {
		return llmcapture.OutcomeDuplicate, p.captureErr
	}
	return llmcapture.OutcomeRecorded, p.captureErr
}
func (p *embeddingCaptureProbe) RecheckEmbeddingRequest(_ context.Context, key, infoHash []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rechecks++
	if len(key) != 32 || len(infoHash) != 20 {
		return errors.New("missing bound admission")
	}
	return p.recheckErr
}
func (p *embeddingCaptureProbe) RecordHTTPResult(_ context.Context, key []byte, result llmcapture.HTTPResult) (llmcapture.ResultReceipt, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.results = append(p.results, result)
	p.keys = append(p.keys, key)
	digest := sha256.Sum256(result.Body)
	return llmcapture.ResultReceipt{CaptureKey: key, ResponseSHA256: digest[:],
		FirstObservation: !p.duplicate, StatusCode: result.StatusCode, ErrorClass: result.ErrorClass}, p.resultErr
}
func (*embeddingCaptureProbe) RecordMatchDecision(context.Context, llmcapture.ResultReceipt, []byte, llmcapture.MatchDecision) error {
	return nil
}

type embeddingBudgetProbe struct{ attempts, used atomic.Int32 }

func (b *embeddingBudgetProbe) Reserve(ctx context.Context, daily, monthly int) (bool, error) {
	b.attempts.Add(1)
	if err := ctx.Err(); err != nil {
		return false, err
	}
	for {
		used := b.used.Load()
		if used >= int32(min(daily, monthly)) {
			return false, nil
		}
		if b.used.CompareAndSwap(used, used+1) {
			return true, nil
		}
	}
}

func embeddingTestClient(endpoint string, capture llmcapture.Capturer, budget CallBudget) *Client {
	cfg := NewDefaultConfig()
	cfg.Enabled, cfg.EnableLive = true, true
	cfg.Endpoint, cfg.APIKey, cfg.Model = endpoint+"/chat", "chat-secret", "synthetic-chat"
	cfg.MinTotalSizeBytes = 0
	cfg.Embeddings.Enabled, cfg.Embeddings.Endpoint = true, endpoint+"/v1/embeddings"
	cfg.Embeddings.Model, cfg.Embeddings.APIKey = "synthetic-embedding", "embedding-secret"
	cfg.Embeddings.Dimensions, cfg.Embeddings.ShortlistSize = 2, 2
	return NewClientWithBudget(cfg, nil, NewMetrics(), zap.NewNop().Sugar(), capture, budget)
}

func embeddingTestCandidates() []Candidate {
	return []Candidate{
		{ID: 101, Title: "Sample Film", Year: 2020, AltTitles: []string{"Independent Alias Never Sent"}},
		{ID: 102, Title: "Sample Film Two", Year: 2021},
		{ID: 103, Title: "Other Film", Year: 2010},
	}
}

const embeddingTestResponse = `{"data":[{"index":3,"embedding":[-1,0]},{"index":1,"embedding":[0,1]},{"index":0,"embedding":[1,0]},{"index":2,"embedding":[1,0]}],"usage":{"prompt_tokens":42}}`

func TestEmbeddingShortlistUsesIndependentRouteExactCaptureAndChatConfidence(t *testing.T) {
	var embeddingCalls, chatCalls atomic.Int32
	var embeddingBody, chatBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		if request.URL.Path == "/v1/embeddings" {
			embeddingCalls.Add(1)
			embeddingBody = body
			require.Equal(t, "Bearer embedding-secret", request.Header.Get("Authorization"))
			_, _ = io.WriteString(w, embeddingTestResponse)
		} else {
			chatCalls.Add(1)
			chatBody = body
			require.Equal(t, "Bearer chat-secret", request.Header.Get("Authorization"))
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"tmdb_id\":102,\"confidence\":0.31}"}}]}`)
		}
	}))
	defer server.Close()
	probe, budget := &embeddingCaptureProbe{}, &embeddingBudgetProbe{}
	client := embeddingTestClient(server.URL, probe, budget)
	candidates := embeddingTestCandidates()
	ctx, trace := llmcapture.WithResultTrace(context.Background())
	id, confidence, err := client.RerankForMediaType(ctx, mediaTorrent("Sample.Film.2020.mkv"),
		Extraction{Title: "Sample Film", Type: "movie", Year: 2020}, "Parser Alias Never Sent", false,
		candidates, llmcapture.CandidateSourceLocal)
	require.NoError(t, err)
	require.EqualValues(t, 102, id)
	require.Equal(t, .31, confidence, "cosine 1.0 must never replace chat probability")
	require.Equal(t, embeddingTestCandidates(), candidates, "full classifier policy evidence must not be mutated")
	require.EqualValues(t, 2, budget.used.Load())
	require.Len(t, probe.requests, 2)
	require.Equal(t, llmcapture.TaskMatcherEmbedding, probe.requests[0].Task)
	require.Equal(t, embeddingBody, []byte(probe.requests[0].ModelInputJSON))
	require.Equal(t, chatBody, []byte(probe.requests[1].ModelInputJSON))
	require.Equal(t, []byte(embeddingTestResponse), probe.results[0].Body)
	require.NotContains(t, string(embeddingBody), "Alias Never Sent")
	require.NotContains(t, string(chatBody), "Alias Never Sent")
	require.NotContains(t, string(chatBody), "Other Film")
	var task struct {
		Candidates       []rerankCaptureCandidate `json:"candidates"`
		PolicyCandidates []rerankCaptureCandidate `json:"policy_candidates"`
		Embedding        embeddingShortlistAudit  `json:"embedding_shortlist"`
	}
	require.NoError(t, json.Unmarshal(probe.requests[1].TaskInputJSON, &task))
	require.Len(t, task.Candidates, 2)
	require.Len(t, task.PolicyCandidates, 3)
	require.Equal(t, candidates[0].AltTitles, task.PolicyCandidates[0].AltTitles)
	require.Equal(t, []int64{102, 101}, task.Embedding.CandidateIDs)
	receipt, ok := trace.Result(llmcapture.TaskMatcherEmbedding, llmcapture.CandidateSourceLocal)
	require.True(t, ok)
	require.Equal(t, hex.EncodeToString(receipt.CaptureKey), task.Embedding.CaptureKey)
	require.Equal(t, hex.EncodeToString(receipt.ResponseSHA256), task.Embedding.ResponseSHA256)
	cacheCtx, _ := llmcapture.WithResultTrace(context.Background())
	_, again, err := client.RerankForMediaType(cacheCtx, mediaTorrent("Sample.Film.2020.mkv"),
		Extraction{Title: "Sample Film", Type: "movie", Year: 2020}, "Parser Alias Never Sent", false,
		candidates, llmcapture.CandidateSourceLocal)
	require.NoError(t, err)
	require.Equal(t, confidence, again)
	require.EqualValues(t, 1, embeddingCalls.Load())
	require.EqualValues(t, 1, chatCalls.Load())
}

func TestEmbeddingProviderFailuresFallBackToOriginalChatList(t *testing.T) {
	for name, response := range map[string]string{
		"wrong_model":     strings.TrimSuffix(embeddingTestResponse, "}") + `,"model":"different-embedding-model"}`,
		"incomplete":      strings.TrimSuffix(embeddingTestResponse, "}") + `,"status":"incomplete"}`,
		"provider_error":  strings.TrimSuffix(embeddingTestResponse, "}") + `,"error":{"message":"synthetic failure"}}`,
		"zero":            `{"data":[{"index":0,"embedding":[0,0]},{"index":1,"embedding":[1,0]},{"index":2,"embedding":[1,0]},{"index":3,"embedding":[1,0]}]}`,
		"duplicate_index": `{"data":[{"index":0,"embedding":[1,0]},{"index":1,"embedding":[1,0]},{"index":1,"embedding":[1,0]},{"index":3,"embedding":[1,0]}]}`,
		"malformed":       `not JSON`, "outage": `unavailable`,
	} {
		t.Run(name, func(t *testing.T) {
			var embeddingCalls, chatCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/embeddings" {
					embeddingCalls.Add(1)
					if name == "outage" {
						w.WriteHeader(503)
					}
					_, _ = io.WriteString(w, response)
				} else {
					chatCalls.Add(1)
					body, err := io.ReadAll(r.Body)
					require.NoError(t, err)
					require.Contains(t, string(body), "Other Film")
					_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"tmdb_id\":103,\"confidence\":0.8}"}}]}`)
				}
			}))
			defer server.Close()
			probe, budget := &embeddingCaptureProbe{}, &embeddingBudgetProbe{}
			client := embeddingTestClient(server.URL, probe, budget)
			id, confidence, err := client.Rerank(context.Background(), mediaTorrent("Sample.Film.2020.mkv"),
				Extraction{Title: "Sample Film", Type: "movie"}, embeddingTestCandidates())
			require.NoError(t, err)
			require.EqualValues(t, 103, id)
			require.Equal(t, .8, confidence)
			require.EqualValues(t, 1, embeddingCalls.Load())
			require.EqualValues(t, 1, chatCalls.Load())
			require.EqualValues(t, 2, budget.used.Load())
			require.Len(t, probe.results, 2)
			require.Equal(t, []byte(response), probe.results[0].Body)
			require.NotEqual(t, "none", probe.results[0].ErrorClass)
		})
	}
}

func TestEmbeddingAdmissionAndAuditFailuresStopFollowOnCalls(t *testing.T) {
	for _, kind := range []string{"capture", "duplicate_capture", "recheck", "result", "duplicate", "native_private", "missing_capture", "zero_budget"} {
		t.Run(kind, func(t *testing.T) {
			var embeddingCalls, chatCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/embeddings" {
					embeddingCalls.Add(1)
					_, _ = io.WriteString(w, embeddingTestResponse)
				} else {
					chatCalls.Add(1)
				}
			}))
			defer server.Close()
			probe, budget := &embeddingCaptureProbe{}, &embeddingBudgetProbe{}
			client := embeddingTestClient(server.URL, probe, budget)
			torrent := mediaTorrent("Sample.Film.2020.mkv")
			switch kind {
			case "capture":
				probe.captureErr = llmcapture.ErrPrivacyBlocked
			case "duplicate_capture":
				probe.captureDuplicate = true
			case "recheck":
				probe.recheckErr = errors.New("source became private")
			case "result":
				probe.resultErr = errors.New("audit unavailable")
			case "duplicate":
				probe.duplicate = true
			case "native_private":
				torrent.Private = true
			case "missing_capture":
				client.capture = nil
			case "zero_budget":
				client.cfg.DailyCallLimit = 0
			}
			_, _, err := client.Rerank(context.Background(), torrent, Extraction{Title: "Sample Film", Type: "movie"}, embeddingTestCandidates())
			if kind != "native_private" {
				require.Error(t, err)
			}
			require.Zero(t, chatCalls.Load())
			if kind == "result" || kind == "duplicate" {
				require.EqualValues(t, 1, embeddingCalls.Load())
			} else {
				require.Zero(t, embeddingCalls.Load())
			}
		})
	}
}

func TestEmbeddingVectorsRejectAmbiguousShapesAndNormalizeSafely(t *testing.T) {
	cfg := NewDefaultEmbeddingConfig()
	cfg.Dimensions, cfg.MaxDimensions = 0, 2
	for _, rows := range []string{
		`[{"index":0,"embedding":[1,0]},{"index":0,"embedding":[1,0]}]`,
		`[{"embedding":[1,0]},{"index":1,"embedding":[1,0]}]`,
		`[{"index":0,"embedding":[1,0]},{"index":1,"embedding":[1]}]`,
		`[{"index":0,"embedding":[1,0,0]},{"index":1,"embedding":[1,0,0]}]`,
		`[{"index":0,"embedding":[1e999,0]},{"index":1,"embedding":[1,0]}]`,
		`[{"index":0,"embedding":[null,1]},{"index":1,"embedding":[1,0]}]`,
		`[{"index":0,"embedding":[0,0]},{"index":1,"embedding":[1,0]}]`,
		`[{"index":0,"index":1,"embedding":[1,0]},{"index":1,"embedding":[1,0]}]`,
		`[{"index":0,"embedding":[1,0],"extra":true},{"index":1,"embedding":[1,0]}]`,
		`[{"index":0,"embedding":[1,0],"object":"completion"},{"index":1,"embedding":[1,0]}]`,
	} {
		_, err := decodeEmbeddingVectors([]byte(`{"data":`+rows+`}`), 2, cfg)
		require.Error(t, err, rows)
	}
	_, err := decodeEmbeddingVectors([]byte(`{"data":[],"Data":[]}`), 2, cfg)
	require.Error(t, err)
	_, err = decodeEmbeddingVectors([]byte(`{"data":[],"data":[]} {}`), 2, cfg)
	require.Error(t, err)
	vectors, err := decodeEmbeddingVectors([]byte(`{"data":[{"index":1,"embedding":[1e308,1e308]},{"index":0,"embedding":[1e-300,1e-300]}]}`), 2, cfg)
	require.NoError(t, err)
	require.Equal(t, vectors[0], vectors[1])
	for _, component := range vectors[0] {
		require.InDelta(t, .70710678118, component, 1e-10)
	}
}

func TestEmbeddingRouteConfigIsExplicitAndBounded(t *testing.T) {
	cfg := NewDefaultEmbeddingConfig()
	require.False(t, cfg.Enabled)
	require.Empty(t, cfg.Endpoint)
	require.Empty(t, cfg.Model)
	cfg.Enabled, cfg.Endpoint, cfg.Model, cfg.APIKey = true, "https://api.example.test/v1/embeddings", "synthetic-model", "synthetic-key"
	require.NoError(t, cfg.validate())
	for _, change := range []func(*EmbeddingConfig){
		func(c *EmbeddingConfig) { c.Endpoint = "http://api.example.test/v1/embeddings" },
		func(c *EmbeddingConfig) { c.Endpoint = "https://user:secret@api.example.test/v1/embeddings" },
		func(c *EmbeddingConfig) { c.Endpoint += "?key=secret" },
		func(c *EmbeddingConfig) { c.APIKey = "" }, func(c *EmbeddingConfig) { c.Model = "" },
		func(c *EmbeddingConfig) { c.MaxDimensions = 4097 }, func(c *EmbeddingConfig) { c.Dimensions = -1 },
		func(c *EmbeddingConfig) { c.ShortlistSize = 1 }, func(c *EmbeddingConfig) { c.Timeout = 0 },
		func(c *EmbeddingConfig) { c.OpenrouterProvider = "some-provider" },
		func(c *EmbeddingConfig) { c.Endpoint = "https://openrouter.ai/api/v1/embeddings" },
	} {
		bad := cfg
		change(&bad)
		require.Error(t, bad.validate())
	}
	for _, endpoint := range []string{"http://127.0.0.1:1234/v1/embeddings", "http://[::1]:1234/v1/embeddings"} {
		local := cfg
		local.Endpoint, local.APIKey = endpoint, ""
		require.NoError(t, local.validate())
	}
	routed := cfg
	routed.Endpoint = "https://openrouter.ai/api/v1/embeddings"
	routed.OpenrouterProvider = "explicit/provider"
	require.NoError(t, routed.validate())
	require.False(t, strings.Contains(routed.Endpoint, "chat"))
	openai := cfg
	openai.Endpoint, openai.Model = "https://api.openai.com/v1/embeddings", "text-embedding-ada-002"
	require.Error(t, openai.validate())
	openai.Dimensions = 0
	require.NoError(t, openai.validate())
}

func TestEmbeddingProviderPinAndOmittedDimensionsUseProductionRequest(t *testing.T) {
	server, calls := chatServer(t, `{"tmdb_id":101,"confidence":0.9}`)
	probe := &embeddingCaptureProbe{}
	client := embeddingTestClient(server.URL, probe, &embeddingBudgetProbe{})
	client.cfg.Endpoint = server.URL
	client.cfg.Embeddings.Endpoint = "https://openrouter.ai/api/v1/embeddings"
	client.cfg.Embeddings.OpenrouterProvider = "explicit/provider"
	client.cfg.Embeddings.Dimensions = 0
	client.httpEmbedding.Transport = resultRoundTripper(func(request *http.Request) (*http.Response, error) {
		require.Equal(t, "https://openrouter.ai/api/v1/embeddings", request.URL.String())
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		var sent embeddingRequest
		require.NoError(t, json.Unmarshal(body, &sent))
		require.NotContains(t, string(body), `"dimensions"`)
		require.Equal(t, []string{"explicit/provider"}, sent.Provider.Order)
		require.Equal(t, sent.Provider.Order, sent.Provider.Only)
		require.False(t, sent.Provider.AllowFallbacks)
		require.True(t, sent.Provider.RequireParameters)
		require.True(t, sent.Provider.ZDR)
		require.Equal(t, "deny", sent.Provider.DataCollection)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(embeddingTestResponse))}, nil
	})
	_, _, err := client.Rerank(context.Background(), mediaTorrent("Sample.Film.2020.mkv"), Extraction{Title: "Sample Film", Type: "movie"}, embeddingTestCandidates())
	require.NoError(t, err)
	require.EqualValues(t, 1, atomic.LoadInt32(calls))
}
