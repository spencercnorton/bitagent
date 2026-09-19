// Package pgstats exposes Postgres health and capacity signals as
// Prometheus metrics. It is scrape-driven: queries run only when Prometheus
// pulls /metrics, protected by a short per-scrape timeout so a slow or
// unreachable DB cannot stall the exporter.
//
// Metric coverage is deliberately focused on the signals an operator needs
// to detect drift before it becomes an incident: table and index bloat,
// autovacuum lag, dead-tuple accumulation, live-row estimates, and
// connection pool saturation. Correctness-of-data metrics (classification
// precision, label coverage) live elsewhere; this package only reports
// storage layer health.
package pgstats

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"go.uber.org/zap"
)

const (
	namespace = "bitagent"
	subsystem = "postgres"

	// scrapeTimeout caps how long the full pgstats scrape can block.
	// Prometheus default scrape_interval is 15s; we budget well under
	// that so a stalled pg_stat query never fails the parent scrape.
	scrapeTimeout = 5 * time.Second
)

// Collector implements prometheus.Collector against a lazy pgx pool.
// Construction does not open the pool; the first scrape resolves it.
type Collector struct {
	pool   lazy.Lazy[*pgxpool.Pool]
	logger *zap.SugaredLogger

	databaseSize          *prometheus.Desc
	tableSize             *prometheus.Desc
	tableIndexSize        *prometheus.Desc
	tableRowsEstimate     *prometheus.Desc
	tableDeadTuples       *prometheus.Desc
	tableLiveTuples       *prometheus.Desc
	tableAutovacuumAge    *prometheus.Desc
	tableAnalyzeAge       *prometheus.Desc
	connectionsByState    *prometheus.Desc
	poolAcquiredConns     *prometheus.Desc
	poolIdleConns         *prometheus.Desc
	poolMaxConns          *prometheus.Desc
	scrapeErrorsTotal     *prometheus.Desc
	scrapeDurationSeconds *prometheus.Desc
}

// NewCollector constructs the collector. It does not query the database;
// the first scrape triggers the first query.
func NewCollector(pool lazy.Lazy[*pgxpool.Pool], logger *zap.SugaredLogger) *Collector {
	tableLabels := []string{"table"}

	return &Collector{
		pool:   pool,
		logger: logger.Named("pgstats"),

		databaseSize: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "database_size_bytes"),
			"Size of the BitAgent database on disk (pg_database_size).",
			nil, nil,
		),
		tableSize: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "table_size_bytes"),
			"Total on-disk size of a table including TOAST and indexes (pg_total_relation_size).",
			tableLabels, nil,
		),
		tableIndexSize: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "table_index_size_bytes"),
			"On-disk size of all indexes on a table (pg_indexes_size).",
			tableLabels, nil,
		),
		tableRowsEstimate: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "table_rows_estimate"),
			"Planner's row-count estimate (pg_class.reltuples). Cheap; exact counts would timeout at scale.",
			tableLabels, nil,
		),
		tableDeadTuples: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "table_dead_tuples"),
			"Dead tuples awaiting vacuum (pg_stat_user_tables.n_dead_tup). Rising unbounded indicates autovacuum is losing.",
			tableLabels, nil,
		),
		tableLiveTuples: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "table_live_tuples"),
			"Live tuples as last observed by statistics (pg_stat_user_tables.n_live_tup).",
			tableLabels, nil,
		),
		tableAutovacuumAge: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "table_last_autovacuum_age_seconds"),
			"Seconds since the last autovacuum on a table. NaN if never vacuumed.",
			tableLabels, nil,
		),
		tableAnalyzeAge: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "table_last_analyze_age_seconds"),
			"Seconds since the last ANALYZE on a table. NaN if never analyzed.",
			tableLabels, nil,
		),
		connectionsByState: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "connections"),
			"Server-side connection count from pg_stat_activity, labelled by state (active, idle, idle in transaction, etc.).",
			[]string{"state"}, nil,
		),
		poolAcquiredConns: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "pool_acquired_connections"),
			"Connections currently checked out of the pgx pool.",
			nil, nil,
		),
		poolIdleConns: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "pool_idle_connections"),
			"Idle connections held by the pgx pool.",
			nil, nil,
		),
		poolMaxConns: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "pool_max_connections"),
			"Configured maximum size of the pgx pool.",
			nil, nil,
		),
		scrapeErrorsTotal: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "scrape_errors_total"),
			"Cumulative count of pgstats scrape failures. Per-component; non-zero indicates partial data.",
			[]string{"component"}, nil,
		),
		scrapeDurationSeconds: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "scrape_duration_seconds"),
			"Duration of the most recent pgstats scrape in seconds.",
			nil, nil,
		),
	}
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.databaseSize
	ch <- c.tableSize
	ch <- c.tableIndexSize
	ch <- c.tableRowsEstimate
	ch <- c.tableDeadTuples
	ch <- c.tableLiveTuples
	ch <- c.tableAutovacuumAge
	ch <- c.tableAnalyzeAge
	ch <- c.connectionsByState
	ch <- c.poolAcquiredConns
	ch <- c.poolIdleConns
	ch <- c.poolMaxConns
	ch <- c.scrapeErrorsTotal
	ch <- c.scrapeDurationSeconds
}

