package evidence

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spencercnorton/bitagent/internal/health"
)

// Freshness records the last time each configured source instance
// completed a poll cycle without error.
//
// Poll failures already increment
// bitagent_evidence_source_errors_total{stage="auth"}, and ops/promql.yml
// even carries an alert expression for it — but no deployed Prometheus
// evaluates that file. That is how a qBittorrent login failure kept the
// qB evidence feed dark from 2026-07-15 to 2026-07-26 with nothing louder
// than a WARN line every 15 minutes. This turns "a feed went dark" into a
// failing health check on /status, which is a surface the estate can and
// does watch.
//
// Freshness deliberately tracks *poll success*, not row counts: a poller
// that reaches its source and finds nothing new is healthy, a poller that
// cannot reach its source at all is not.
type Freshness struct {
	mu     sync.Mutex
	last   map[string]time.Time
	maxAge map[string]time.Duration
}

// NewFreshness returns an empty tracker.
func NewFreshness() *Freshness {
	return &Freshness{
		last:   make(map[string]time.Time),
		maxAge: make(map[string]time.Duration),
	}
}

// Track registers an instance as expected to report in, with the staleness
// window that instance is held to. Called once per configured instance at
// worker start, seeded with the current time so a poller that has *never*
// succeeded goes stale on the same clock as one that stopped succeeding — a
// source broken from boot must not read as healthy forever.
//
// The window is per instance rather than global because a slow poller must not
// buy a fast one extra grace: a 15m qB feed held to a 1h *arr feed's window
// would stay "healthy" for three hours after going dark instead of 45 minutes.
func (f *Freshness) Track(src Source, instance string, maxAge time.Duration) {
	key := freshnessKey(src, instance)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last[key] = time.Now()
	f.maxAge[key] = maxAge
}

// MarkSuccess records a completed poll cycle for one instance. Callers must
// only call it once the WHOLE cycle succeeded — a partial success that still
// refreshes the instance would let one permanently dark sub-feed hide behind a
// sibling that keeps working.
func (f *Freshness) MarkSuccess(src Source, instance string) {
	f.set(freshnessKey(src, instance), time.Now())
}

func (f *Freshness) set(key string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last[key] = at
}

// Active reports whether any instance is being tracked. Used to keep the
// health check inactive on deploys that configure no evidence sources.
func (f *Freshness) Active() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.last) > 0
}

// Stale returns the tracked instances whose last success is older than that
// instance's own window, sorted for a stable error message.
func (f *Freshness) Stale(now time.Time) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var stale []string
	for key, at := range f.last {
		maxAge := f.maxAge[key]
		if maxAge <= 0 {
			continue
		}
		if now.Sub(at) > maxAge {
			stale = append(stale, fmt.Sprintf(
				"%s (last success %s ago, window %s)",
				key,
				now.Sub(at).Truncate(time.Minute),
				maxAge,
			))
		}
	}
	sort.Strings(stale)
	return stale
}

func freshnessKey(src Source, instance string) string {
	return string(src) + "/" + instance
}

// FreshnessWindow sizes an instance's staleness window from its own poll
// interval: long enough to absorb a transient failure plus the next retry,
// short enough that an operator hears about a dead feed the same hour rather
// than eleven days later.
func FreshnessWindow(interval time.Duration) time.Duration {
	if interval <= 0 {
		interval = 15 * time.Minute
	}

	return 3 * interval
}

// NewFreshnessCheck reports the service down when any tracked evidence
// source has not completed a poll cycle within maxAge. Registered on the
// shared health checker, so it surfaces in the /status payload alongside
// the dht and postgres checks.
func NewFreshnessCheck(f *Freshness) health.Check {
	return health.Check{
		Name:     "evidence_sources",
		IsActive: f.Active,
		Timeout:  time.Second,
		Check: func(context.Context) error {
			if stale := f.Stale(time.Now()); len(stale) > 0 {
				return fmt.Errorf(
					"evidence source has not completed a poll cycle within its window: %s",
					strings.Join(stale, "; "),
				)
			}
			return nil
		},
	}
}
