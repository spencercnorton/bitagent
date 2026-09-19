package processor

import (
	"sync"
	"sync/atomic"

	"go.uber.org/zap"
)

// deleteAuditBudget is the enforceable stop condition on delete-name capture.
//
// The capture exists to answer a bounded question — a few hundred adjudicated
// names give a Wilson bound on the false-delete rate of the classifier's
// destructive rules. Without a cap, "we will turn it off once
// that is done" is a promise, not a mechanism, and this estate has already
// been bitten by exactly that shape: junkpurge's 30-day quarantine review
// window was signed off as the safety argument and
// `torrent_verdict_events` then recorded zero operator reviews for the whole
// period. A privacy-narrowing collection whose only stop condition is a future
// human edit will run until someone remembers.
//
// So the process stops on its own. Once Max names have been recorded the
// capture goes quiet and stays quiet, regardless of the flag.
//
// The counter is per-process and resets on restart. That is deliberate and
// sufficient: it bounds a runaway to Max names per restart rather than
// unbounded growth, and it needs no query on the delete path (which runs
// ~160k times a day). A durable cap would mean counting rows per delete — far
// more machinery than the guarantee is worth.
//
// ponytail: in-memory counter, per-process. If collection ever needs to
// survive restarts with an exact global cap, count rows once at startup and
// seed the counter — do not add a per-delete query.
type deleteAuditBudget struct {
	// on is the operator's opt-in (CLASSIFIER_DELETE_AUDIT_SAMPLE).
	on bool
	// max is the number of names this process may record; <= 0 disables
	// capture entirely rather than meaning "unlimited", so a
	// mis-set-to-zero config fails closed.
	max int64

	recorded atomic.Int64
	// exhaustedOnce keeps the "budget spent" log line to exactly one
	// emission instead of one per delete for the rest of the process life.
	exhaustedOnce sync.Once
	logger        *zap.SugaredLogger
}

func newDeleteAuditBudget(on bool, max int, logger *zap.SugaredLogger) *deleteAuditBudget {
	return &deleteAuditBudget{on: on, max: int64(max), logger: logger}
}

// enabled reports whether capture is switched on AND configured with a usable
// cap. A non-positive cap is treated as off: an unlimited collection is
// exactly what this type exists to prevent, so it must not be reachable by
// setting the value to zero.
func (b *deleteAuditBudget) enabled() bool {
	return b != nil && b.on && b.max > 0
}

// consume claims one unit of budget, reporting false once the cap is reached.
// Safe for concurrent use — the classify loop runs one goroutine per torrent.
func (b *deleteAuditBudget) consume() bool {
	if !b.enabled() {
		return false
	}
	// Add-then-check rather than check-then-add: two goroutines racing on a
	// compare could otherwise both observe recorded == max-1 and both record.
	if b.recorded.Add(1) > b.max {
		b.exhaustedOnce.Do(func() {
			if b.logger != nil {
				b.logger.Infow(
					"classifier delete-audit sample complete; no further names will be recorded",
					"recorded", b.max,
					"note", "set CLASSIFIER_DELETE_AUDIT_SAMPLE=false to stop the flag drifting on",
				)
			}
		})
		return false
	}
	return true
}

// Recorded exposes the running count for tests and operator logging, clamped
// to [0, max]. The clamp matters at both ends: consume() adds before comparing
// so the raw counter overshoots max by design, and a disabled budget has a
// non-positive max that must not surface as a negative "recorded" count.
func (b *deleteAuditBudget) Recorded() int64 {
	if !b.enabled() {
		return 0
	}
	if n := b.recorded.Load(); n < b.max {
		return n
	}
	return b.max
}
