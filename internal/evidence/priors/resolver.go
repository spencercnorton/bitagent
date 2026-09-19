package priors

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spencercnorton/bitagent/internal/evidence"
	"go.uber.org/zap"
)

// store is the resolver's dependency on the persistence layer. The
// interface lets tests drive the resolver with an in-memory fake.
type store interface {
	RecordGrab(ctx context.Context, ev GrabAttempt) (int64, error)
	ResolvePendingAndIncrement(
		ctx context.Context, infoHash []byte, outcome Outcome, resolvedAt time.Time,
	) ([]GrabAttempt, []FeatureKey, error)
	DeletePendingByInfoHash(ctx context.Context, infoHash []byte) (int64, error)
	IncrementPriors(ctx context.Context, successKeys []FeatureKey, failureKeys []FeatureKey) error
}

// metricsSink is the subset of *Metrics the resolver writes to.
type metricsSink interface {
	Observation(event, outcome string)
	Outcome(class string)
}

// SourceLookup is the optional dependency used to enrich a grab with
// indexer/source information at the time of the grab. The fork's
// canonical-label projection has already produced a torrent_canonical_labels
// row at the time the priors resolver fires (it runs after the
// evidence Insert commits), but the source list lives in
// torrents_torrent_sources and may not yet be populated for a hash
// the *arr only just discovered. Implementations that can look
// sources up should return them; implementations that can't return
// nil and the resolver falls back to feature extraction from title
// alone.
type SourceLookup interface {
	SourcesForInfoHash(ctx context.Context, infoHash []byte) ([]string, string, error)
}

// Resolver applies the priors state machine to incoming evidence.
// One Resolver per process; HandleEvidence is safe for concurrent
// callers because the underlying store batches its own transactions.
type Resolver struct {
	store    store
	sources  SourceLookup
	cfg      evidence.OutcomePriorsConfig
	metrics  metricsSink
	logger   *zap.SugaredLogger
	now      func() time.Time
	resolveT time.Duration
}

// NewResolver constructs a resolver. now is injectable for
// deterministic tests; production callers pass time.Now.
func NewResolver(
	s store,
	sources SourceLookup,
	cfg evidence.OutcomePriorsConfig,
	metrics metricsSink,
	logger *zap.SugaredLogger,
) *Resolver {
	return &Resolver{
		store:    s,
		sources:  sources,
		cfg:      cfg,
		metrics:  metrics,
		logger:   logger,
		now:      time.Now,
		resolveT: 5 * time.Second,
	}
}

// HandleEvidence is invoked by the evidence store on every successful
// Insert. Dispatches by Kind:
//
//   - KindWebhookGrab → record a pending torrent_grab_attempts row.
//   - KindWebhookImport → resolve any pending rows for the same
//     infohash as success and increment α priors.
//
// Other kinds are ignored. Errors are logged and counted but never
// returned — the resolver is a derived projection that must not block
// ingest.
func (r *Resolver) HandleEvidence(ctx context.Context, ev evidence.Evidence) {
	if !r.cfg.Enabled {
		return
	}
	if len(ev.InfoHash) == 0 {
		return
	}
	switch ev.Kind {
	case evidence.KindWebhookGrab:
		r.handleGrab(ctx, ev)
	case evidence.KindWebhookImport:
		// Mirror liveness's private-tracker filter: a successful
		// import that came from a private tracker proves nothing
		// about whether the same hash succeeds via the public
		// catalog. We don't update priors on private grabs.
		if r.cfg.ExcludePrivateTrackerGrabs && isPrivateCategory(ev.Category) {
			// A pending grab row may exist from an earlier
			// KindWebhookGrab on the same hash. Leaving it pending
			// means the expirer eventually marks it failure — turning
			// a successful private import into a false-negative β
			// signal. Delete the pending rows instead so neither side
			// of the prior moves.
			r.handlePrivateImport(ctx, ev)
			return
		}
		r.handleImport(ctx, ev)
	}
}

func (r *Resolver) handleGrab(ctx context.Context, ev evidence.Evidence) {
	cctx, cancel := context.WithTimeout(ctx, r.resolveT)
	defer cancel()

	sources, primaryExt := r.lookupSources(cctx, ev.InfoHash)
	features := Extract(ev.Title, sources, primaryExt)

	if _, err := r.store.RecordGrab(cctx, GrabAttempt{
		InfoHash:       ev.InfoHash,
		Source:         string(ev.Source),
		SourceInstance: ev.SourceInstance,
		ReleaseTitle:   ev.Title,
		Features:       features,
		GrabbedAt:      ev.ObservedAt,
	}); err != nil {
		r.logFailure("priors record grab", err, ev.InfoHash)
		r.metrics.Observation("grab_recorded", "error")
		return
	}
	r.metrics.Observation("grab_recorded", "ok")
}

