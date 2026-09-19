package externalip

import "time"

// FxConfig is the configfx-registered config block for the
// external-IP resolver and watcher. Lives in its own config section
// (separate from `dht_server`) so operational tuning doesn't touch
// the locked qbt-vpn-adjacent surface.
//
// All zero-values map to sensible defaults (see NewDefaultFxConfig).
type FxConfig struct {
	// Override short-circuits the resolver to a static value. Leave
	// empty in production. Break-glass only; see resolver.go.
	Override string

	// Interval is the base poll period for the watcher.
	Interval time.Duration

	// JitterFraction randomizes each sleep by ±this fraction of
	// Interval. 0.1 = ±10%. Set to -1 to disable. Used for fleet
	// stagger.
	JitterFraction float64

	// PerCheckTimeout bounds each resolve attempt.
	PerCheckTimeout time.Duration

	// ConsecutiveConfirmations is the number of agreeing
	// invalidating resolves required before the watcher fires.
	ConsecutiveConfirmations int

	// StartupTimeout bounds the initial resolve at container boot.
	// Independent from PerCheckTimeout because the boot path should
	// give up faster: we'd rather start in random-fallback mode and
	// let the watcher self-heal than block the whole fx graph on a
	// slow IP echo service.
	StartupTimeout time.Duration
}

// NewDefaultFxConfig returns the operator-friendly defaults. Values
// chosen per the reviewer's guidance:
//
//   - 15m cadence with ±10% jitter (non-zero default, but soft enough
//     not to trigger thrash)
//   - 2 consecutive confirmations filter single-source flips
//   - 10s per-check timeout, 30s startup timeout (allow VPN warmup)
func NewDefaultFxConfig() FxConfig {
	return FxConfig{
		Override:                 "",
		Interval:                 15 * time.Minute,
		JitterFraction:           0.1,
		PerCheckTimeout:          10 * time.Second,
		ConsecutiveConfirmations: 2,
		StartupTimeout:           30 * time.Second,
	}
}
