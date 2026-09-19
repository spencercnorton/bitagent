package externalip

import (
	"net/netip"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
)

const (
	namespace = "bitagent"
	subsystem = "dht_external_ip"
)

// WatcherMetrics exposes the watcher's health surface so an operator
// can tell at a glance:
//
//   - whether the resolver is succeeding at all (last_success_unix_seconds)
//   - how many consecutive failures we've tolerated (failures_total, last_failure_unix_seconds)
//   - whether a node-ID rotation is "pending" (invalidating resolves seen but below confirmation threshold)
//   - whether the current node ID is BEP-42-compliant (secure_node_id_valid)
//
// Without these, a silent resolver outage looks exactly like a
// healthy "IP hasn't changed" state — that's the operational
// anti-pattern the reviewer called out.
type WatcherMetrics struct {
	LastSuccessUnix *dualemit.Gauge
	LastFailureUnix *dualemit.Gauge
	SuccessTotal    *dualemit.Counter
	FailureTotal    *dualemit.Counter
	// PendingConfirmations is the count of agreeing invalidating
	// resolves observed since the last confirmation reset. Reads
	// are gauge-style (point-in-time); writes are guarded by the
	// watcher's internal mutex.
	PendingConfirmations *dualemit.Gauge
	// SecureNodeIDValid is 1 iff the crawler is running with a
	// BEP-42-derived node ID (i.e. boot didn't fall back to random).
	SecureNodeIDValid *dualemit.Gauge

	mu            sync.Mutex
	pendingCount  int
	pendingTarget netip.Addr
}

// NewWatcherMetrics constructs the collector set. All are
// pre-registered values; the caller is expected to expose them via
// the `prometheus_collectors` fx group.
func NewWatcherMetrics() *WatcherMetrics {
	return &WatcherMetrics{
		LastSuccessUnix: dualemit.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "last_success_unix_seconds",
			Help:      "Unix timestamp of the most recent successful external-IP resolve (validated). Zero when never succeeded.",
		}),
		LastFailureUnix: dualemit.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "last_failure_unix_seconds",
			Help:      "Unix timestamp of the most recent failed external-IP resolve. Zero when never failed.",
		}),
		SuccessTotal: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "resolve_success_total",
			Help:      "Count of successful external-IP resolves.",
		}),
		FailureTotal: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "resolve_failure_total",
			Help:      "Count of failed external-IP resolves (all sources unreachable or address failed validation).",
		}),
		PendingConfirmations: dualemit.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "pending_confirmations",
			Help:      "Number of agreeing invalidating resolves observed, below the confirmation threshold required to trigger restart. Reset to 0 on any successful non-invalidating resolve.",
		}),
		SecureNodeIDValid: dualemit.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "secure_node_id_valid",
			Help:      "1 iff the crawler is running with a BEP-42-derived node ID; 0 when boot fell back to random.",
		}),
	}
}

// Collectors returns the exportable prometheus.Collector values, for
// registration via the shared fx group.
func (m *WatcherMetrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.LastSuccessUnix,
		m.LastFailureUnix,
		m.SuccessTotal,
		m.FailureTotal,
		m.PendingConfirmations,
		m.SecureNodeIDValid,
	}
}

func (m *WatcherMetrics) recordSuccess(addr netip.Addr) {
	_ = addr // addr is available to callers via last_success_unix_seconds correlation; we don't label it to avoid cardinality
	m.LastSuccessUnix.Set(float64(time.Now().Unix()))
	m.SuccessTotal.Inc()
}

func (m *WatcherMetrics) recordFailure() {
	m.LastFailureUnix.Set(float64(time.Now().Unix()))
	m.FailureTotal.Inc()
}

// addPendingConfirmation bumps the pending-confirmation count if
// `addr` matches the previous pending target (or starts a new run).
// Returns the updated count. A different target resets the count to
// 1 — we only count **consecutive agreeing** resolves.
func (m *WatcherMetrics) addPendingConfirmation(addr netip.Addr) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.pendingTarget != addr {
		m.pendingTarget = addr
		m.pendingCount = 0
	}
	m.pendingCount++
	m.PendingConfirmations.Set(float64(m.pendingCount))

	return m.pendingCount
}

// resetPendingConfirmations is called on a successful
// non-invalidating resolve. Without this, a single transient flip
// would leave the pending count stuck above 0 and weaken the next
// confirmation threshold.
func (m *WatcherMetrics) resetPendingConfirmations() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.pendingTarget = netip.Addr{}
	m.pendingCount = 0
	m.PendingConfirmations.Set(0)
}

// SetSecureNodeIDValid should be called exactly once at boot to
// publish whether the crawler is in BEP-42-compliant or fallback
// mode.
func (m *WatcherMetrics) SetSecureNodeIDValid(valid bool) {
	if valid {
		m.SecureNodeIDValid.Set(1)
	} else {
		m.SecureNodeIDValid.Set(0)
	}
}
