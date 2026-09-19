package databasefx

import (
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/database"
	"github.com/spencercnorton/bitagent/internal/database/cache"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/database/healthcheck"
	"github.com/spencercnorton/bitagent/internal/database/migrations"
	"github.com/spencercnorton/bitagent/internal/database/pgstats/pgstatsfx"
	"github.com/spencercnorton/bitagent/internal/database/postgres"
	"github.com/spencercnorton/bitagent/internal/database/search"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"database",
		configfx.NewConfigModule[postgres.Config]("postgres", postgres.NewDefaultConfig()),
		configfx.NewConfigModule[cache.Config]("gorm_cache", cache.NewDefaultConfig()),
		fx.Provide(
			cache.NewInMemoryCacher,
			cache.NewPlugin,
			dao.New,
			database.New,
			healthcheck.New,
			migrations.New,
			postgres.New,
			search.New,
		),
		fx.Decorate(
			cache.NewDecorator,
		),
		pgstatsfx.New(),
	)
}
