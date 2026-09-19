package externalip

import (
	"context"
	"math/rand/v2"
	"net/netip"
	"time"

	"github.com/spencercnorton/bitagent/internal/protocol"
	"go.uber.org/zap"
)

// OnChange is invoked when the watcher has seen `ConfirmationCount`
// consecutive agreeing resolves that either (a) no longer verify
// against `currentNodeID` under BEP-42 or (b) represent the first
// valid IP after a boot-time random-ID fallback. It MUST NOT block
// for long — the intended use is `shutdowner.Shutdown()` so the
// container runtime starts us back up with a fresh node ID.
type OnChange func(old, current netip.Addr)

// WatcherConfig controls the watcher's polling behaviour and the
// trigger policy. Defaults chosen so that the zero-value
// construction still produces something sensible; only
// `PerCheckTimeout` and `OnChange` need to be supplied to NewWatcher
// in practice.
type WatcherConfig struct {
	// Interval is the base poll period. Actual sleeps are
	// `Interval ± Interval*JitterFraction`. Zero-value is 15 min.
	Interval time.Duration
	// JitterFraction is the ±fraction of Interval to randomize each
	// sleep by, e.g. 0.1 = ±10%. Staggers restarts across fleet
	// instances so a provider-side hiccup doesn't cause a thundering
	// herd. Zero-value is 0.1; pass -1 to disable jitter.
	JitterFraction float64
	// PerCheckTimeout bounds each resolve attempt. Zero-value is 10s.
	PerCheckTimeout time.Duration
	// ConsecutiveConfirmations is the number of agreeing polls
	// required before OnChange fires. 1 = fire on first divergence
	// (flaky); 2+ filters single-source glitches. Zero-value is 2.
	ConsecutiveConfirmations int
}

// Watcher periodically re-resolves the external IP and fires
// OnChange when the current node ID no longer verifies against the
// observed IP (or when we started up with a random-fallback ID and
// can finally get a real anchor).
//
// Three explicit design choices (each called out in the GPT-5.4-pro
// review):
//
//  1. **Validity-based, not IP-equality-based.** Raw IP inequality
//     can fire spuriously if a source flips to a different valid v4
//     egress (rare, but possible with ISPs that rotate between two
//     public addresses). The correctness condition is "does our
//     current node ID still satisfy BEP-42 for the observed IP?",
//     not "did the string change?".
//  2. **Multi-confirmation.** One divergent resolve is not enough to
//     restart the process. N consecutive agreeing resolves are
//     required before OnChange fires.
//  3. **Errors never fire.** A flaky ipinfo.io or a transient VPN
//     blip must not bounce the container. Only a confirmed,
//     validated, BEP-42-invalidating IP does.
type Watcher struct {
	resolver       Resolver
	currentNodeID  protocol.ID
	bootIP         netip.Addr // zero when boot fell back to random ID
	randomFallback bool       // true when boot ID is NOT BEP-42 derived
	cfg            WatcherConfig
	onChange       OnChange
	logger         *zap.SugaredLogger
	metrics        *WatcherMetrics
	rand           *rand.Rand
}

// NewWatcher constructs a Watcher. Pass `randomFallback=true` if the
// caller booted without a valid external IP (i.e. the node ID is
// `RandomNodeIDWithClientSuffix` rather than `SecureNodeID`) — the
// watcher will then fire on the first confirmed valid resolve rather
// than waiting for a change, so the container self-heals into BEP-42
// compliance as soon as the VPN / resolver recovers.
func NewWatcher(
	resolver Resolver,
	currentNodeID protocol.ID,
	bootIP netip.Addr,
	randomFallback bool,
	cfg WatcherConfig,
	onChange OnChange,
	logger *zap.SugaredLogger,
	metrics *WatcherMetrics,
) *Watcher {
	if cfg.Interval <= 0 {
		cfg.Interval = 15 * time.Minute
	}
	if cfg.PerCheckTimeout <= 0 {
		cfg.PerCheckTimeout = 10 * time.Second
	}
	if cfg.ConsecutiveConfirmations <= 0 {
		cfg.ConsecutiveConfirmations = 2
	}
	if cfg.JitterFraction == 0 {
		cfg.JitterFraction = 0.1
	}
	if cfg.JitterFraction < 0 { // explicit disable
		cfg.JitterFraction = 0
	}

	return &Watcher{
		resolver:       resolver,
		currentNodeID:  currentNodeID,
		bootIP:         bootIP,
		randomFallback: randomFallback,
		cfg:            cfg,
		onChange:       onChange,
		logger:         logger,
		metrics:        metrics,
		// Deterministic per-process seed is fine — the goal of jitter
		// is inter-instance staggering, not unpredictability.
		rand: rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0)),
	}
}

