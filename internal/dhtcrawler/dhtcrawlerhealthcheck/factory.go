package dhtcrawlerhealthcheck

import (
	"time"

	"github.com/spencercnorton/bitagent/internal/concurrency"
	"github.com/spencercnorton/bitagent/internal/health"
	"github.com/spencercnorton/bitagent/internal/protocol/dht/server"
	"go.uber.org/fx"
)

type Params struct {
	fx.In
	DhtCrawlerActive       *concurrency.AtomicValue[bool]                 `name:"dht_crawler_active"`
	DhtServerLastResponses *concurrency.AtomicValue[server.LastResponses] `name:"dht_server_last_responses"`
}

type Result struct {
	fx.Out
	Option health.CheckerOption `group:"health_check_options"`
}

func New(params Params) Result {
	return Result{
		Option: health.WithPeriodicCheck(
			time.Second*10,
			time.Second*1,
			NewCheck(params.DhtCrawlerActive, params.DhtServerLastResponses),
		),
	}
}
