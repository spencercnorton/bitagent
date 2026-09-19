package pgstats

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"go.uber.org/zap"
)

// TestDescribeEmitsAllDescriptors exercises Describe with no live DB.
// It catches missed descriptor wiring, panic-on-construct bugs, and
// duplicate metric registration — none of which require Postgres.
func TestDescribeEmitsAllDescriptors(t *testing.T) {
	c := NewCollector(
		lazy.New(func() (*pgxpool.Pool, error) { return nil, nil }),
		zap.NewNop().Sugar(),
	)

	ch := make(chan *prometheus.Desc, 64)
	c.Describe(ch)
	close(ch)

	descriptors := map[string]bool{}
	for d := range ch {
		descriptors[d.String()] = true
	}

	// 14 distinct descriptors are exposed. If this count changes, update
	// the Describe() implementation and this test together — they must
	// stay in sync per prometheus.Collector contract.
	const expected = 14
	if got := len(descriptors); got != expected {
		t.Fatalf("Describe emitted %d unique descriptors, expected %d", got, expected)
	}
}

// TestCollectHandlesPoolError verifies that a pool that fails to
// initialize does not panic the collector and that Collect still emits
// a scrape_duration sample so the scrape itself reports as completed.
func TestCollectHandlesPoolError(t *testing.T) {
	c := NewCollector(
		lazy.New(func() (*pgxpool.Pool, error) { return nil, errFakePoolInit }),
		zap.NewNop().Sugar(),
	)

	ch := make(chan prometheus.Metric, 64)
	c.Collect(ch)
	close(ch)

	var gotDuration, gotErrorCounter bool
	for m := range ch {
		desc := m.Desc().String()
		switch {
		case contains(desc, "scrape_duration_seconds"):
			gotDuration = true
		case contains(desc, "scrape_errors_total"):
			gotErrorCounter = true
		}
	}
	if !gotDuration {
		t.Error("Collect did not emit scrape_duration_seconds on pool failure")
	}
	if !gotErrorCounter {
		t.Error("Collect did not emit scrape_errors_total on pool failure")
	}
}

var errFakePoolInit = &fakeErr{"simulated pool init failure"}

type fakeErr struct{ msg string }

func (e *fakeErr) Error() string { return e.msg }

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	if len(needle) == 0 {
		return 0
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
