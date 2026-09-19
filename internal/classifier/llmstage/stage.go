package llmstage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/evidence"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/llmprovider"
	"github.com/spencercnorton/bitagent/internal/model"
	"go.uber.org/zap"
)

// PrivacyStore is the interface the stage calls to check whether a
// torrent originated from a private tracker or qB category. Any such
// match hard-blocks the LLM call — private-tracker content never
// leaves the host.
type PrivacyStore interface {
	IsPrivateInfoHash(ctx context.Context, infoHash []byte) (bool, error)
}

// Decision is the cached result of a single LLM call. The Source
// field records where the answer came from so metrics can separate
// cache hits from fresh calls.
type Decision struct {
	MediaType  evidence.MediaType
	Confidence float64
	receipt    llmcapture.ResultReceipt
}

// Stage wraps an inner Runner. The wrapper only considers unknown, unattached
// results after a successful workflow (shadow only) or an explicit ErrUnmatched.
// The core workflow consumes unmatched actions internally, so an unresolved type is
// normally returned with no error. Known types, attached identities, deletion
// and other workflow errors remain authoritative. Shadow returns the exact
// inner result and error after observing the audited decision.
type Stage struct {
	cfg        Config
	inner      classifier.Runner
	privacy    PrivacyStore
	cache      *lruCache
	metrics    *Metrics
	logger     *zap.SugaredLogger
	http       *http.Client
	admission  Admission
	slots      chan struct{}
	retryAfter atomic.Int64
}

