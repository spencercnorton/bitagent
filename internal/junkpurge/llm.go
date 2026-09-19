package junkpurge

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

	"github.com/spencercnorton/bitagent/internal/llmprovider"
)

// ErrLLMUnavailable marks an LLM failure caused by the endpoint being
// unreachable — connection refused, timeout/context-deadline, or a transient
// HTTP 5xx/429. The worker treats this as a circuit-breaker signal (skip the
// cycle's deletions), distinct from a 4xx or an unparseable 2xx (endpoint is
// up but the request/reply is bad — those are per-item errors, not outages).
var ErrLLMUnavailable = errors.New("junkpurge llm: endpoint unavailable")

const (
	llmFailureCanceled          = "canceled"
	llmFailureTimeout           = "timeout"
	llmFailureTransport         = "transport"
	llmFailureResponseRead      = "response_read"
	llmFailureRateLimited       = "rate_limited"
	llmFailureServerError       = "server_error"
	llmFailureRequestRejected   = "request_rejected"
	llmFailureRequestBuild      = "request_build"
	llmFailureBudgetExhausted   = "budget_exhausted"
	llmFailureBudgetUnavailable = "budget_unavailable"
	llmFailureUnavailable       = "unavailable"
	llmFailureInvalidResponse   = "invalid_response"
)

// llmRequestError retains a bounded, low-cardinality failure reason without
// weakening the existing errors.Is(err, ErrLLMUnavailable) contract. Before
// this wrapper every timeout, transport error, 429, 5xx and response-read
// failure collapsed into one sentinel and the worker then discarded the
// concrete error, so production could report only "unavailable".
type llmRequestError struct {
	reason      string
	unavailable bool
	detail      string
	cause       error
}

func (e *llmRequestError) Error() string {
	prefix := "junkpurge llm request failed"
	if e.unavailable {
		prefix = ErrLLMUnavailable.Error()
	}
	if e.detail != "" {
		return fmt.Sprintf("%s (%s): %s", prefix, e.reason, e.detail)
	}
	if e.cause != nil {
		return fmt.Sprintf("%s (%s): %v", prefix, e.reason, e.cause)
	}
	return fmt.Sprintf("%s (%s)", prefix, e.reason)
}

func (e *llmRequestError) Unwrap() error { return e.cause }

func (e *llmRequestError) Is(target error) bool {
	return target == ErrLLMUnavailable && e.unavailable
}

func llmFailureReason(err error) string {
	if errors.Is(err, context.Canceled) {
		return llmFailureCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return llmFailureTimeout
	}
	var requestErr *llmRequestError
	if errors.As(err, &requestErr) {
		return requestErr.reason
	}
	if errors.Is(err, ErrLLMUnavailable) {
		return llmFailureUnavailable
	}
	return llmFailureInvalidResponse
}

// Verdict classes. Only Junk is ever actioned, and only above the confidence
// floor. Everything else keeps the torrent.
const (
	verdictJunk        = "junk"
	verdictRealMangled = "real_mangled"
	verdictRealAbsent  = "real_absent"
	verdictUnsure      = "unsure"
	judgePromptVersion = "v2-2026-06-26"
	judgeMaxTokens     = 512
	judgeReasoning     = "low"
)

// Judgment is the structured reply expected from the LLM.
type Judgment struct {
	Verdict    string  `json:"verdict"`
	Confidence float64 `json:"confidence"`
}

// IsJunk reports whether the verdict is a confident junk call.
func (j Judgment) IsJunk(minConfidence float64) bool {
	return j.Verdict == verdictJunk && j.Confidence >= minConfidence
}

// Judge classifies one torrent name. The contract mirrors the contentfilter
// LLM client so a fake can be substituted in tests.
type Judge interface {
	Judge(ctx context.Context, torrentName string) (Judgment, error)
	// JudgeBatch classifies several names in ONE request, amortising the
	// ~470-token policy prompt across the group. The returned slice is
	// positionally aligned with torrentNames. Any defect in the grouped
	// reply (wrong count, bad index set, one invalid item) fails the WHOLE
	// group — the caller falls back to per-name Judge calls, so a format
	// regression costs money, never coverage or correctness.
	JudgeBatch(ctx context.Context, torrentNames []string) ([]Judgment, error)
}

