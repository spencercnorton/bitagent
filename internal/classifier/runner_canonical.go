package classifier

import (
	"context"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/evaltrace"
	"github.com/spencercnorton/bitagent/internal/evidence"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
)

// CanonicalStore is the minimum subset of evidence.Store the
// preemption decorator needs. Keeping it as a local interface lets us
// avoid a hard dependency on the evidence package's concrete Store
// in tests, and makes it obvious the decorator is read-only against
// evidence state.
type CanonicalStore interface {
	CanonicalForInfoHash(ctx context.Context, infoHash []byte) (*evidence.CanonicalLabel, error)
}

// NewCanonicalRunner wraps inner with a canonical-evidence check. Before
// delegating to inner, it consults the canonical store for an authoritative
// label on the torrent's infohash. Type-only content can still preempt the
// workflow, but movie/TV labels constrain the hint and continue through the
// normal enrichment path. This preserves an existing compatible identity and
// lets resolvable TMDB/TVDB/IMDb evidence attach exact catalog content instead
// of replacing it with a type-only result.
//
// If the canonical lookup fails (context cancelled, DB unreachable)
// the decorator falls through to inner. Availability of classification
// dominates; a canonical-label miss is not a reason to fail the torrent.
func NewCanonicalRunner(inner Runner, store CanonicalStore, metrics *PreemptMetrics, opts ...CanonicalOption) Runner {
	r := &canonicalRunner{inner: inner, store: store, metrics: metrics}
	for _, opt := range opts {
		opt(r)
	}

	return r
}

// CanonicalOption configures optional canonical-runner behaviour.
type CanonicalOption func(*canonicalRunner)

// WithTitleEvidence lets *arr labels on other torrents with the same parsed
// title choose the identity of a torrent that has no label of its own
// (CLASSIFIER_EVIDENCE_TITLE_IDENTITY). Nil leaves the runner unchanged.
func WithTitleEvidence(e *TitleEvidence) CanonicalOption {
	return func(r *canonicalRunner) { r.titles = e }
}

type canonicalRunner struct {
	inner   Runner
	store   CanonicalStore
	metrics *PreemptMetrics
	titles  *TitleEvidence // nil unless CLASSIFIER_EVIDENCE_TITLE_IDENTITY
}

func (r *canonicalRunner) Run(
	ctx context.Context,
	workflow string,
	flags Flags,
	t model.Torrent,
) (classification.Result, error) {
	if evaltrace.SkipPreempt(ctx) {
		// Replay skips the exact-infohash preempt because a torrent's own label
		// would copy the answer back. Title evidence never counts a torrent's own
		// label, so it runs — and the benchmark can measure it.
		return r.inner.Run(ctx, workflow, flags, r.withTitleEvidence(ctx, t))
	}

	label, err := r.store.CanonicalForInfoHash(ctx, t.InfoHash.Bytes())
	switch {
	case err != nil:
		r.metrics.lookupErrors.Inc()
	case label != nil:
		contentType, ok := canonicalToContentType(label.MediaType)
		if ok {
			if canonicalNeedsIdentityEnrichment(contentType) {
				r.metrics.constrainedTotal.WithLabelValues(
					string(label.ResolvedSource), string(label.MediaType),
				).Inc()
				evaltrace.Record(ctx, "canonical_constrain")

				return r.inner.Run(ctx, workflow, flags, applyCanonicalHint(t, label, contentType))
			}
			r.metrics.preemptedTotal.WithLabelValues(
				string(label.ResolvedSource), string(label.MediaType),
			).Inc()
			evaltrace.Record(ctx, "canonical_preempt")
			return buildCanonicalResult(label, contentType), nil
		}
		// Unknown or empty media type — fall through to classifier.
		r.metrics.unknownMediaType.WithLabelValues(string(label.ResolvedSource)).Inc()
	default:
		r.metrics.missesTotal.Inc()
	}
	return r.inner.Run(ctx, workflow, flags, r.withTitleEvidence(ctx, t))
}

