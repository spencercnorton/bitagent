package llmmatch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/llmprovider"
	"github.com/spencercnorton/bitagent/internal/model"
	"go.uber.org/zap"
)

// PrivacyStore hard-blocks the LLM call for private-tracker content. Mirrors
// llmstage.PrivacyStore; satisfied by *evidence.Store.
type PrivacyStore interface {
	IsPrivateInfoHash(ctx context.Context, infoHash []byte) (bool, error)
}

// Client performs the two LLM stages plus gating and caching. It is safe to
// construct when disabled — Enabled()/Live() gate all work.
type Client struct {
	cfg     Config
	privacy PrivacyStore
	cache   *lruCache
	metrics *Metrics
	logger  *zap.SugaredLogger
	http    *http.Client
	// httpLong has no client-level timeout — batch extract calls scale their
	// deadline with batch size via a request context, which a fixed
	// client Timeout would override.
	httpLong           *http.Client
	capture            llmcapture.Capturer
	budget             CallBudget
	slots              chan struct{}
	budgetBlockedUntil atomic.Int64
}

func NewClient(cfg Config, privacy PrivacyStore, metrics *Metrics, logger *zap.SugaredLogger) *Client {
	return NewClientWithCapture(cfg, privacy, metrics, logger, nil)
}

func NewClientWithCapture(
	cfg Config,
	privacy PrivacyStore,
	metrics *Metrics,
	logger *zap.SugaredLogger,
	capture llmcapture.Capturer,
) *Client {
	return NewClientWithBudget(cfg, privacy, metrics, logger, capture, &memoryCallBudget{now: time.Now})
}

// NewClientWithBudget is used by production with a durable allowance. A nil
// budget fails closed at dispatch, never silently turning off the spend fuse.
func NewClientWithBudget(cfg Config, privacy PrivacyStore, metrics *Metrics,
	logger *zap.SugaredLogger, capture llmcapture.Capturer, budget CallBudget,
) *Client {
	metrics.configure(cfg)
	return &Client{
		cfg:      cfg,
		privacy:  privacy,
		cache:    newLRU(cfg.CacheSize),
		metrics:  metrics,
		logger:   logger.Named("llmmatch"),
		http:     &http.Client{Timeout: cfg.Timeout},
		httpLong: &http.Client{},
		capture:  capture,
		budget:   budget,
		slots:    make(chan struct{}, max(1, cfg.MaxConcurrentCalls)),
	}
}

// IsolateBudget swaps the shared call ledger for a private allowance of n
// provider calls in this process. For measurement commands only: matcher-eval
// at the production limits (15/day, 450/month, one ledger shared with the
// crawler) either stalls after a handful of rows or spends the crawler's month.
// Never call it from a worker.
func (c *Client) IsolateBudget(n int) {
	b := &capBudget{}
	b.left.Store(int64(n))
	c.budget = b
}

func (c *Client) Enabled() bool            { return c != nil && c.cfg.Enabled }
func (c *Client) Live() bool               { return c != nil && c.cfg.EnableLive }
func (c *Client) Model() string            { return c.cfg.Model }
func (c *Client) MaxCandidates() int       { return c.cfg.MaxCandidates }
func (c *Client) MinConfidence() float64   { return c.cfg.MinConfidence }
func (c *Client) RequireSourceTitle() bool { return c.cfg.RequireSourceTitle }

// AnimeEnglishOK decides whether an anime extraction has an acceptable English
// track. Non-anime always passes. Policy: reject a confirmed "none" (a raw);
// "unknown" passes (don't drop on uncertainty — most anime torrents are English
// fansubs the model reads as "sub"); "sub" passes unless AnimeAllowSubOnly is
// off; "dub" always passes.
func (c *Client) AnimeEnglishOK(ext Extraction) bool {
	if !ext.IsAnime || !c.cfg.AnimeRequireEnglish {
		return true
	}
	switch ext.English {
	case EnglishNone:
		return false
	case EnglishSub:
		return c.cfg.AnimeAllowSubOnly
	default: // dub, unknown, or unrecognised
		return true
	}
}

// RecordExtractGate bumps the gate-reject counter for a post-extract content
// gate (reason: "pack" | "adult").
func (c *Client) RecordExtractGate(reason string) {
	c.metrics.gateRejects.WithLabelValues(reason).Inc()
}

