package contentfilter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/llmprovider"
)

// ErrLLMUnavailable marks an LLM failure caused by the endpoint being
// unreachable — connection refused, timeout/context-deadline, or a
// transient HTTP 5xx/429. The filter treats these as defer-eligible
// (re-queue) when LLMDeferOnUnavailable is set, distinct from a 4xx or
// an unparseable 2xx response (the endpoint IS up but the request or
// reply is bad — those stay fail-open-keep so they can't defer-loop).
var ErrLLMUnavailable = errors.New("llm: endpoint unavailable")

// LLMVerdict is the structured response we expect from the LLM.
// Strict JSON shape (no chain-of-thought, no prose) so a malformed
// response is a parse error and falls through to "keep" rather than
// silently corrupting the cache.
//
// Fields:
//
//	IsEnglish    — best-effort yes/no on whether the title is
//	               English-language content.
//	Confidence   — 0.0–1.0; the LLM's self-assessed confidence.
//	Reason       — short tag identifying the rule that the LLM
//	               applied. Examples: "cyrillic-words",
//	               "russian-particle", "spanish-article",
//	               "english-only". Used by the rule miner to spot
//	               recurring patterns and promote them to
//	               deterministic checks.
type LLMVerdict struct {
	IsEnglish  bool    `json:"is_english"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`

	// Model is stamped by the client with the model it ACTUALLY called, not
	// the one config asked for. Every bitagent LLM metric was previously
	// unlabelled, so when the matcher was flipped from granite-4.1-8b to
	// gpt-5.4-nano not one metric name, label or value changed and no outcome
	// could be attributed to a model.
	Model string `json:"-"`
	// Usage is the provider's own token accounting. Zero when the provider
	// omits it — treat zero as "unknown", never as "free".
	Usage TokenUsage `json:"-"`
}

// TokenUsage is the per-call token count reported by the provider. Every
// OpenAI-compatible and Ollama endpoint returns this on every response and
// bitagent discarded all of it, which is why no token or cost metric existed
// anywhere in the service and a $207/mo spend could run unseen.
type TokenUsage struct {
	PromptTokens      int
	CompletionTokens  int
	CachedInputTokens int
	ReasoningTokens   int
}

// Total is prompt+completion. Zero means the provider reported nothing.
func (u TokenUsage) Total() int { return u.PromptTokens + u.CompletionTokens }

func (u TokenUsage) Valid() bool {
	return u.PromptTokens >= 0 && u.CompletionTokens >= 0 && u.Total() > 0 &&
		u.CachedInputTokens >= 0 && u.CachedInputTokens <= u.PromptTokens &&
		u.ReasoningTokens >= 0 && u.ReasoningTokens <= u.CompletionTokens
}

// LLMClient is the minimal abstraction the filter needs. Returns a
// verdict for one title or an error. Implementations: openaiClient
// (default), self-hosted via OpenAI-compatible endpoint, or a fake
// for tests.
type LLMClient interface {
	Classify(ctx context.Context, title string) (LLMVerdict, error)
}

// AuditedLLMClient exposes the exact bounded HTTP result for durable production
// evidence. The ordinary interface remains available to isolated library users
// and unit tests; production wiring requires this stronger contract.
type AuditedLLMClient interface {
	LLMClient
	ClassifyWithResult(context.Context, string) (LLMVerdict, llmcapture.HTTPResult, error)
}

// openaiClient calls api.openai.com/v1/responses with a strict
// instructions block. Per `feedback_openai_gpt5_temperature.md`,
// gpt-5* models reject the `temperature` parameter — we omit it.
type openaiClient struct {
	apiKey  string
	model   string
	baseURL string // override for self-hosted (defaults to OpenAI)
	http    *http.Client

	// apiStyle is "responses" (OpenAI Responses API) or "chat"
	// (OpenAI /chat/completions, used for self-hosted Ollama/vLLM).
	// Resolved at construction; never empty after NewOpenAIClient.
	apiStyle string

	// promptVersion lets us bust the cache when we change the
	// instructions. Increment when the prompt changes.
	promptVersion string

	openrouterProvider string
	openAIDataSharing  bool
	maxOutputTokens    int
}

