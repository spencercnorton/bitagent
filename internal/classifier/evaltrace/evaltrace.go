// Package evaltrace carries an optional, context-scoped attribution trace
// through a classifier run. When a Trace is attached to the context, the
// workflow records which stage attached content (per-pass attribution for the
// eval harness); when absent every call is a no-op, so the live path pays
// nothing. The trace can also ask the canonical-label decorator to skip its
// preemption shortcut so replays exercise the full matching ladder even for
// torrents that have arr ground truth.
package evaltrace

import (
	"context"
	"sync"
)

type ctxKey struct{}

type Trace struct {
	// SkipCanonicalPreempt makes runner_canonical fall through to the inner
	// runner even when a canonical label exists. Set by eval-replay so gold
	// rows exercise the matcher instead of the evidence shortcut.
	SkipCanonicalPreempt bool

	mu     sync.Mutex
	stages []string
}

// With attaches t to the context. A nil t returns ctx unchanged.
func With(ctx context.Context, t *Trace) context.Context {
	if t == nil {
		return ctx
	}

	return context.WithValue(ctx, ctxKey{}, t)
}

// From returns the Trace attached to ctx, or nil.
func From(ctx context.Context) *Trace {
	t, _ := ctx.Value(ctxKey{}).(*Trace)
	return t
}

// Record appends a successful stage name to the trace, if one is attached.
func Record(ctx context.Context, stage string) {
	t := From(ctx)
	if t == nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.stages = append(t.stages, stage)
}

// SkipPreempt reports whether the canonical preemption shortcut should be
// bypassed for this run.
func SkipPreempt(ctx context.Context) bool {
	t := From(ctx)
	return t != nil && t.SkipCanonicalPreempt
}

// Stages returns a copy of the recorded stage names in order.
func (t *Trace) Stages() []string {
	if t == nil {
		return nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	out := make([]string, len(t.stages))
	copy(out, t.stages)

	return out
}

// AttachedBy returns the last recorded stage, or "" when nothing attached.
func (t *Trace) AttachedBy() string {
	if t == nil {
		return ""
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.stages) == 0 {
		return ""
	}

	return t.stages[len(t.stages)-1]
}
