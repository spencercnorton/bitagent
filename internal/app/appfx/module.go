package appfx

import (
	"github.com/spencercnorton/bitagent/internal/animedb/animedbfx"
	"github.com/spencercnorton/bitagent/internal/app/cli"
	"github.com/spencercnorton/bitagent/internal/app/cli/args"
	"github.com/spencercnorton/bitagent/internal/app/cli/hooks"
	"github.com/spencercnorton/bitagent/internal/app/cmd/animebackfillcmd"
	"github.com/spencercnorton/bitagent/internal/app/cmd/attributioncmd"
	"github.com/spencercnorton/bitagent/internal/app/cmd/backloglocalclassifycmd"
	"github.com/spencercnorton/bitagent/internal/app/cmd/batchllmmatchcmd"
	"github.com/spencercnorton/bitagent/internal/app/cmd/classifiercmd"
	"github.com/spencercnorton/bitagent/internal/app/cmd/configcmd"
	"github.com/spencercnorton/bitagent/internal/app/cmd/episodesbackfillcmd"
	"github.com/spencercnorton/bitagent/internal/app/cmd/evalfreezecmd"
	"github.com/spencercnorton/bitagent/internal/app/cmd/evalreplaycmd"
	"github.com/spencercnorton/bitagent/internal/app/cmd/granularitybackfillcmd"
	"github.com/spencercnorton/bitagent/internal/app/cmd/matcherevalcmd"
	"github.com/spencercnorton/bitagent/internal/app/cmd/processcmd"
	"github.com/spencercnorton/bitagent/internal/app/cmd/purgecontenttypescmd"
	"github.com/spencercnorton/bitagent/internal/app/cmd/refreshalttitlescmd"
	"github.com/spencercnorton/bitagent/internal/app/cmd/refreshanimetitlescmd"
	"github.com/spencercnorton/bitagent/internal/app/cmd/refreshseedscmd"
	"github.com/spencercnorton/bitagent/internal/app/cmd/reprocesscmd"
	"github.com/spencercnorton/bitagent/internal/app/cmd/workercmd"
	"github.com/spencercnorton/bitagent/internal/attribution"
	"github.com/spencercnorton/bitagent/internal/blocking/blockingfx"
	"github.com/spencercnorton/bitagent/internal/classifier/classifierfx"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter/contentfilterfx"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch/llmmatchfx"
	"github.com/spencercnorton/bitagent/internal/classifier/llmstage/llmstagefx"
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/csamblocklist/csamblocklistfx"
	"github.com/spencercnorton/bitagent/internal/dashstats/dashstatsfx"
	"github.com/spencercnorton/bitagent/internal/database/databasefx"
	"github.com/spencercnorton/bitagent/internal/database/migrations"
	"github.com/spencercnorton/bitagent/internal/dhtcrawler/dhtcrawlerfx"
	"github.com/spencercnorton/bitagent/internal/evidence/evidencefx"
	"github.com/spencercnorton/bitagent/internal/gql/gqlfx"
	"github.com/spencercnorton/bitagent/internal/health/healthfx"
	"github.com/spencercnorton/bitagent/internal/httpserver/httpserverfx"
	"github.com/spencercnorton/bitagent/internal/importer/importerfx"
	"github.com/spencercnorton/bitagent/internal/junkpurge/junkpurgefx"
	"github.com/spencercnorton/bitagent/internal/llmcapture/llmcapturefx"
	"github.com/spencercnorton/bitagent/internal/logging/loggingfx"
	"github.com/spencercnorton/bitagent/internal/metrics/metricsfx"
	"github.com/spencercnorton/bitagent/internal/processor/processorfx"
	"github.com/spencercnorton/bitagent/internal/protocol/dht/dhtfx"
	"github.com/spencercnorton/bitagent/internal/protocol/metainfo/metainfofx"
	"github.com/spencercnorton/bitagent/internal/queue/queuefx"
	"github.com/spencercnorton/bitagent/internal/queueclean/queuecleanfx"
	"github.com/spencercnorton/bitagent/internal/retention/retentionfx"
	"github.com/spencercnorton/bitagent/internal/seeds/seedsfx"
	"github.com/spencercnorton/bitagent/internal/telemetry/telemetryfx"
	"github.com/spencercnorton/bitagent/internal/tmdb/tmdbfx"
	"github.com/spencercnorton/bitagent/internal/torznab/torznabfx"
	"github.com/spencercnorton/bitagent/internal/validation/validationfx"
	"github.com/spencercnorton/bitagent/internal/verdicts/verdictsfx"
	"github.com/spencercnorton/bitagent/internal/version/versionfx"
	"github.com/spencercnorton/bitagent/internal/wantbridge/wantbridgefx"
	"github.com/spencercnorton/bitagent/internal/worker/workerfx"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"app",
		animedbfx.New(),
		blockingfx.New(),
		classifierfx.New(),
		contentfilterfx.New(),
		llmstagefx.New(),
		llmmatchfx.New(),
		configfx.New(),
		csamblocklistfx.New(),
		dhtcrawlerfx.New(),
		dhtfx.New(),
		databasefx.New(),
		dashstatsfx.New(),
		evidencefx.New(),
		gqlfx.New(),
		healthfx.New(),
		httpserverfx.New(),
		importerfx.New(),
		loggingfx.New(),
		llmcapturefx.New(),
		metainfofx.New(),
		metricsfx.New(),
		processorfx.New(),
		queuefx.New(),
		queuecleanfx.New(),
		junkpurgefx.New(),
		verdictsfx.New(),
		retentionfx.New(),
		seedsfx.New(),
		telemetryfx.New(),
		tmdbfx.New(),
		torznabfx.New(),
		validationfx.New(),
		versionfx.New(),
		wantbridgefx.New(),
		workerfx.New(),
		// Attribution config — registered here rather than its own
		// fx module because attribution doesn't need a worker /
		// background loop yet (one-shot CLI only). When that
		// changes, promote to attributionfx.New().
		configfx.NewConfigModule[attribution.Config]("attribution", attribution.NewDefaultConfig()),
		fx.Provide(
			args.New,
			cli.New,
			hooks.New,
			// cli commands:
			animebackfillcmd.New,
			attributioncmd.New,
			backloglocalclassifycmd.New,
			batchllmmatchcmd.New,
			classifiercmd.New,
			episodesbackfillcmd.New,
			granularitybackfillcmd.New,
			configcmd.New,
			evalfreezecmd.New,
			evalreplaycmd.New,
			matcherevalcmd.New,
			purgecontenttypescmd.New,
			refreshalttitlescmd.New,
			refreshanimetitlescmd.New,
			refreshseedscmd.New,
			reprocesscmd.New,
			processcmd.New,
			workercmd.New,
		),
		fx.Decorate(migrations.NewDecorator),
		// Parent-scoped LLM-stage decorator. Module-scoped fx.Decorate
		// inside llmstagefx only affected llmstagefx's own consumers,
		// silently bypassing the LLM stage for processorfx (where it
		// matters). Provide-time wrapping handled the canonical preempt
		// in classifierfx; here we layer the LLM stage at parent scope
		// so EVERY consumer of `lazy.Lazy[classifier.Runner]` sees:
		//
		//   processor → llmStage(canonicalPreempt(celRunner))
		//
		// regardless of which module asks for the dependency.
		fx.Decorate(llmstagefx.WrapWithLLMStage),
	)
}
