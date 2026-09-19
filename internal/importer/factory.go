package importer

import (
	"time"

	"github.com/spencercnorton/bitagent/internal/blocking"
	"github.com/spencercnorton/bitagent/internal/csamblocklist"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"go.uber.org/fx"
)

type Params struct {
	fx.In
	Dao lazy.Lazy[*dao.Query]
	// CsamBlocklist + BlockingManager are the /import gate (design §2.3).
	// Both are always-provided fx singletons — the SAME instances the
	// dhtcrawler uses (csam has a NoOp fallback when no feeds configured).
	// Required, not optional: the daemon must never wire an ungated
	// importer.
	CsamBlocklist   csamblocklist.Manager
	BlockingManager lazy.Lazy[blocking.Manager]
	Metrics         *Metrics
}

type Result struct {
	fx.Out
	Importer lazy.Lazy[Importer]
}

func New(p Params) Result {
	return Result{
		Importer: lazy.New(func() (Importer, error) {
			d, err := p.Dao.Get()
			if err != nil {
				return nil, err
			}
			bm, err := p.BlockingManager.Get()
			if err != nil {
				return nil, err
			}
			return importer{
				dao:             d,
				bufferSize:      100,
				maxWaitTime:     500 * time.Millisecond,
				csamBlocklist:   p.CsamBlocklist,
				blockingManager: bm,
				metrics:         p.Metrics,
			}, nil
		}),
	}
}
