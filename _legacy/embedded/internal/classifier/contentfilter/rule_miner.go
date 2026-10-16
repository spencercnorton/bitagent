package contentfilter

import (
	"sync"
	"time"
)

// ruleMiner watches the LLM verdict stream and promotes recurring
// patterns to deterministic-rule candidates. The operator's spec
// (2026-04-25): "if recurring patterns are found we should just
// implement rules instead of running hundreds of thousands of
// random torrent names through an llm."
//
// Mechanism:
//
//   - Every LLM verdict carries a `Reason` tag (kebab-case from
//     the prompt — e.g. "russian-particle", "spanish-article").
//   - The miner counts occurrences per (Reason, IsEnglish) tuple
//     across a sliding time window (default 7 days).
//   - When a tuple's count crosses Threshold (default 100), it's
//     emitted as a "candidate rule" for human review. A
//     bitagent_contentfilter_rule_candidate{reason} metric lights
//     up so the operator sees promotions in real time.
//   - Promoted rules go to a `derived_rules.go` file (emitted as
//     a runtime listing on the dashboard / in logs); a human
//     codifies them as proper deterministic checks in patterns.go
//     in a follow-up MR. The miner does NOT auto-modify code.
//
// Why threshold-based, not auto-codification: the LLM occasionally
// hallucinates a reason ("avante-garde-french" for an English film
// titled "Avant-Garde"). One-shot codification would entrench the
// hallucination. Requiring N=100 occurrences makes it implausible
// for a hallucination to cross the threshold without genuine
// recurrence.
type ruleMiner struct {
	mu sync.Mutex

	// counts tracks occurrences per (reason, isEnglish) over
	// time. Key: reason+":"+("en"|"non-en"). Value: timestamps
	// for the sliding window.
	counts map[string][]time.Time

	// promoted tracks reasons that have already crossed the
	// threshold. We keep counting but don't double-emit
	// candidate metrics for the same reason.
	promoted map[string]struct{}

	// candidateNotifier is invoked once when a (reason, isEnglish)
	// crosses the threshold. Set by the Filter to bump a metric
	// and emit a structured log line. Optional; nil disables
	// notification (counts still accumulate).
	candidateNotifier func(reason string, isEnglish bool, count int)

	window    time.Duration
	threshold int
	clock     func() time.Time
}

func newRuleMiner(window time.Duration, threshold int, notifier func(string, bool, int)) *ruleMiner {
	if window <= 0 {
		window = 7 * 24 * time.Hour
	}
	if threshold <= 0 {
		threshold = 100
	}
	return &ruleMiner{
		counts:            make(map[string][]time.Time),
		promoted:          make(map[string]struct{}),
		candidateNotifier: notifier,
		window:            window,
		threshold:         threshold,
		clock:             time.Now,
	}
}

func reasonKey(reason string, isEnglish bool) string {
	if isEnglish {
		return reason + ":en"
	}
	return reason + ":non-en"
}

// Record adds one observation to the miner. Returns true iff this
// observation just promoted the (reason, isEnglish) bucket to
// candidate-rule status.
func (m *ruleMiner) Record(verdict LLMVerdict) bool {
	if verdict.Reason == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	key := reasonKey(verdict.Reason, verdict.IsEnglish)
	now := m.clock()
	cutoff := now.Add(-m.window)

	// Append + prune-by-window. The slice stays small in
	// steady state because we evict everything older than
	// m.window on every Record.
	stamps := m.counts[key]
	stamps = append(stamps, now)
	// Drop expired (linear scan; the slice is sorted by insertion
	// order so we stop at the first non-expired stamp).
	i := 0
	for i < len(stamps) && stamps[i].Before(cutoff) {
		i++
	}
	stamps = stamps[i:]
	m.counts[key] = stamps

	if _, already := m.promoted[key]; already {
		return false
	}
	if len(stamps) >= m.threshold {
		m.promoted[key] = struct{}{}
		if m.candidateNotifier != nil {
			m.candidateNotifier(verdict.Reason, verdict.IsEnglish, len(stamps))
		}
		return true
	}
	return false
}

// Snapshot returns a copy of the current per-reason counts. Used
// by tests + a future operator-facing endpoint that lists
// candidates ranked by frequency.
func (m *ruleMiner) Snapshot() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]int, len(m.counts))
	now := m.clock()
	cutoff := now.Add(-m.window)
	for k, stamps := range m.counts {
		c := 0
		for _, t := range stamps {
			if !t.Before(cutoff) {
				c++
			}
		}
		out[k] = c
	}
	return out
}