// RecordRerankGate bumps the gate-reject counter for a post-rerank identity
// invariant. Keeping this separate from the model's chosen/not-chosen metric
// makes an overconfident but incompatible choice visible to operators.
func (c *Client) RecordRerankGate(reason string) {
	c.metrics.gateRejects.WithLabelValues(reason).Inc()
}

// RecordCandidateSource bumps the candidate-set counter ("local" | "api").
func (c *Client) RecordCandidateSource(source string) {
	c.metrics.candidatesTotal.WithLabelValues(source).Inc()
}

// RecordAnime bumps the anime observation counter (by english track + whether
// it was kept or rejected on the English gate).
func (c *Client) RecordAnime(english string, kept bool) {
	outcome := "kept"
	if !kept {
		outcome = "rejected"
	}
	if english == "" {
		english = EnglishUnknown
	}
	c.metrics.animeTotal.WithLabelValues(english, outcome).Inc()
}

// RecordMatch bumps the chosen-match counter (mode=shadow|live).
func (c *Client) RecordMatch(live, isTV bool) {
	mode := "shadow"
	if live {
		mode = "live"
	}
	mt := "movie"
	if isTV {
		mt = "tv"
	}
	c.metrics.matchesTotal.WithLabelValues(mode, mt).Inc()
}

// nativePrivateBlocked enforces the torrent's metainfo private flag without
// consulting evidence. Evidence can be absent or stale; a native private flag
// is authoritative and must prevent every outbound model request on its own.
func (c *Client) nativePrivateBlocked(t model.Torrent) bool {
	if !t.Private {
		return false
	}
	c.metrics.gateRejects.WithLabelValues("privacy").Inc()
	return true
}

// Allow runs the plausibility + privacy gates. Fails closed on privacy error.
func (c *Client) Allow(ctx context.Context, t model.Torrent) bool {
	if c.nativePrivateBlocked(t) {
		return false
	}
	if t.Size == 0 || int64(t.Size) < c.cfg.MinTotalSizeBytes || int64(t.Size) > c.cfg.MaxTotalSizeBytes {
		c.metrics.gateRejects.WithLabelValues("size").Inc()
		return false
	}
	if c.cfg.MaxFiles > 0 && len(t.Files) > c.cfg.MaxFiles {
		c.metrics.gateRejects.WithLabelValues("files").Inc()
		return false
	}
	if !anyMediaExt(t) {
		c.metrics.gateRejects.WithLabelValues("plausibility").Inc()
		return false
	}
	if c.privacy != nil {
		isPriv, err := c.privacy.IsPrivateInfoHash(ctx, t.InfoHash.Bytes())
		if err != nil {
			c.metrics.gateRejects.WithLabelValues("privacy").Inc()
			c.logger.Warnw("privacy gate errored; failing closed", "err", err)
			return false
		}
		if isPriv {
			c.metrics.gateRejects.WithLabelValues("privacy").Inc()
			return false
		}
	}
	return true
}

// extractKey is the cache key for a stage-1 decision. ExtractMany must write
// under exactly this key so a later Extract for the same name is a pure hit.
func (c *Client) extractKey(name string) string {
	return "extract|" + c.cfg.Model + "|" + c.cfg.PromptVersion + "|" + strings.ToLower(name)
}

// Extract is stage 1: read the canonical identity from the release name.
func (c *Client) Extract(ctx context.Context, t model.Torrent) (Extraction, error) {
	if c.nativePrivateBlocked(t) {
		return Extraction{}, nil
	}
	key := c.extractKey(t.Name)
	if v, ok := c.cache.Get(key); ok {
		c.metrics.cacheHits.Inc()
		return v.(Extraction), nil
	}
	c.metrics.cacheMisses.Inc()
	if c.budgetCoolingDown() {
		return Extraction{}, ErrCallBudget
	}

	ext, err := c.callExtract(ctx, t)
	if err != nil {
		if errors.Is(err, llmcapture.ErrPrivacyBlocked) {
			c.metrics.gateRejects.WithLabelValues("privacy").Inc()
			return Extraction{}, err
		}
		c.metrics.extractTotal.WithLabelValues("error").Inc()
		return Extraction{}, err
	}
	if !ext.OK || strings.TrimSpace(ext.Title) == "" {
		c.metrics.extractTotal.WithLabelValues("empty").Inc()
		ext.OK = false
	} else {
		c.metrics.extractTotal.WithLabelValues("ok").Inc()
	}
	c.cache.Put(key, ext)
	return ext, nil
}

