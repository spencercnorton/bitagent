package httpserver

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
)

// Metrics covers the torznab HTTP serving surface — every Search /
// Caps request that arrives via /torznab/* (typically from Prowlarr).
//
// Why this exists: pre-2026-04-25 bitagent emitted ZERO metrics for
// torznab queries despite serving ~1k/week to Prowlarr in production.
// Tuning indexer-side behaviour (search latency, zero-result rates,
// category distribution) was blind. This package closes the gap.
//
// Surface:
//
//	bitagent_torznab_search_total{type,cat_class,profile,status}
//	    — every Search call. type is the torznab function (search /
//	      tvsearch / movie / music / book). cat_class buckets the
//	      Newznab category code: tv / movies / audio / books /
//	      software / xxx / other. status: ok | error.
//
//	bitagent_torznab_search_duration_seconds{type,profile}
//	    — wall-clock latency. Buckets cover 1ms (cache hit) to 5s
//	      (slow Postgres path).
//
//	bitagent_torznab_search_results{type,profile}
//	    — histogram of result counts. The le=0 bucket is the
//	      "zero-result query" signal — opportunities where Sonarr
//	      asked for something we couldn't find.
//
//	bitagent_torznab_caps_total{profile}
//	    — caps endpoint hits. Useful for spotting Prowlarr / *arr
//	      retrying caps after a config reload.
type Metrics struct {
	searchTotal    *dualemit.CounterVec   // type, cat_class, profile, status
	searchDuration *dualemit.HistogramVec // type, profile
	searchResults  *dualemit.HistogramVec // type, profile
	capsTotal      *dualemit.CounterVec   // profile
	requestsTotal  *dualemit.CounterVec   // key_name, status
}

const (
	tnNamespace = "bitagent"
	tnSubsystem = "torznab"
)

// NewMetrics builds the metric set. Call Collectors() for the
// prometheus_collectors fx group.
func NewMetrics() *Metrics {
	return &Metrics{
		searchTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: tnNamespace,
			Subsystem: tnSubsystem,
			Name:      "search_total",
			Help: "Torznab Search() requests served by /torznab/*. " +
				"type=search|tvsearch|movie|music|book; " +
				"cat_class=tv|movies|audio|books|software|xxx|other (bucketed Newznab category code); " +
				"profile=default|<custom>; " +
				"status=ok|error.",
		}, []string{"type", "cat_class", "profile", "status"}),
		searchDuration: dualemit.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: tnNamespace,
			Subsystem: tnSubsystem,
			Name:      "search_duration_seconds",
			Help:      "Wall-clock latency of Torznab Search() calls.",
			Buckets: []float64{
				0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0,
			},
		}, []string{"type", "profile"}),
		searchResults: dualemit.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: tnNamespace,
			Subsystem: tnSubsystem,
			Name:      "search_results",
			Help: "Distribution of result counts per Torznab search. " +
				"The le=0 bucket counts zero-result queries — searches " +
				"the operator's *arr stack made that bitagent had nothing " +
				"to satisfy. High zero-result rate on a category suggests " +
				"that category is poorly served by DHT and needs a private-tracker fallback.",
			Buckets: []float64{0, 1, 5, 10, 25, 50, 100, 250, 500, 1000},
		}, []string{"type", "profile"}),
		capsTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: tnNamespace,
			Subsystem: tnSubsystem,
			Name:      "caps_total",
			Help:      "Torznab Caps() endpoint requests by profile.",
		}, []string{"profile"}),
		requestsTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: tnNamespace,
			Subsystem: tnSubsystem,
			Name:      "requests_total",
			Help: "Torznab requests grouped by api-key name and auth result. " +
				"key_name=open|<name from TORZNAB_API_KEYS>|default (legacy " +
				"TORZNAB_API_KEY); " +
				"status=ok (matched, request handled) | rejected (401, key " +
				"missing or wrong). Use this to audit per-consumer indexer " +
				"usage and to revoke a single key without rotating the others.",
		}, []string{"key_name", "status"}),
	}
}