// NewStage constructs the stage. It does not perform I/O; the HTTP
// client is prebuilt with the configured timeout.
func NewStage(
	cfg Config,
	inner classifier.Runner,
	privacy PrivacyStore,
	metrics *Metrics,
	logger *zap.SugaredLogger,
	admission ...Admission,
) *Stage {
	cacheSize := cfg.CacheSize
	if cacheSize <= 0 || cacheSize > 100000 {
		cacheSize = 1
	}
	s := &Stage{
		cfg:     cfg,
		inner:   inner,
		privacy: privacy,
		cache:   newLRU(cacheSize),
		metrics: metrics,
		logger:  logger.Named("llmstage"),
		http:    &http.Client{Timeout: cfg.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
	if len(admission) == 1 {
		s.admission = admission[0]
	}
	for name, value := range map[string]int{"daily_call_limit": cfg.DailyCallLimit, "monthly_call_limit": cfg.MonthlyCallLimit, "max_concurrent_calls": cfg.MaxConcurrentCalls, "max_request_bytes": cfg.MaxRequestBytes, "max_output_tokens": cfg.MaxOutputTokens} {
		metrics.config.WithLabelValues(name).Set(float64(value))
	}
	metrics.config.WithLabelValues("min_confidence").Set(cfg.MinConfidence)
	metrics.config.WithLabelValues("enabled").Set(0)
	metrics.config.WithLabelValues("live").Set(0)
	if cfg.Enabled {
		metrics.config.WithLabelValues("enabled").Set(1)
	}
	if cfg.EnableLive {
		metrics.config.WithLabelValues("live").Set(1)
	}
	metrics.callsTotal.WithLabelValues(cfg.Model)
	for _, kind := range []string{"input", "output", "cached_input", "reasoning"} {
		metrics.tokensTotal.WithLabelValues(cfg.Model, kind)
	}
	metrics.usageMissingTotal.WithLabelValues(cfg.Model)
	// Do not allocate an operator-controlled unbounded channel.
	if cfg.MaxConcurrentCalls > 0 && cfg.MaxConcurrentCalls <= 16 {
		s.slots = make(chan struct{}, cfg.MaxConcurrentCalls)
	}
	return s
}

// EvalMatch delegates to the inner runner. This llmstage decorator is a
// type-only fallback unrelated to the TMDB matcher the eval path measures.
func (s *Stage) EvalMatch(ctx context.Context, t model.Torrent, ct model.NullContentType) (classifier.MatchDecision, error) {
	return s.inner.EvalMatch(ctx, t, ct)
}

// Run implements classifier.Runner. The full inner workflow, including its
// deletion policy, finishes before unknown-type fallback admission is evaluated.
func (s *Stage) Run(
	ctx context.Context,
	workflow string,
	flags classifier.Flags,
	t model.Torrent,
) (classification.Result, error) {
	innerResult, innerErr := s.inner.Run(ctx, workflow, flags, t)

	if enabled, ok := flags["llm_stage_enabled"].(bool); ok && !enabled {
		s.metrics.gateRejectsTotal.WithLabelValues("runtime_flag").Inc()
		return innerResult, innerErr
	}
	if !s.cfg.Enabled {
		return innerResult, innerErr
	}
	if innerErr != nil && !onlyUnmatched(innerErr) {
		return innerResult, innerErr
	}
	if innerResult.ContentType.Valid || innerResult.Content != nil {
		return innerResult, innerErr
	}
	// A successful core workflow has already applied its deletion/content
	// policy to the unknown type. Retyping after that point could bypass it.
	// Natural nil-error survivors are therefore shadow-only until a separately
	// reviewed policy-aware application point exists, regardless of category.
	if innerErr == nil && s.cfg.EnableLive {
		s.metrics.gateRejectsTotal.WithLabelValues("policy_live_unavailable").Inc()
		return innerResult, innerErr
	}

	s.metrics.invocationsTotal.WithLabelValues("unmatched").Inc()

	// The metainfo private flag is authoritative and requires no evidence
	// lookup. Check it before plausibility, cache, or the evidence-backed gate
	// so missing/stale evidence can never expose a private release name or file
	// list to the external model. The deterministic inner result is preserved.
	if t.Private {
		s.metrics.gateRejectsTotal.WithLabelValues("privacy").Inc()
		return innerResult, innerErr
	}
	if err := s.cfg.Validate(); err != nil {
		s.metrics.gateRejectsTotal.WithLabelValues("config").Inc()
		return innerResult, innerErr
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	if !s.plausibleMedia(t) {
		return innerResult, innerErr
	}

	// Privacy gate. If the store errors (DB down) we MUST fail
	// closed — better to let the CEL result stand than to risk
	// leaking private-tracker content.
	if s.privacy == nil {
		s.metrics.gateRejectsTotal.WithLabelValues("privacy").Inc()
		return innerResult, innerErr
	} else {
		isPriv, err := s.privacy.IsPrivateInfoHash(ctx, t.InfoHash.Bytes())
		if err != nil {
			s.metrics.gateRejectsTotal.WithLabelValues("privacy").Inc()
			s.logger.Warnw("privacy gate errored; failing closed", "err", err)
			return innerResult, innerErr
		}
		if isPriv {
			s.metrics.gateRejectsTotal.WithLabelValues("privacy").Inc()
			return innerResult, innerErr
		}
	}

	decision, err := s.classify(ctx, t)
	if err != nil {
		return innerResult, innerErr
	}
	if err := s.recordDecision(ctx, t, decision, false); err != nil {
		return innerResult, innerErr
	}

	s.metrics.decisionsTotal.WithLabelValues(string(decision.MediaType)).Inc()

	if !s.cfg.EnableLive {
		s.metrics.shadowSkippedTotal.Inc()
		return innerResult, innerErr
	}
	if decision.Confidence < s.cfg.MinConfidence {
		return innerResult, innerErr
	}
	contentType, ok := mediaTypeToContentType(decision.MediaType)
	if !ok {
		return innerResult, innerErr
	}

	s.metrics.liveAppliedTotal.WithLabelValues(string(decision.MediaType)).Inc()
	result := innerResult
	result.ContentType = model.NewNullContentType(contentType)
	return result, nil
}

// onlyUnmatched permits the sentinel and ordinary single-error wrappers such
// as classification.RuntimeError. A joined error may contain a deletion or
// runtime failure alongside ErrUnmatched, so errors.Is alone is insufficient.
// Multi-error chains and errors that only claim a match through Is are rejected.
func onlyUnmatched(err error) bool {
	for err != nil {
		if err == classification.ErrUnmatched {
			return true
		}
		wrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = wrapped.Unwrap()
	}
	return false
}

// plausibleMedia bounds what we send to the LLM. Oversized dumps
// (linux distros, data archives) and pure-archive torrents rarely
// classify well and waste tokens. We also reject torrents with file
// counts in the thousands.
func (s *Stage) plausibleMedia(t model.Torrent) bool {
	if t.Size == 0 {
		s.metrics.gateRejectsTotal.WithLabelValues("size").Inc()
		return false
	}
	if int64(t.Size) < s.cfg.MinTotalSizeBytes || int64(t.Size) > s.cfg.MaxTotalSizeBytes {
		s.metrics.gateRejectsTotal.WithLabelValues("size").Inc()
		return false
	}
	if len(t.Files) > s.cfg.MaxFiles {
		s.metrics.gateRejectsTotal.WithLabelValues("files").Inc()
		return false
	}
	if !anyMediaExt(t) {
		s.metrics.gateRejectsTotal.WithLabelValues("plausibility").Inc()
		return false
	}
	return true
}

// classify is the cache-check + HTTP call. Returns a Decision or
// error. Callers map error to a gate-reject metric.
func (s *Stage) classify(ctx context.Context, t model.Torrent) (Decision, error) {
	if err := s.admissionReady(); err != nil {
		return Decision{}, err
	}
	body := buildBoundedRequestBody(s.cfg, t)
	if len(body) > s.cfg.MaxRequestBytes {
		s.metrics.gateRejectsTotal.WithLabelValues("request_size").Inc()
		return Decision{}, errors.New("type request exceeds byte limit")
	}
	key := s.cacheKey(t)
	if d, ok := s.cache.Get(key); ok {
		s.metrics.cacheHitsTotal.Inc()
		return d, nil
	}
	s.metrics.cacheMissesTotal.Inc()

	decision, err := s.callOpenAI(ctx, t, body)
	if err != nil {
		return Decision{}, err
	}
	s.cache.Put(key, decision)
	return decision, nil
}

// Cache identity binds the exact request, source and application policy.
// A receipt must never be reused for a different infohash or policy.
func (s *Stage) cacheKey(t model.Torrent) string {
	h := sha256.New()
	_, _ = h.Write(t.InfoHash.Bytes())
	_, _ = h.Write(buildBoundedRequestBody(s.cfg, t))
	_, _ = fmt.Fprintf(h, "|%s|%t|%g", s.cfg.Endpoint, s.cfg.EnableLive, s.cfg.MinConfidence)
	return hex.EncodeToString(h.Sum(nil))
}

// callOpenAI issues a single chat.completions call with a compact
// prompt. Response schema is strict JSON with two fields; anything
// else is rejected at decode time.
//
// Per feedback_openai_gpt5_temperature.md we do NOT send the
// temperature field — gpt-5.* models 400 when it is set.
// chatRequest + chatResponse are the minimal shape we need for
// gpt-5.4-nano via /v1/chat/completions. No temperature. No
// reasoning field requested — wastes tokens and leaks context.
type chatRequest struct {
	Model    string    `json:"model"`
	Messages []message `json:"messages"`
	// JSON mode is not schema validation; parseResponse validates the answer.
	ResponseFormat struct {
		Type string `json:"type"`
	} `json:"response_format"`
	// max_completion_tokens is the current OpenAI name; small
	// number because the response is only {category, confidence}.
	MaxCompletionTokens int             `json:"max_completion_tokens"`
	Provider            *providerPolicy `json:"provider,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	Store               *bool           `json:"store,omitempty"`
}

type providerPolicy struct {
	Order             []string `json:"order"`
	Only              []string `json:"only"`
	AllowFallbacks    bool     `json:"allow_fallbacks"`
	RequireParameters bool     `json:"require_parameters"`
	DataCollection    string   `json:"data_collection"`
	ZDR               bool     `json:"zdr"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content string `json:"content"`
			Refusal string `json:"refusal"`
		} `json:"message"`
	} `json:"choices"`
}

type chatUsageResponse struct {
	Usage *struct {
		PromptTokens     *int64 `json:"prompt_tokens"`
		CompletionTokens *int64 `json:"completion_tokens"`
		PromptDetails    struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionDetails struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage,omitempty"`
}

type llmAnswer struct {
	Category   string   `json:"category"`
	Confidence *float64 `json:"confidence"`
}

const systemPrompt = `You classify torrents by media type from the title and file list. Reply ONLY with compact JSON: {"category":"movie|tv|music|audiobook|book|unknown","confidence":0.0-1.0}. Confidence is your self-assessment in [0,1]. Use "unknown" when unsure — do not guess.`

func buildRequestBody(model, promptVersion string, t model.Torrent) []byte {
	cfg := NewDefaultConfig()
	cfg.Model, cfg.PromptVersion = model, promptVersion
	return buildBoundedRequestBody(cfg, t)
}

func buildBoundedRequestBody(cfg Config, t model.Torrent) []byte {
	userPrompt := renderUserPrompt(cfg.PromptVersion, t)
	req := chatRequest{
		Model: cfg.Model,
		Messages: []message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		MaxCompletionTokens: cfg.MaxOutputTokens,
	}
	req.ResponseFormat.Type = "json_object"
	if cfg.OpenaiDataSharing {
		req.ReasoningEffort = llmprovider.OpenAIReasoningEffort
		store := false
		req.Store = &store
	}
	if cfg.OpenrouterProvider != "" {
		req.Provider = &providerPolicy{
			Order: []string{cfg.OpenrouterProvider}, Only: []string{cfg.OpenrouterProvider},
			AllowFallbacks: false, RequireParameters: true, DataCollection: "deny", ZDR: true,
		}
	}
	b, _ := json.Marshal(req)
	return b
}

func renderUserPrompt(promptVersion string, t model.Torrent) string {
	var sb strings.Builder
	sb.WriteString("prompt_version: ")
	sb.WriteString(promptVersion)
	sb.WriteString("\ntitle: ")
	sb.WriteString(t.Name)
	sb.WriteString("\ntotal_size_bytes: ")
	fmt.Fprintf(&sb, "%d", t.Size)
	sb.WriteString("\nfile_count: ")
	fmt.Fprintf(&sb, "%d", len(t.Files))
	sb.WriteString("\nfiles:\n")
	const maxFiles = 50
	for i, f := range t.Files {
		if i >= maxFiles {
			sb.WriteString("... (truncated)\n")
			break
		}
		fmt.Fprintf(&sb, "  - %d\t%s\n", f.Size, f.Path)
	}
	return sb.String()
}

func parseResponse(raw []byte) (Decision, error) {
	var outer chatResponse
	if err := json.Unmarshal(raw, &outer); err != nil {
		return Decision{}, fmt.Errorf("decode chat response: %w", err)
	}
	if len(outer.Choices) != 1 || outer.Choices[0].FinishReason != "stop" || outer.Choices[0].Message.Refusal != "" {
		return Decision{}, errors.New("incomplete, refused or ambiguous chat response")
	}
	var ans llmAnswer
	decoder := json.NewDecoder(strings.NewReader(outer.Choices[0].Message.Content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&ans); err != nil {
		return Decision{}, fmt.Errorf("decode llm answer: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Decision{}, errors.New("trailing llm answer data")
	}
	mt := normalizeMediaType(ans.Category)
	if mt == "" {
		return Decision{}, fmt.Errorf("unknown category %q", ans.Category)
	}
	if ans.Confidence == nil || *ans.Confidence < 0 || *ans.Confidence > 1 {
		return Decision{}, errors.New("missing or out-of-range confidence")
	}
	return Decision{MediaType: mt, Confidence: *ans.Confidence}, nil
}

func normalizeMediaType(s string) evidence.MediaType {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "movie":
		return evidence.MediaTypeMovie
	case "tv", "tv_show", "show":
		return evidence.MediaTypeTV
	case "music":
		return evidence.MediaTypeMusic
	case "audiobook":
		return evidence.MediaTypeAudiobook
	case "book", "ebook":
		return evidence.MediaTypeBook
	case "unknown":
		return evidence.MediaTypeUnknown
	}
	return ""
}

func mediaTypeToContentType(mt evidence.MediaType) (model.ContentType, bool) {
	switch mt {
	case evidence.MediaTypeMovie:
		return model.ContentTypeMovie, true
	case evidence.MediaTypeTV:
		return model.ContentTypeTvShow, true
	case evidence.MediaTypeMusic:
		return model.ContentTypeMusic, true
	case evidence.MediaTypeAudiobook:
		return model.ContentTypeAudiobook, true
	case evidence.MediaTypeBook:
		return model.ContentTypeEbook, true
	}
	return "", false
}

// anyMediaExt reports whether the torrent's file list includes at
// least one media-shaped extension. Strict set — things like .iso,
// .bin, .exe are excluded because they classify poorly.
var mediaExts = map[string]struct{}{
	"mkv": {}, "mp4": {}, "m4v": {}, "avi": {}, "mov": {}, "webm": {},
	"mp3": {}, "flac": {}, "aac": {}, "m4a": {}, "wav": {}, "ogg": {}, "opus": {},
	"epub": {}, "pdf": {}, "mobi": {}, "azw3": {},
}

func anyMediaExt(t model.Torrent) bool {
	if t.Extension.Valid {
		if _, ok := mediaExts[strings.ToLower(t.Extension.String)]; ok {
			return true
		}
	}
	for _, f := range t.Files {
		if !f.Extension.Valid {
			continue
		}
		if _, ok := mediaExts[strings.ToLower(f.Extension.String)]; ok {
			return true
		}
	}
	return false
}

// fileListHash returns a stable hash of the sorted (path,size) pairs
// in the torrent. Used as part of the cache key — different file
// layouts under the same title should hit different cache entries.
func fileListHash(t model.Torrent) string {
	if len(t.Files) == 0 {
		return "no-files"
	}
	h := sha256.New()
	for _, f := range t.Files {
		fmt.Fprintf(h, "%d\t%s\n", f.Size, strings.ToLower(f.Path))
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// sizeBucket maps a byte total to a coarse bucket so slightly-different
// sizes at the same tier don't produce cache misses.
func sizeBucket(n int64) int {
	switch {
	case n < 100*1024*1024:
		return 1
	case n < 1*1024*1024*1024:
		return 2
	case n < 5*1024*1024*1024:
		return 3
	case n < 20*1024*1024*1024:
		return 4
	case n < 80*1024*1024*1024:
		return 5
	}
	return 6
}