const (
	apiStyleResponses = "responses"
	apiStyleChat      = "chat"
	// apiStyleOllama uses Ollama's NATIVE /api/chat with think:false —
	// the only reliable way to disable a reasoning model's (qwen3)
	// chain-of-thought on Ollama. The OpenAI-compat /v1/chat/completions
	// endpoint ignores every think-disable field (verified), so qwen
	// burns ~1.5k reasoning tokens (~2min/call) there — unusable.
	apiStyleOllama = "ollama"
)

// NewOpenAIClient builds the default OpenAI-backed classifier.
// Empty apiKey returns a "noop" client that always errors — the
// caller's contract is that LLMEnabled=false skips the call entirely
// so this branch should never be hit, but the safe-default is still
// a hard error rather than a silent pass.
func NewOpenAIClient(apiKey, model, baseURL, apiStyle, promptVersion string, timeout time.Duration) LLMClient {
	return NewOpenAIClientWithPolicy(
		apiKey, model, baseURL, apiStyle, promptVersion, "", 0, timeout,
	)
}

// NewOpenAIClientWithPolicy adds the bounded-output and exact-provider policy
// used by production. A blank provider retains the direct/local API shape.
func NewOpenAIClientWithPolicy(
	apiKey, model, baseURL, apiStyle, promptVersion, openrouterProvider string,
	maxOutputTokens int,
	timeout time.Duration,
	openAIDataSharing ...bool,
) LLMClient {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	if model == "" {
		model = "gpt-5.4-nano"
	}
	if promptVersion == "" {
		promptVersion = "v1"
	}
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	baseURL = strings.TrimRight(baseURL, "/")
	apiStyle = resolveAPIStyle(baseURL, apiStyle)
	dataSharing := len(openAIDataSharing) > 0 && openAIDataSharing[0]
	return &openaiClient{
		apiKey:             apiKey,
		model:              model,
		baseURL:            baseURL,
		apiStyle:           apiStyle,
		promptVersion:      promptVersion,
		openrouterProvider: strings.TrimSpace(openrouterProvider),
		openAIDataSharing:  dataSharing,
		maxOutputTokens:    maxOutputTokens,
		http:               &http.Client{Timeout: timeout},
	}
}

func resolveAPIStyle(baseURL, apiStyle string) string {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	// Empty = auto: chat-completions for any non-OpenAI base URL
	// (Ollama/vLLM/LocalAI speak it universally), Responses for OpenAI.
	switch apiStyle {
	case apiStyleResponses, apiStyleChat, apiStyleOllama:
		return apiStyle
	default:
		if baseURL == "https://api.openai.com/v1" {
			return apiStyleResponses
		}
		return apiStyleChat
	}
}

// APIStyle reports the resolved API style ("responses" or "chat").
func (c *openaiClient) APIStyle() string { return c.apiStyle }

// PromptVersion is exposed so the cache key can include it. A
// prompt change automatically invalidates every cached entry —
// without this, a smarter prompt would be silently masked by stale
// cache hits.
func (c *openaiClient) PromptVersion() string { return c.promptVersion }
func (c *openaiClient) Model() string         { return c.model }

