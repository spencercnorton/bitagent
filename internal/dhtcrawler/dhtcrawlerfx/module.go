package dhtcrawlerfx

import (
	"net"
	"net/netip"

	adht "github.com/anacrolix/dht/v2"
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/dhtcrawler"
	"github.com/spencercnorton/bitagent/internal/dhtcrawler/dhtcrawlerhealthcheck"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"dht_crawler",
		configfx.NewConfigModule[dhtcrawler.Config]("dht_crawler", dhtcrawler.NewDefaultConfig()),
		// Content filter config + *contentfilter.Filter + *contentfilter.Metrics
		// are provided by contentfilterfx so the same Filter instance is
		// shared with the post-classifier hook in internal/processor.
		// (Previously the registration lived here; the move keeps the
		// two consumers honest about which Filter they're using.)
		fx.Provide(
			fx.Annotated{
				Name: "dht_bootstrap_nodes",
				Target: func() []netip.AddrPort {
					addrs := make([]netip.AddrPort, 0, len(adht.DefaultGlobalBootstrapHostPorts))
					for _, strAddr := range adht.DefaultGlobalBootstrapHostPorts {
						addr, err := net.ResolveUDPAddr("udp", strAddr)
						if err != nil {
							panic(err)
						}
						addrs = append(addrs, addr.AddrPort())
					}
					return addrs
				},
			},
			dhtcrawler.New,
			dhtcrawler.NewDiscoveredNodes,
			dhtcrawlerhealthcheck.New,
		),
	)
}