func (r *Resolver) handleImport(ctx context.Context, ev evidence.Evidence) {
	cctx, cancel := context.WithTimeout(ctx, r.resolveT)
	defer cancel()

	// Resolve pending rows AND increment α priors in one transaction.
	// If the priors update fails, the resolution rolls back so the
	// next equivalent event (or operator-driven retry) can recover —
	// rather than the previous behaviour where a transient failure on
	// the second statement permanently lost the success signal.
	resolved, keys, err := r.store.ResolvePendingAndIncrement(
		cctx, ev.InfoHash, OutcomeSuccess, r.now(),
	)
	if err != nil {
		r.logFailure("priors resolve+increment import", err, ev.InfoHash)
		r.metrics.Observation("import_resolved", "error")
		return
	}
	if len(resolved) == 0 {
		// Imports without a prior Grab are common (recorded a
		// successful import after we restarted; the bot started a
		// download outside *arr; the *arr is freshly bootstrapped
		// against an existing library). Count, but no priors update —
		// we do not have a frozen feature set to apply.
		r.metrics.Observation("import_resolved", "no_pending")
		return
	}
	if len(keys) == 0 {
		r.metrics.Observation("import_resolved", "no_features")
		return
	}
	for range resolved {
		r.metrics.Outcome("success")
	}
	r.metrics.Observation("import_resolved", "ok")
}

// handlePrivateImport drops any pending grab attempts for the
// imported infohash without updating priors. The CountPending gauge
// then reflects only attempts the resolver might still have a
// legitimate signal for, and the expirer cannot mis-attribute the
// pending row as a public-tracker failure.
func (r *Resolver) handlePrivateImport(ctx context.Context, ev evidence.Evidence) {
	cctx, cancel := context.WithTimeout(ctx, r.resolveT)
	defer cancel()
	deleted, err := r.store.DeletePendingByInfoHash(cctx, ev.InfoHash)
	if err != nil {
		r.logFailure("priors drop pending on private import", err, ev.InfoHash)
		r.metrics.Observation("import_resolved", "skip_private_error")
		return
	}
	if deleted > 0 {
		r.metrics.Observation("import_resolved", "skip_private_dropped")
		return
	}
	r.metrics.Observation("import_resolved", "skip_private")
}

func (r *Resolver) lookupSources(ctx context.Context, infoHash []byte) ([]string, string) {
	if r.sources == nil {
		return nil, ""
	}
	srcs, ext, err := r.sources.SourcesForInfoHash(ctx, infoHash)
	if err != nil {
		// Source lookup failure must not abort grab recording —
		// extract features from the title alone and continue.
		r.metrics.Observation("lookup_error", "ok")
		return nil, ""
	}
	return srcs, ext
}

// ApplyExpired increments β-side priors for a batch of attempts the
// expirer just marked failure. Separated from HandleEvidence so the
// expirer worker drives this path without going through the
// evidence-store hook.
func (r *Resolver) ApplyExpired(ctx context.Context, expired []GrabAttempt) {
	if !r.cfg.Enabled {
		return
	}
	failureKeys := mergeFeatures(expired)
	if len(failureKeys) == 0 {
		return
	}
	if err := r.store.IncrementPriors(ctx, nil, failureKeys); err != nil {
		r.logFailure("priors increment failure", err, nil)
		r.metrics.Observation("expirer_resolved", "error")
		return
	}
	for range expired {
		r.metrics.Outcome("failure")
	}
	r.metrics.Observation("expirer_resolved", "ok")
}

func (r *Resolver) logFailure(msg string, err error, infoHash []byte) {
	if r.logger == nil || errors.Is(err, context.Canceled) {
		return
	}
	if len(infoHash) > 0 {
		r.logger.Warnw(msg, "err", err, "info_hash_len", len(infoHash))
	} else {
		r.logger.Warnw(msg, "err", err)
	}
}

// mergeFeatures coalesces feature lists from multiple GrabAttempts
// into a deduplicated slice. Each (key_type, key_value) pair appears
// at most once in the result regardless of how many attempts carried
// it, so a single import event with two pending grabs increments α
// once per unique feature.
func mergeFeatures(attempts []GrabAttempt) []FeatureKey {
	if len(attempts) == 0 {
		return nil
	}
	seen := make(map[FeatureKey]struct{})
	out := make([]FeatureKey, 0, len(attempts)*4)
	for _, a := range attempts {
		for _, k := range a.Features {
			if _, ok := seen[k]; ok {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, k)
		}
	}
	return out
}

// isPrivateCategory mirrors liveness's filter — kept here rather than
// shared so the two modules don't grow a coupling that the next
// reader of either has to chase.
func isPrivateCategory(category string) bool {
	switch strings.ToLower(strings.TrimSpace(category)) {
	case "private", "bitgrab":
		return true
	}
	return false
}

// validateConfig is a startup helper used by the fx module to surface
// misconfiguration as a hard error rather than a silent fallthrough.
func validateConfig(cfg evidence.OutcomePriorsConfig) error {
	if !cfg.Enabled {
		return nil
	}
	if cfg.ResolutionWindow <= 0 {
		return fmt.Errorf("priors: ResolutionWindow must be positive when enabled, got %s", cfg.ResolutionWindow)
	}
	if cfg.ExpirerInterval <= 0 {
		return fmt.Errorf("priors: ExpirerInterval must be positive when enabled, got %s", cfg.ExpirerInterval)
	}
	if cfg.ExpirerInterval > cfg.ResolutionWindow {
		return fmt.Errorf("priors: ExpirerInterval (%s) must not exceed ResolutionWindow (%s)",
			cfg.ExpirerInterval, cfg.ResolutionWindow)
	}
	if cfg.MinObservations < 0 {
		return fmt.Errorf("priors: MinObservations must be non-negative, got %d", cfg.MinObservations)
	}
	return nil
}