// CallBudget reserves a hosted provider request immediately before dispatch.
// A transport failure may still be billed, so reservations are never refunded.
type CallBudget interface {
	Reserve(context.Context, int, int) (bool, error)
}

// judgeInstructions v2 (2026-06-26): reframed around "is this a real movie/TV
// show?" instead of "is it junk?". The v1 prompt over-junked real-but-messy
// content (anime fansubs, talk/game shows, foreign titles, niche TV: Fullmetal
// Alchemist, Conan, Jeopardy, Bewitched, Emilia Perez). Validated on the local
// qwen against 18 labelled live examples: v1 = 3/12 false-junk + 1/6 missed;
// v2 = 0/12 + 0/6. The reframe + explicit real-categories + "messy markers are
// normal" is what closed those false positives.
//
// The "0/12" above is IN-SAMPLE: the prompt was tuned against those very
// examples until it scored zero. It is evidence that the reframe fixed the
// specific regressions it was written for, and nothing more. It must never be
// cited as this prompt's false-junk rate. The measured realised false-junk on
// actioned deletions is >=0.43% [0.25%, 0.74%] (13/3,000; see
// docs/design/gold-corpus-rescope.md §6), and that is itself a floor.
//
// These instructions are the ACTUAL deployed policy artifact for the junk-purge
// pool. ops/llm-eval/junk-disposition-policy-v1.json anchors the gold label
// space to them and pins their SHA-256; changing the text below without
// updating that file fails TestJudgePromptHashMatchesDispositionPolicy.
const judgeInstructions = `You decide whether a torrent NAME is a real MOVIE or TV SHOW, or junk, for a
movie/TV indexer. The name could not be auto-matched to a database, usually
because the name is messy — that alone does NOT make it junk.

Return STRICT JSON only: {"verdict":"<junk|real_mangled|real_absent|unsure>","confidence":<float 0..1>}

A name is a REAL movie/TV show (verdict "real_mangled", or "real_absent" if real
but very obscure) when it plausibly names ANY of:
- a film or TV series in ANY language; foreign / transliterated titles COUNT
  ("Emiliya Peres" = Emilia Perez; "Licencja na zabijanie" = Licence to Kill);
- ANIME / animation, often tagged with a fansub group in brackets ([Some-Stuffs],
  [RPG-sama], [HorribleSubs]) — e.g. "[Some-Stuffs] Pocket Monsters", "Fullmetal Alchemist (2003)";
- a talk / late-night / game / variety / reality show, often host+date or show+date
  — e.g. "Conan.2016.11.03.Tracy.Morgan", "Jeopardy.2018.05.22", "bobross s24e09";
- a documentary/docuseries; an old, niche, or regional show.
Messy markers are NORMAL for real content and must NOT push you to "junk":
site prefixes ("www.UIndex.org - "), scene/release tags, group brackets, SxxExx,
dates, resolution/codec (1080p, x265, WEB-DL), .mkv/.mp4.

A name is "junk" ONLY when the CONTENT itself is NOT a movie/TV show:
- pornography/adult (studio or performer names, "casting", explicit acts, adult domains);
- music (albums, concerts, discographies, FLAC);
- sports or live events (matches, leagues, "vs", race meets);
- software, games, keygens, e-books;
- spam, fakes, password-bait, ads, corrupt/placeholder names.

Be CONSERVATIVE: if the name could plausibly be a real film/series in any language,
choose real_mangled. Reserve "junk" for cases CLEARLY in the not-a-movie/TV list.
Output ONLY the JSON.`

type ollamaJudge struct {
	baseURL           string
	model             string
	apiStyle          string
	apiKey            string
	http              *http.Client
	metrics           *Metrics
	openAIDataSharing bool
	budget            CallBudget
	dailyCallLimit    int
	monthlyCallLimit  int
}

