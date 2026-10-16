package contentfilter

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeLLM is a deterministic stand-in for the OpenAI client. It lets
// tests pin the verdict per call and count invocations so we can
// distinguish cache hits from live calls.
type fakeLLM struct {
	calls   atomic.Int32
	verdict LLMVerdict
	err     error
}

func (f *fakeLLM) Classify(_ context.Context, _ string) (LLMVerdict, error) {
	f.calls.Add(1)
	return f.verdict, f.err
}

func phase2Config() Config {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.Enforce = false // shadow mode keeps Allow=true
	cfg.LLMEnabled = true
	cfg.LLMOpenaiApiKey = "test-key"
	cfg.LLMModel = "gpt-5.4-nano-test"
	cfg.LLMPromptVersion = "v1"
	cfg.LLMTimeout = "1s"
	cfg.LLMDailyBudget = 10
	cfg.LLMCacheMaxEntries = 100
	cfg.LLMCacheTTL = "1h"
	cfg.LLMMinConfidenceForDrop = 0.85
	cfg.LLMRuleMinerThreshold = 100
	cfg.LLMRuleMinerWindow = "1h"
	return cfg
}

func TestDecide_LLMCachesAcrossCalls(t *testing.T) {
	llm := &fakeLLM{verdict: LLMVerdict{IsEnglish: false, Confidence: 0.95, Reason: "russian-particle"}}
	var hits, misses atomic.Int32
	cb := LLMCallbacks{
		OnCacheHit:  func() { hits.Add(1) },
		OnCacheMiss: func() { misses.Add(1) },
	}
	f := NewWithLLM(phase2Config(), llm, cb)

	// Same normalized title twice — second call must hit the cache.
	in := Input{Title: "Pelicula.2024.1080p.x264-RARBG"}
	d1 := f.Decide(in)
	d2 := f.Decide(in)

	// Both decisions identical (would-drop, llm-non-english).
	if !d1.WouldDrop || d1.Reason != ReasonLLMNonEnglish {
		t.Errorf("first call: got %+v", d1)
	}
	if !d2.WouldDrop || d2.Reason != ReasonLLMNonEnglish {
		t.Errorf("second call: got %+v", d2)
	}
	if llm.calls.Load() != 1 {
		t.Errorf("LLM should be called once (cache hit on 2nd); got %d", llm.calls.Load())
	}
	if hits.Load() != 1 {
		t.Errorf("expected 1 cache hit, got %d", hits.Load())
	}
	if misses.Load() != 1 {
		t.Errorf("expected 1 cache miss, got %d", misses.Load())
	}
}

func TestDecide_LLMReleaseTagsShareCache(t *testing.T) {
	// The whole point of normalizeTitle is that titles differing
	// only in scene-tag noise hit the same cache slot — so the
	// LLM is called ONCE for "Pelicula 2024" regardless of the
	// release variant.
	llm := &fakeLLM{verdict: LLMVerdict{IsEnglish: false, Confidence: 0.92, Reason: "spanish-article"}}
	f := NewWithLLM(phase2Config(), llm, LLMCallbacks{})

	variants := []string{
		"Pelicula.2024.1080p.x264-RARBG",
		"Pelicula 2024 [WEB-DL]",
		"Pelicula.2024.HEVC.x265",
	}
	for _, v := range variants {
		f.Decide(Input{Title: v})
	}

	if llm.calls.Load() != 1 {
		t.Errorf("normalized cache miss: LLM called %d times for variants of same title", llm.calls.Load())
	}
}

func TestDecide_LLMSkippedWhenLanguageTagPresent(t *testing.T) {
	// If the upstream classifier already set a language, the LLM
	// is not consulted — saves budget on titles already pinned.
	llm := &fakeLLM{verdict: LLMVerdict{IsEnglish: false, Confidence: 0.99, Reason: "x"}}
	f := NewWithLLM(phase2Config(), llm, LLMCallbacks{})

	d := f.Decide(Input{Title: "Some Movie 2024", Languages: []string{"en"}})
	if !d.Allow || d.WouldDrop {
		t.Errorf("English-tagged title should be allowed: %+v", d)
	}
	if llm.calls.Load() != 0 {
		t.Errorf("LLM should not be called when language tag present; got %d", llm.calls.Load())
	}
}