const llmInstructions = `You classify torrent titles for a media indexer.
Goal: decide whether the title represents English-language content.
Return STRICT JSON only — no prose, no explanation outside the JSON:

{"is_english": <bool>, "confidence": <float 0..1>, "reason": "<short-tag>"}

Rules:
- A title is English if its primary language is English. Mixed-language
  with English as primary (e.g. "Movie 2024 [eng-dub]") is English.
- A foreign title in transliterated Latin script (e.g. "Pelicula 2024",
  "Yidiotka 2024") is NOT English even though it's Latin script.
- ANIME EXCEPTION: a Japanese anime title — romaji ("Sousou no Frieren",
  "Shingeki no Kyojin"), or carrying a fansub-group bracket ([SubsPlease],
  [Erai-raws], [HorribleSubs], [Judas]), or "Title - <number>" absolute
  episode numbering — is treated as English (is_english=true). Anime is
  overwhelmingly English-subbed/dubbed and must NOT be dropped on a romaji
  title alone.
- A title that's just numbers / years / generic descriptors is "unsure"
  with low confidence; we'll keep it.
- Reason is a short kebab-case tag that lets a rule miner spot
  recurring patterns. Examples: "english-clear", "russian-particle",
  "russian-cyrillic-words", "spanish-article", "anime-romaji-keep",
  "transliterated-cyrillic", "generic-low-info", "ambiguous".

Be terse. Output ONLY the JSON.`

// Classify implements LLMClient via the OpenAI Responses API.
// Caller must check LLMEnabled and budget BEFORE invoking.
//
// API-key handling:
//   - When `baseURL` points at the OpenAI default
//     (https://api.openai.com/v1) an empty key is a hard error —
//     OpenAI requires Authorization on every call.
//   - When `baseURL` is overridden (self-hosted Ollama / vLLM /
//     LocalAI) an empty key is fine; we omit the Authorization
//     header so the local endpoint isn't sent a bogus "Bearer ".
func (c *openaiClient) Classify(ctx context.Context, title string) (LLMVerdict, error) {
	verdict, _, err := c.ClassifyWithResult(ctx, title)
	return verdict, err
}

func (c *openaiClient) ClassifyWithResult(
	ctx context.Context,
	title string,
) (LLMVerdict, llmcapture.HTTPResult, error) {
	const openAIDefaultBase = "https://api.openai.com/v1"
	if c.apiKey == "" && c.baseURL == openAIDefaultBase {
		return LLMVerdict{}, llmcapture.HTTPResult{}, errors.New("openai client: empty API key (required for api.openai.com; set CONTENT_FILTER_LLM_BASE_URL for a self-hosted endpoint that doesn't need auth)")
	}
	switch c.apiStyle {
	case apiStyleOllama:
		return c.classifyOllama(ctx, title)
	case apiStyleChat:
		return c.classifyChat(ctx, title)
	}
	return c.classifyResponses(ctx, title)
}

// doJSON POSTs a JSON body to the given absolute url and returns the raw
// 2xx body. Network failures, timeouts/context-deadline, and transient
// HTTP 5xx/429 are wrapped as ErrLLMUnavailable (defer-eligible); a
// 4xx returns a plain error (the endpoint is up but rejected the
// request — not an availability problem, so not defer-worthy).
func (c *openaiClient) doJSON(
	ctx context.Context,
	url string,
	body any,
) ([]byte, llmcapture.HTTPResult, error) {
	bodyBytes, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, llmcapture.HTTPResult{}, err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// Connection refused / DNS / timeout / context deadline —
		// endpoint unreachable.
		result := llmcapture.HTTPResult{ErrorClass: "transport"}
		return nil, result, fmt.Errorf("%w: %v", ErrLLMUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, llmcapture.MaxResultBodyBytes+1))
	result := llmcapture.HTTPResult{
		Body: respBytes, StatusCode: resp.StatusCode, ErrorClass: "none",
	}
	if len(respBytes) > llmcapture.MaxResultBodyBytes {
		result.Body = respBytes[:llmcapture.MaxResultBodyBytes]
		err = errors.New("response exceeds audit byte limit")
	}
	if err != nil {
		result.ErrorClass = "read"
		return nil, result, fmt.Errorf("%w: read response: %v", ErrLLMUnavailable, err)
	}
	if resp.StatusCode/100 != 2 {
		result.ErrorClass = "http_status"
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode/100 == 5 {
			return nil, result, fmt.Errorf("%w: http %d: %s", ErrLLMUnavailable, resp.StatusCode, truncate(string(respBytes), 200))
		}
		return nil, result, fmt.Errorf("llm: http %d: %s", resp.StatusCode, truncate(string(respBytes), 200))
	}
	return respBytes, result, nil
}