// Rerank is stage 2: choose the correct TMDB id among candidates, or 0. It
// accepts the full torrent so the native private flag remains available at
// this second outbound-model boundary.
func (c *Client) Rerank(ctx context.Context, t model.Torrent, ext Extraction, cands []Candidate) (int64, float64, error) {
	return c.RerankFrom(
		ctx,
		t,
		ext,
		strings.EqualFold(strings.TrimSpace(ext.Type), "tv"),
		cands,
		llmcapture.CandidateSourceAPI,
	)
}

// RerankFrom is the production stage-2 boundary. source records whether the
// immutable ordered candidate list came from the local mirror or TMDB API.
func (c *Client) RerankFrom(
	ctx context.Context,
	t model.Torrent,
	ext Extraction,
	isTV bool,
	cands []Candidate,
	source llmcapture.CandidateSource,
) (int64, float64, error) {
	return c.RerankForMediaType(ctx, t, ext, "", isTV, cands, source)
}

// RerankForMediaType is the production stage-2 boundary with the independent
// parser title used by the source-identity gate. parsedTitle is audit-only: it
// is captured for exact replay but never rendered into the provider request.
// RerankFrom remains the compatibility surface for callers without parser
// evidence.
func (c *Client) RerankForMediaType(
	ctx context.Context,
	t model.Torrent,
	ext Extraction,
	parsedTitle string,
	isTV bool,
	cands []Candidate,
	source llmcapture.CandidateSource,
) (int64, float64, error) {
	if c.nativePrivateBlocked(t) {
		return 0, 0, nil
	}
	name := t.Name
	// Cache evidence may be replayed after a failed final-decision write. Bind
	// it to the complete source/policy context, never just name and candidate IDs.
	keyContext := map[string]any{
		"name": name, "info_hash": t.InfoHash.Bytes(), "extraction": ext,
		"parsed_title": parsedTitle, "is_tv": isTV, "source": source,
		"candidates": rerankCaptureCandidates(cands), "model": c.cfg.Model,
		"prompt_version": c.cfg.PromptVersion, "endpoint": c.cfg.Endpoint,
		"provider":             c.cfg.OpenrouterProvider,
		"min_confidence":       c.cfg.MinConfidence,
		"require_source_title": c.cfg.RequireSourceTitle, "live": c.Live(),
	}
	if c.cfg.OpenaiDataSharing {
		keyContext["openai_data_sharing"] = true
	}
	keyInput, err := json.Marshal(keyContext)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid rerank cache context: %w", err)
	}
	keyDigest := sha256.Sum256(keyInput)
	key := "rerank|" + hex.EncodeToString(keyDigest[:])
	_, recordsResults := c.capture.(llmcapture.ResultRecorder)
	recordsResults = recordsResults && c.capture.Enabled()
	if recordsResults && llmcapture.ResultTraceFrom(ctx) == nil {
		ctx, _ = llmcapture.WithResultTrace(ctx)
	}
	if v, ok := c.cache.Get(key); ok {
		c.metrics.cacheHits.Inc()
		r := v.(rerankResult)
		if r.Receipt != nil {
			receipt := *r.Receipt
			receipt.FromCache = true
			llmcapture.ResultTraceFrom(ctx).RecordResult(llmcapture.TaskMatcherRerank, source, receipt)
		}
		return r.ID, r.Confidence, nil
	}
	c.metrics.cacheMisses.Inc()
	if c.budgetCoolingDown() {
		return 0, 0, ErrCallBudget
	}

	id, conf, err := c.callRerank(
		ctx, t, ext, parsedTitle, isTV, cands, source,
	)
	if err != nil {
		if errors.Is(err, llmcapture.ErrPrivacyBlocked) {
			c.metrics.gateRejects.WithLabelValues("privacy").Inc()
			return 0, 0, err
		}
		c.metrics.rerankTotal.WithLabelValues("error").Inc()
		return 0, 0, err
	}
	if id == 0 {
		c.metrics.rerankTotal.WithLabelValues("none").Inc()
	} else {
		c.metrics.rerankTotal.WithLabelValues("match").Inc()
		// This diagnostic is a model choice, not a final policy decision.
		// The bounded capture/result ledger records natural provider-backed
		// evidence; the classifier observer records final attachment guards.
		c.logger.Infow(
			"rerank chose",
			"name", name,
			"title", ext.Title,
			"candidate_title", rerankCandidateTitle(cands, id),
			"tmdb_id", id,
			"confidence", conf,
		)
	}
	cached := rerankResult{ID: id, Confidence: conf}
	if recordsResults {
		receipt, ok := llmcapture.ResultTraceFrom(ctx).Result(llmcapture.TaskMatcherRerank, source)
		if !ok || !receipt.FirstObservation {
			// A concurrent/restarted duplicate must not replace the first
			// response's retry authority or become unaudited cached evidence.
			return id, conf, nil
		}
		cached.Receipt = &receipt
	}
	c.cache.Put(key, cached)
	return id, conf, nil
}

