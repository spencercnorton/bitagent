package liveness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spencercnorton/bitagent/internal/evidence"
	"go.uber.org/zap"
)

// store is the resolver's dependency on the persistence layer. It
// is an interface (not the concrete *Store) so the unit tests can
// drive the state machine with an in-memory fake without spinning up
// Postgres.
type store interface {
	Get(ctx context.Context, infoHash []byte) (*Record, error)
	MarkAlive(ctx context.Context, infoHash []byte, observedAt time.Time, qbState, source string) error
	RecordSuspect(ctx context.Context, infoHash []byte, observedAt time.Time, qbState string) (*Record, error)
	MarkDead(ctx context.Context, infoHash []byte, blacklistedAt time.Time, ttl time.Duration) error
}

// observationSink is the subset of counters the resolver
// increments. Implemented by *Metrics in this package (see
// metrics.go); modelled as an interface so tests can plug a no-op.
type observationSink interface {
	Observation(class, outcome string)
}

// Resolver applies the liveness state machine to incoming evidence
// rows. One Resolver per process; all methods are safe to call
// concurrently because the underlying store handles its own locking
// (Postgres row-level via the upserts).
type Resolver struct {
	store   store
	cfg     evidence.LivenessConfig
	metrics observationSink
	logger  *zap.SugaredLogger
	now     func() time.Time
}

// NewResolver constructs a resolver. now is injectable to keep
// tests deterministic; production callers pass time.Now.
func NewResolver(s store, cfg evidence.LivenessConfig, metrics observationSink, logger *zap.SugaredLogger) *Resolver {
	return &Resolver{
		store:   s,
		cfg:     cfg,
		metrics: metrics,
		logger:  logger,
		now:     time.Now,
	}
}

// HandleEvidence is the single entry point invoked by the evidence
// store on every successful Insert. It dispatches by Kind:
//
//   - KindQBStateObservation → drive the alive/suspect/dead state
//     machine.
//   - KindWebhookImport / KindPollHistory (when payload indicates an
//     import) → mark alive, subject to the private-tracker filter.
//
// All other kinds are ignored. Errors are logged but not returned —
// the evidence row is already persisted, and the resolver is a
// derived projection that should never block ingest.
func (r *Resolver) HandleEvidence(ctx context.Context, ev evidence.Evidence) {
	if !r.cfg.Enabled {
		return
	}
	if len(ev.InfoHash) == 0 {
		return
	}
	switch ev.Kind {
	case evidence.KindQBStateObservation:
		r.handleQBState(ctx, ev)
	case evidence.KindWebhookImport, evidence.KindPollHistory:
		// Only treat *arr import outcomes as alive. KindPollHistory
		// also covers *arr "grab" rows; the strength constants
		// distinguish them. A grab is intent, an import is
		// confirmation; we want the latter only.
		if ev.Strength >= evidence.StrengthArrPollImport {
			r.handleArrImport(ctx, ev)
		}
	}
}

func (r *Resolver) handleQBState(ctx context.Context, ev evidence.Evidence) {
	class := evidence.ClassifyQBState(ev.QBState)
	switch class {
	case evidence.QBStateClassAlive:
		if err := r.store.MarkAlive(ctx, ev.InfoHash, ev.ObservedAt, ev.QBState, AliveSourceQBState); err != nil {
			r.logFailure("liveness mark alive", err, ev.InfoHash)
			r.metrics.Observation(string(class), "error")
			return
		}
		r.metrics.Observation(string(class), "upsert")
	case evidence.QBStateClassSuspect:
		rec, err := r.store.RecordSuspect(ctx, ev.InfoHash, ev.ObservedAt, ev.QBState)
		if err != nil {
			r.logFailure("liveness record suspect", err, ev.InfoHash)
			r.metrics.Observation(string(class), "error")
			return
		}
		r.metrics.Observation(string(class), "upsert")
		// Promotion to dead: must be in suspect status (we don't
		// downgrade alive on a single observation), must have first
		// been seen as suspect at least StallThreshold ago, and must
		// have accumulated at least MinObservations rows.
		if rec.Status != StatusSuspect {
			return
		}
		if rec.SuspectFirstSeenAt == nil {
			return
		}
		if r.now().Sub(*rec.SuspectFirstSeenAt) < r.cfg.StallThreshold() {
			return
		}
		if rec.SuspectObservations < r.cfg.MinObservations {
			return
		}
		if err := r.store.MarkDead(ctx, ev.InfoHash, r.now(), r.cfg.BlacklistTTL()); err != nil {
			r.logFailure("liveness mark dead", err, ev.InfoHash)
			r.metrics.Observation(string(class), "error")
			return
		}
		r.metrics.Observation(string(class), "blacklisted")
	default:
		// QBStateClassIgnore — should never reach here because the
		// poller refuses to emit ignore-class rows, but defend the
		// invariant.
	}
}

func (r *Resolver) handleArrImport(ctx context.Context, ev evidence.Evidence) {
	if r.cfg.ExcludePrivateTrackerGrabs && isPrivateCategory(ev.Category) {
		r.metrics.Observation("alive", "skip_private")
		return
	}
	if err := r.store.MarkAlive(ctx, ev.InfoHash, ev.ObservedAt, "", AliveSourceArrWebhook); err != nil {
		r.logFailure("liveness mark alive (arr)", err, ev.InfoHash)
		r.metrics.Observation("alive", "error")
		return
	}
	r.metrics.Observation("alive", "upsert")
}

// isPrivateCategory mirrors the qB-category convention used
// elsewhere in this codebase: private/bitgrab are the private
// labels. Kept here rather than in the evidence package because the
// liveness module is the only place that interprets categories from
// *arr-routed evidence.
func isPrivateCategory(category string) bool {
	switch strings.ToLower(strings.TrimSpace(category)) {
	case "private", "bitgrab":
		return true
	}
	return false
}

// MarkAliveFromDHT is the entry point used by the revalidator
// worker. It is separate from HandleEvidence because revalidation
// does not flow through the evidence store — there is no Evidence
// row for "DHT confirmed peers exist".
func (r *Resolver) MarkAliveFromDHT(ctx context.Context, infoHash []byte) error {
	if !r.cfg.Enabled {
		return nil
	}
	if err := r.store.MarkAlive(ctx, infoHash, r.now(), "", AliveSourceDHTRevalidate); err != nil {
		return fmt.Errorf("liveness: mark alive from dht: %w", err)
	}
	r.metrics.Observation("alive", "dht_recovered")
	return nil
}

func (r *Resolver) logFailure(msg string, err error, infoHash []byte) {
	if r.logger == nil || errors.Is(err, context.Canceled) {
		return
	}
	r.logger.Warnw(msg, "err", err, "info_hash_len", len(infoHash))
}