// classifyResponses uses the OpenAI Responses API (POST /responses).
func (c *openaiClient) classifyResponses(ctx context.Context, title string) (LLMVerdict, llmcapture.HTTPResult, error) {
	body := responsesRequestBodyBounded(c.model, title, c.maxOutputTokens)
	respBytes, result, err := c.doJSON(ctx, c.baseURL+"/responses", body)
	if err != nil {
		return LLMVerdict{}, result, err
	}
	verdict := LLMVerdict{Model: c.model}
	var ru struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			InputDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(respBytes, &ru) == nil {
		if strings.TrimSpace(ru.Model) != "" {
			verdict.Model = ru.Model
		}
		verdict.Usage = TokenUsage{
			PromptTokens: ru.Usage.InputTokens, CompletionTokens: ru.Usage.OutputTokens,
			CachedInputTokens: ru.Usage.InputDetails.CachedTokens,
			ReasoningTokens:   ru.Usage.OutputDetails.ReasoningTokens,
		}
	}
	// Parse the OpenAI Responses envelope. Two shapes:
	//   1. {"output_text": "..."}             (sometimes)
	//   2. {"output":[{"content":[{"type":"output_text","text":"..."}]}]}
	text, parseErr := extractOutputText(respBytes)
	if parseErr != nil {
		return verdict, result, fmt.Errorf("llm: extract: %w; body=%s", parseErr, truncate(string(respBytes), 200))
	}
	parsed, vErr := parseLLMVerdict(text)
	if vErr != nil {
		return verdict, result, fmt.Errorf("llm: verdict: %w; text=%s", vErr, truncate(text, 200))
	}
	parsed.Model, parsed.Usage = verdict.Model, verdict.Usage
	verdict = parsed
	return verdict, result, nil
}

// classifyChat uses OpenAI chat-completions (POST /chat/completions) —
// the generic shape for self-hosted OpenAI-compatible servers (vLLM /
// LocalAI). NOTE: for an Ollama-hosted *reasoning* model (qwen3) use the
// "ollama" style instead — this endpoint cannot disable the model's
// chain-of-thought (the "/no_think" hint and every think-field are
// ignored), so qwen burns ~2min/call here. The hint is kept for models
// that do honour it.
func (c *openaiClient) classifyChat(ctx context.Context, title string) (LLMVerdict, llmcapture.HTTPResult, error) {
	body := chatRequestBodyWithRouting(
		c.model, title, c.openrouterProvider, c.maxOutputTokens, c.openAIDataSharing,
	)
	respBytes, result, err := c.doJSON(ctx, c.baseURL+"/chat/completions", body)
	if err != nil {
		return LLMVerdict{}, result, err
	}
	var cr struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			PromptDetails    struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBytes, &cr); err != nil {
		return LLMVerdict{}, result, fmt.Errorf("llm: chat decode: %w; body=%s", err, truncate(string(respBytes), 200))
	}
	verdict := LLMVerdict{Model: c.model, Usage: TokenUsage{
		PromptTokens: cr.Usage.PromptTokens, CompletionTokens: cr.Usage.CompletionTokens,
		CachedInputTokens: cr.Usage.PromptDetails.CachedTokens,
		ReasoningTokens:   cr.Usage.CompletionDetails.ReasoningTokens,
	}}
	if strings.TrimSpace(cr.Model) != "" {
		verdict.Model = cr.Model
	}
	if len(cr.Choices) == 0 || strings.TrimSpace(cr.Choices[0].Message.Content) == "" {
		return verdict, result, fmt.Errorf("llm: chat: empty content; body=%s", truncate(string(respBytes), 200))
	}
	parsed, vErr := parseLLMVerdict(cr.Choices[0].Message.Content)
	if vErr != nil {
		return verdict, result, fmt.Errorf("llm: verdict: %w; text=%s", vErr, truncate(cr.Choices[0].Message.Content, 200))
	}
	parsed.Model, parsed.Usage = verdict.Model, verdict.Usage
	verdict = parsed
	return verdict, result, nil
}