// NewJudge builds the LLM judge from config. It defaults to the self-hosted
// qwen via Ollama's native API (the only style that reliably disables a
// reasoning model's chain-of-thought — see contentfilter/llm.go).
func NewJudge(cfg Config, metrics *Metrics) Judge {
	return NewJudgeWithBudget(cfg, metrics, nil)
}

// NewJudgeWithBudget is the production constructor for hosted chat routes.
// Local Ollama does not consume the budget; direct data-sharing traffic fails
// closed when the budget is absent or unavailable.
func NewJudgeWithBudget(cfg Config, metrics *Metrics, budget CallBudget) Judge {
	return &ollamaJudge{
		baseURL:           strings.TrimRight(cfg.LLMBaseURL, "/"),
		model:             cfg.LLMModel,
		apiStyle:          cfg.LLMApiStyle,
		apiKey:            cfg.LLMApiKey,
		http:              &http.Client{Timeout: cfg.timeout()},
		metrics:           metrics,
		openAIDataSharing: cfg.LLMOpenaiDataSharing,
		budget:            budget,
		dailyCallLimit:    cfg.LLMDailyCallLimit,
		monthlyCallLimit:  cfg.LLMMonthlyCallLimit,
	}
}

func (j *ollamaJudge) Judge(ctx context.Context, torrentName string) (Judgment, error) {
	switch j.apiStyle {
	case "ollama":
		return j.judgeOllama(ctx, torrentName)
	case "chat":
		return j.judgeChat(ctx, torrentName)
	default:
		return Judgment{}, fmt.Errorf(
			"junkpurge llm: unsupported api_style %q", j.apiStyle,
		)
	}
}

// batchFormatInstructions is the reply contract appended AFTER
// judgeInstructions for grouped requests. It changes only the I/O shape —
// the policy rubric itself is judgeInstructions verbatim, which is what
// ops/llm-eval/junk-disposition-policy-v1.json SHA-pins;
// TestJudgeBatchChatParsesGroupedVerdicts holds that embedding.
func batchFormatInstructions(n int) string {
	return fmt.Sprintf(`

You will receive %d torrent names as a numbered list, one per line.
Judge EACH name independently under the policy above; never let one name
influence another. Return STRICT JSON only: an array of exactly %d objects,
one per input line, each
{"i":<line number>,"verdict":"<junk|real_mangled|real_absent|unsure>","confidence":<float 0..1>}.
Every line number 1..%d must appear exactly once. Output ONLY the JSON array.`, n, n, n)
}

// batchUserContent renders the numbered list. Newlines inside a name (never
// seen in practice — these are file names) are flattened so a name cannot
// masquerade as an extra list line.
func batchUserContent(names []string) string {
	var b strings.Builder
	for i, name := range names {
		if i > 0 {
			b.WriteByte('\n')
		}
		name = strings.NewReplacer("\n", " ", "\r", " ").Replace(name)
		fmt.Fprintf(&b, "%d. %s", i+1, name)
	}
	return b.String()
}

// batchMaxTokens scales the completion cap with group size: the single-name
// cap plus ~48 tokens per additional item (measured production output is
// ~17 tokens per judgment; 48 leaves headroom without inviting prose).
func batchMaxTokens(n int) int {
	return judgeMaxTokens + 48*(n-1)
}