func TestDecide_NativePrivateSkipsLLMWhilePublicBehaviorIsUnchanged(t *testing.T) {
	llm := &fakeLLM{verdict: LLMVerdict{IsEnglish: false, Confidence: 0.99, Reason: "foreign"}}
	f := NewWithLLM(phase2Config(), llm, LLMCallbacks{})

	privateDecision := f.Decide(Input{Private: true, Title: "Some Private Movie 2026"})
	if !privateDecision.Allow || privateDecision.WouldDrop {
		t.Fatalf("private residual should be kept without model use: %+v", privateDecision)
	}
	if llm.calls.Load() != 0 {
		t.Fatalf("native private flag must block the LLM, got %d calls", llm.calls.Load())
	}

	publicDecision := f.Decide(Input{Title: "Some Public Movie 2026"})
	if !publicDecision.WouldDrop || publicDecision.Reason != ReasonLLMNonEnglish {
		t.Fatalf("public residual should retain existing LLM behavior: %+v", publicDecision)
	}
	if llm.calls.Load() != 1 {
		t.Fatalf("public residual should call the LLM once, got %d", llm.calls.Load())
	}
}

func TestDecide_NativePrivateStillRunsDeterministicChecks(t *testing.T) {
	llm := &fakeLLM{}
	f := NewWithLLM(phase2Config(), llm, LLMCallbacks{})

	d := f.Decide(Input{Private: true, Title: "Some Private Movie 2026", PrimaryExtension: "iso"})
	if d.Reason != ReasonBlockedExtension {
		t.Fatalf("native private flag must not bypass deterministic policy: %+v", d)
	}
	if llm.calls.Load() != 0 {
		t.Fatalf("deterministic private decision must not call LLM, got %d", llm.calls.Load())
	}
}

func TestDecide_LLMSkippedWhenScriptAlreadyDropped(t *testing.T) {
	// A Cyrillic title is dropped by the script filter (step 5)
	// BEFORE the LLM tier — no LLM call needed.
	llm := &fakeLLM{verdict: LLMVerdict{}}
	f := NewWithLLM(phase2Config(), llm, LLMCallbacks{})

	d := f.Decide(Input{Title: "Война и мир 2024"})
	if d.Reason != ReasonNonLatinScript {
		t.Errorf("Cyrillic title: got %v, want ReasonNonLatinScript", d.Reason)
	}
	if llm.calls.Load() != 0 {
		t.Errorf("LLM should not be called when script filter drops first; got %d", llm.calls.Load())
	}
}

func TestDecide_LLMConfidenceBelowThresholdKeeps(t *testing.T) {
	// Verdict is "non-English" but confidence < threshold — keep
	// the torrent. We'd rather keep an ambiguous title than
	// wrongly drop one.
	llm := &fakeLLM{verdict: LLMVerdict{IsEnglish: false, Confidence: 0.5, Reason: "ambiguous"}}
	cfg := phase2Config()
	cfg.LLMMinConfidenceForDrop = 0.85
	f := NewWithLLM(cfg, llm, LLMCallbacks{})

	d := f.Decide(Input{Title: "Some Title 2024"})
	if d.WouldDrop {
		t.Errorf("low-confidence non-English should be kept: %+v", d)
	}
}

func TestDecide_InvalidLLMConfidenceThresholdFailsOpen(t *testing.T) {
	for _, threshold := range []float64{math.NaN(), math.Inf(1), -0.1, 0, 1.1} {
		llm := &fakeLLM{verdict: LLMVerdict{
			IsEnglish:  false,
			Confidence: 1,
			Reason:     "non-english",
		}}
		cfg := phase2Config()
		cfg.Enforce = true
		cfg.LLMMinConfidenceForDrop = threshold
		decision := NewWithLLM(cfg, llm, LLMCallbacks{}).Decide(
			Input{Title: "Some Title 2024"},
		)
		if decision.WouldDrop {
			t.Errorf(
				"threshold %v must fail open, got %+v",
				threshold,
				decision,
			)
		}
	}
}

func TestDecide_InvalidLLMModelConfidenceFailsOpen(t *testing.T) {
	for _, confidence := range []float64{
		math.NaN(),
		math.Inf(1),
		-0.1,
		1.1,
	} {
		llm := &fakeLLM{verdict: LLMVerdict{
			IsEnglish:  false,
			Confidence: confidence,
			Reason:     "non-english",
		}}
		cfg := phase2Config()
		cfg.Enforce = true
		decision := NewWithLLM(cfg, llm, LLMCallbacks{}).Decide(
			Input{Title: "Some Title 2024"},
		)
		if decision.WouldDrop {
			t.Errorf(
				"model confidence %v must fail open, got %+v",
				confidence,
				decision,
			)
		}
	}
}

func TestDecide_LLMErrorFailsOpen(t *testing.T) {
	// LLM error should never drop a torrent — fail open. The
	// caller's safe default is "keep, defer to other signals."
	llm := &fakeLLM{err: errors.New("boom")}
	var ok atomic.Int32
	var fail atomic.Int32
	cb := LLMCallbacks{
		OnLLMCall: func(success bool, _ float64) {
			if success {
				ok.Add(1)
			} else {
				fail.Add(1)
			}
		},
	}
	f := NewWithLLM(phase2Config(), llm, cb)

	d := f.Decide(Input{Title: "Test 2024"})
	if d.WouldDrop {
		t.Errorf("LLM error should fail open; got %+v", d)
	}
	if fail.Load() != 1 {
		t.Errorf("OnLLMCall(false) should fire once; got %d", fail.Load())
	}
}