// Run blocks until ctx is done, polling on a jittered interval.
// Returns after OnChange fires (exactly once) or ctx is canceled.
func (w *Watcher) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(w.nextSleep()):
			if done := w.tick(ctx); done {
				return
			}
		}
	}
}

// tick runs one iteration of the watch loop and returns true iff the
// watcher should exit (OnChange fired).
func (w *Watcher) tick(ctx context.Context) bool {
	callCtx, cancel := context.WithTimeout(ctx, w.cfg.PerCheckTimeout)
	defer cancel()

	current, err := w.resolver.Resolve(callCtx)
	if err != nil {
		w.logger.Warnw("external IP check failed; keeping current node ID",
			"err", err.Error())
		w.metrics.recordFailure()

		return false
	}

	w.metrics.recordSuccess(current)

	// Decide whether this resolve represents a BEP-42-invalidating
	// outcome. Two paths:
	//
	//   (a) We booted with a random-fallback ID. Any valid resolve
	//       is a graduation opportunity; fire on confirmation.
	//   (b) We booted with a BEP-42 ID. Fire only when the current
	//       ID no longer verifies for the observed IP. IP-equality
	//       is the common case of invalidation but not the only one
	//       — VerifySecureNodeID captures the full correctness check.
	var invalid bool
	switch {
	case w.randomFallback:
		invalid = true // graduate out of fallback
	default:
		invalid = !protocol.VerifySecureNodeID(w.currentNodeID, current)
	}

	if !invalid {
		w.metrics.resetPendingConfirmations()
		w.logger.Debugw("external IP still matches current node ID",
			"ip", current.String())

		return false
	}

	// Confirmation window: require N consecutive agreeing
	// invalidating resolves before firing. This filters the
	// single-source-flipped case where one endpoint momentarily
	// returns a different valid v4 (e.g. a CDN node on a different
	// egress).
	pending := w.metrics.addPendingConfirmation(current)
	if pending < w.cfg.ConsecutiveConfirmations {
		w.logger.Infow("external IP appears invalidating; waiting for confirmation",
			"current", current.String(),
			"pending", pending,
			"required", w.cfg.ConsecutiveConfirmations,
			"random_fallback", w.randomFallback)

		return false
	}

	w.logger.Warnw(
		"external IP confirmed BEP-42-invalidating — invoking onChange",
		"boot_ip", w.bootIP.String(),
		"current", current.String(),
		"random_fallback", w.randomFallback,
	)
	w.onChange(w.bootIP, current)

	return true
}

// nextSleep returns the base interval ± jitter. Purely additive on
// top of the existing deterministic cadence — a JitterFraction of
// 0.1 means each sleep is uniformly in [0.9*Interval, 1.1*Interval].
func (w *Watcher) nextSleep() time.Duration {
	if w.cfg.JitterFraction <= 0 {
		return w.cfg.Interval
	}
	span := float64(w.cfg.Interval) * w.cfg.JitterFraction
	// w.rand.Float64() is [0, 1) → 2*f - 1 is [-1, 1)
	delta := time.Duration(span * (2*w.rand.Float64() - 1))

	return w.cfg.Interval + delta
}
