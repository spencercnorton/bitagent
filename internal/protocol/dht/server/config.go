package server

import "time"

type Config struct {
	Port         uint16
	QueryTimeout time.Duration

	// QueryLimiterRatePerSec is the sustained outbound-query rate per
	// remote peer (keyed by IP) in tokens/second. The token bucket
	// keeps us a polite DHT citizen when we discover a chatty peer
	// that'd happily answer everything we can throw at it.
	//
	// Default 1.0 matches upstream bitmagnet's original tuning.
	QueryLimiterRatePerSec float64

	// QueryLimiterBurst is the burst capacity of the per-remote-peer
	// token bucket. Matters more than the sustained rate for a DHT
	// crawler: after a successful sample_infohashes reveals N new
	// infohashes from one peer we typically fan out N get_peers back
	// at that same peer, and the burst governs how much of that fan
	// goes out before we queue.
	//
	// Default raised from upstream's 4 to 16 after observing
	// outbound get_peers p50 latency sitting ~4x above ping p50
	// (827ms vs 150ms, 2026-04-24). Evidence the burst was the
	// bottleneck: ping traffic is sparse (no fan-in cluster), so
	// ping queries rarely exhaust a bucket; get_peers clusters on
	// productive peers and queues 4-8 deep at 1/sec drain =
	// 500-1000ms added wait. 16 gives a 4x headroom without raising
	// sustained per-peer send rate.
	QueryLimiterBurst int
}

func NewDefaultConfig() Config {
	return Config{
		Port:                   3334,
		QueryTimeout:           time.Second * 4,
		QueryLimiterRatePerSec: 1.0,
		QueryLimiterBurst:      16,
	}
}
