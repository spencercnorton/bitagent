package contentfilter

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/spencercnorton/bitagent/internal/anime"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
)

// Decision is what Filter.Decide returns for one torrent.
type Decision struct {
	// Allow: caller should persist the torrent.
	// false → caller should drop it (in enforce mode).
	Allow bool

	// WouldDrop is true iff the deterministic checks said "drop"
	// regardless of Enforce. In shadow mode (Enforce=false) Allow
	// stays true while WouldDrop is true; that's the
	// counterfactual measurement primitive.
	WouldDrop bool

	// Reason is the first matching DropReason. Stable per
	// DropReason.String() for metric labels. Reason==ReasonNone
	// when the torrent is genuinely allowed (not just
	// shadow-mode-allowed).
	Reason DropReason

	// BlockedExt is the primary file extension that triggered a
	// ReasonBlockedExtension drop. Empty for all other reasons.
	// Used by Metrics.Observe to populate the per-extension counter.
	BlockedExt string

	// Defer is true when the decision could not be made because the
	// LLM tier was needed but its endpoint was unreachable, AND the
	// operator opted into LLMDeferOnUnavailable. The caller must NOT
	// persist or drop the torrent — it should re-queue it for a later
	// retry. Mutually exclusive with a real Allow/drop outcome
	// (Allow is false when Defer is true). Only the post-classifier
	// Decide() path can set this; DecideDeterministic never does.
	Defer bool
}

// Input bundles everything Decide() needs from upstream. The caller
// (the BEP-9 fetcher's persistence path) builds it from the
// torrent's metainfo + the classifier's already-computed
// content_type / language hints.
type Input struct {
	// Private is the authoritative private flag from the torrent's metainfo.
	// It blocks only the optional LLM tier; deterministic policy checks still
	// run. This must not depend on qBittorrent/evidence privacy being present.
	Private bool

	// Title is the human-readable torrent name. Used for NSFW
	// keyword matching and non-Latin script detection.
	Title string

	// PrimaryExtension is the lowercase file extension of the
	// torrent's "main" file (largest media-ish file by size, or
	// the only file for single-file torrents). No leading dot.
	// Empty string is allowed — it just means "unknown ext."
	PrimaryExtension string

	// AllExtensions is the set of file extensions in the torrent
	// (lowercase, no leading dot, deduped). For
	// content_type="music", this lets us differentiate "mp3 only"
	// from "lossless" or "mixed."
	AllExtensions []string

	// ContentType is the CEL classifier's emitted type, e.g.
	// "movie", "tv_show", "music", "ebook", "audiobook", "xxx".
	// Empty string when the classifier didn't match.
	ContentType string

	// Languages is the classifier-set language tag set for the
	// torrent's content. Examples: ["en"], ["ru"], [], nil. The
	// classifier populates these from TMDB lookup; ~40% of
	// torrents have empty/nil languages here.
	Languages []string
}

// Filter is the content filter. Phase 1's deterministic ladder is
// stateless; Phase 2 attaches optional LLM components (cache,
// budget, miner) that the operator activates via env. When the LLM
// is disabled or unconfigured, Decide() degrades to pure-Phase-1
// behaviour with zero overhead.
type Filter struct {
	cfg       Config
	llm       LLMClient
	cache     *llmCache
	miner     *ruleMiner
	llmCb     LLMCallbacks // optional metrics+logging hooks
	budget    *dailyBudget
	admission Admission
	slots     chan struct{}
}

type CallBudget interface {
	Reserve(context.Context, int, int) (bool, error)
}

// Admission is mandatory in production: every possible billed dispatch must
// reserve durable capacity and retain an exact request/result/decision chain.
type Admission struct {
	Budget  CallBudget
	Capture llmcapture.Capturer
}

type AuditSource struct {
	InfoHash []byte
	GroupKey []byte
}

// LLMCallbacks lets the wiring layer plug in per-event metrics +
// structured logs without giving the filter package a hard
// dependency on prometheus or zap. All hooks are optional.
type LLMCallbacks struct {
	OnCacheHit  func()
	OnCacheMiss func()
	OnLLMCall   func(ok bool, latencyMs float64)
	// OnLLMTokens is additive rather than folded into OnLLMCall: OnLLMCall is
	// on the BEP-9 fetcher's hot path and changing its signature would touch
	// every caller and test for a purely observational field.
	OnLLMTokens            func(model string, usage TokenUsage)
	OnLLMUsageMissing      func(model string)
	OnBudgetExhausted      func()
	OnGateReject           func(reason string)
	OnAudit                func(outcome string)
	OnDeterministicResidue func() // residual case kept because LLM disabled or unsure
	OnRuleCandidate        func(reason string, isEnglish bool, count int)
}