type rerankResult struct {
	ID         int64
	Confidence float64
	Receipt    *llmcapture.ResultReceipt
}

func rerankCandidateTitle(candidates []Candidate, id int64) string {
	for _, candidate := range candidates {
		if candidate.ID == id {
			return candidate.Title
		}
	}
	return ""
}

// ── HTTP + prompts ────────────────────────────────────────────────────────

type chatRequest struct {
	Model          string    `json:"model"`
	Messages       []message `json:"messages"`
	ResponseFormat struct {
		Type string `json:"type"`
	} `json:"response_format"`
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

// extractRules is the per-item rule block shared verbatim between the
// single-name prompt (extractSystemPrompt) and the batch prompt
// (batchExtractSystemPrompt) so both stages make identical judgements and can
// share cache entries.
const extractRules = `Rules:
- title = the widely recognised English (canonical TMDB) title. Translate or de-alias foreign/alternate titles, expand abbreviations, strip site prefixes, resolution, codecs, and release/fansub groups. If you do not recognise a real title, set title to "" — never invent one.
- year = original theatrical/first-air year (0 if unknown).
- season/episode = 0 unless the name clearly encodes them.

ANIME (Japanese animation): set is_anime=true. Anime names are their own dialect — expect fansub group brackets like [SubsPlease] [Erai-raws] [HorribleSubs], romaji or Japanese titles, and ABSOLUTE episode numbering (a lone number after the title, e.g. "Show Name - 137", is an absolute episode, NOT a year; convert to season/episode only if you are confident, otherwise leave season=0, episode=<the number>). For title use the canonical title TMDB indexes (usually the official English title, e.g. "Shingeki no Kyojin" -> "Attack on Titan"; "Kimetsu no Yaiba" -> "Demon Slayer").

ENGLISH TRACK — judge the release's ENGLISH availability specifically:
- "dub"  : has an ENGLISH audio dub. Explicit signals: "English Dub", "ENG DUB", "Dubbed". "Dual Audio" / "Multi Audio" / "[Dual]" / "DUAL" mean an English dub ONLY when English is among the named AUDIO languages or no audio languages are named at all; when the named audio languages exclude English (e.g. "Dual Audio [Hindi+Jpn]"), there is no English dub — judge by the remaining signals (English subs present -> "sub", otherwise "none"). A subtitle-language list ("Multi-Sub: Spa, Fra", "Subs: Spanish, French") names SUBTITLE languages and never negates the English dub implied by Dual/Multi-Audio.
- "sub"  : Japanese audio with an English subtitle track. Signals: English fansub groups (SubsPlease, Erai-raws, HorribleSubs, Judas, etc.), "Eng Sub", "Multi-Sub", ".eng.srt"/".eng.ass" subtitle files.
- "none" : Japanese-only, no English audio or subtitles (a "raw"). Signals: "RAW", Japanese-only tags, or a bare Japanese/romaji title with no English group or dub/sub marker.
- "unknown" : you cannot tell.
For non-anime, set english to "unknown" unless a non-English audio is explicit.

PACKS (multi-film bundles): if the release bundles MULTIPLE different films — trilogy, duology, collection, filmography, "complete collection" of movies, or a year range spanning several films (e.g. "2001-2003", "2013-2019") — set is_pack=true (title may name the franchise). A multi-season or complete-series TV pack is NOT a pack: it still belongs to exactly one show, so set is_pack=false for those.

ADULT: if the release is pornographic — adult-site prefixes, performer names, explicit scene descriptions — set is_adult=true and title to "". Adult releases must never be matched to a mainstream movie or TV title, even when the name resembles one.`

const extractSystemPrompt = `You extract the canonical media identity from a torrent release name. Output ONLY compact JSON: {"title":string,"year":int,"type":"movie"|"tv","season":int,"episode":int,"is_anime":bool,"english":"dub"|"sub"|"none"|"unknown","is_pack":bool,"is_adult":bool}.

` + extractRules

const rerankSystemPrompt = `You match a torrent to the correct TMDB entry. You are given the raw release name and a numbered list of TMDB candidates (id, title, year, overview). Choose the single candidate the torrent is a release of. Output ONLY compact JSON: {"tmdb_id":int,"confidence":float}. confidence in [0,1]. Prefer an exact year match. Do NOT pick a sequel, remake, or different-year entry unless the release name clearly indicates it. If none of the candidates clearly match, output {"tmdb_id":0,"confidence":0}. A wrong match is worse than no match.`

func (c *Client) callExtract(ctx context.Context, t model.Torrent) (Extraction, error) {
	ctx = withPendingCapture(ctx)
	files := make([]string, 0, len(t.Files))
	for _, file := range t.Files {
		files = append(files, file.Path)
	}
	modelFiles := files
	if len(modelFiles) > 5 {
		modelFiles = []string{}
	}
	user := ExtractInput(t.Name, files)
	if err := c.captureRequest(
		ctx,
		t,
		llmcapture.TaskMatcherExtract,
		llmcapture.CandidateSourceNone,
		matcherContractID(c.cfg, "llmmatch-chat-extract-v1"),
		[]byte(contentfilter.EvaluationGroupKey(t.Name)),
		ExtractPrompt(),
		user,
		120,
		map[string]any{
			"release_name": t.Name,
			"file_paths":   modelFiles,
			"admission": map[string]any{
				"native_private":  false,
				"size_bytes":      t.Size,
				"file_count":      len(t.Files),
				"media_plausible": anyMediaExt(t),
			},
		},
	); err != nil {
		return Extraction{}, err
	}
	raw, err := c.call(ctx, "extract", ExtractPrompt(), user, 120)
	if err != nil {
		return Extraction{}, err
	}
	ext, err := decodeExtraction(raw)
	if err != nil {
		c.metrics.callErrors.WithLabelValues("extract", "decode").Inc()
		return Extraction{}, fmt.Errorf("decode extract: %w", err)
	}
	if err := normalizeAndValidateExtraction(&ext); err != nil {
		c.metrics.callErrors.WithLabelValues("extract", "decode").Inc()
		return Extraction{}, fmt.Errorf("decode extract: %w", err)
	}
	return ext, nil
}

// normalizeExtraction canonicalises model output in place — shared by the
// single and batch extract paths so cached entries are shape-identical.
func normalizeExtraction(ext *Extraction) {
	ext.Type = strings.ToLower(strings.TrimSpace(ext.Type))
	ext.English = strings.ToLower(strings.TrimSpace(ext.English))
	if ext.English == "" {
		ext.English = EnglishUnknown
	}
	ext.OK = strings.TrimSpace(ext.Title) != ""
}

// normalizeAndValidateExtraction is the action boundary shared by live
// single-item extraction, batched extraction, and production-fidelity
// evaluation. Values outside these bounds cannot identify real TMDB media and
// therefore fail closed before search or persistence.
func normalizeAndValidateExtraction(ext *Extraction) error {
	normalizeExtraction(ext)
	if !ext.OK {
		return nil
	}
	if len(ext.Title) > 1024 || strings.ContainsRune(ext.Title, '\x00') {
		return fmt.Errorf("title is outside the supported action contract")
	}
	if ext.Year != 0 && (ext.Year < 1800 || ext.Year > 2200) {
		return fmt.Errorf("year is outside 1800..2200 or 0")
	}
	if ext.Season < 0 || ext.Season > 100000 {
		return fmt.Errorf("season is outside 0..100000")
	}
	if ext.Episode < 0 || ext.Episode > 1000000 {
		return fmt.Errorf("episode is outside 0..1000000")
	}
	switch ext.English {
	case EnglishDub, EnglishSub, EnglishNone, EnglishUnknown:
	default:
		return fmt.Errorf("english is outside the supported action contract")
	}
	return nil
}

func (c *Client) callRerank(
	ctx context.Context,
	t model.Torrent,
	ext Extraction,
	parsedTitle string,
	isTV bool,
	cands []Candidate,
	source llmcapture.CandidateSource,
) (int64, float64, error) {
	user := RerankInput(t.Name, ext, cands)
	ctx = withPendingCapture(ctx)
	effectiveMediaType := "movie"
	if isTV {
		effectiveMediaType = "tv"
	}
	if err := c.captureRequest(
		ctx,
		t,
		llmcapture.TaskMatcherRerank,
		source,
		matcherContractID(c.cfg, "llmmatch-chat-rerank-v2"),
		[]byte(fmt.Sprintf(
			"%s\x00%s\x00%d",
			effectiveMediaType,
			strings.ToLower(strings.TrimSpace(ext.Title)),
			ext.Year,
		)),
		RerankPrompt(),
		user,
		60,
		map[string]any{
			"release_name":         t.Name,
			"parsed_title":         parsedTitle,
			"extraction":           ext,
			"effective_media_type": effectiveMediaType,
			"candidates":           rerankCaptureCandidates(cands),
		},
	); err != nil {
		return 0, 0, err
	}
	raw, err := c.call(ctx, "rerank", RerankPrompt(), user, 60)
	if err != nil {
		return 0, 0, err
	}
	tmdbID, confidence, err := decodeAndNormalizeRerank(raw, cands)
	if err != nil {
		c.metrics.callErrors.WithLabelValues("rerank", "decode").Inc()
		return 0, 0, fmt.Errorf("decode rerank: %w", err)
	}
	return tmdbID, confidence, nil
}

// rerankCaptureCandidate preserves policy-only catalogue aliases in the
// structured capture. Candidate deliberately excludes AltTitles from JSON so
// the provider cannot rationalize from the evidence used to check its answer.
type rerankCaptureCandidate struct {
	ID        int64    `json:"tmdb_id"`
	Title     string   `json:"title"`
	Year      int      `json:"year"`
	Overview  string   `json:"overview,omitempty"`
	AltTitles []string `json:"alt_titles,omitempty"`
}

func rerankCaptureCandidates(candidates []Candidate) []rerankCaptureCandidate {
	out := make([]rerankCaptureCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, rerankCaptureCandidate{
			ID:        candidate.ID,
			Title:     candidate.Title,
			Year:      candidate.Year,
			Overview:  candidate.Overview,
			AltTitles: append([]string(nil), candidate.AltTitles...),
		})
	}
	return out
}