// JudgeBatch sends one grouped request. n==1 delegates to Judge so a
// tail group of one is byte-identical to the classic request shape.
func (j *ollamaJudge) JudgeBatch(ctx context.Context, names []string) ([]Judgment, error) {
	switch len(names) {
	case 0:
		return nil, nil
	case 1:
		jm, err := j.Judge(ctx, names[0])
		if err != nil {
			return nil, err
		}
		return []Judgment{jm}, nil
	}
	system := judgeInstructions + batchFormatInstructions(len(names))
	user := batchUserContent(names)
	var (
		content string
		usage   tokenUsage
		err     error
	)
	switch j.apiStyle {
	case "ollama":
		content, usage, err = j.completeOllama(ctx, system, user)
	case "chat":
		content, usage, err = j.completeChat(ctx, system, user, batchMaxTokens(len(names)))
	default:
		return nil, fmt.Errorf("junkpurge llm: unsupported api_style %q", j.apiStyle)
	}
	if err != nil {
		j.observeFailure("grouped", usage, err)
		return nil, err
	}
	judgments, perr := parseBatchJudgments(content, len(names))
	if perr != nil {
		j.observeFailure("grouped", usage, perr)
		return nil, perr
	}
	j.observeRequest("grouped", "ok", usage)
	return judgments, nil
}

// completeOllama POSTs arbitrary system+user messages to the native
// /api/chat endpoint (think:false) and returns the reply content.
func (j *ollamaJudge) completeOllama(ctx context.Context, system, user string) (string, tokenUsage, error) {
	body := map[string]any{
		"model": j.model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"think":  false,
		"stream": false,
	}
	url := strings.TrimSuffix(j.baseURL, "/v1") + "/api/chat"
	raw, err := j.doJSON(ctx, url, body)
	if err != nil {
		return "", tokenUsage{}, err
	}
	var or struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		PromptEvalCount int64 `json:"prompt_eval_count"`
		EvalCount       int64 `json:"eval_count"`
	}
	if err := json.Unmarshal(raw, &or); err != nil {
		return "", tokenUsage{}, fmt.Errorf("junkpurge llm: ollama decode: %w; body=%s", err, truncate(string(raw), 200))
	}
	return or.Message.Content, tokenUsage{Input: or.PromptEvalCount, Output: or.EvalCount}, nil
}

// completeChat POSTs arbitrary system+user messages to the OpenAI-compatible
// /chat/completions endpoint and returns the reply content plus usage.
func (j *ollamaJudge) completeChat(ctx context.Context, system, user string, maxTokens int) (string, tokenUsage, error) {
	raw, err := j.doJSON(ctx, j.baseURL+"/chat/completions", buildChatRequestWithRouting(
		j.model, user, system, maxTokens, judgeReasoning, j.openAIDataSharing,
	))
	if err != nil {
		return "", tokenUsage{}, err
	}
	return decodeChatReply(raw)
}

// judgeOllama POSTs to <host>/api/chat with think:false — terse output from a
// reasoning model. A trailing "/v1" on baseURL is stripped (native API lives
// at the host root). Response shape: {"message":{"content":"..."}}.
func (j *ollamaJudge) judgeOllama(ctx context.Context, name string) (Judgment, error) {
	body := map[string]any{
		"model": j.model,
		"messages": []map[string]string{
			{"role": "system", "content": judgeInstructions},
			{"role": "user", "content": name},
		},
		"think":  false,
		"stream": false,
	}
	url := strings.TrimSuffix(j.baseURL, "/v1") + "/api/chat"
	raw, err := j.doJSON(ctx, url, body)
	if err != nil {
		j.observeFailure("standard", tokenUsage{}, err)
		return Judgment{}, err
	}
	var or struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		PromptEvalCount int64 `json:"prompt_eval_count"`
		EvalCount       int64 `json:"eval_count"`
	}
	if err := json.Unmarshal(raw, &or); err != nil {
		wrapped := fmt.Errorf("junkpurge llm: ollama decode: %w; body=%s", err, truncate(string(raw), 200))
		j.observeFailure("standard", tokenUsage{}, wrapped)
		return Judgment{}, wrapped
	}
	judgment, err := parseJudgment(or.Message.Content)
	if err != nil {
		j.observeFailure("standard", tokenUsage{}, err)
		return Judgment{}, err
	}
	j.observeRequest("standard", "ok", tokenUsage{
		Input: or.PromptEvalCount, Output: or.EvalCount,
	})
	return judgment, nil
}