func TestDecide_LLMBudgetExhaustedFailsOpen(t *testing.T) {
	llm := &fakeLLM{verdict: LLMVerdict{IsEnglish: false, Confidence: 0.99, Reason: "russian-particle"}}
	cfg := phase2Config()
	cfg.LLMDailyBudget = 1
	var exhausted atomic.Int32
	cb := LLMCallbacks{
		OnBudgetExhausted: func() { exhausted.Add(1) },
	}
	f := NewWithLLM(cfg, llm, cb)

	// 1st call uses the budget AND drops.
	d1 := f.Decide(Input{Title: "first title"})
	if d1.Reason != ReasonLLMNonEnglish {
		t.Errorf("first call should drop via LLM; got %+v", d1)
	}
	// 2nd call has no budget AND a different normalized title (no
	// cache hit). Must fail open.
	d2 := f.Decide(Input{Title: "second title"})
	if d2.WouldDrop {
		t.Errorf("budget-exhausted call should fail open; got %+v", d2)
	}
	if exhausted.Load() != 1 {
		t.Errorf("OnBudgetExhausted should fire once; got %d", exhausted.Load())
	}
}

func TestDecide_LLMBudgetSurvivesCacheHits(t *testing.T) {
	// Cache hits MUST NOT consume budget — that's the whole point
	// of caching. Budget=1, but if we hit cache 99 times, the LLM
	// only gets called once.
	llm := &fakeLLM{verdict: LLMVerdict{IsEnglish: false, Confidence: 0.95, Reason: "russian-particle"}}
	cfg := phase2Config()
	cfg.LLMDailyBudget = 1
	f := NewWithLLM(cfg, llm, LLMCallbacks{})

	for i := 0; i < 100; i++ {
		f.Decide(Input{Title: "same title 2024"})
	}
	if llm.calls.Load() != 1 {
		t.Errorf("LLM should be called once; got %d", llm.calls.Load())
	}
}

func TestDailyBudget_RollsOverAtUTCMidnight(t *testing.T) {
	b := newDailyBudget(2)
	day0 := time.Date(2026, 4, 24, 23, 59, 0, 0, time.UTC)
	day1 := time.Date(2026, 4, 25, 0, 0, 1, 0, time.UTC)

	// 2 calls on day0 — both succeed.
	if !b.TryConsume(day0) {
		t.Errorf("call 1: should succeed")
	}
	if !b.TryConsume(day0) {
		t.Errorf("call 2: should succeed")
	}
	// 3rd on day0 — over cap.
	if b.TryConsume(day0) {
		t.Errorf("call 3 (day0): should fail")
	}
	// 1st on day1 — bucket reset, succeeds.
	if !b.TryConsume(day1) {
		t.Errorf("call 1 (day1): should succeed after rollover")
	}
}

func TestDailyBudget_ZeroCapDeniesAlways(t *testing.T) {
	// Budget=0 is the operator's "kill switch" without flipping
	// LLMEnabled — the LLM tier is configured but cannot fire.
	b := newDailyBudget(0)
	if b.TryConsume(time.Now()) {
		t.Errorf("zero-cap budget should always deny")
	}
}

func TestDecide_LLMRuleMinerWiredToCallback(t *testing.T) {
	// When threshold is crossed, the rule-candidate notifier must
	// fire via LLMCallbacks.OnRuleCandidate. We pin the threshold
	// low (3) and feed 3 distinct titles with the same reason to
	// confirm the wiring.
	llm := &fakeLLM{verdict: LLMVerdict{IsEnglish: false, Confidence: 0.95, Reason: "russian-particle"}}
	cfg := phase2Config()
	cfg.LLMRuleMinerThreshold = 3

	type evt struct {
		reason  string
		english bool
		count   int
	}
	var events []evt
	cb := LLMCallbacks{
		OnRuleCandidate: func(r string, e bool, c int) {
			events = append(events, evt{r, e, c})
		},
	}
	f := NewWithLLM(cfg, llm, cb)

	// Three distinct titles → three distinct cache entries → three LLM calls.
	titles := []string{"film alpha", "film beta", "film gamma"}
	for _, tt := range titles {
		f.Decide(Input{Title: tt})
	}

	if len(events) != 1 {
		t.Errorf("rule-candidate should fire once at threshold; got %d events: %+v", len(events), events)
	}
	if len(events) > 0 && events[0].reason != "russian-particle" {
		t.Errorf("event reason: got %q want russian-particle", events[0].reason)
	}
}

