package processorsfx

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/httpserver"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
	"github.com/spencercnorton/bitagent/internal/processingstate"
	"github.com/spencercnorton/bitagent/internal/processors"
	"github.com/spencercnorton/bitagent/internal/worker"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module("processors", configfx.NewConfigModule[processors.Config]("processors", processors.NewDefaultConfig()), fx.Provide(
		func(c processors.Config, p lazy.Lazy[*pgxpool.Pool], cc classifier.Config, cf contentfilter.Config, n *namepolicy.Policy) (*processors.Store, error) {
			return processors.NewConfiguredStore(c, p, cc, cf, n)
		},
		func(s *processors.Store) processingstate.Recorder { return s },
		fx.Annotate(func(s *processors.Store) worker.Worker { return s.Worker() }, fx.ResultTags(`group:"workers"`)),
		fx.Annotate(func(s *processors.Store) httpserver.Option { return s }, fx.ResultTags(`group:"http_server_options"`)),
		fx.Annotate(func(s *processors.Store) []prometheus.Collector { return s.Collectors() }, fx.ResultTags(`group:"prometheus_collectors,flatten"`)),
	))
}