// New returns a Filter with only the deterministic ladder enabled.
// Use NewWithLLM when the operator has activated LLMEnabled+API key.
func New(cfg Config) *Filter {
	return &Filter{cfg: cfg}
}

// NewWithLLM returns a Filter that consults the LLM after the
// deterministic ladder for "residual" cases — Latin-script titles
// without a language tag. Pass cb=nil to skip metrics.
func NewWithLLM(cfg Config, llm LLMClient, cb LLMCallbacks) *Filter {
	return newWithLLM(cfg, llm, cb, Admission{})
}

// NewWithLLMAdmission is the production constructor. Direct NewWithLLM users
// retain the in-memory test/library path, but cannot call DecideAudited.
func NewWithLLMAdmission(cfg Config, llm LLMClient, cb LLMCallbacks, admission Admission) *Filter {
	return newWithLLM(cfg, llm, cb, admission)
}

func newWithLLM(cfg Config, llm LLMClient, cb LLMCallbacks, admission Admission) *Filter {
	cacheTTL, _ := time.ParseDuration(cfg.LLMCacheTTL)
	minerWindow, _ := time.ParseDuration(cfg.LLMRuleMinerWindow)
	cache := newLLMCache(cfg.LLMCacheMaxEntries, cacheTTL)
	notifier := func(reason string, isEnglish bool, count int) {
		if cb.OnRuleCandidate != nil {
			cb.OnRuleCandidate(reason, isEnglish, count)
		}
	}
	maxConcurrent := cfg.LLMMaxConcurrentCalls
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	return &Filter{
		cfg:       cfg,
		llm:       llm,
		cache:     cache,
		miner:     newRuleMiner(minerWindow, cfg.LLMRuleMinerThreshold, notifier),
		llmCb:     cb,
		budget:    newDailyBudget(cfg.LLMDailyBudget),
		admission: admission,
		slots:     make(chan struct{}, maxConcurrent),
	}
}

// Enabled reports whether the operator has activated the filter.
// Callers MUST gate Decide() / DecideDeterministic() / metrics.Observe()
// on this — when false, all should be skipped entirely so a disabled
// filter is a genuine no-op (no examined_total / keep_total leakage).
// The config doc on Enabled is explicit: "no metrics are emitted."
func (f *Filter) Enabled() bool { return f.cfg.Enabled }

// dailyBudget is a UTC-day-bucketed counter that resets at 00:00
// UTC. Goroutine-safe (atomic counter; clock read is a regular
// time.Now). When TryConsume returns false the caller should
// bypass the LLM and take the safe default.
type dailyBudget struct {
	cap        int64
	used       atomic.Int64
	currentDay atomic.Int64
}

func newDailyBudget(cap int) *dailyBudget {
	b := &dailyBudget{cap: int64(cap)}
	b.currentDay.Store(utcDay(time.Now()))
	return b
}

func utcDay(t time.Time) int64 {
	return t.UTC().Unix() / 86400
}

// TryConsume returns true iff there's headroom in today's budget.
// Increments the counter on success. Resets the counter when the
// UTC day rolls over.
func (b *dailyBudget) TryConsume(now time.Time) bool {
	day := utcDay(now)
	if b.currentDay.Load() != day {
		// Day rolled — reset. Race-tolerant: a stray increment
		// on the boundary is harmless (worst case: the very
		// first call after midnight starts at 1 instead of 0).
		b.currentDay.Store(day)
		b.used.Store(0)
	}
	if b.cap < 0 {
		// Negative budget means "unlimited" — no daily cap. For a
		// free self-hosted endpoint (Ollama/vLLM) where per-call
		// cost is zero, so the cost-control cap is pointless. We
		// still increment used (for the gauge) but never refuse.
		b.used.Add(1)
		return true
	}
	if b.cap == 0 {
		// Budget=0 means "no LLM calls allowed" (operator can
		// effectively disable the LLM tier without flipping
		// LLMEnabled, useful for debugging).
		return false
	}
	if b.used.Add(1) > b.cap {
		// Pre-decrement so the counter doesn't drift past cap.
		b.used.Add(-1)
		return false
	}
	return true
}

// Used returns the current day's consumption (for a metric gauge).
func (b *dailyBudget) Used() int64 { return b.used.Load() }