func TestDecide_LLMDisabledIsNoOp(t *testing.T) {
	// LLMEnabled=false: the filter must NOT consult the LLM even
	// for residual titles.
	llm := &fakeLLM{}
	cfg := phase2Config()
	cfg.LLMEnabled = false
	f := NewWithLLM(cfg, llm, LLMCallbacks{})

	d := f.Decide(Input{Title: "Some Title 2024"})
	if d.WouldDrop {
		t.Errorf("LLM disabled: should be allow: %+v", d)
	}
	if llm.calls.Load() != 0 {
		t.Errorf("LLM should not be called; got %d", llm.calls.Load())
	}
}

func TestDecide_LLMNilClientIsSafe(t *testing.T) {
	// Defensive: NewWithLLM(cfg, nil, cb) must not panic in Decide.
	cfg := phase2Config()
	f := NewWithLLM(cfg, nil, LLMCallbacks{})

	d := f.Decide(Input{Title: "Some Title 2024"})
	if d.WouldDrop {
		t.Errorf("nil LLM client: should be allow: %+v", d)
	}
}

func TestDecideDeterministic_NeverConsultsLLM(t *testing.T) {
	// DecideDeterministic is the pre-classifier hook contract:
	// the deterministic ladder runs but the LLM tier MUST be
	// skipped, even when LLMEnabled=true and the residual
	// condition (Latin script, no language tag) is satisfied.
	// Regression guard for Jeeves's high-confidence finding on
	// the BEP-9 hook used Decide() instead of
	// DecideDeterministic and burned LLM budget on every
	// Latin-script title because Languages was empty
	// pre-classifier.
	llm := &fakeLLM{verdict: LLMVerdict{IsEnglish: false, Confidence: 0.99, Reason: "x"}}
	f := NewWithLLM(phase2Config(), llm, LLMCallbacks{})

	for i := 0; i < 5; i++ {
		d := f.DecideDeterministic(Input{Title: "Some Title 2024"})
		if d.WouldDrop {
			t.Errorf("DecideDeterministic should not drop on LLM verdict: %+v", d)
		}
	}
	if llm.calls.Load() != 0 {
		t.Errorf("DecideDeterministic must not call the LLM; got %d calls", llm.calls.Load())
	}
}

func TestDecideDeterministic_StillFiresDeterministicChecks(t *testing.T) {
	// DecideDeterministic must still fire steps 1-8 of the
	// ladder. A Cyrillic title gets dropped; an .iso primary
	// extension gets dropped.
	f := NewWithLLM(phase2Config(), &fakeLLM{}, LLMCallbacks{})

	d := f.DecideDeterministic(Input{Title: "Война и мир 2024"})
	if d.Reason != ReasonNonLatinScript {
		t.Errorf("Cyrillic via DecideDeterministic: got %v want ReasonNonLatinScript", d.Reason)
	}

	d = f.DecideDeterministic(Input{Title: "Some Movie 2024", PrimaryExtension: "iso"})
	if d.Reason != ReasonBlockedExtension {
		t.Errorf("iso via DecideDeterministic: got %v want ReasonBlockedExtension", d.Reason)
	}
}

func TestNewOpenAIClient_SelfHostedEmptyKeyAllowed(t *testing.T) {
	// Regression guard for Jeeves's medium-confidence finding on
	// a self-hosted base URL with an empty API key MUST
	// not error in Classify — the buildContentFilter wiring
	// allows that combination as "self-hosted, no auth."
	c := NewOpenAIClient("", "test-model", "http://127.0.0.1:1/v1", "", "v1", 200*time.Millisecond).(*openaiClient)

	// We can't run a real HTTP call in a unit test; verify by
	// pointing at an unreachable port and confirming the error
	// is a network error, NOT the "empty API key" pre-flight.
	_, err := c.Classify(context.Background(), "test")
	if err == nil {
		t.Fatalf("expected network error from unreachable port")
	}
	if strings.Contains(err.Error(), "empty API key") {
		t.Errorf("self-hosted with empty key should not pre-flight error; got: %v", err)
	}
}

func TestNewOpenAIClient_DefaultBaseRequiresKey(t *testing.T) {
	// The hard-error path: empty key + default OpenAI base URL is
	// still a fail-fast. We don't want to silently send unauth'd
	// calls to api.openai.com.
	c := NewOpenAIClient("", "test-model", "", "", "v1", 0).(*openaiClient)
	_, err := c.Classify(context.Background(), "test")
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "empty API key") {
		t.Errorf("default base + empty key should hard-error; got: %v", err)
	}
}
