package wantbridge

import (
	"context"
	"time"
)

// NoOp is the zero-cost Wantbridge implementation. Returned by the
// factory when Config.Enabled == false or no *arr sources are
// configured. Every Match() call returns Tier1 and zero state is
// maintained — the typical fresh-deploy behaviour.
//
// The DHT crawler's hot path can call NoOp.Match() at line rate
// without measurable overhead. NoOp is intentionally non-allocating
// in the steady state.
type NoOp struct{}

// New returns a NoOp Wantbridge. Use NewWith to attach logger +
// callbacks for the real Service when Config.Enabled=true.
func New(cfg Config) Wantbridge {
	return NewWith(cfg, nil, ServiceCallbacks{})
}

// NewWith builds the appropriate Wantbridge for the operator's
// config:
//
//   - Config.Enabled=false  -> NoOp (zero state, hot-path-safe)
//   - Config.Enabled=true && HasAnySource()=false -> NoOp (no
//     wantlist sources to poll)
//   - Config.Enabled=true && HasAnySource()=true -> live Service
//
// The wantbridgefx module is the typical caller; tests construct
// directly.
func NewWith(cfg Config, logger serviceLogger, cb ServiceCallbacks) Wantbridge {
	if !cfg.Enabled {
		return NoOp{}
	}
	if !cfg.HasAnySource() {
		return NoOp{}
	}
	return NewService(cfg, logger, cb)
}

// Match always returns Tier1 — no wantlist data, no signal.
// Important: returning Tier1 (not Tier0) means the dhtcrawler does
// nothing different than the pre-wantbridge behaviour. NoOp is a
// pure pass-through.
func (NoOp) Match(_ string) MatchResult {
	return MatchResult{Tier: Tier1}
}

func (NoOp) Enabled() bool { return false }
func (NoOp) Enforce() bool { return false }

func (NoOp) Snapshot() Snapshot {
	return Snapshot{
		Sources:       map[Source]SourceSnapshot{},
		FingerprintN:  0,
		BloomCapacity: 0,
		BloomFillRate: 0,
		LastRebuild:   time.Time{},
	}
}

// Refresh is a no-op for NoOp. Returns nil — there's nothing to
// refresh.
func (NoOp) Refresh(_ context.Context) error {
	return nil
}
