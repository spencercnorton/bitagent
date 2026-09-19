package prometheus

import (
	"context"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
	"go.uber.org/zap"
)

// Namespace for the queue metrics. Kept as a compound string for the
// scrape-time-computed tasksQueuedDesc below; during the BitAgent
// Phase 2 dual-emit window we also emit the legacy `bitmagnet_queue`
// family so any surviving dashboards keep working.
const (
	namespace       = "bitagent_queue"
	legacyNamespace = "bitmagnet_queue"
)

// queueMetricsCollector gathers queue metrics.
// It implements prometheus.Collector interface.
type queueMetricsCollector struct {
	query  lazy.Lazy[*dao.Query]
	logger *zap.SugaredLogger
}

var tasksQueuedDesc = prometheus.NewDesc(
	prometheus.BuildFQName(namespace, "", "jobs_total"),
	"Number of tasks enqueued; broken down by queue and status.",
	[]string{"queue", "status"}, nil,
)

// Legacy desc for the dual-emit window. When dualemit.EmitLegacy is
// false, the loop below short-circuits and this desc is never emitted.
var tasksQueuedDescLegacy = prometheus.NewDesc(
	prometheus.BuildFQName(legacyNamespace, "", "jobs_total"),
	"Number of tasks enqueued; broken down by queue and status.",
	[]string{"queue", "status"}, nil,
)

func (qmc *queueMetricsCollector) Describe(ch chan<- *prometheus.Desc) {
	prometheus.DescribeByCollect(qmc, ch)
}

func (qmc *queueMetricsCollector) Collect(ch chan<- prometheus.Metric) {
	queueInfos, err := qmc.collectQueueStatusInfos()
	if err != nil {
		qmc.logger.Errorf("Failed to collect metrics data: %s", err)
	}

	for _, info := range queueInfos {
		ch <- prometheus.MustNewConstMetric(
			tasksQueuedDesc,
			prometheus.GaugeValue,
			float64(info.Count),
			info.Queue,
			info.Status.String(),
		)
		if dualemit.EmitLegacy {
			ch <- prometheus.MustNewConstMetric(
				tasksQueuedDescLegacy,
				prometheus.GaugeValue,
				float64(info.Count),
				info.Queue,
				info.Status.String(),
			)
		}
	}
}

type queueStatusInfo struct {
	Queue  string
	Status model.QueueJobStatus
	Count  int
}

func (qmc *queueMetricsCollector) collectQueueStatusInfos() ([]*queueStatusInfo, error) {
	q, err := qmc.query.Get()
	if err != nil {
		return nil, fmt.Errorf("failed to get query: %w", err)
	}

	var queueInfos []*queueStatusInfo

	err = q.QueueJob.WithContext(context.Background()).UnderlyingDB().Raw(
		"SELECT queue, status, count(*) FROM queue_jobs GROUP BY queue, status",
	).Find(&queueInfos).Error
	if err != nil {
		return nil, fmt.Errorf("failed to get queue status info: %w", err)
	}

	return queueInfos, nil
}
