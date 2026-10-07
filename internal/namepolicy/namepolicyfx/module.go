package namepolicyfx

import (
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/httpserver"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
	policyhttp "github.com/spencercnorton/bitagent/internal/namepolicy/httpserver"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module("name_policy",
		configfx.NewConfigModule[namepolicy.Config]("name_policy", namepolicy.NewDefaultConfig()),
		fx.Provide(namepolicy.New),
		fx.Provide(fx.Annotated{Group: "http_server_options", Target: func(p *namepolicy.Policy) httpserver.Option { return policyhttp.New(p) }}),
	)
}