// classifyOllama uses Ollama's NATIVE chat API (POST <host>/api/chat) with
// think:false — the only way to make a reasoning model (qwen3) return a terse
// verdict on Ollama, since /v1/chat/completions ignores every think-disable
// field. The native API lives at the host root, so a trailing "/v1" is stripped
// from baseURL. Response shape is {"message":{"content":"..."}}.
func (c *openaiClient) classifyOllama(ctx context.Context, title string) (LLMVerdict, llmcapture.HTTPResult, error) {
	body := ollamaRequestBodyBounded(c.model, title, c.maxOutputTokens)
	url := strings.TrimSuffix(c.baseURL, "/v1") + "/api/chat"
	respBytes, result, err := c.doJSON(ctx, url, body)
	if err != nil {
		return LLMVerdict{}, result, err
	}
	var or struct {
		Model   string `json:"model"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		// Ollama's native API does NOT use the OpenAI usage block; it reports
		// prompt_eval_count / eval_count at the top level.
		PromptEvalCount int `json:"prompt_eval_count"`
		EvalCount       int `json:"eval_count"`
	}
	if err := json.Unmarshal(respBytes, &or); err != nil {
		return LLMVerdict{}, result, fmt.Errorf("llm: ollama decode: %w; body=%s", err, truncate(string(respBytes), 200))
	}
	verdict := LLMVerdict{Model: c.model, Usage: TokenUsage{
		PromptTokens: or.PromptEvalCount, CompletionTokens: or.EvalCount,
	}}
	if strings.TrimSpace(or.Model) != "" {
		verdict.Model = or.Model
	}
	if strings.TrimSpace(or.Message.Content) == "" {
		return verdict, result, fmt.Errorf("llm: ollama: empty content; body=%s", truncate(string(respBytes), 200))
	}
	parsed, vErr := parseLLMVerdict(or.Message.Content)
	if vErr != nil {
		return verdict, result, fmt.Errorf("llm: verdict: %w; text=%s", vErr, truncate(or.Message.Content, 200))
	}
	parsed.Model, parsed.Usage = verdict.Model, verdict.Usage
	verdict = parsed
	return verdict, result, nil
}

func responsesRequestBody(model, title string) map[string]any {
	return responsesRequestBodyBounded(model, title, 0)
}

func responsesRequestBodyBounded(model, title string, maxOutputTokens int) map[string]any {
	body := map[string]any{
		"model":        model,
		"instructions": llmInstructions,
		"input":        title,
		// Deliberately omit temperature: gpt-5* rejects it.
	}
	if maxOutputTokens > 0 {
		body["max_output_tokens"] = maxOutputTokens
	}
	return body
}

func chatRequestBody(model, title string) map[string]any {
	return chatRequestBodyWithPolicy(model, title, "", 0)
}

func chatRequestBodyWithPolicy(model, title, provider string, maxOutputTokens int) map[string]any {
	return chatRequestBodyWithRouting(model, title, provider, maxOutputTokens, false)
}

func chatRequestBodyWithRouting(
	model, title, provider string,
	maxOutputTokens int,
	openAIDataSharing bool,
) map[string]any {
	systemPrompt := llmInstructions + "\n/no_think"
	if openAIDataSharing {
		systemPrompt = llmInstructions
	}
	body := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": title},
		},
		"stream": false,
	}
	if maxOutputTokens > 0 {
		body["max_completion_tokens"] = maxOutputTokens
	}
	if provider != "" {
		body["provider"] = map[string]any{
			"order": []string{provider}, "only": []string{provider},
			"allow_fallbacks": false, "require_parameters": true,
			"data_collection": "deny", "zdr": true,
		}
	}
	if openAIDataSharing {
		llmprovider.ApplyDataSharingChatOptions(body)
	}
	return body
}

func ollamaRequestBody(model, title string) map[string]any {
	return ollamaRequestBodyBounded(model, title, 0)
}

func ollamaRequestBodyBounded(model, title string, maxOutputTokens int) map[string]any {
	body := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": llmInstructions},
			{"role": "user", "content": title},
		},
		"think":  false,
		"stream": false,
	}
	if maxOutputTokens > 0 {
		body["options"] = map[string]any{"num_predict": maxOutputTokens}
	}
	return body
}

// extractOutputText pulls the textual content out of the Responses
// API envelope. Tolerates both the convenience field and the full
// nested shape.
func extractOutputText(raw []byte) (string, error) {
	var conv struct {
		OutputText string `json:"output_text"`
		Output     []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &conv); err != nil {
		return "", err
	}
	if conv.OutputText != "" {
		return conv.OutputText, nil
	}
	for _, item := range conv.Output {
		for _, c := range item.Content {
			if c.Type == "output_text" || c.Type == "text" {
				if c.Text != "" {
					return c.Text, nil
				}
			}
		}
	}
	return "", errors.New("no output text in response")
}

// parseLLMVerdict accepts the LLM's strict-JSON output. Tolerates
// surrounding whitespace and a single fenced code block (```json
// ... ```), since some models wrap structured output despite
// instructions. Anything else fails fast — the caller treats parse
// errors as "unsure, keep."
func parseLLMVerdict(text string) (LLMVerdict, error) {
	s := strings.TrimSpace(text)
	// Strip a leading <think>...</think> reasoning block. Reasoning
	// models (qwen3) can emit one even with /no_think, and Ollama
	// returns it inline in message.content.
	if strings.HasPrefix(s, "<think>") {
		if end := strings.Index(s, "</think>"); end >= 0 {
			s = strings.TrimSpace(s[end+len("</think>"):])
		}
	}
	if strings.HasPrefix(s, "```") {
		// strip leading fence
		nl := strings.IndexByte(s, '\n')
		if nl > 0 {
			s = s[nl+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
		s = strings.TrimSpace(s)
	}
	// Pointers are deliberate. With value fields, a missing or null
	// is_english silently became false; paired with a model-supplied high
	// confidence that crossed the live drop threshold and violated the
	// classifier's fail-open contract.
	var raw struct {
		IsEnglish  *bool    `json:"is_english"`
		Confidence *float64 `json:"confidence"`
		Reason     *string  `json:"reason"`
	}
	decoder := json.NewDecoder(strings.NewReader(s))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return LLMVerdict{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return LLMVerdict{}, errors.New("multiple JSON values")
		}
		return LLMVerdict{}, err
	}
	if raw.IsEnglish == nil || raw.Confidence == nil || raw.Reason == nil {
		return LLMVerdict{}, errors.New("missing or null verdict field")
	}
	if math.IsNaN(*raw.Confidence) || math.IsInf(*raw.Confidence, 0) ||
		*raw.Confidence < 0 || *raw.Confidence > 1 {
		return LLMVerdict{}, errors.New("confidence must be finite and in [0,1]")
	}
	reason := strings.TrimSpace(*raw.Reason)
	if reason == "" || len(reason) > 64 {
		return LLMVerdict{}, errors.New("reason must be 1-64 bytes")
	}
	return LLMVerdict{
		IsEnglish:  *raw.IsEnglish,
		Confidence: *raw.Confidence,
		Reason:     reason,
	}, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "...[truncated]"
	}
	return s
}
