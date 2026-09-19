package devfx

import (
	"github.com/spencercnorton/bitagent/internal/app/cli"
	"github.com/spencercnorton/bitagent/internal/app/cli/args"
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/database"
	"github.com/spencercnorton/bitagent/internal/database/migrations"
	"github.com/spencercnorton/bitagent/internal/database/postgres"
	"github.com/spencercnorton/bitagent/internal/dev/app/cmd/gormcmd"
	"github.com/spencercnorton/bitagent/internal/dev/app/cmd/migratecmd"
	"github.com/spencercnorton/bitagent/internal/logging/loggingfx"
	"github.com/spencercnorton/bitagent/internal/validation/validationfx"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"dev",
		configfx.NewConfigModule[postgres.Config]("postgres", postgres.NewDefaultConfig()),
		configfx.New(),
		loggingfx.New(),
		validationfx.New(),
		fx.Provide(args.New),
		fx.Provide(cli.New),
		fx.Provide(database.New),
		fx.Provide(migrations.New),
		fx.Provide(postgres.New),
		fx.Provide(gormcmd.New),
		fx.Provide(migratecmd.New),
	)
}
