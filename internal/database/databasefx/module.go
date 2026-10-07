package databasefx

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/database"
	"github.com/spencercnorton/bitagent/internal/database/cache"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/database/healthcheck"
	"github.com/spencercnorton/bitagent/internal/database/migrations"
	"github.com/spencercnorton/bitagent/internal/database/pgstats/pgstatsfx"
	"github.com/spencercnorton/bitagent/internal/database/postgres"
	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
	policyhttp "github.com/spencercnorton/bitagent/internal/namepolicy/httpserver"
	"github.com/spencercnorton/bitagent/internal/serving"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"database",
		configfx.NewConfigModule[postgres.Config]("postgres", postgres.NewDefaultConfig()),
		configfx.NewConfigModule[cache.Config]("gorm_cache", cache.NewDefaultConfig()),
		configfx.NewConfigModule[serving.Config]("serving", serving.Config{}),
		fx.Provide(
			serving.NewMetrics,
			func(cfg serving.Config, names *namepolicy.Policy) (*serving.Policy, error) {
				strong, media, err := classifier.CoreAdultServingEvidence()
				if err != nil {
					return nil, err
				}
				return serving.NewPolicy(cfg, strong, media, names)
			},
			cache.NewInMemoryCacher,
			cache.NewPlugin,
			dao.New,
			database.New,
			healthcheck.New,
			migrations.New,
			postgres.New,
			search.New,
		),
		fx.Provide(fx.Annotated{
			Group:  "http_server_options",
			Target: policyhttp.NewPublicHash,
		}),
		fx.Provide(fx.Annotated{
			Group:  "prometheus_collectors,flatten",
			Target: func(m *serving.Metrics) []prometheus.Collector { return m.Collectors() },
		}),
		fx.Decorate(
			cache.NewDecorator,
		),
		pgstatsfx.New(),
	)
}
