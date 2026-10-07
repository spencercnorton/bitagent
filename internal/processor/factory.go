package processor

import (
	"github.com/spencercnorton/bitagent/internal/blocking"
	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/csamblocklist"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/evidence"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
	"github.com/spencercnorton/bitagent/internal/verdicts"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type Params struct {
	fx.In
	ClassifierConfig classifier.Config
	NamePolicy       *namepolicy.Policy `optional:"true"`
	Search           lazy.Lazy[search.Search]
	Workflow         lazy.Lazy[classifier.Runner]
	Dao              lazy.Lazy[*dao.Query]
	BlockingManager  lazy.Lazy[blocking.Manager]
	CsamExporter     csamblocklist.Exporter
	// ContentFilter + ContentFilterMetrics are optional (`fx:"optional"`)
	// so a partial wiring (older deploys, narrow tests) doesn't need to
	// provide them. When nil the post-classifier hook in Process is a
	// no-op — the pre-classifier dhtcrawler hook keeps doing its job
	// independently.
	ContentFilter        *contentfilter.Filter  `optional:"true"`
	ContentFilterMetrics *contentfilter.Metrics `optional:"true"`
	// EvidenceStore exposes IsPrivateInfoHash, the privacy gate that
	// keeps the contentfilter post-classifier hook (and therefore
	// OpenAI) away from private-tracker content. Optional only so the
	// processor stays buildable in narrow test harnesses; production
	// always supplies a real store via evidencefx.
	EvidenceStore *evidence.Store `optional:"true"`
	// Verdicts is retained for wiring compatibility. Removal authority now
	// belongs to the atomic recovery writer, without a later blacklist write.
	Verdicts *verdicts.Store `optional:"true"`
	// DeleteMetrics counts classifier-driven deletes (nil-safe optional).
	DeleteMetrics *DeleteMetrics `optional:"true"`
	Logger        *zap.SugaredLogger
}

type Result struct {
	fx.Out
	Processor lazy.Lazy[Processor]
}

func New(p Params) Result {
	return Result{
		Processor: lazy.New(func() (Processor, error) {
			s, err := p.Search.Get()
			if err != nil {
				return nil, err
			}
			d, err := p.Dao.Get()
			if err != nil {
				return nil, err
			}
			bm, err := p.BlockingManager.Get()
			if err != nil {
				return nil, err
			}
			w, err := p.Workflow.Get()
			if err != nil {
				return nil, err
			}

			return processor{
				dao:             d,
				namePolicy:      p.NamePolicy,
				search:          s,
				blockingManager: bm,
				runner:          w,
				defaultWorkflow: p.ClassifierConfig.Workflow,
				deleteAuditBudget: newDeleteAuditBudget(
					p.ClassifierConfig.DeleteAuditSample,
					p.ClassifierConfig.DeleteAuditSampleMax,
					p.Logger,
				),
				csamExporter:         p.CsamExporter,
				contentFilter:        p.ContentFilter,
				contentFilterMetrics: p.ContentFilterMetrics,
				privacy:              p.EvidenceStore,
				verdicts:             p.Verdicts,
				deleteMetrics:        p.DeleteMetrics,
				logger:               p.Logger,
			}, nil
		}),
	}
}