// Decide evaluates the torrent against the deterministic ladder
// first; if nothing matches AND the residual conditions hold (Latin
// script, no language tag, LLM enabled), consults the LLM tier with
// cache + budget. Returns {Allow:true, Reason:None} when nothing
// drops.
//
// Order of checks (fail-fast):
//
//  1. Disabled → allow always
//  2. Blocked extension     (cheapest; bytewise compare)
//  3. Blocked content_type
//  4. NSFW content_type
//  5. NSFW keyword in title
//  6. Non-Latin script in title
//  7. Foreign-audio release (non-English track, no English one)
//  8. Non-English language tag (when language is set)
//  9. mp3-only music
//  10. LLM tier (only when steps 2-9 cleared AND residual qualifies)
//
// Steps 2-9 are constant-time. Step 10 is cache-first; an LLM call
// fires only on cache miss within the daily budget.
//
// **Hook-point note for callers:** Decide() includes the LLM tier,
// which is only meaningful AFTER the upstream classifier has set
// `in.Languages` and `in.ContentType`. At a pre-classifier hook
// (e.g. the BEP-9 success path) Languages is always empty, so every
// Latin-script title would fall through to the LLM and waste cache /
// budget. Call DecideDeterministic at that stage instead. Reserve
// Decide for post-classifier integration points.
func (f *Filter) Decide(in Input) Decision {
	decision, err := f.decide(context.Background(), in, true /* allowLLM */, nil)
	if err != nil {
		// A production filter with durable admission must be called through
		// DecideAudited. An accidental legacy call fails open without egress;
		// it must never turn an audit wiring mistake into a content drop.
		return Decision{Allow: true}
	}
	return decision
}

// DecideAudited is the only production LLM path. Audit failures are returned
// to the processor so the torrent is retried without an unobserved model call.
func (f *Filter) DecideAudited(ctx context.Context, in Input, source AuditSource) (Decision, error) {
	return f.decide(ctx, in, true /* allowLLM */, &source)
}

// DecideDeterministic runs ONLY the deterministic ladder (steps
// 1-8). The LLM tier (step 9) is skipped regardless of LLMEnabled.
// Use this at hook points where the classifier has not yet
// populated `in.Languages` / `in.ContentType` — calling Decide
// there would waste cache + budget on every Latin-script title
// because shouldConsultLLM keys off "language tag empty."
func (f *Filter) DecideDeterministic(in Input) Decision {
	decision, _ := f.decide(context.Background(), in, false /* allowLLM */, nil)
	return decision
}

// decide is the shared core for Decide / DecideDeterministic.
// allowLLM=false short-circuits step 9.
func (f *Filter) decide(ctx context.Context, in Input, allowLLM bool, source *AuditSource) (Decision, error) {
	if !f.cfg.Enabled {
		return Decision{Allow: true}, nil
	}

	reason, blockedExt := f.evaluate(in)
	if reason != ReasonNone {
		// Deterministic match — drop in enforce mode,
		// shadow-flag in observe mode.
		return Decision{
			Allow:      !f.cfg.Enforce,
			WouldDrop:  true,
			Reason:     reason,
			BlockedExt: blockedExt,
		}, nil
	}

	// Deterministic ladder cleared. Consider LLM tier for the
	// "residual" cohort: Latin-script title (script filter
	// passed), no language tag (language filter didn't fire),
	// LLM is configured. Skipped when the caller asked for
	// deterministic-only.
	if allowLLM && f.shouldConsultLLM(in) {
		llmReason, deferDecision, err := f.consultLLM(ctx, in, source)
		if err != nil {
			return Decision{}, err
		}
		if deferDecision && f.cfg.Enforce {
			// LLM endpoint unreachable + operator opted into deferral
			// + we're enforcing: signal the caller to re-queue rather
			// than keep (which would leak foreign content) or drop
			// (a false positive). Allow stays false. In shadow mode
			// (Enforce=false) we never change persistence, so an
			// unreachable LLM just falls through to keep below.
			return Decision{Defer: true}, nil
		}
		if llmReason != ReasonNone {
			return Decision{
				Allow:     !f.cfg.Enforce,
				WouldDrop: true,
				Reason:    llmReason,
			}, nil
		}
	}

	return Decision{Allow: true}, nil
}

// shouldConsultLLM gates the LLM tier on (a) operator opt-in,
// (b) configured client, (c) the residual condition that the
// deterministic ladder couldn't classify.
func (f *Filter) shouldConsultLLM(in Input) bool {
	if !f.cfg.LLMEnabled || f.llm == nil {
		return false
	}
	return f.llmEligibleInput(in)
}

