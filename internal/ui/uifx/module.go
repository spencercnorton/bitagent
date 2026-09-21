// Package uifx wires the ui worker (supervised Python console) into the app.
// UI_ENABLED=false (default) makes it a pure no-op.
package uifx

import (
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/ui"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"ui",
		configfx.NewConfigModule[ui.Config]("ui", ui.NewDefaultConfig()),
		fx.Provide(ui.New),
	)
}