func decodeAndNormalizeRerank(
	raw []byte,
	candidates []Candidate,
) (int64, float64, error) {
	tmdbID, confidence, err := decodeRerank(raw)
	if err != nil {
		return 0, 0, err
	}
	if math.IsNaN(confidence) || math.IsInf(confidence, 0) ||
		confidence < 0 || confidence > 1 {
		return 0, 0, fmt.Errorf(
			"confidence must be finite and in [0,1]",
		)
	}
	if tmdbID == 0 {
		return 0, confidence, nil
	}
	for _, candidate := range candidates {
		if candidate.ID == tmdbID {
			return tmdbID, confidence, nil
		}
	}
	return 0, 0, nil
}

// call issues one chat.completions request and returns the decoded message
// content (the model's JSON string). No temperature (gpt-5.* rejects it; ollama
// ignores its absence).
func (c *Client) call(ctx context.Context, stage, system, user string, maxTokens int) ([]byte, error) {
	return c.callWith(ctx, c.http, stage, system, user, maxTokens)
}

func (c *Client) newChatRequest(
	system string,
	user string,
	maxTokens int,
) chatRequest {
	return newConfiguredChatRequest(c.cfg, system, user, maxTokens)
}

func newConfiguredChatRequest(cfg Config, system, user string, maxTokens int) chatRequest {
	req := newChatRequest(cfg.Model, system, user, maxTokens)
	if cfg.OpenaiDataSharing {
		// The direct OpenAI route has a real reasoning control; do not send the
		// Ollama-only prompt hint as part of the production contract.
		req.Messages[1].Content = user
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
	return req
}

func matcherContractID(cfg Config, legacy string) string {
	if cfg.OpenaiDataSharing {
		return legacy + "-openai-data-sharing-v1"
	}
	return legacy
}

func newChatRequest(
	model string,
	system string,
	user string,
	maxTokens int,
) chatRequest {
	req := chatRequest{
		Model: model,
		Messages: []message{
			{Role: "system", Content: system},
			// This is part of the exact deployed user message. The capture
			// stores this same request struct before the HTTP call.
			{Role: "user", Content: user + "\n\n/no_think"},
		},
		MaxCompletionTokens: maxTokens,
	}
	req.ResponseFormat.Type = "json_object"
	return req
}

func (c *Client) captureRequest(
	ctx context.Context,
	t model.Torrent,
	task llmcapture.Task,
	source llmcapture.CandidateSource,
	contractID string,
	groupKey []byte,
	system string,
	user string,
	maxTokens int,
	taskInput map[string]any,
) error {
	if c.capture == nil || !c.capture.Enabled() {
		return nil
	}
	if _, ok := c.capture.(llmcapture.ResultRecorder); !ok {
		return fmt.Errorf("%w: enabled matcher capture requires result recording", llmcapture.ErrCaptureUnavailable)
	}
	modelInput, err := json.Marshal(
		c.newChatRequest(system, user, maxTokens),
	)
	if err != nil {
		return fmt.Errorf(
			"%w: marshal matcher model input: %v",
			llmcapture.ErrCaptureUnavailable,
			err,
		)
	}
	// The provider pin is audit-only evidence. Reconstructing the configured
	// request from this single value keeps provider privacy/fallback policy in
	// the production builder instead of trusting a captured policy object.
	if c.cfg.OpenrouterProvider != "" {
		withProvider := make(map[string]any, len(taskInput)+1)
		for key, value := range taskInput {
			withProvider[key] = value
		}
		withProvider["openrouter_provider"] = c.cfg.OpenrouterProvider
		taskInput = withProvider
	}
	if c.cfg.OpenaiDataSharing {
		withRoute := make(map[string]any, len(taskInput)+1)
		for key, value := range taskInput {
			withRoute[key] = value
		}
		withRoute["openai_data_sharing"] = true
		taskInput = withRoute
	}
	taskInputJSON, err := json.Marshal(taskInput)
	if err != nil {
		return fmt.Errorf(
			"%w: marshal matcher task input: %v",
			llmcapture.ErrCaptureUnavailable,
			err,
		)
	}
	req := llmcapture.Request{
		Task:            task,
		CandidateSource: source,
		InfoHash:        t.InfoHash.Bytes(),
		GroupKey:        groupKey,
		NativePrivate:   t.Private,
		Model:           c.cfg.Model,
		Endpoint:        c.cfg.Endpoint,
		PromptVersion:   c.cfg.PromptVersion,
		SystemPrompt:    system,
		ModelInputJSON:  modelInput,
		TaskInputJSON:   taskInputJSON,
		BuildIdentity:   llmcapture.CurrentBuildIdentity(),
		ContractID:      contractID,
	}
	outcome, err := c.capture.Capture(ctx, req)
	if err != nil {
		if errors.Is(err, llmcapture.ErrPrivacyBlocked) ||
			errors.Is(err, llmcapture.ErrCaptureUnavailable) {
			return err
		}
		return fmt.Errorf(
			"%w: matcher capture: %v",
			llmcapture.ErrCaptureUnavailable,
			err,
		)
	}
	if outcome != llmcapture.OutcomeRecorded &&
		outcome != llmcapture.OutcomeDuplicate {
		return fmt.Errorf(
			"%w: enabled matcher capture returned outcome %q",
			llmcapture.ErrCaptureUnavailable,
			outcome,
		)
	}
	if pending, _ := ctx.Value(pendingCaptureContextKey{}).(*pendingCapture); pending != nil {
		key, keyErr := llmcapture.KeyForRequest(req)
		if keyErr != nil {
			return fmt.Errorf("%w: capture result identity: %v", llmcapture.ErrCaptureUnavailable, keyErr)
		}
		pending.key, pending.task, pending.source = key, task, source
	}
	return nil
}

func (c *Client) callWith(ctx context.Context, hc *http.Client, stage, system, user string, maxTokens int) ([]byte, error) {
	if !c.Enabled() {
		return nil, fmt.Errorf("matcher is disabled")
	}
	// Library/CLI callers can bypass fx startup validation; route constraints
	// must also hold at the actual outbound boundary.
	if err := c.cfg.Validate(); err != nil {
		return nil, err
	}
	if c.budgetCoolingDown() {
		return nil, ErrCallBudget
	}
	req := c.newChatRequest(system, user, maxTokens)
	body, _ := json.Marshal(req)
	if c.cfg.MaxRequestBytes <= 0 || len(body) > c.cfg.MaxRequestBytes ||
		maxTokens <= 0 || maxTokens > c.cfg.MaxOutputTokens {
		c.metrics.gateRejects.WithLabelValues("request_size").Inc()
		return nil, fmt.Errorf("matcher request exceeds input/output limit")
	}
	// Skip instead of waiting for another LLM call: ingestion must not queue
	// behind the optional matcher. Retry can happen on a later reprocess pass.
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	default:
		c.metrics.gateRejects.WithLabelValues("concurrency").Inc()
		return nil, ErrCallBudget
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	budgetCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if c.budget == nil {
		c.metrics.budgetSkips.WithLabelValues("unavailable").Inc()
		return nil, ErrCallBudget
	}
	allowed, budgetErr := c.budget.Reserve(budgetCtx, c.cfg.DailyCallLimit, c.cfg.MonthlyCallLimit)
	if budgetErr != nil || !allowed {
		reason := "exhausted"
		now := time.Now().UTC()
		until := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
		if budgetErr != nil {
			reason = "unavailable"
			until = now.Add(30 * time.Second)
		}
		c.budgetBlockedUntil.Store(until.UnixNano())
		c.metrics.budgetSkips.WithLabelValues(reason).Inc()
		return nil, ErrCallBudget
	}
	c.metrics.calls.WithLabelValues(c.cfg.Model, stage).Inc()

	start := time.Now()
	resp, err := hc.Do(httpReq)
	c.metrics.callDuration.WithLabelValues(stage).Observe(time.Since(start).Seconds())
	if err != nil {
		c.metrics.callErrors.WithLabelValues(stage, "timeout").Inc()
		c.metrics.usageMissing.WithLabelValues(c.cfg.Model, stage).Inc()
		if recordErr := c.recordCapturedResult(ctx, stage, llmcapture.HTTPResult{ErrorClass: "transport"}); recordErr != nil {
			return nil, recordErr
		}
		return nil, err
	}
	defer resp.Body.Close()
	raw, content, err := ReadMatcherChatResponse(resp.StatusCode, resp.Body)
	c.recordUsage(stage, raw)
	if recordErr := c.recordCapturedResult(ctx, stage, llmcapture.HTTPResult{
		Body: raw, StatusCode: resp.StatusCode, ErrorClass: matcherResponseErrorClass(err),
	}); recordErr != nil {
		return nil, recordErr
	}
	if err != nil {
		switch {
		case errors.Is(err, ErrMatcherChatHTTPStatus):
			c.metrics.callErrors.WithLabelValues(stage, "http_status").Inc()
		default:
			c.metrics.callErrors.WithLabelValues(stage, "decode").Inc()
		}
		return nil, err
	}
	return content, nil
}

func (c *Client) budgetCoolingDown() bool {
	if time.Now().UnixNano() < c.budgetBlockedUntil.Load() {
		c.metrics.budgetSkips.WithLabelValues("cooldown").Inc()
		return true
	}
	return false
}

func (c *Client) recordUsage(stage string, raw []byte) {
	var response chatUsageResponse
	if json.Unmarshal(raw, &response) != nil || response.Usage == nil {
		c.metrics.usageMissing.WithLabelValues(c.cfg.Model, stage).Inc()
		return
	}
	u := response.Usage
	if u.PromptTokens == nil || u.CompletionTokens == nil || *u.PromptTokens < 0 || *u.CompletionTokens < 0 ||
		u.PromptDetails.CachedTokens < 0 || u.PromptDetails.CachedTokens > *u.PromptTokens ||
		u.CompletionDetails.ReasoningTokens < 0 || u.CompletionDetails.ReasoningTokens > *u.CompletionTokens {
		c.metrics.usageMissing.WithLabelValues(c.cfg.Model, stage).Inc()
		return
	}
	for kind, n := range map[string]int64{
		"input": *u.PromptTokens, "cached_input": u.PromptDetails.CachedTokens,
		"output": *u.CompletionTokens, "reasoning": u.CompletionDetails.ReasoningTokens,
	} {
		c.metrics.tokens.WithLabelValues(c.cfg.Model, stage, kind).Add(float64(n))
	}
}

// anyMediaExt reports whether the torrent has at least one video/audio file —
// a cheap plausibility signal so we don't burn inference on data dumps.
var mediaExts = map[string]struct{}{
	"mkv": {}, "mp4": {}, "m4v": {}, "avi": {}, "mov": {}, "webm": {}, "ts": {}, "wmv": {}, "flv": {}, "mpg": {}, "mpeg": {},
}

func anyMediaExt(t model.Torrent) bool {
	if t.Extension.Valid {
		if _, ok := mediaExts[strings.ToLower(t.Extension.String)]; ok {
			return true
		}
	}
	for _, f := range t.Files {
		if f.Extension.Valid {
			if _, ok := mediaExts[strings.ToLower(f.Extension.String)]; ok {
				return true
			}
		}
	}
	return false
}
