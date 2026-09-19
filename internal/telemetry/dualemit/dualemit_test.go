package dualemit

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests guard the contract the Phase 2 cutover relies on:
// every observation must fire under BOTH the primary namespace
// (configured via opts.Namespace at the call site — e.g. "bitagent")
// AND the legacy "bitmagnet" namespace, so dashboards can migrate
// without a flip day.

func withEmitLegacy(t *testing.T, v bool, fn func()) {
	t.Helper()
	saved := EmitLegacy
	EmitLegacy = v
	t.Cleanup(func() { EmitLegacy = saved })
	fn()
}

func scrape(t *testing.T, reg *prometheus.Registry) string {
	t.Helper()
	metrics, err := reg.Gather()
	require.NoError(t, err)
	var sb strings.Builder
	for _, mf := range metrics {
		sb.WriteString(mf.GetName())
		sb.WriteString("\n")
	}
	return sb.String()
}

func TestCounterVec_DualEmit(t *testing.T) {
	withEmitLegacy(t, true, func() {
		reg := prometheus.NewRegistry()
		c := NewCounterVec(prometheus.CounterOpts{
			Namespace: "bitagent",
			Subsystem: "sub",
			Name:      "hits_total",
			Help:      "hits",
		}, []string{"kind"})
		reg.MustRegister(c)

		c.With(prometheus.Labels{"kind": "a"}).Inc()
		c.With(prometheus.Labels{"kind": "a"}).Add(2)
		c.With(prometheus.Labels{"kind": "b"}).Inc()

		// Both families present in the registry.
		names := scrape(t, reg)
		assert.Contains(t, names, "bitagent_sub_hits_total")
		assert.Contains(t, names, "bitmagnet_sub_hits_total")

		// Both families carry the same per-label totals.
		assert.InDelta(t, 3.0, testutil.ToFloat64(c.primary.With(prometheus.Labels{"kind": "a"})), 0.0001)
		assert.InDelta(t, 3.0, testutil.ToFloat64(c.legacy.With(prometheus.Labels{"kind": "a"})), 0.0001)
		assert.InDelta(t, 1.0, testutil.ToFloat64(c.primary.With(prometheus.Labels{"kind": "b"})), 0.0001)
		assert.InDelta(t, 1.0, testutil.ToFloat64(c.legacy.With(prometheus.Labels{"kind": "b"})), 0.0001)
	})
}

func TestGaugeVec_DualEmit(t *testing.T) {
	withEmitLegacy(t, true, func() {
		reg := prometheus.NewRegistry()
		g := NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "bitagent",
			Subsystem: "sub",
			Name:      "inflight",
			Help:      "inflight",
		}, []string{"q"})
		reg.MustRegister(g)

		g.With(prometheus.Labels{"q": "ping"}).Set(7)
		g.With(prometheus.Labels{"q": "ping"}).Inc()

		names := scrape(t, reg)
		assert.Contains(t, names, "bitagent_sub_inflight")
		assert.Contains(t, names, "bitmagnet_sub_inflight")

		assert.InDelta(t, 8.0, testutil.ToFloat64(g.primary.With(prometheus.Labels{"q": "ping"})), 0.0001)
		assert.InDelta(t, 8.0, testutil.ToFloat64(g.legacy.With(prometheus.Labels{"q": "ping"})), 0.0001)
	})
}

func TestHistogramVec_DualEmit(t *testing.T) {
	withEmitLegacy(t, true, func() {
		reg := prometheus.NewRegistry()
		h := NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "bitagent",
			Subsystem: "sub",
			Name:      "dur_seconds",
			Help:      "dur",
		}, []string{"q"})
		reg.MustRegister(h)

		h.With(prometheus.Labels{"q": "ping"}).Observe(0.1)
		h.With(prometheus.Labels{"q": "ping"}).Observe(0.2)

		names := scrape(t, reg)
		assert.Contains(t, names, "bitagent_sub_dur_seconds")
		assert.Contains(t, names, "bitmagnet_sub_dur_seconds")
	})
}

func TestScalar_DualEmit(t *testing.T) {
	withEmitLegacy(t, true, func() {
		reg := prometheus.NewRegistry()
		c := NewCounter(prometheus.CounterOpts{
			Namespace: "bitagent", Subsystem: "s", Name: "c", Help: "",
		})
		g := NewGauge(prometheus.GaugeOpts{
			Namespace: "bitagent", Subsystem: "s", Name: "g", Help: "",
		})
		h := NewHistogram(prometheus.HistogramOpts{
			Namespace: "bitagent", Subsystem: "s", Name: "h", Help: "",
		})
		reg.MustRegister(c, g, h)

		c.Inc()
		c.Add(4)
		g.Set(3)
		g.Inc()
		h.Observe(1.5)

		names := scrape(t, reg)
		for _, want := range []string{
			"bitagent_s_c", "bitmagnet_s_c",
			"bitagent_s_g", "bitmagnet_s_g",
			"bitagent_s_h", "bitmagnet_s_h",
		} {
			assert.Contains(t, names, want)
		}

		assert.InDelta(t, 5.0, testutil.ToFloat64(c.primary), 0.0001)
		assert.InDelta(t, 5.0, testutil.ToFloat64(c.legacy), 0.0001)
		assert.InDelta(t, 4.0, testutil.ToFloat64(g.primary), 0.0001)
		assert.InDelta(t, 4.0, testutil.ToFloat64(g.legacy), 0.0001)
	})
}

// Off-switch: with EmitLegacy=false, only the primary namespace exists.
// This is the end-of-cutover path.
func TestEmitLegacy_OffSuppressesLegacy(t *testing.T) {
	withEmitLegacy(t, false, func() {
		reg := prometheus.NewRegistry()
		c := NewCounterVec(prometheus.CounterOpts{
			Namespace: "bitagent", Subsystem: "s", Name: "only_primary",
			Help: "",
		}, []string{"k"})
		reg.MustRegister(c)

		c.With(prometheus.Labels{"k": "x"}).Inc()

		names := scrape(t, reg)
		assert.Contains(t, names, "bitagent_s_only_primary")
		assert.NotContains(t, names, "bitmagnet_s_only_primary")
	})
}
