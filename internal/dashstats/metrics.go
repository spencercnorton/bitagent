// Package dashstats periodically computes a couple of headline dashboard
// figures from the DB and exposes them as Prometheus gauges, so the bitagent-ui
// dashboard (which already scrapes /metrics) can render them without direct DB
// access. Read-only; no side effects.
package dashstats

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
)

// Metrics holds the dashboard gauges. Raw counts are emitted (not ratios) so
// the dashboard owns the framing — e.g. grab success% = success/(success+failure),
// match% = matched/total.
type Metrics struct {
	grabSuccess       *dualemit.Gauge
	grabFailure       *dualemit.Gauge
	grabPending       *dualemit.Gauge
	matchVideoTotal   *dualemit.Gauge
	matchVideoMatched *dualemit.Gauge
	altTitleTotal     *dualemit.Gauge
	altTitleChecked   *dualemit.Gauge
	altTitleWithAlt   *dualemit.Gauge
	grabsTotal30d     *dualemit.Gauge
	grabsBitagent30d  *dualemit.Gauge
}

func NewMetrics() *Metrics {
	const ns, sub = "bitagent", "dashstats"
	g := func(name, help string) *dualemit.Gauge {
		return dualemit.NewGauge(prometheus.GaugeOpts{
			Namespace: ns, Subsystem: sub, Name: name, Help: help,
		})
	}
	return &Metrics{
		grabSuccess:       g("grab_success", "Grab attempts that resolved to a successful (live) download."),
		grabFailure:       g("grab_failure", "Grab attempts that resolved to failure (dead/unavailable torrent)."),
		grabPending:       g("grab_pending", "Grab attempts not yet resolved."),
		matchVideoTotal:   g("match_video_total_30d", "Movie/TV torrents added in the last 30 days."),
		matchVideoMatched: g("match_video_matched_30d", "Movie/TV torrents added in the last 30 days that have a content match."),
		altTitleTotal: g("alt_title_content_total",
			"TMDB content rows eligible for alt-title backfill (movie/tv_show/xxx)."),
		altTitleChecked: g("alt_title_content_checked",
			"Eligible content rows already swept for alt titles (checked marker or >=1 alt_title:* attribute)."),
		altTitleWithAlt: g("alt_title_content_with_alt",
			"Eligible content rows carrying at least one alt_title:* attribute."),
		grabsTotal30d: g("indexer_grabs_total_30d",
			"*arr grab webhooks recorded in the last 30 days, all indexers."),
		grabsBitagent30d: g("indexer_grabs_bitagent_30d",
			"*arr grab webhooks in the last 30 days won by the BitAgent indexer (north-star numerator)."),
	}
}

func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.grabSuccess, m.grabFailure, m.grabPending,
		m.matchVideoTotal, m.matchVideoMatched,
		m.altTitleTotal, m.altTitleChecked, m.altTitleWithAlt,
		m.grabsTotal30d, m.grabsBitagent30d,
	}
}
