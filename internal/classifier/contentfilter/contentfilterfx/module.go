package contentfilterfx

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"os"
	"strconv"
)

func New() fx.Option {
	return fx.Module("content_filter", configfx.NewConfigModule[contentfilter.Config]("content_filter", contentfilter.NewDefaultConfig()), fx.Provide(contentfilter.NewMetrics, provideFilter, fx.Annotated{Group: "prometheus_collectors,flatten", Target: func(m *contentfilter.Metrics) []prometheus.Collector { return m.Collectors() }}))
}

type filterParams struct {
	fx.In
	Config     contentfilter.Config
	NamePolicy *namepolicy.Policy `optional:"true"`
	Logger     *zap.SugaredLogger
}

func provideFilter(p filterParams) (*contentfilter.Filter, error) {
	f := contentfilter.New(applyDropLossyAudioOnlyBackCompat(p.Config, p.Logger))
	f.SetNamePolicy(p.NamePolicy)
	return f, nil
}
func applyDropLossyAudioOnlyBackCompat(cfg contentfilter.Config, logger *zap.SugaredLogger) contentfilter.Config {
	const (
		newKey = "CONTENT_FILTER_DROP_LOSSY_AUDIO_ONLY"
		oldKey = "CONTENT_FILTER_DROP_MP_3_ONLY"
	)
	if _, newSet := os.LookupEnv(newKey); newSet {
		return cfg // new var present — resolver already applied it; ignore legacy.
	}
	legacyVal, oldSet := os.LookupEnv(oldKey)
	if !oldSet {
		return cfg
	}
	parsed, err := strconv.ParseBool(legacyVal)
	if err != nil {
		logger.Named("contentfilter").Warnw(
			"deprecated env "+oldKey+" set to an unparseable bool; ignoring — "+
				"please migrate to "+newKey,
			"value", legacyVal)
		return cfg
	}
	cfg.DropLossyAudioOnly = parsed
	logger.Named("contentfilter").Warnw(
		"deprecated env "+oldKey+" is set; honoring it for backward compatibility. "+
			"Rename it to "+newKey+" — the old name will be removed in a future release.",
		"drop_lossy_audio_only", parsed)
	return cfg
}