// Collectors returns every collector for fx-group registration.
func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.searchTotal, m.searchDuration, m.searchResults, m.capsTotal,
		m.requestsTotal,
	}
}

// observeAuth records one authentication attempt. keyName is the
// matched key's configured name, "default" for the legacy
// TORZNAB_API_KEY single-key path, "open" when no key is configured
// (operator opted in to open mode), or "" when the request was
// rejected. status is "ok" (matched / open) or "rejected" (401).
func (m *Metrics) observeAuth(keyName, status string) {
	if m == nil {
		return
	}
	if keyName == "" {
		keyName = "rejected"
	}
	m.requestsTotal.With(prometheus.Labels{
		"key_name": keyName,
		"status":   status,
	}).Inc()
}

// observeSearch records a single Search() call. profile is the
// torznab profile id; tp is the type string from the request; cats
// is the raw Newznab category list; resultCount is the count of
// items in the response (0 if status=error). dur is the wall-clock
// latency. status is "ok" or "error".
//
// The cat_class label collapses the (often multi-valued) cats slice
// to a single coarse bucket so Prometheus cardinality stays bounded.
// When multiple categories are passed, the FIRST recognised one wins
// — that's typically what *arr stacks send anyway (one primary cat
// per search).
func (m *Metrics) observeSearch(profile, tp string, cats []int, resultCount int, dur time.Duration, status string) {
	if m == nil {
		return
	}
	if profile == "" {
		profile = "default"
	}
	catClass := classifyCats(cats)
	m.searchTotal.With(prometheus.Labels{
		"type":      tp,
		"cat_class": catClass,
		"profile":   profile,
		"status":    status,
	}).Inc()
	// Duration + results only on successful path; errors don't have
	// meaningful latency or result count.
	if status == "ok" {
		m.searchDuration.With(prometheus.Labels{
			"type":    tp,
			"profile": profile,
		}).Observe(dur.Seconds())
		m.searchResults.With(prometheus.Labels{
			"type":    tp,
			"profile": profile,
		}).Observe(float64(resultCount))
	}
}

// observeCaps records a single Caps() call. Cheap; just a counter.
func (m *Metrics) observeCaps(profile string) {
	if m == nil {
		return
	}
	if profile == "" {
		profile = "default"
	}
	m.capsTotal.With(prometheus.Labels{"profile": profile}).Inc()
}

// classifyCats collapses a slice of Newznab category codes to one
// coarse bucket. Newznab categories are 4-digit codes:
//
//	2000-2999  Movies
//	3000-3999  Audio
//	4000-4999  Software / PC
//	5000-5999  TV
//	6000-6999  XXX
//	7000-7999  Books
//	8000-8999  Other
//
// The first recognised category wins. Empty / unrecognised falls
// to "other" so the metric always has a non-empty label.
func classifyCats(cats []int) string {
	for _, c := range cats {
		switch {
		case c >= 2000 && c < 3000:
			return "movies"
		case c >= 3000 && c < 4000:
			return "audio"
		case c >= 4000 && c < 5000:
			return "software"
		case c >= 5000 && c < 6000:
			return "tv"
		case c >= 6000 && c < 7000:
			return "xxx"
		case c >= 7000 && c < 8000:
			return "books"
		}
	}
	return "other"
}

// statusForErr translates an error / nil into the "ok"/"error"
// label value. Pulled out so the handler doesn't repeat the
// conditional.
func statusForErr(err error) string {
	if err == nil {
		return "ok"
	}
	return "error"
}

// resultCountIfOK returns the result count for a search response, or
// 0 if err != nil. Pulled out for the same reason.
func resultCountIfOK(err error, count int) int {
	if err != nil {
		return 0
	}
	return count
}

// _ is a compile-time assertion that strconv is imported (the
// handler may want it for query-param parsing later).
var _ = strconv.Itoa
