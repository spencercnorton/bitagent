package dhtcrawler

import (
	"time"
)

type Config struct {
	// ScalingFactor is a rough proxy for resource usage of the crawler; concurrency and buffer size of the various
	// pipeline channels are multiplied by this value. Diminishing returns may result from exceeding the
	// default value of 10. Since the software has not been tested on a wide variety of hardware and network
	// conditions; your mileage may vary here...
	ScalingFactor                uint
	BootstrapNodes               []string
	ReseedBootstrapNodesInterval time.Duration
	// SaveFilesThreshold specifies a maximum number of files in a torrent before file information is discarded.
	// Some torrents contain thousands of files which can severely impact performance and uses a lot of disk space.
	SaveFilesThreshold uint
	// SavePieces when true, torrent pieces will be persisted to the database.
	// The pieces take up quite a lot of space, and aren't currently very useful,
	// but they may be used by future features.
	SavePieces bool
	// RescrapeThreshold is the amount of time that must pass before a torrent is rescraped
	// to count seeders and leechers.
	RescrapeThreshold time.Duration
	// BootstrapStatePath, if non-empty, enables persistence of a
	// warm-start bootstrap list across container restarts. On
	// startup the file is loaded and its addresses prepended to
	// BootstrapNodes; on clean shutdown the current ktable
	// membership is written back. Useful when restart-on-IP-change
	// is expected to be a normal recovery path — it keeps
	// the graph reachable without an RTT against the well-known
	// DHT routers. Empty = disabled.
	//
	// Default-empty so this MR is a no-op until an operator opts
	// in by pointing at a persistent bind mount (e.g.
	// `/config/state/dht-bootstrap.peers` on the standard
	// `/config` volume).
	BootstrapStatePath string
	// BootstrapStateSize caps the number of addresses read from
	// and written to the state file. Zero-value is 200.
	BootstrapStateSize uint
	// MetainfoConcurrency overrides the per-process cap on
	// concurrent BEP-9 metadata fetches. When zero, falls back to
	// the legacy 40 × ScalingFactor formula (default 400). When
	// non-zero, this value wins regardless of ScalingFactor.
	//
	// Provided as an independent knob (not just a different multiplier)
	// because the BEP-9 fetcher is the dominant cost center on a busy
	// crawler. The maintainer's 2026-04-24 audit showed:
	//
	//	meta_info_requester_success_total = 207,578
	//	meta_info_requester_error_total   = 8,680,079   (~98% wasted)
	//	meta_info_requester_concurrency   = ~500 in-flight
	//
	// The 8.7M wasted handshakes are mostly TCP attempts to dead /
	// NAT-bound peers that will never reply. Lowering the in-flight
	// cap from 400 to a smaller value frees CPU for the 2.3% that
	// will succeed. The right value is empirical, not assumed — the
	// GPT-5.5-pro review explicitly warned against shipping a 150
	// default cold. So this MR adds the dial without changing the
	// effective behaviour: zero means "use legacy formula." Operators
	// flip it via env (`DHT_CRAWLER_METAINFO_CONCURRENCY=200`) and
	// observe `meta_info_requester_concurrency` /
	// `..._success_total` / `..._error_total` rates over an
	// A/B-style schedule (e.g. 400 → 200 → 400 → 100 in 60-90 min
	// windows) to find the knee.
	MetainfoConcurrency uint
}

func NewDefaultConfig() Config {
	return Config{
		ScalingFactor:                10,
		BootstrapNodes:               defaultBootstrapNodes,
		ReseedBootstrapNodesInterval: time.Minute,
		SaveFilesThreshold:           100,
		SavePieces:                   false,
		RescrapeThreshold:            time.Hour * 24 * 30,
		BootstrapStatePath:           "",
		BootstrapStateSize:           200,
	}
}

// https://github.com/anacrolix/dht/blob/92b36a3fa7a37a15e08684337b47d8d0fb322ab6/dht.go#L106
var defaultBootstrapNodes = []string{
	"router.utorrent.com:6881",
	"router.bittorrent.com:6881",
	"dht.transmissionbt.com:6881",
	"dht.aelitis.com:6881",     // Vuze
	"router.silotis.us:6881",   // IPv6
	"dht.libtorrent.org:25401", // @arvidn's
}