// judgeChat uses OpenAI-compatible /chat/completions for non-Ollama endpoints.
func (j *ollamaJudge) judgeChat(ctx context.Context, name string) (Judgment, error) {
	raw, err := j.doJSON(ctx, j.baseURL+"/chat/completions", buildChatRequestForRoute(
		j.model, name, j.openAIDataSharing,
	))
	if err != nil {
		j.observeFailure("standard", tokenUsage{}, err)
		return Judgment{}, err
	}
	judgment, usage, err := parseChatJudgment(raw)
	if err != nil {
		j.observeFailure("standard", usage, err)
		return Judgment{}, err
	}
	j.observeRequest("standard", "ok", usage)
	return judgment, nil
}

func buildChatRequest(model, name string) map[string]any {
	return buildChatRequestForRoute(model, name, false)
}

func buildChatRequestForRoute(model, name string, openAIDataSharing bool) map[string]any {
	systemPrompt := judgeInstructions + "\n/no_think"
	reasoningEffort := judgeReasoning
	if openAIDataSharing {
		systemPrompt = judgeInstructions
		reasoningEffort = llmprovider.OpenAIReasoningEffort
	}
	return buildChatRequestWithRouting(
		model,
		name,
		systemPrompt,
		judgeMaxTokens,
		reasoningEffort,
		openAIDataSharing,
	)
}

func buildChatRequestWithPolicy(
	model, name, systemPrompt string,
	maxCompletionTokens int,
	reasoningEffort string,
) map[string]any {
	return buildChatRequestWithRouting(
		model, name, systemPrompt, maxCompletionTokens, reasoningEffort, false,
	)
}

func buildChatRequestWithRouting(
	model, name, systemPrompt string,
	maxCompletionTokens int,
	reasoningEffort string,
	openAIDataSharing bool,
) map[string]any {
	body := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": name},
		},
		"stream": false,
		// gpt-5.x reasoning models reject max_tokens/temperature (we send
		// neither); cap the output and keep the reasoning pass light so a
		// one-line junk verdict stays fast and cheap on a metered API.
		"max_completion_tokens": maxCompletionTokens,
		"reasoning_effort":      reasoningEffort,
	}
	if openAIDataSharing {
		llmprovider.ApplyDataSharingChatOptions(body)
	}
	return body
}

func parseChatJudgment(raw []byte) (Judgment, tokenUsage, error) {
	content, usage, err := decodeChatReply(raw)
	if err != nil {
		return Judgment{}, usage, err
	}
	judgment, err := parseJudgment(content)
	return judgment, usage, err
}

