package metainfofx

import (
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/protocol/metainfo/banning"
	"github.com/spencercnorton/bitagent/internal/protocol/metainfo/metainforequester"
	"github.com/spencercnorton/bitagent/internal/protocol/metainfo/peerrep"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"metainfo",
		configfx.NewConfigModule[metainforequester.Config](
			"metainfo_requester",
			metainforequester.NewDefaultConfig(),
		),
		// Per-peer reputation cache. Default Enabled=false makes the
		// wrapper inside metainforequester.New a no-op, so deploying
		// this MR is zero-effect until operator opts in:
		//
		//   PEER_REP_ENABLED=true       # start tracking + shadow metrics
		//   PEER_REP_ENFORCE=true       # actually skip suppressed peers
		//
		// Two-stage opt-in mirrors the retention package shape so
		// operators can A/B by metric before enforcing.
		configfx.NewConfigModule[peerrep.Config](
			"peer_rep",
			peerrep.NewDefaultConfig(),
		),
		fx.Provide(
			metainforequester.New,
			banning.New,
		),
	)
}
