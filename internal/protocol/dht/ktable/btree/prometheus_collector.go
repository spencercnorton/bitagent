package btree

import "github.com/spencercnorton/bitagent/internal/telemetry/dualemit"

type PrometheusCollector struct {
	Btree          Btree
	CountGauge     *dualemit.Gauge
	AddedCounter   *dualemit.Counter
	DroppedCounter *dualemit.Counter
}

func (p PrometheusCollector) N() int {
	return p.Btree.N()
}

func (p PrometheusCollector) Put(id NodeID) PutResult {
	result := p.Btree.Put(id)
	if result == PutAccepted {
		p.CountGauge.Add(1)
		p.AddedCounter.Inc()
	}

	return result
}

func (p PrometheusCollector) Has(id NodeID) bool {
	return p.Btree.Has(id)
}

func (p PrometheusCollector) Drop(id NodeID) bool {
	result := p.Btree.Drop(id)
	if result {
		p.CountGauge.Add(-1)
		p.DroppedCounter.Inc()
	}

	return result
}

func (p PrometheusCollector) Closest(id NodeID, count int) []NodeID {
	return p.Btree.Closest(id, count)
}

func (p PrometheusCollector) Count() int {
	return p.Btree.Count()
}
