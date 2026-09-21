package responder

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/concurrency"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/protocol/dht/ktable"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/time/rate"
)

type Params struct {
	fx.In
	KTable          lazy.Lazy[ktable.Table]
	DiscoveredNodes concurrency.BatchingChannel[ktable.Node] `name:"dht_discovered_nodes"`
	Logger          *zap.SugaredLogger
}

type Result struct {
	fx.Out
	Responder         lazy.Lazy[Responder]
	QueryDuration     prometheus.Collector `group:"prometheus_collectors"`
	QuerySuccessTotal prometheus.Collector `group:"prometheus_collectors"`
	QueryErrorTotal   prometheus.Collector `group:"prometheus_collectors"`
	QueryConcurrency  prometheus.Collector `group:"prometheus_collectors"`
}

const (
	namespace = "bitagent"
	subsystem = "dht_responder"
)

func New(p Params) Result {
	// Collectors are allocated up front so they register with Prometheus
	// exactly once; the responder waits for the routing table, whose
	// node identity is resolved on first use.
	collector := newPrometheusCollector(nil)

	return Result{
		Responder: lazy.New(func() (Responder, error) {
			kTable, err := p.KTable.Get()
			if err != nil {
				return nil, err
			}
			c := collector
			c.responder = responderLimiter{
				responder: responder{
					nodeID:                   kTable.Origin(),
					kTable:                   kTable,
					tokenSecret:              protocol.RandomNodeID().Bytes(),
					sampleInfoHashesInterval: 10,
				},
				limiter: NewLimiter(rate.Every(time.Second/50), 20, rate.Every(time.Second), 10, 1000, time.Second*20),
			}
			return responderNodeDiscovery{
				responder: responderLogger{
					responder: c,
					logger: p.Logger.WithOptions(zap.WrapCore(func(core zapcore.Core) zapcore.Core {
						return zapcore.NewSamplerWithOptions(core, time.Minute, 10, 0)
					})).Named(subsystem),
				},
				discoveredNodes: p.DiscoveredNodes.In(),
			}, nil
		}),
		QueryDuration:     collector.queryDuration,
		QuerySuccessTotal: collector.querySuccessTotal,
		QueryErrorTotal:   collector.queryErrorTotal,
		QueryConcurrency:  collector.queryConcurrency,
	}
}
