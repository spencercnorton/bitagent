// Package dualemit publishes every Prometheus metric under two
// namespaces simultaneously — the configured primary namespace (see
// opts.Namespace at the call site, which is now "bitagent") AND the
// legacy "bitmagnet" namespace the project was born under.
//
// This exists for the BitAgent Phase 2 cutover. Dashboards + alerts
// built against `bitmagnet_*` series keep working; new dashboards
// build against `bitagent_*`. Once every downstream is confirmed on
// the primary names, flip `EmitLegacy` to false at the factory
// (one-line change, no touch to call sites) and the legacy metrics
// stop being observed.
//
// Every wrapper implements `prometheus.Collector`, so the existing
// fx group `prometheus_collectors` treats them identically to plain
// prometheus.*Vec / prometheus.Gauge / etc.
package dualemit

import (
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// LegacyNamespace is the namespace we dual-emit under during the
// BitAgent cutover. Hard-coded on purpose: this is not an operator
// knob, it's a migration mechanism. When the cutover completes the
// whole package goes away.
const LegacyNamespace = "bitmagnet"

// EmitLegacy controls whether the legacy namespace is populated.
// While true every observation fires twice; the overhead is an extra
// map lookup + atomic increment per call, and every legacy series is
// also described and collected on each scrape.
//
// Set false on 2026-09-02, closing the dual-emit window opened on
// 2026-04-24. The exit criterion this package documents — "once every
// downstream is confirmed on the primary names" — was checked rather
// than assumed: `grep -rl 'bitmagnet_'` over observability/, ops/ and
// deploy/ returns nothing, so the Grafana dashboards and PromQL in
// this repo are fully migrated to `bitagent_*`.
//
// Collect and Describe both gate on this flag, so flipping it removes
// the legacy series from /metrics rather than merely freezing them at
// their last value. Before the flip production served 549 legacy
// `bitmagnet_*` lines beside 780 `bitagent_*`.
//
// Left as a var, not deleted outright: restoring the legacy family is
// a one-line revert if an unmigrated external dashboard surfaces.
// Deleting this package entirely — which reverts every call site to
// plain prometheus constructors — is the follow-up, not this change.
var EmitLegacy = false

// HistogramVec tees Observe() to a primary + legacy vec.
type HistogramVec struct {
	primary *prometheus.HistogramVec
	legacy  *prometheus.HistogramVec
}

func NewHistogramVec(opts prometheus.HistogramOpts, labels []string) *HistogramVec {
	legacyOpts := opts
	legacyOpts.Namespace = LegacyNamespace
	return &HistogramVec{
		primary: prometheus.NewHistogramVec(opts, labels),
		legacy:  prometheus.NewHistogramVec(legacyOpts, labels),
	}
}

func (v *HistogramVec) With(labels prometheus.Labels) prometheus.Observer {
	if !EmitLegacy {
		return v.primary.With(labels)
	}
	return teeObserver{v.primary.With(labels), v.legacy.With(labels)}
}

func (v *HistogramVec) WithLabelValues(lvs ...string) prometheus.Observer {
	if !EmitLegacy {
		return v.primary.WithLabelValues(lvs...)
	}
	return teeObserver{v.primary.WithLabelValues(lvs...), v.legacy.WithLabelValues(lvs...)}
}

func (v *HistogramVec) Describe(ch chan<- *prometheus.Desc) {
	v.primary.Describe(ch)
	if EmitLegacy {
		v.legacy.Describe(ch)
	}
}

func (v *HistogramVec) Collect(ch chan<- prometheus.Metric) {
	v.primary.Collect(ch)
	if EmitLegacy {
		v.legacy.Collect(ch)
	}
}

// CounterVec tees Inc()/Add() to a primary + legacy vec.
type CounterVec struct {
	primary *prometheus.CounterVec
	legacy  *prometheus.CounterVec
}

func NewCounterVec(opts prometheus.CounterOpts, labels []string) *CounterVec {
	legacyOpts := opts
	legacyOpts.Namespace = LegacyNamespace
	return &CounterVec{
		primary: prometheus.NewCounterVec(opts, labels),
		legacy:  prometheus.NewCounterVec(legacyOpts, labels),
	}
}

func (v *CounterVec) With(labels prometheus.Labels) prometheus.Counter {
	if !EmitLegacy {
		return v.primary.With(labels)
	}
	return teeCounter{v.primary.With(labels), v.legacy.With(labels)}
}

func (v *CounterVec) WithLabelValues(lvs ...string) prometheus.Counter {
	if !EmitLegacy {
		return v.primary.WithLabelValues(lvs...)
	}
	return teeCounter{v.primary.WithLabelValues(lvs...), v.legacy.WithLabelValues(lvs...)}
}

func (v *CounterVec) Describe(ch chan<- *prometheus.Desc) {
	v.primary.Describe(ch)
	if EmitLegacy {
		v.legacy.Describe(ch)
	}
}

func (v *CounterVec) Collect(ch chan<- prometheus.Metric) {
	v.primary.Collect(ch)
	if EmitLegacy {
		v.legacy.Collect(ch)
	}
}

// GaugeVec tees Set()/Inc()/Dec()/Add()/Sub() to a primary + legacy vec.
type GaugeVec struct {
	primary *prometheus.GaugeVec
	legacy  *prometheus.GaugeVec
}

func NewGaugeVec(opts prometheus.GaugeOpts, labels []string) *GaugeVec {
	legacyOpts := opts
	legacyOpts.Namespace = LegacyNamespace
	return &GaugeVec{
		primary: prometheus.NewGaugeVec(opts, labels),
		legacy:  prometheus.NewGaugeVec(legacyOpts, labels),
	}
}

func (v *GaugeVec) With(labels prometheus.Labels) prometheus.Gauge {
	if !EmitLegacy {
		return v.primary.With(labels)
	}
	return teeGauge{v.primary.With(labels), v.legacy.With(labels)}
}

func (v *GaugeVec) WithLabelValues(lvs ...string) prometheus.Gauge {
	if !EmitLegacy {
		return v.primary.WithLabelValues(lvs...)
	}
	return teeGauge{v.primary.WithLabelValues(lvs...), v.legacy.WithLabelValues(lvs...)}
}

func (v *GaugeVec) Describe(ch chan<- *prometheus.Desc) {
	v.primary.Describe(ch)
	if EmitLegacy {
		v.legacy.Describe(ch)
	}
}

func (v *GaugeVec) Collect(ch chan<- prometheus.Metric) {
	v.primary.Collect(ch)
	if EmitLegacy {
		v.legacy.Collect(ch)
	}
}

// --- Non-Vec scalar wrappers. These bind the tee at construction
// time rather than per-With() call, because there are no labels. ---

// Counter is a dual-emit scalar counter.
type Counter struct {
	primary prometheus.Counter
	legacy  prometheus.Counter
}

func NewCounter(opts prometheus.CounterOpts) *Counter {
	legacyOpts := opts
	legacyOpts.Namespace = LegacyNamespace
	return &Counter{
		primary: prometheus.NewCounter(opts),
		legacy:  prometheus.NewCounter(legacyOpts),
	}
}

func (c *Counter) Inc() {
	c.primary.Inc()
	if EmitLegacy {
		c.legacy.Inc()
	}
}

func (c *Counter) Add(v float64) {
	c.primary.Add(v)
	if EmitLegacy {
		c.legacy.Add(v)
	}
}

func (c *Counter) Describe(ch chan<- *prometheus.Desc) {
	c.primary.Describe(ch)
	if EmitLegacy {
		c.legacy.Describe(ch)
	}
}

func (c *Counter) Collect(ch chan<- prometheus.Metric) {
	c.primary.Collect(ch)
	if EmitLegacy {
		c.legacy.Collect(ch)
	}
}

// Gauge is a dual-emit scalar gauge.
type Gauge struct {
	primary prometheus.Gauge
	legacy  prometheus.Gauge
}

func NewGauge(opts prometheus.GaugeOpts) *Gauge {
	legacyOpts := opts
	legacyOpts.Namespace = LegacyNamespace
	return &Gauge{
		primary: prometheus.NewGauge(opts),
		legacy:  prometheus.NewGauge(legacyOpts),
	}
}

func (g *Gauge) Set(v float64) {
	g.primary.Set(v)
	if EmitLegacy {
		g.legacy.Set(v)
	}
}

func (g *Gauge) Inc() {
	g.primary.Inc()
	if EmitLegacy {
		g.legacy.Inc()
	}
}

func (g *Gauge) Dec() {
	g.primary.Dec()
	if EmitLegacy {
		g.legacy.Dec()
	}
}

func (g *Gauge) Add(v float64) {
	g.primary.Add(v)
	if EmitLegacy {
		g.legacy.Add(v)
	}
}

func (g *Gauge) Sub(v float64) {
	g.primary.Sub(v)
	if EmitLegacy {
		g.legacy.Sub(v)
	}
}

func (g *Gauge) SetToCurrentTime() {
	g.primary.SetToCurrentTime()
	if EmitLegacy {
		g.legacy.SetToCurrentTime()
	}
}

func (g *Gauge) Describe(ch chan<- *prometheus.Desc) {
	g.primary.Describe(ch)
	if EmitLegacy {
		g.legacy.Describe(ch)
	}
}

func (g *Gauge) Collect(ch chan<- prometheus.Metric) {
	g.primary.Collect(ch)
	if EmitLegacy {
		g.legacy.Collect(ch)
	}
}

// Histogram is a dual-emit scalar histogram.
type Histogram struct {
	primary prometheus.Histogram
	legacy  prometheus.Histogram
}

func NewHistogram(opts prometheus.HistogramOpts) *Histogram {
	legacyOpts := opts
	legacyOpts.Namespace = LegacyNamespace
	return &Histogram{
		primary: prometheus.NewHistogram(opts),
		legacy:  prometheus.NewHistogram(legacyOpts),
	}
}

func (h *Histogram) Observe(v float64) {
	h.primary.Observe(v)
	if EmitLegacy {
		h.legacy.Observe(v)
	}
}

func (h *Histogram) Describe(ch chan<- *prometheus.Desc) {
	h.primary.Describe(ch)
	if EmitLegacy {
		h.legacy.Describe(ch)
	}
}

func (h *Histogram) Collect(ch chan<- prometheus.Metric) {
	h.primary.Collect(ch)
	if EmitLegacy {
		h.legacy.Collect(ch)
	}
}

// --- Internal tee types for the *Vec.With() return values. ---

type teeObserver struct {
	a, b prometheus.Observer
}

func (t teeObserver) Observe(v float64) {
	t.a.Observe(v)
	t.b.Observe(v)
}

type teeCounter struct {
	a, b prometheus.Counter
}

func (t teeCounter) Inc()             { t.a.Inc(); t.b.Inc() }
func (t teeCounter) Add(v float64)    { t.a.Add(v); t.b.Add(v) }
func (t teeCounter) Desc() *prometheus.Desc {
	// Describe isn't typically called via a teeCounter, but the interface
	// requires it. Primary is canonical.
	return t.a.Desc()
}
func (t teeCounter) Write(m *dto.Metric) error { return t.a.Write(m) }
func (t teeCounter) Describe(ch chan<- *prometheus.Desc) {
	t.a.Describe(ch)
	t.b.Describe(ch)
}
func (t teeCounter) Collect(ch chan<- prometheus.Metric) {
	t.a.Collect(ch)
	t.b.Collect(ch)
}

type teeGauge struct {
	a, b prometheus.Gauge
}

func (t teeGauge) Set(v float64)        { t.a.Set(v); t.b.Set(v) }
func (t teeGauge) Inc()                 { t.a.Inc(); t.b.Inc() }
func (t teeGauge) Dec()                 { t.a.Dec(); t.b.Dec() }
func (t teeGauge) Add(v float64)        { t.a.Add(v); t.b.Add(v) }
func (t teeGauge) Sub(v float64)        { t.a.Sub(v); t.b.Sub(v) }
func (t teeGauge) SetToCurrentTime()    { t.a.SetToCurrentTime(); t.b.SetToCurrentTime() }
func (t teeGauge) Desc() *prometheus.Desc { return t.a.Desc() }
func (t teeGauge) Write(m *dto.Metric) error { return t.a.Write(m) }
func (t teeGauge) Describe(ch chan<- *prometheus.Desc) {
	t.a.Describe(ch)
	t.b.Describe(ch)
}
func (t teeGauge) Collect(ch chan<- prometheus.Metric) {
	t.a.Collect(ch)
	t.b.Collect(ch)
}