// EvaluationLLMEligible reports whether the exact production deterministic
// ladder would pass an input to the residual LLM tier. It performs no model
// call, does not consult the cache/budget, and is used only by the read-only
// corpus exporter.
func (f *Filter) EvaluationLLMEligible(in Input) bool {
	if !f.cfg.Enabled || !f.cfg.LLMEnabled {
		return false
	}
	reason, _ := f.evaluate(in)
	return reason == ReasonNone && f.llmEligibleInput(in)
}

func (f *Filter) llmEligibleInput(in Input) bool {
	if in.Private {
		return false
	}
	// A recognised anime release is kept, not language-classified: the
	// LLM English-detector reads a romaji title ("Sousou no Frieren") as
	// non-English and drops it. Skipping the tier keeps the anime AND
	// saves an inference call. (Romaji anime the deterministic detector
	// misses is still protected by the prompt's anime KEEP rule.)
	if f.cfg.AnimeAware && anime.Detect(in.Title).IsAnime() {
		return false
	}
	// Only call the LLM when the deterministic ladder couldn't
	// place the torrent — i.e. Latin script (script filter passed)
	// AND no language tag (language filter didn't fire). If the
	// classifier already set a tag and it was English, we're done;
	// no LLM needed.
	if len(in.Languages) > 0 {
		return false
	}
	if in.Title == "" {
		return false
	}
	return true
}

// consultLLM cache-first. Returns (dropReason, deferDecision).
// dropReason is ReasonLLMNonEnglish iff the LLM (cached or live) is
// confident the title is non-English; ReasonNone otherwise.
// deferDecision is true ONLY when the LLM endpoint was unreachable AND
// the operator set LLMDeferOnUnavailable — the caller must then
// re-queue the torrent. Every other outcome (cache miss + budget
// exhausted, non-availability error, low confidence, IsEnglish=true)
// returns (ReasonNone, false) → keep.
func (f *Filter) consultLLM(ctx context.Context, in Input, source *AuditSource) (DropReason, bool, error) {
	norm := normalizeTitle(in.Title)
	if norm == "" {
		return ReasonNone, false, nil
	}

	model := f.cfg.LLMModel
	pv := f.cfg.LLMPromptVersion
	if pv == "" {
		pv = "v1"
	}
	key := llmCacheKey(model, pv, norm)

	// Cache hit path.
	if v, ok := f.cache.Get(key); ok {
		if f.llmCb.OnCacheHit != nil {
			f.llmCb.OnCacheHit()
		}
		return f.verdictToReason(v), false, nil
	}
	if f.llmCb.OnCacheMiss != nil {
		f.llmCb.OnCacheMiss()
	}

	// Budget gate.
	if source == nil {
		if f.admission.Budget != nil || f.admission.Capture != nil {
			f.observeGateReject("audit_unavailable")
			return ReasonNone, false, llmcapture.ErrCaptureUnavailable
		}
		if !f.budget.TryConsume(time.Now()) {
			if f.llmCb.OnBudgetExhausted != nil {
				f.llmCb.OnBudgetExhausted()
			}
			return ReasonNone, false, nil
		}
		return f.consultDirect(ctx, in, key)
	}
	return f.consultAudited(ctx, in, key, *source)
}

