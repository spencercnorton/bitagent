package classifierfx

import (
	"context"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/evidence"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/fx"
)

// fakeStore satisfies classifier.CanonicalStore. Returns the configured
// label/err for every infohash; tests can flip the label between calls
// to drive each preempt branch.
type fakeStore struct {
	label *evidence.CanonicalLabel
	err   error
	calls int
}

func (s *fakeStore) CanonicalForInfoHash(_ context.Context, _ []byte) (*evidence.CanonicalLabel, error) {
	s.calls++
	return s.label, s.err
}

// recordedRunner is the inner Runner. Tests assert it gets called when
// canonical evidence misses or constrains movie/TV enrichment, while
// non-video type-only evidence can still short-circuit.
type recordedRunner struct{ called int }

func TestMatchDecisionObserverOptionWiresWithoutOptionalCaptureModules(t *testing.T) {
	var observer classifier.MatchDecisionObserver
	app := fx.New(
		matchDecisionObserverOption(),
		fx.Populate(&observer),
		fx.NopLogger,
	)
	require.NoError(t, app.Err())
	require.NotNil(t, observer)
	require.NoError(t, observer.ObserveLLMMatchDecision(
		context.Background(), classifier.MatchDecisionObservation{},
	))
}

func (r *recordedRunner) Run(_ context.Context, _ string, _ classifier.Flags, _ model.Torrent) (classification.Result, error) {
	r.called++
	return classification.Result{}, nil
}

func (r *recordedRunner) EvalMatch(_ context.Context, _ model.Torrent, _ model.NullContentType) (classifier.MatchDecision, error) {
	return classifier.MatchDecision{}, nil
}

// TestWrapRunnerWithCanonical_ConstrainsMovieHit guards the regression that
// the 2026-04-24 audit caught: in the deployed image the canonical
// decorator was wired via fx.Decorate inside classifierfx, which is
// module-scoped — sibling modules (notably processorfx) consumed the
// undecorated Runner. The result was 200,340 classified torrents with
// `bitagent_classifier_preempt_*` metrics frozen at zero.
//
// The fix moved the wrapping to provide-time via WrapRunnerWithCanonical.
// This test asserts that the wrapper:
//
//  1. produces a non-nil lazy
//  2. when resolved, returns a Runner that constrains movie/TV canonical hits
//     and delegates to normal identity enrichment
//  3. when no label exists, falls through (inner IS called).
//
// With the original module-scoped decorator, point (1) still worked but
// the assertion in this test would never run against the wired
// processor — the bug was scope, not logic. Keeping the test here
// pins the new wiring so a future "let's move this back to fx.Decorate"
// gets caught.
func TestWrapRunnerWithCanonical_ConstrainsMovieHit(t *testing.T) {
	store := &fakeStore{label: &evidence.CanonicalLabel{
		MediaType:      evidence.MediaTypeMovie,
		ResolvedSource: evidence.SourceRadarr,
	}}
	inner := &recordedRunner{}
	innerLazy := lazy.New(func() (classifier.Runner, error) { return inner, nil })

	wrapped := WrapRunnerWithCanonical(innerLazy, store, classifier.NewPreemptMetrics())
	r, err := wrapped.Get()
	require.NoError(t, err)
	require.NotNil(t, r)

	_, err = r.Run(context.Background(), "default", classifier.Flags{}, model.Torrent{})
	require.NoError(t, err)

	assert.Equal(t, 1, store.calls, "store must be consulted exactly once per Run")
	assert.Equal(t, 1, inner.called, "movie identity enrichment must delegate to inner")
}

func TestWrapRunnerWithCanonical_PreemptsNonVideoHit(t *testing.T) {
	store := &fakeStore{label: &evidence.CanonicalLabel{
		MediaType:      evidence.MediaTypeBook,
		ResolvedSource: evidence.SourceReadarr,
	}}
	inner := &recordedRunner{}
	innerLazy := lazy.New(func() (classifier.Runner, error) { return inner, nil })

	wrapped := WrapRunnerWithCanonical(innerLazy, store, classifier.NewPreemptMetrics())
	r, err := wrapped.Get()
	require.NoError(t, err)

	_, err = r.Run(context.Background(), "default", classifier.Flags{}, model.Torrent{})
	require.NoError(t, err)

	assert.Equal(t, 1, store.calls)
	assert.Equal(t, 0, inner.called, "non-video type-only evidence should still short-circuit")
}

func TestWrapRunnerWithCanonical_FallthroughOnMiss(t *testing.T) {
	store := &fakeStore{label: nil} // no canonical label found
	inner := &recordedRunner{}
	innerLazy := lazy.New(func() (classifier.Runner, error) { return inner, nil })

	wrapped := WrapRunnerWithCanonical(innerLazy, store, classifier.NewPreemptMetrics())
	r, err := wrapped.Get()
	require.NoError(t, err)

	_, err = r.Run(context.Background(), "default", classifier.Flags{}, model.Torrent{})
	require.NoError(t, err)

	assert.Equal(t, 1, store.calls)
	assert.Equal(t, 1, inner.called, "miss must fall through to the inner CEL runner")
}

// TestWrapRunnerWithCanonical_LazyShape verifies the wrapper preserves
// the lazy contract — the inner is only invoked when .Get() is called,
// not at wrap time. fx wiring relies on this; eager evaluation would
// make the classifier boot far more expensive.
func TestWrapRunnerWithCanonical_LazyShape(t *testing.T) {
	innerInvocations := 0
	innerLazy := lazy.New(func() (classifier.Runner, error) {
		innerInvocations++
		return &recordedRunner{}, nil
	})

	_ = WrapRunnerWithCanonical(innerLazy, &fakeStore{}, classifier.NewPreemptMetrics())
	assert.Equal(t, 0, innerInvocations,
		"WrapRunnerWithCanonical must not eagerly resolve the inner lazy")
}