// withTitleEvidence hints the identity the *arrs agree on for this torrent's
// title, through the same hint the canonical constrain path uses, so the
// workflow's attach-by-id actions resolve it (tvdb: included).
func (r *canonicalRunner) withTitleEvidence(ctx context.Context, t model.Torrent) model.Torrent {
	if r.titles == nil {
		return t
	}
	label, outcome := r.titles.Preferred(ctx, t)
	r.metrics.evidenceTitle.WithLabelValues(string(outcome)).Inc()
	if outcome != titleApplied {
		return t
	}
	contentType, ok := canonicalToContentType(label.MediaType)
	if !ok {
		return t
	}
	evaltrace.Record(ctx, "evidence_title")

	return applyCanonicalHint(t, label, contentType)
}

func canonicalNeedsIdentityEnrichment(contentType model.ContentType) bool {
	return contentType == model.ContentTypeMovie || contentType == model.ContentTypeTvShow
}

// applyCanonicalHint makes the evidence's media type authoritative while
// retaining a compatible existing identity when the label carries only an
// *arr-local ID (radarr:/sonarr:) or no ID. Catalog-resolvable identifiers
// replace the old hint so the normal local/TMDB ID actions can resolve them.
// A cross-type prior hint is never allowed to survive the canonical type.
func applyCanonicalHint(
	t model.Torrent,
	label *evidence.CanonicalLabel,
	contentType model.ContentType,
) model.Torrent {
	existingRef, hadCompatibleIdentity := compatibleContentRef(t, contentType)

	t.Hint.ContentType = contentType
	if ref, ok := canonicalContentRef(label.MediaID, contentType); ok {
		t.Hint.ContentSource = model.NewNullString(ref.Source)
		t.Hint.ContentID = model.NewNullString(ref.ID)

		return t
	}

	if !hadCompatibleIdentity {
		t.Hint.ContentSource = model.NullString{}
		t.Hint.ContentID = model.NullString{}
	} else {
		t.Hint.ContentSource = model.NewNullString(existingRef.Source)
		t.Hint.ContentID = model.NewNullString(existingRef.ID)
	}

	return t
}

// compatibleContentRef returns a single compatible catalog identity from the
// current hint or hydrated TorrentContent rows. Looking at Contents is
// essential for ClassifyModeRematch, where processor deliberately does not
// synthesize an identity hint. Multiple different identities are ambiguous and
// therefore not selected implicitly.
func compatibleContentRef(t model.Torrent, contentType model.ContentType) (model.ContentRef, bool) {
	if !t.Hint.IsNil() &&
		t.Hint.ContentType == contentType &&
		t.Hint.ContentSource.Valid &&
		t.Hint.ContentID.Valid &&
		t.Hint.ContentSource.String != "" &&
		t.Hint.ContentID.String != "" {
		return model.ContentRef{
			Type:   contentType,
			Source: t.Hint.ContentSource.String,
			ID:     t.Hint.ContentID.String,
		}, true
	}

	var found model.ContentRef
	for _, tc := range t.Contents {
		if !tc.ContentType.Valid || tc.ContentType.ContentType != contentType ||
			!tc.ContentSource.Valid || !tc.ContentID.Valid ||
			tc.ContentSource.String == "" || tc.ContentID.String == "" {
			continue
		}
		candidate := model.ContentRef{
			Type:   contentType,
			Source: tc.ContentSource.String,
			ID:     tc.ContentID.String,
		}
		if found.ID == "" {
			found = candidate

			continue
		}
		if found != candidate {
			return model.ContentRef{}, false
		}
	}

	return found, found.ID != ""
}

// canonicalContentRef accepts only identifier namespaces understood by the
// local content index and TMDB crosswalk. radarr:/sonarr: IDs identify rows in
// those applications, not catalog entities, and must not overwrite a known
// TMDB/TVDB hint.
func canonicalContentRef(mediaID string, contentType model.ContentType) (model.ContentRef, bool) {
	source, id, ok := strings.Cut(strings.TrimSpace(mediaID), ":")
	source = strings.ToLower(strings.TrimSpace(source))
	id = strings.TrimSpace(id)
	if !ok || id == "" {
		return model.ContentRef{}, false
	}

	switch source {
	case model.SourceTmdb, model.SourceTvdb, model.SourceImdb:
		return model.ContentRef{Type: contentType, Source: source, ID: id}, true
	default:
		return model.ContentRef{}, false
	}
}

