package contentfilter

import (
	"sync/atomic"
	"testing"
	"time"
)

// Test plan:
//   - Threshold-crossing fires the notifier exactly ONCE per
//     (reason, isEnglish) tuple (no double-emission).
//   - Below-threshold counts accumulate without firing.
//   - Sliding window evicts old observations.
//   - Snapshot returns a coherent count map.
//   - Empty-reason verdicts are ignored.
//   - Different (reason, isEnglish) tuples are independent.

func TestRuleMiner_BelowThresholdNoFire(t *testing.T) {
	var fired atomic.Int32
	m := newRuleMiner(time.Hour, 5, func(string, bool, int) {
		fired.Add(1)
	})
	for i := 0; i < 4; i++ {
		m.Record(LLMVerdict{Reason: "russian-particle", IsEnglish: false})
	}
	if fired.Load() != 0 {
		t.Errorf("below threshold should not fire; fired=%d", fired.Load())
	}
}

func TestRuleMiner_ThresholdCrossingFiresOnce(t *testing.T) {
	var fired atomic.Int32
	m := newRuleMiner(time.Hour, 5, func(reason string, isEnglish bool, count int) {
		if reason != "russian-particle" {
			t.Errorf("notifier reason: got %q want russian-particle", reason)
		}
		if isEnglish {
			t.Errorf("notifier isEnglish: got true want false")
		}
		if count < 5 {
			t.Errorf("notifier count: got %d want >=5", count)
		}
		fired.Add(1)
	})
	// Cross threshold.
	for i := 0; i < 10; i++ {
		m.Record(LLMVerdict{Reason: "russian-particle", IsEnglish: false})
	}
	if fired.Load() != 1 {
		t.Errorf("should fire exactly once; fired=%d", fired.Load())
	}
}

func TestRuleMiner_IndependentTuples(t *testing.T) {
	type call struct {
		reason  string
		english bool
	}
	var calls []call
	m := newRuleMiner(time.Hour, 3, func(r string, e bool, _ int) {
		calls = append(calls, call{r, e})
	})

	for i := 0; i < 3; i++ {
		m.Record(LLMVerdict{Reason: "russian-particle", IsEnglish: false})
	}
	for i := 0; i < 3; i++ {
		m.Record(LLMVerdict{Reason: "spanish-article", IsEnglish: false})
	}
	for i := 0; i < 3; i++ {
		// Same reason, different IsEnglish — separate bucket.
		m.Record(LLMVerdict{Reason: "russian-particle", IsEnglish: true})
	}

	if len(calls) != 3 {
		t.Fatalf("want 3 promotions (3 distinct buckets), got %d: %+v", len(calls), calls)
	}
	want := map[call]bool{
		{"russian-particle", false}: true,
		{"spanish-article", false}:  true,
		{"russian-particle", true}:  true,
	}
	for _, c := range calls {
		if !want[c] {
			t.Errorf("unexpected promotion: %+v", c)
		}
		delete(want, c)
	}
	if len(want) != 0 {
		t.Errorf("missing promotions: %+v", want)
	}
}

func TestRuleMiner_EmptyReasonIgnored(t *testing.T) {
	var fired atomic.Int32
	m := newRuleMiner(time.Hour, 1, func(string, bool, int) {
		fired.Add(1)
	})
	// 100 empty-reason verdicts should never fire — the miner
	// can't sensibly aggregate "we don't know why."
	for i := 0; i < 100; i++ {
		m.Record(LLMVerdict{Reason: "", IsEnglish: false})
	}
	if fired.Load() != 0 {
		t.Errorf("empty reason should be ignored; fired=%d", fired.Load())
	}
}

func TestRuleMiner_NilNotifierDoesNotPanic(t *testing.T) {
	m := newRuleMiner(time.Hour, 2, nil)
	// Crossing threshold with nil notifier should NOT panic — we
	// still want the bucket marked promoted so Snapshot can show
	// it on a future operator dashboard.
	for i := 0; i < 5; i++ {
		m.Record(LLMVerdict{Reason: "x", IsEnglish: false})
	}
	snap := m.Snapshot()
	if snap["x:non-en"] != 5 {
		t.Errorf("snapshot count: got %d want 5", snap["x:non-en"])
	}
}

func TestRuleMiner_SlidingWindowEvicts(t *testing.T) {
	// Drive the clock manually so we can age observations out.
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }
	m := newRuleMiner(10*time.Second, 5, nil)
	m.clock = clock

	// 4 records at t=0
	for i := 0; i < 4; i++ {
		m.Record(LLMVerdict{Reason: "r", IsEnglish: false})
	}
	// Advance past the window.
	now = now.Add(15 * time.Second)
	// One more record at t=15s. The previous 4 should have aged
	// out, so total count = 1, not 5.
	m.Record(LLMVerdict{Reason: "r", IsEnglish: false})

	snap := m.Snapshot()
	if snap["r:non-en"] != 1 {
		t.Errorf("after window expiry: got %d, want 1", snap["r:non-en"])
	}
}

func TestRuleMiner_SnapshotIsAPureCopy(t *testing.T) {
	m := newRuleMiner(time.Hour, 100, nil)
	m.Record(LLMVerdict{Reason: "a", IsEnglish: true})
	m.Record(LLMVerdict{Reason: "a", IsEnglish: true})
	m.Record(LLMVerdict{Reason: "b", IsEnglish: false})

	snap := m.Snapshot()
	if snap["a:en"] != 2 {
		t.Errorf("a:en got %d want 2", snap["a:en"])
	}
	if snap["b:non-en"] != 1 {
		t.Errorf("b:non-en got %d want 1", snap["b:non-en"])
	}

	// Mutating the snapshot map should NOT affect the miner.
	snap["a:en"] = 999
	snap2 := m.Snapshot()
	if snap2["a:en"] != 2 {
		t.Errorf("internal state leaked through snapshot: %d", snap2["a:en"])
	}
}

func TestRuleMiner_DefaultsOnInvalidArgs(t *testing.T) {
	// Negative/zero window → 7d default; negative threshold → 100.
	m := newRuleMiner(0, 0, nil)
	if m.window != 7*24*time.Hour {
		t.Errorf("default window: got %v want 168h", m.window)
	}
	if m.threshold != 100 {
		t.Errorf("default threshold: got %d want 100", m.threshold)
	}
}

func TestReasonKey(t *testing.T) {
	if reasonKey("foo", true) != "foo:en" {
		t.Errorf("en suffix: got %q", reasonKey("foo", true))
	}
	if reasonKey("foo", false) != "foo:non-en" {
		t.Errorf("non-en suffix: got %q", reasonKey("foo", false))
	}
}