// decodeChatReply extracts the first choice's content and the token usage
// from an OpenAI-compatible /chat/completions reply.
func decodeChatReply(raw []byte) (string, tokenUsage, error) {
	var cr struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			PromptDetails    struct {
				CachedTokens     int64 `json:"cached_tokens"`
				CacheWriteTokens int64 `json:"cache_write_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionDetails struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", tokenUsage{}, fmt.Errorf("junkpurge llm: chat decode: %w; body=%s", err, truncate(string(raw), 200))
	}
	usage := tokenUsage{
		Input:      cr.Usage.PromptTokens,
		Cached:     cr.Usage.PromptDetails.CachedTokens,
		CacheWrite: cr.Usage.PromptDetails.CacheWriteTokens,
		Output:     cr.Usage.CompletionTokens,
		Reasoning:  cr.Usage.CompletionDetails.ReasoningTokens,
	}
	if len(cr.Choices) == 0 {
		return "", usage, fmt.Errorf("junkpurge llm: chat: empty choices; body=%s", truncate(string(raw), 200))
	}
	return cr.Choices[0].Message.Content, usage, nil
}

func (j *ollamaJudge) observeRequest(processing, outcome string, usage tokenUsage) {
	if j.metrics == nil {
		return
	}
	j.metrics.llmRequests.WithLabelValues(processing, j.model, outcome).Inc()
	j.metrics.observeProviderUsage(processing, j.model, usage)
}

func (j *ollamaJudge) observeFailure(processing string, usage tokenUsage, err error) {
	j.observeRequest(processing, "error", usage)
	if j.metrics != nil {
		j.metrics.llmFailures.WithLabelValues(
			processing,
			j.model,
			llmFailureReason(err),
		).Inc()
	}
}

func (j *ollamaJudge) doJSON(ctx context.Context, url string, body any) ([]byte, error) {
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, &llmRequestError{
			reason: llmFailureRequestBuild,
			cause:  err,
		}
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, &llmRequestError{
			reason: llmFailureRequestBuild,
			cause:  err,
		}
	}
	req.Header.Set("Content-Type", "application/json")
	// Bearer auth for hosted OpenAI (api.openai.com). Omitted when empty so a
	// self-hosted/local endpoint isn't sent a bogus "Bearer " header.
	if j.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+j.apiKey)
	}
	// The production provider installs a durable budget for every hosted chat
	// route. The strict data-sharing route additionally requires one even for
	// standalone construction, so a wiring regression cannot create unbounded
	// complimentary-or-paid egress.
	if j.apiStyle == "chat" && (j.openAIDataSharing || j.budget != nil) {
		if j.budget == nil {
			return nil, &llmRequestError{
				reason:      llmFailureBudgetUnavailable,
				unavailable: true,
				detail:      "durable call budget is not configured",
			}
		}
		allowed, reserveErr := j.budget.Reserve(
			ctx, j.dailyCallLimit, j.monthlyCallLimit,
		)
		if reserveErr != nil {
			return nil, &llmRequestError{
				reason:      llmFailureBudgetUnavailable,
				unavailable: true,
				cause:       reserveErr,
			}
		}
		if !allowed {
			return nil, &llmRequestError{
				reason:      llmFailureBudgetExhausted,
				unavailable: true,
				detail:      "daily or monthly call allowance exhausted",
			}
		}
	}
	resp, err := j.http.Do(req)
	if err != nil {
		reason := llmFailureTransport
		switch {
		case errors.Is(err, context.Canceled):
			reason = llmFailureCanceled
		case errors.Is(err, context.DeadlineExceeded):
			reason = llmFailureTimeout
		}
		return nil, &llmRequestError{
			reason:      reason,
			unavailable: true,
			cause:       err,
		}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &llmRequestError{
			reason:      llmFailureResponseRead,
			unavailable: true,
			cause:       err,
		}
	}
	if resp.StatusCode/100 != 2 {
		detail := fmt.Sprintf("http %d: %s", resp.StatusCode, truncate(string(raw), 200))
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, &llmRequestError{
				reason:      llmFailureRateLimited,
				unavailable: true,
				detail:      detail,
			}
		}
		if resp.StatusCode/100 == 5 {
			return nil, &llmRequestError{
				reason:      llmFailureServerError,
				unavailable: true,
				detail:      detail,
			}
		}
		return nil, &llmRequestError{
			reason: llmFailureRequestRejected,
			detail: detail,
		}
	}
	return raw, nil
}

// parseJudgment tolerates a leading <think>...</think> block (qwen) and a
// fenced code block, then requires the complete JSON decision contract. A
// malformed reply is an error rather than a successful "unsure" judgment so
// it is observable and can be retried; it must never be promoted into an
// action merely by clamping an invalid confidence.
func parseJudgment(text string) (Judgment, error) {
	s := stripReplyWrapping(text)
	if s == "" {
		return Judgment{}, errors.New("junkpurge llm: empty content")
	}
	var raw struct {
		Verdict    *string  `json:"verdict"`
		Confidence *float64 `json:"confidence"`
	}
	decoder := json.NewDecoder(strings.NewReader(s))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return Judgment{}, fmt.Errorf("junkpurge llm: verdict: %w; text=%s", err, truncate(s, 200))
	}
	if err := requireEOF(decoder); err != nil {
		return Judgment{}, fmt.Errorf(
			"junkpurge llm: verdict: %w; text=%s",
			err,
			truncate(s, 200),
		)
	}
	return validateRawJudgment(raw.Verdict, raw.Confidence)
}

// stripReplyWrapping removes a leading <think>...</think> block (qwen) and a
// fenced code block, leaving the JSON payload.
func stripReplyWrapping(text string) string {
	s := strings.TrimSpace(text)
	if strings.HasPrefix(s, "<think>") {
		if end := strings.Index(s, "</think>"); end >= 0 {
			s = strings.TrimSpace(s[end+len("</think>"):])
		}
	}
	if strings.HasPrefix(s, "```") {
		if nl := strings.IndexByte(s, '\n'); nl > 0 {
			s = s[nl+1:]
		}
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "```"))
	}
	return s
}