func (f *Filter) consultDirect(ctx context.Context, in Input, key string) (DropReason, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	timeout, err := time.ParseDuration(f.cfg.LLMTimeout)
	if err != nil || timeout <= 0 {
		timeout = 8 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	t0 := time.Now()
	verdict, err := f.llm.Classify(ctx, in.Title)
	f.observeLLMCall(verdict, err, time.Since(t0))
	if err != nil {
		if f.cfg.LLMDeferOnUnavailable && errors.Is(err, ErrLLMUnavailable) {
			return ReasonNone, true, nil
		}
		return ReasonNone, false, nil
	}
	f.cache.Put(key, verdict)
	f.miner.Record(verdict)
	return f.verdictToReason(verdict), false, nil
}

func (f *Filter) consultAudited(
	ctx context.Context,
	in Input,
	cacheKey string,
	source AuditSource,
) (DropReason, bool, error) {
	recorder, ok := f.admission.Capture.(llmcapture.ContentFilterResultRecorder)
	client, clientOK := f.llm.(AuditedLLMClient)
	if f.admission.Budget == nil || f.admission.Capture == nil ||
		!f.admission.Capture.Enabled() || !ok || !clientOK {
		f.observeGateReject("audit_unavailable")
		return ReasonNone, false, llmcapture.ErrCaptureUnavailable
	}
	if len(source.InfoHash) != 20 || len(source.GroupKey) == 0 {
		f.observeGateReject("audit_unavailable")
		return ReasonNone, false, llmcapture.ErrCaptureUnavailable
	}
	select {
	case f.slots <- struct{}{}:
		defer func() { <-f.slots }()
	default:
		f.observeGateReject("concurrency")
		return ReasonNone, false, fmt.Errorf("contentfilter concurrency allowance exhausted")
	}
	timeout, err := time.ParseDuration(f.cfg.LLMTimeout)
	if err != nil || timeout <= 0 {
		timeout = 8 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	contract, err := f.EvaluationCapture(in)
	if err != nil {
		f.observeGateReject("request_bounds")
		return ReasonNone, false, llmcapture.ErrCaptureUnavailable
	}
	request := llmcapture.Request{
		Task: llmcapture.TaskContentFilter, InfoHash: append([]byte(nil), source.InfoHash...),
		GroupKey: append([]byte(nil), source.GroupKey...), NativePrivate: in.Private,
		Model: contract.Model, Endpoint: contract.Endpoint, PromptVersion: contract.PromptVersion,
		SystemPrompt: contract.SystemPrompt, ModelInputJSON: contract.ModelInputJSON,
		TaskInputJSON: contract.TaskInputJSON, BuildIdentity: llmcapture.CurrentBuildIdentity(),
		ContractID: contract.ContractID,
	}
	captureKey, err := llmcapture.KeyForRequest(request)
	if err != nil {
		f.observeGateReject("audit_unavailable")
		return ReasonNone, false, llmcapture.ErrCaptureUnavailable
	}
	replay, err := recorder.FindContentFilterReplay(ctx, captureKey, source.InfoHash)
	if err != nil {
		f.observeGateReject("audit_unavailable")
		return ReasonNone, false, llmcapture.ErrCaptureUnavailable
	}
	if replay.Found {
		f.observeGateReject("already_captured")
		if replay.Decision == nil {
			replay, err = f.completeIncompleteContentFilterAudit(ctx, recorder, captureKey, source.InfoHash, replay)
			if err != nil {
				f.observeAudit("incomplete_recovery_error")
				return ReasonNone, false, llmcapture.ErrCaptureUnavailable
			}
		}
		f.observeAudit("decision_replayed")
		verdict := LLMVerdict{
			IsEnglish:  replay.Decision.IsEnglish,
			Confidence: replay.Decision.Confidence,
			Reason:     replay.Decision.Reason,
			Model:      f.cfg.LLMModel,
		}
		f.cache.Put(cacheKey, verdict)
		return f.verdictToReason(verdict), false, nil
	}
	reserved, err := f.admission.Budget.Reserve(ctx, f.cfg.LLMDailyBudget, f.cfg.LLMMonthlyBudget)
	if err != nil {
		f.observeGateReject("budget_unavailable")
		return ReasonNone, false, llmcapture.ErrCaptureUnavailable
	}
	if !reserved {
		if f.llmCb.OnBudgetExhausted != nil {
			f.llmCb.OnBudgetExhausted()
		}
		f.observeGateReject("budget_exhausted")
		return ReasonNone, false, nil
	}
	outcome, err := f.admission.Capture.Capture(ctx, request)
	if err != nil {
		f.observeGateReject("audit_unavailable")
		return ReasonNone, false, llmcapture.ErrCaptureUnavailable
	}
	if outcome != llmcapture.OutcomeRecorded {
		f.observeGateReject("already_captured")
		// Another replica may have won between the replay lookup and capture.
		// Never dispatch a second paid call; complete/replay the winner's durable
		// chain instead of leaving an ordinary unaudited keep.
		replay, err := recorder.FindContentFilterReplay(ctx, captureKey, source.InfoHash)
		if err != nil || !replay.Found {
			return ReasonNone, false, llmcapture.ErrCaptureUnavailable
		}
		if replay.Decision == nil {
			replay, err = f.completeIncompleteContentFilterAudit(ctx, recorder, captureKey, source.InfoHash, replay)
			if err != nil {
				return ReasonNone, false, llmcapture.ErrCaptureUnavailable
			}
		}
		verdict := LLMVerdict{
			IsEnglish: replay.Decision.IsEnglish, Confidence: replay.Decision.Confidence,
			Reason: replay.Decision.Reason, Model: f.cfg.LLMModel,
		}
		f.cache.Put(cacheKey, verdict)
		return f.verdictToReason(verdict), false, nil
	}
	if err := recorder.RecheckContentFilterRequest(ctx, captureKey, source.InfoHash); err != nil {
		f.observeGateReject("privacy")
		return ReasonNone, false, llmcapture.ErrCaptureUnavailable
	}
	t0 := time.Now()
	verdict, result, callErr := client.ClassifyWithResult(ctx, in.Title)
	f.observeLLMCall(verdict, callErr, time.Since(t0))
	cleanup, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	receipt, recordErr := recorder.RecordHTTPResult(cleanup, captureKey, result)
	cleanupCancel()
	if recordErr != nil || !receipt.FirstObservation {
		f.observeAudit("result_error")
		return ReasonNone, false, llmcapture.ErrCaptureUnavailable
	}
	f.observeAudit("result_recorded")
	invalid := callErr != nil || result.StatusCode != http.StatusOK || result.ErrorClass != "none"
	policy := contentFilterAuditDecision(verdict, f.cfg.LLMMinConfidenceForDrop, f.cfg.Enforce, invalid)
	auditCtx, auditCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	recordErr = recorder.RecordContentFilterDecision(auditCtx, receipt, source.InfoHash, policy)
	auditCancel()
	if recordErr != nil {
		f.observeAudit("decision_error")
		return ReasonNone, false, llmcapture.ErrCaptureUnavailable
	}
	f.observeAudit("decision_recorded")
	if callErr != nil {
		if f.cfg.LLMDeferOnUnavailable && errors.Is(callErr, ErrLLMUnavailable) {
			return ReasonNone, true, nil
		}
		return ReasonNone, false, nil
	}
	f.cache.Put(cacheKey, verdict)
	f.miner.Record(verdict)
	return f.verdictToReason(verdict), false, nil
}

func (f *Filter) completeIncompleteContentFilterAudit(
	ctx context.Context,
	recorder llmcapture.ContentFilterResultRecorder,
	captureKey, infoHash []byte,
	replay llmcapture.ContentFilterReplay,
) (llmcapture.ContentFilterReplay, error) {
	if !replay.Found || replay.Decision != nil {
		return replay, nil
	}
	receipt := replay.Result
	if receipt == nil {
		recovered, err := recorder.RecordHTTPResult(ctx, captureKey, llmcapture.HTTPResult{
			Body:       []byte(`{"audit_state":"capture_without_result"}`),
			StatusCode: 0,
			ErrorClass: "audit_incomplete",
		})
		if err != nil {
			return llmcapture.ContentFilterReplay{}, err
		}
		if recovered.FirstObservation {
			receipt = &recovered
			f.observeAudit("incomplete_result_recorded")
		} else {
			refreshed, err := recorder.FindContentFilterReplay(ctx, captureKey, infoHash)
			if err != nil {
				return llmcapture.ContentFilterReplay{}, err
			}
			if refreshed.Decision != nil {
				return refreshed, nil
			}
			if refreshed.Result == nil {
				return llmcapture.ContentFilterReplay{}, llmcapture.ErrCaptureUnavailable
			}
			receipt = refreshed.Result
		}
	}
	// The result was loaded through the exact capture key, source identity,
	// retention and current privacy predicates. It is therefore authorized to
	// receive its one missing terminal decision.
	receipt.FirstObservation = true
	outcome := "audit_incomplete"
	if receipt.ErrorClass != "none" && receipt.ErrorClass != "audit_incomplete" ||
		receipt.StatusCode != 0 && receipt.StatusCode != http.StatusOK {
		outcome = "invalid_response"
	}
	decision := llmcapture.ContentFilterDecision{
		Outcome: outcome, MinConfidence: f.cfg.LLMMinConfidenceForDrop,
		Live: f.cfg.Enforce,
	}
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	err := recorder.RecordContentFilterDecision(auditCtx, *receipt, infoHash, decision)
	cancel()
	if err != nil {
		return llmcapture.ContentFilterReplay{}, err
	}
	f.observeAudit("incomplete_decision_recorded")
	replay.Result, replay.Decision = receipt, &decision
	return replay, nil
}

func (f *Filter) observeGateReject(reason string) {
	if f.llmCb.OnGateReject != nil {
		f.llmCb.OnGateReject(reason)
	}
}

func (f *Filter) observeAudit(outcome string) {
	if f.llmCb.OnAudit != nil {
		f.llmCb.OnAudit(outcome)
	}
}

func (f *Filter) observeLLMCall(verdict LLMVerdict, err error, elapsed time.Duration) {
	if f.llmCb.OnLLMCall != nil {
		f.llmCb.OnLLMCall(err == nil, float64(elapsed.Microseconds())/1000.0)
	}
	if verdict.Model == "" {
		verdict.Model = f.cfg.LLMModel
	}
	if err == nil && verdict.Usage.Valid() && f.llmCb.OnLLMTokens != nil {
		f.llmCb.OnLLMTokens(verdict.Model, verdict.Usage)
	} else if err == nil && !verdict.Usage.Valid() && f.llmCb.OnLLMUsageMissing != nil {
		f.llmCb.OnLLMUsageMissing(verdict.Model)
	}
}

func contentFilterAuditDecision(v LLMVerdict, minConfidence float64, live, invalid bool) llmcapture.ContentFilterDecision {
	d := llmcapture.ContentFilterDecision{
		IsEnglish: v.IsEnglish, Confidence: v.Confidence, Reason: v.Reason,
		MinConfidence: minConfidence, Live: live,
	}
	switch {
	case invalid:
		d.Outcome, d.IsEnglish, d.Confidence, d.Reason = "invalid_response", false, 0, ""
	case v.IsEnglish:
		d.Outcome = "english"
	case v.Confidence < minConfidence:
		d.Outcome = "low_confidence"
	default:
		d.Outcome, d.WouldDrop = "non_english", true
	}
	return d
}

// verdictToReason converts a verdict (from cache or live) to a
// drop-reason. Honours LLMMinConfidenceForDrop — below that, even
// IsEnglish=false keeps the torrent.
func (f *Filter) verdictToReason(v LLMVerdict) DropReason {
	if v.IsEnglish {
		return ReasonNone
	}
	if math.IsNaN(v.Confidence) || math.IsInf(v.Confidence, 0) ||
		v.Confidence < 0 || v.Confidence > 1 ||
		math.IsNaN(f.cfg.LLMMinConfidenceForDrop) ||
		math.IsInf(f.cfg.LLMMinConfidenceForDrop, 0) ||
		f.cfg.LLMMinConfidenceForDrop <= 0 ||
		f.cfg.LLMMinConfidenceForDrop > 1 {
		// An invalid model/config score must fail open. Comparisons against
		// NaN are always false, so relying on the threshold check below would
		// turn every non-English verdict into an actionable drop.
		return ReasonNone
	}
	if v.Confidence < f.cfg.LLMMinConfidenceForDrop {
		// Verdict is "non-English but unsure" — keep, defer to
		// other signals (Sonarr/Radarr evidence later, or just
		// general English bias of the rest of the corpus).
		return ReasonNone
	}
	return ReasonLLMNonEnglish
}

// evaluate runs the check ladder and returns the first matching
// reason plus a secondary detail string. The detail is only non-empty
// for ReasonBlockedExtension (the matched extension) — all other
// reasons return "". Pulled out of Decide() so the WouldDrop / Allow
// logic is in one place.
func (f *Filter) evaluate(in Input) (DropReason, string) {
	// Anime carve-out (computed lazily, at most once, and only when a
	// destructive step below would otherwise fire). When AnimeAware is on,
	// a recognised anime release is exempted from the script / language /
	// NSFW-keyword drops that were deleting watchable anime — the single
	// largest anime data-loss source at ingest. See Config.AnimeAware.
	var (
		animeOnce bool
		animeSig  anime.Signals
	)
	detectAnime := func() anime.Signals {
		if !animeOnce {
			animeSig = anime.Detect(in.Title)
			animeOnce = true
		}
		return animeSig
	}

	// 1. Blocked extension. Cheapest check; lots of zero-content
	// torrents (iso, exe) get caught here so the rest of the
	// ladder doesn't run. Normalise the returned ext so Prometheus
	// label cardinality stays bounded — "ISO" and "iso" must
	// produce the same series, not two.
	if in.PrimaryExtension != "" && extInBlocklist(in.PrimaryExtension, f.cfg.BlockedExtensions) {
		return ReasonBlockedExtension, strings.ToLower(strings.TrimSpace(in.PrimaryExtension))
	}

	// 2. Blocked content_type from operator config (e.g. "ebook").
	if in.ContentType != "" {
		ct := strings.ToLower(strings.TrimSpace(in.ContentType))
		for _, blocked := range f.cfg.BlockedContentTypes {
			if strings.EqualFold(blocked, ct) {
				return ReasonBlockedContentType, ""
			}
		}
	}

	// 3. NSFW content_type. Separate from BlockedContentTypes
	// because NSFW has its own toggle (DropNSFW) and the operator
	// might want to block it without listing every variant.
	if f.cfg.DropNSFW && isNSFWContentType(in.ContentType) {
		return ReasonNSFWContentType, ""
	}

	// 4. NSFW keyword in title. Catches studio-tag-style entries
	// the classifier might miss (e.g. "[brazzers] ..."). A KNOWN anime
	// fansub group carves this out — rating tokens like "18+"/"r18"
	// collide with mainstream ecchi/seinen anime, and the known-group
	// allowlist is disjoint from adult studios so no porn is rescued.
	// Non-anime "[18+]" releases (no fansub group) still drop.
	if f.cfg.DropNSFW && titleMatchesNSFW(in.Title) {
		if !(f.cfg.AnimeAware && detectAnime().IsKnownFansub()) {
			return ReasonNSFWKeyword, ""
		}
	}

	// 5. Non-Latin script in title. Catches the ~40% of torrents
	// with empty languages tag that are obviously non-English by
	// script alone (Cyrillic, CJK, etc.). A recognised anime release
	// (a native-title anime with a fansub-group bracket) is carved out —
	// a single CJK glyph must not delete an English-fansubbed anime.
	if f.cfg.DropNonLatinScript && titleContainsNonLatinScript(in.Title) {
		if !(f.cfg.AnimeAware && detectAnime().IsAnime()) {
			return ReasonNonLatinScript, ""
		}
	}

	// 7. Foreign-audio release. Placed after the script check and BEFORE the
	// language check because the language check is what lets this class
	// through: TMDB reports the language of the work, so a dub of an English
	// film is tagged "en" and cleared. Deliberately NOT anime-carved — a
	// Chinese-subtitled [LoliHouse] release is precisely what the carve-out
	// must not rescue, and a curated English subbing group already counts as
	// English evidence inside ForeignAudioOnly.
	if f.cfg.DropForeignAudio && anime.ForeignAudioOnly(in.Title) {
		return ReasonForeignAudio, ""
	}

	// 6. Language tag check. Only when the classifier actually
	// set a language; an empty/nil set is "unknown, defer to
	// script check above + Phase 2 LLM tier later." A recognised anime
	// release is carved out — TMDB tags anime with its ORIGINAL language
	// ("ja"), which is the language of the work, not of this (subbed/
	// dubbed) release; dropping on it deletes exactly the anime matched
	// best.
	if f.cfg.RequireEnglishLanguage && len(in.Languages) > 0 {
		if !anyAllowedLanguage(in.Languages, f.cfg.AllowedLanguages) {
			if !(f.cfg.AnimeAware && detectAnime().IsAnime()) {
				return ReasonNonEnglishLanguage, ""
			}
		}
	}

	// 7. mp3-only music. Music is kept generally, but not the
	// pure-mp3 subset. Mixed (mp3 + lossless) is fine because
	// most operators want lossless tagged with the mp3 also
	// available for mobile playback.
	if f.cfg.DropLossyAudioOnly && strings.EqualFold(in.ContentType, "music") {
		if isPureMP3(in.AllExtensions) {
			return ReasonMP3Only, ""
		}
	}

	return ReasonNone, ""
}

// anyAllowedLanguage returns true iff at least one tag in `langs`
// is in `allowed` (case-insensitive). Empty `allowed` is treated as
// "permit nothing"; the caller's contract is that AllowedLanguages
// is non-empty when RequireEnglishLanguage=true (defaults handle
// this).
func anyAllowedLanguage(langs []string, allowed []string) bool {
	if len(allowed) == 0 {
		return false
	}
	for _, lang := range langs {
		l := strings.ToLower(strings.TrimSpace(lang))
		for _, a := range allowed {
			if strings.EqualFold(a, l) {
				return true
			}
		}
	}
	return false
}

// isPureMP3 reports true iff every extension in `exts` is mp3 OR
// the torrent contains mp3 audio AND no other audio formats. The
// "OR" branch handles single-file torrents (one .mp3); the AND
// branch handles album-style torrents that include cue/log/jpg
// alongside the audio — those non-audio extensions don't count
// against the "pure mp3" determination.
//
// Empty `exts` returns false — we don't have enough info to call
// it pure mp3.
func isPureMP3(exts []string) bool {
	if len(exts) == 0 {
		return false
	}
	hasAnyMP3 := false
	hasOtherAudio := false
	for _, e := range exts {
		if isMP3(e) {
			hasAnyMP3 = true
			continue
		}
		if _, ok := nonMP3MusicExtensions[e]; ok {
			hasOtherAudio = true
		}
	}
	return hasAnyMP3 && !hasOtherAudio
}
