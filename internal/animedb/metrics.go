package animedb

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
)

// Metrics holds the anime-titles refresh and resolver counters/gauges.
type Metrics struct {
	refreshTotal    *dualemit.Counter    // completed refresh cycles (dry-run or live)
	refreshDuration *dualemit.Histogram  // wall-clock of one refresh cycle
	refreshErrors   *dualemit.CounterVec // stage: download | parse | build | persist

	// Last-build gauges (set at the end of each successful build).
	mappingsBuilt *dualemit.Gauge // TMDB-mapped anime entries parsed
	titlesBuilt   *dualemit.Gauge // alias lines parsed (after language filter)
	aliasesBuilt  *dualemit.Gauge // deduplicated alias rows produced
	tableRows     *dualemit.Gauge // rows currently persisted in anime_titles

	// Resolver activity.
	resolveTotal *dualemit.CounterVec // result: exact | seed_contains | miss
}

// NewMetrics constructs the metric set.
func NewMetrics() *Metrics {
	const ns, sub = "bitagent", "animedb"
	c := func(name, help string) *dualemit.Counter {
		return dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: ns, Subsystem: sub, Name: name, Help: help,
		})
	}
	cv := func(name, help string, labels []string) *dualemit.CounterVec {
		return dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Subsystem: sub, Name: name, Help: help,
		}, labels)
	}
	g := func(name, help string) *dualemit.Gauge {
		return dualemit.NewGauge(prometheus.GaugeOpts{
			Namespace: ns, Subsystem: sub, Name: name, Help: help,
		})
	}
	return &Metrics{
		refreshTotal: c("refresh_total", "Completed anime-titles refresh cycles (dry-run or live)."),
		refreshDuration: dualemit.NewHistogram(prometheus.HistogramOpts{
			Namespace: ns, Subsystem: sub, Name: "refresh_duration_seconds",
			Help:    "Wall-clock duration of one anime-titles refresh cycle.",
			Buckets: prometheus.ExponentialBuckets(1, 2, 10),
		}),
		refreshErrors: cv("refresh_errors_total", "Refresh errors by stage: download | parse | build | persist.", []string{"stage"}),
		mappingsBuilt: g("mappings_built", "TMDB-mapped anime entries parsed from anime-list-full.xml in the last build."),
		titlesBuilt:   g("titles_built", "Alias lines parsed from the AniDB dump in the last build (after language filter)."),
		aliasesBuilt:  g("aliases_built", "Deduplicated alias rows produced by the last build."),
		tableRows:     g("table_rows", "Rows currently persisted in the anime_titles table."),
		resolveTotal:  cv("resolve_total", "Resolver lookups by result: exact | seed_contains | miss.", []string{"result"}),
	}
}

// Collectors returns the metric collectors for registration into the shared
// Prometheus collector group.
func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.refreshTotal, m.refreshDuration, m.refreshErrors,
		m.mappingsBuilt, m.titlesBuilt, m.aliasesBuilt, m.tableRows,
		m.resolveTotal,
	}
}
