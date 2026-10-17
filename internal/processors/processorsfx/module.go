package processorsfx

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/evidence"
	"github.com/spencercnorton/bitagent/internal/httpserver"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
	"github.com/spencercnorton/bitagent/internal/processingstate"
	"github.com/spencercnorton/bitagent/internal/processors"
	"github.com/spencercnorton/bitagent/internal/worker"
	"go.uber.org/fx"
)

type storeParams struct {
	fx.In

	Config      processors.Config
	Pool        lazy.Lazy[*pgxpool.Pool]
	Classifier  classifier.Config
	Filter      contentfilter.Config
	NamePolicy  *namepolicy.Policy
	Evidence    *evidence.Store `optional:"true"`
}

func New() fx.Option {
	return fx.Module("processors", configfx.NewConfigModule[processors.Config]("processors", processors.NewDefaultConfig()), fx.Provide(
		func(p storeParams) (*processors.Store, error) {
			s, err := processors.NewConfiguredStore(p.Config, p.Pool, p.Classifier, p.Filter, p.NamePolicy)
			if err != nil {
				return nil, err
			}
			if p.Evidence != nil {
				p.Evidence.AddPostInsertHook(s.HandleEvidence)
			}
			return s, nil
		},
		func(s *processors.Store) processingstate.Recorder { return s },
		fx.Annotate(func(s *processors.Store) worker.Worker { return s.Worker() }, fx.ResultTags(`group:"workers"`)),
		fx.Annotate(func(s *processors.Store) httpserver.Option { return s }, fx.ResultTags(`group:"http_server_options"`)),
		fx.Annotate(func(s *processors.Store) []prometheus.Collector { return s.Collectors() }, fx.ResultTags(`group:"prometheus_collectors,flatten"`)),
	))
}
