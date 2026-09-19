package adapter

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/spencercnorton/bitagent/internal/database/search"
)

type fakeSet struct {
	set map[string]struct{}
	err error
}

func (f fakeSet) DeadSet(_ context.Context, _ [][]byte) (map[string]struct{}, error) {
	return f.set, f.err
}

func (f fakeSet) BlockedSet(_ context.Context, _ [][]byte) (map[string]struct{}, error) {
	return f.set, f.err
}

type fakeVerdictsMetrics struct{ counts map[string]int }

func (m *fakeVerdictsMetrics) Reader(reader, outcome string, n int) {
	if m.counts == nil {
		m.counts = map[string]int{}
	}
	m.counts[reader+"/"+outcome] += n
}

func hexOf(b byte) string {
	h := make([]byte, 20)
	h[0] = b
	return hashKey(h)
}

// Phase-B torznab reader semantics (docs/design/verdict-ledger.md §4):
// shadow mode meters divergences but never changes serving; live mode
// excludes ledger-blocked items in UNION with the dead set; a ledger error
// fails open.
func TestApplyLivenessFilter_VerdictsShadowAndLive(t *testing.T) {
	now := time.Date(2026, 7, 19, 8, 0, 0, 0, time.UTC)
	res := func() search.TorrentContentResult {
		return search.TorrentContentResult{Items: []search.TorrentContentResultItem{
			makeItem(1, sourceWithSeeders("dht", 4, true, now)), // dead only
			makeItem(2, sourceWithSeeders("dht", 4, true, now)), // ledger-blocked only (resurrection cohort)
			makeItem(3, sourceWithSeeders("dht", 4, true, now)), // both
			makeItem(4, sourceWithSeeders("dht", 4, true, now)), // clean
		}}
	}
	dead := map[string]struct{}{hexOf(1): {}, hexOf(3): {}}
	blocked := map[string]struct{}{hexOf(2): {}, hexOf(3): {}}

	// Shadow: only dead items drop; divergences counted.
	m := &fakeVerdictsMetrics{}
	a := Adapter{liveness: fakeSet{set: dead}}.WithVerdicts(fakeSet{set: blocked}, false, m)
	out, err := a.applyLivenessFilter(context.Background(), res())
	if err != nil || len(out.Items) != 2 {
		t.Fatalf("shadow: want 2 kept (ledger must not drop), got %d err=%v", len(out.Items), err)
	}
	if m.counts["torznab/ledger_only"] != 1 || m.counts["torznab/both"] != 1 || m.counts["torznab/liveness_only"] != 1 {
		t.Fatalf("shadow divergence counts wrong: %v", m.counts)
	}
	if m.counts["torznab/excluded"] != 0 {
		t.Fatalf("shadow must not count exclusions: %v", m.counts)
	}

	// Live: union — dead OR blocked drop.
	m = &fakeVerdictsMetrics{}
	a = Adapter{liveness: fakeSet{set: dead}}.WithVerdicts(fakeSet{set: blocked}, true, m)
	out, err = a.applyLivenessFilter(context.Background(), res())
	if err != nil || len(out.Items) != 1 {
		t.Fatalf("live: want 1 kept, got %d err=%v", len(out.Items), err)
	}
	if m.counts["torznab/excluded"] != 1 {
		t.Fatalf("live: ledger-driven drop must count excluded once: %v", m.counts)
	}
	if m.counts["torznab/ledger_only"] != 0 || m.counts["torznab/both"] != 0 {
		t.Fatalf("live: shadow divergence counters must go quiet: %v", m.counts)
	}

	// Ledger error: fail open (dead still dropped), error counted.
	m = &fakeVerdictsMetrics{}
	a = Adapter{liveness: fakeSet{set: dead}}.WithVerdicts(fakeSet{err: errors.New("down")}, true, m)
	out, err = a.applyLivenessFilter(context.Background(), res())
	if err != nil || len(out.Items) != 2 {
		t.Fatalf("error: want fail-open with 2 kept, got %d err=%v", len(out.Items), err)
	}
	if m.counts["torznab/error"] != 1 {
		t.Fatalf("error must be counted: %v", m.counts)
	}

	// Verdicts consult runs even with liveness disabled.
	m = &fakeVerdictsMetrics{}
	a = Adapter{}.WithVerdicts(fakeSet{set: blocked}, true, m)
	out, err = a.applyLivenessFilter(context.Background(), res())
	if err != nil || len(out.Items) != 2 {
		t.Fatalf("no-liveness live: want 2 kept, got %d err=%v", len(out.Items), err)
	}
}
