package classifierfx

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/evidence"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"go.uber.org/fx"
)

// New wires the classifier module. The Runner returned by
// classifier.New is wrapped with the canonical-label preemption
// decorator at PROVIDE time (not via fx.Decorate). Module-scoped
// fx.Decorate calls only affect that module's own consumers — sibling
// modules like processorfx see the un-decorated value, which silently
// bypasses canonical preempt for every classifier call. The 2026-04-24
// audit hit exactly that footgun: 200,340 torrents persisted but
// `bitagent_classifier_preempt_*` metrics all sat at 0.
//
// Provide-time wrapping side-steps fx scope. Consumers see a single
// `lazy.Lazy[classifier.Runner]` whose .Get() returns
// canonicalRunner(innerCEL) regardless of which module asks.
func New() fx.Option {
	return fx.Module(
		"workflow",
		configfx.NewConfigModule[classifier.Config]("classifier", classifier.NewDefaultConfig()),
		matchDecisionObserverOption(),
		fx.Provide(
			classifier.NewPreemptMetrics,
			provideRunner,
		),
		// Export the preempt metric collectors into the shared
		// prometheus_collectors group so /metrics surfaces them.
		fx.Provide(fx.Annotated{
			Group: "prometheus_collectors,flatten",
			Target: func(m *classifier.PreemptMetrics) []prometheus.Collector {
				return m.Collectors()
			},
		}),
	)
}

func matchDecisionObserverOption() fx.Option {
	return fx.Provide(provideMatchDecisionObserver)
}

type matchDecisionObserverParams struct {
	fx.In
	Capture llmcapture.Capturer `optional:"true"`
	Matcher *llmmatch.Client    `optional:"true"`
}

func provideMatchDecisionObserver(p matchDecisionObserverParams) classifier.MatchDecisionObserver {
	return classifier.NewMatchDecisionObserver(p.Capture, p.Matcher)
}

// provideRunner runs classifier.New and wraps the resulting lazy Runner
// in the canonical evidence decorator. Wrapping happens here (not in an
// fx.Decorate) so the wrapped lazy is the only `lazy.Lazy[Runner]`
// instance fx ever creates — every consumer, regardless of module
// boundary, gets the canonical-aware version.
func provideRunner(
	params classifier.Params,
	store *evidence.Store,
	metrics *classifier.PreemptMetrics,
) classifier.Result {
	res := classifier.New(params)
	var opts []classifier.CanonicalOption
	if params.Config.EvidenceTitleIdentity {
		opts = append(opts, classifier.WithTitleEvidence(classifier.NewTitleEvidence(store)))
	}
	res.Runner = WrapRunnerWithCanonical(res.Runner, store, metrics, opts...)
	return res
}

// WrapRunnerWithCanonical layers the canonical-evidence decorator over
// any inner Runner lazy. Exported (and accepting the narrower
// classifier.CanonicalStore interface rather than *evidence.Store) so
// tests can verify the wrapping pattern in isolation, without booting
// the full classifier graph (CEL compiler + source provider + TMDB
// client). The fx provider passes a real *evidence.Store, which
// satisfies the interface.
func WrapRunnerWithCanonical(
	inner lazy.Lazy[classifier.Runner],
	store classifier.CanonicalStore,
	metrics *classifier.PreemptMetrics,
	opts ...classifier.CanonicalOption,
) lazy.Lazy[classifier.Runner] {
	return lazy.New(func() (classifier.Runner, error) {
		r, err := inner.Get()
		if err != nil {
			return nil, err
		}
		return classifier.NewCanonicalRunner(r, store, metrics, opts...), nil
	})
}