// EvalMatch delegates to the inner runner. The canonical preemption check is a
// pre-classification shortcut that has no bearing on the matcher-only eval
// path (which operates on already-typed torrents).
func (r *canonicalRunner) EvalMatch(ctx context.Context, t model.Torrent, ct model.NullContentType) (MatchDecision, error) {
	return r.inner.EvalMatch(ctx, t, ct)
}

// canonicalToContentType maps evidence's media vocabulary to
// bitmagnet's existing ContentType enum. Returns ok=false when the
// mapping is not decisive, in which case the classifier should run.
func canonicalToContentType(mt evidence.MediaType) (model.ContentType, bool) {
	switch mt {
	case evidence.MediaTypeMovie:
		return model.ContentTypeMovie, true
	case evidence.MediaTypeTV:
		return model.ContentTypeTvShow, true
	case evidence.MediaTypeMusic:
		return model.ContentTypeMusic, true
	case evidence.MediaTypeAudiobook:
		return model.ContentTypeAudiobook, true
	case evidence.MediaTypeBook:
		return model.ContentTypeEbook, true
	}
	return "", false
}

// buildCanonicalResult materializes a classification.Result from a
// canonical label. It does NOT touch TMDB or the local content index —
// those are enrichment paths the classifier owns. The resolved
// content_id can be looked up later by reprocessing if needed.
func buildCanonicalResult(label *evidence.CanonicalLabel, contentType model.ContentType) classification.Result {
	var result classification.Result
	result.ContentType = model.NewNullContentType(contentType)
	return result
}

// PreemptMetrics covers how often canonical preemption saves a
// classifier run vs how often it misses.
type PreemptMetrics struct {
	preemptedTotal   *dualemit.CounterVec
	constrainedTotal *dualemit.CounterVec
	missesTotal      *dualemit.Counter
	lookupErrors     *dualemit.Counter
	unknownMediaType *dualemit.CounterVec
	evidenceTitle    *dualemit.CounterVec
}

// NewPreemptMetrics constructs the metric set.
func NewPreemptMetrics() *PreemptMetrics {
	const (
		namespace = "bitagent"
		subsystem = "classifier_preempt"
	)
	return &PreemptMetrics{
		preemptedTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "preempted_total",
			Help: "Classifier runs skipped because a canonical label exists; labelled by source and media_type.",
		}, []string{"source", "media_type"}),
		constrainedTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "constrained_total",
			Help: "Classifier runs whose type or identity hint was constrained by canonical evidence before normal enrichment.",
		}, []string{"source", "media_type"}),
		missesTotal: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "misses_total",
			Help: "Classifier runs that proceeded because no canonical label was present. " +
				"For freshly-DHT-discovered torrents this is the EXPECTED steady-state — " +
				"no upstream qB/*arr observation has yet labelled the hash. The preempt " +
				"path only short-circuits work on REPROCESS of previously-grabbed torrents. " +
				"A near-zero preempted_total / misses_total ratio is normal on a DHT-heavy " +
				"workload; it does not indicate a cache miss or efficiency leak.",
		}),
		lookupErrors: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "lookup_errors_total",
			Help: "Canonical-label lookups that failed; decorator falls through to inner classifier.",
		}),
		unknownMediaType: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "unknown_media_type_total",
			Help: "Canonical labels found but with a media_type the classifier cannot short-circuit; classifier still runs.",
		}, []string{"source"}),
		evidenceTitle: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "evidence_title_total",
			Help: "Title-level *arr evidence for torrents with no label of their own: " +
				"applied (identity hinted) | weak (under 2 votes or no 2/3 majority) | " +
				"no_preference | no_key | no_index.",
		}, []string{"outcome"}),
	}
}

// Collectors returns the preempt metric collectors for registration
// into the shared Prometheus collector group.
func (m *PreemptMetrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.preemptedTotal,
		m.constrainedTotal,
		m.missesTotal,
		m.lookupErrors,
		m.unknownMediaType,
		m.evidenceTitle,
	}
}