// requireEOF rejects trailing JSON values after the decoded document.
func requireEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return errors.New("multiple JSON values")
	}
	return err
}

// validateRawJudgment applies the shared decision-contract validation: both
// fields present, confidence finite in [0,1], verdict in the enum. A defect
// is an error rather than a clamped "unsure" so it stays observable and can
// never be promoted into an action.
func validateRawJudgment(verdict *string, confidence *float64) (Judgment, error) {
	if verdict == nil || confidence == nil {
		return Judgment{}, errors.New(
			"junkpurge llm: verdict: missing or null verdict field",
		)
	}
	if math.IsNaN(*confidence) || math.IsInf(*confidence, 0) ||
		*confidence < 0 || *confidence > 1 {
		return Judgment{}, errors.New(
			"junkpurge llm: verdict: confidence must be finite and in [0,1]",
		)
	}
	v := Judgment{
		Verdict:    strings.ToLower(strings.TrimSpace(*verdict)),
		Confidence: *confidence,
	}
	switch v.Verdict {
	case verdictJunk, verdictRealMangled, verdictRealAbsent, verdictUnsure:
	default:
		return Judgment{}, fmt.Errorf(
			"junkpurge llm: verdict: unsupported verdict %q",
			v.Verdict,
		)
	}
	return v, nil
}

// parseBatchJudgments decodes the grouped reply: a JSON array of exactly n
// {"i","verdict","confidence"} objects whose i values are a permutation of
// 1..n. ANY defect — wrong count, missing/duplicate/out-of-range index, or
// one invalid item — fails the whole group; the caller falls back to
// per-name calls, so a partially-usable grouped reply is never half-acted
// on (a misaligned index set is indistinguishable from swapped verdicts).
func parseBatchJudgments(text string, n int) ([]Judgment, error) {
	s := stripReplyWrapping(text)
	if s == "" {
		return nil, errors.New("junkpurge llm: empty content")
	}
	var raw []struct {
		I          *int     `json:"i"`
		Verdict    *string  `json:"verdict"`
		Confidence *float64 `json:"confidence"`
	}
	decoder := json.NewDecoder(strings.NewReader(s))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("junkpurge llm: grouped verdict: %w; text=%s", err, truncate(s, 200))
	}
	if err := requireEOF(decoder); err != nil {
		return nil, fmt.Errorf("junkpurge llm: grouped verdict: %w; text=%s", err, truncate(s, 200))
	}
	if len(raw) != n {
		return nil, fmt.Errorf(
			"junkpurge llm: grouped verdict: got %d items, want %d", len(raw), n,
		)
	}
	out := make([]Judgment, n)
	seen := make([]bool, n)
	for _, item := range raw {
		if item.I == nil || *item.I < 1 || *item.I > n {
			return nil, fmt.Errorf(
				"junkpurge llm: grouped verdict: index out of range 1..%d", n,
			)
		}
		if seen[*item.I-1] {
			return nil, fmt.Errorf(
				"junkpurge llm: grouped verdict: duplicate index %d", *item.I,
			)
		}
		seen[*item.I-1] = true
		jm, err := validateRawJudgment(item.Verdict, item.Confidence)
		if err != nil {
			return nil, err
		}
		out[*item.I-1] = jm
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "...[truncated]"
	}
	return s
}