// Collect implements prometheus.Collector. Each sub-scrape is independent
// so a failure in one component does not prevent the others from reporting.
// Errors are counted and logged, not propagated.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
	defer cancel()

	errs := map[string]int{}

	pool, err := c.pool.Get()
	if err != nil {
		c.logger.Warnw("pgstats cannot resolve pool", "err", err)
		errs["pool_init"]++
	} else {
		if err := c.scrapeDatabaseSize(ctx, pool, ch); err != nil {
			errs["database_size"]++
			c.logger.Warnw("pgstats scrape failed", "component", "database_size", "err", err)
		}
		if err := c.scrapeUserTables(ctx, pool, ch); err != nil {
			errs["user_tables"]++
			c.logger.Warnw("pgstats scrape failed", "component", "user_tables", "err", err)
		}
		if err := c.scrapeConnections(ctx, pool, ch); err != nil {
			errs["connections"]++
			c.logger.Warnw("pgstats scrape failed", "component", "connections", "err", err)
		}
		c.scrapePoolStats(pool, ch) // cannot fail; reads in-process pool state
	}

	for component, count := range errs {
		ch <- prometheus.MustNewConstMetric(
			c.scrapeErrorsTotal,
			prometheus.CounterValue,
			float64(count),
			component,
		)
	}

	ch <- prometheus.MustNewConstMetric(
		c.scrapeDurationSeconds,
		prometheus.GaugeValue,
		time.Since(start).Seconds(),
	)
}

func (c *Collector) scrapeDatabaseSize(ctx context.Context, pool *pgxpool.Pool, ch chan<- prometheus.Metric) error {
	var sizeBytes int64
	err := pool.QueryRow(ctx, `SELECT pg_database_size(current_database())`).Scan(&sizeBytes)
	if err != nil {
		return err
	}
	ch <- prometheus.MustNewConstMetric(c.databaseSize, prometheus.GaugeValue, float64(sizeBytes))
	return nil
}

// scrapeUserTables pulls size, estimated rows, dead/live tuple counts,
// and autovacuum/analyze ages for every user table in the public schema.
// BitAgent has ~30 tables so label cardinality is bounded.
func (c *Collector) scrapeUserTables(ctx context.Context, pool *pgxpool.Pool, ch chan<- prometheus.Metric) error {
	const q = `
SELECT
  c.relname                                                            AS table_name,
  pg_total_relation_size(c.oid)                                        AS total_size,
  pg_indexes_size(c.oid)                                               AS index_size,
  c.reltuples                                                          AS rows_estimate,
  COALESCE(s.n_dead_tup, 0)                                            AS dead_tuples,
  COALESCE(s.n_live_tup, 0)                                            AS live_tuples,
  EXTRACT(EPOCH FROM (now() - GREATEST(s.last_autovacuum, s.last_vacuum))) AS autovacuum_age,
  EXTRACT(EPOCH FROM (now() - GREATEST(s.last_autoanalyze, s.last_analyze))) AS analyze_age
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_stat_user_tables s ON s.relid = c.oid
WHERE c.relkind = 'r'
  AND n.nspname = 'public'
ORDER BY c.relname`

	rows, err := pool.Query(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			tableName     string
			totalSize     int64
			indexSize     int64
			rowsEstimate  float64
			deadTuples    int64
			liveTuples    int64
			autovacuumAge *float64
			analyzeAge    *float64
		)
		if err := rows.Scan(
			&tableName, &totalSize, &indexSize, &rowsEstimate,
			&deadTuples, &liveTuples, &autovacuumAge, &analyzeAge,
		); err != nil {
			return err
		}
		ch <- prometheus.MustNewConstMetric(c.tableSize, prometheus.GaugeValue, float64(totalSize), tableName)
		ch <- prometheus.MustNewConstMetric(c.tableIndexSize, prometheus.GaugeValue, float64(indexSize), tableName)
		ch <- prometheus.MustNewConstMetric(c.tableRowsEstimate, prometheus.GaugeValue, rowsEstimate, tableName)
		ch <- prometheus.MustNewConstMetric(c.tableDeadTuples, prometheus.GaugeValue, float64(deadTuples), tableName)
		ch <- prometheus.MustNewConstMetric(c.tableLiveTuples, prometheus.GaugeValue, float64(liveTuples), tableName)
		if autovacuumAge != nil {
			ch <- prometheus.MustNewConstMetric(c.tableAutovacuumAge, prometheus.GaugeValue, *autovacuumAge, tableName)
		}
		if analyzeAge != nil {
			ch <- prometheus.MustNewConstMetric(c.tableAnalyzeAge, prometheus.GaugeValue, *analyzeAge, tableName)
		}
	}
	return rows.Err()
}

func (c *Collector) scrapeConnections(ctx context.Context, pool *pgxpool.Pool, ch chan<- prometheus.Metric) error {
	const q = `
SELECT COALESCE(state, 'unknown') AS state, count(*)::bigint AS n
FROM pg_stat_activity
WHERE datname = current_database()
GROUP BY state`

	rows, err := pool.Query(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var state string
		var n int64
		if err := rows.Scan(&state, &n); err != nil {
			return err
		}
		ch <- prometheus.MustNewConstMetric(
			c.connectionsByState, prometheus.GaugeValue, float64(n), state,
		)
	}
	return rows.Err()
}

func (c *Collector) scrapePoolStats(pool *pgxpool.Pool, ch chan<- prometheus.Metric) {
	stat := pool.Stat()
	ch <- prometheus.MustNewConstMetric(
		c.poolAcquiredConns, prometheus.GaugeValue, float64(stat.AcquiredConns()),
	)
	ch <- prometheus.MustNewConstMetric(
		c.poolIdleConns, prometheus.GaugeValue, float64(stat.IdleConns()),
	)
	ch <- prometheus.MustNewConstMetric(
		c.poolMaxConns, prometheus.GaugeValue, float64(stat.MaxConns()),
	)
}
