package queue

import (
	"context"
	"encoding/json"
	"time"

	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/processor"
	"github.com/spencercnorton/bitagent/internal/queue/handler"
	"go.uber.org/fx"
)

type Params struct {
	fx.In
	Processor lazy.Lazy[processor.Processor]
}

type Result struct {
	fx.Out
	Handler lazy.Lazy[handler.Handler] `group:"queue_handlers"`
}

func New(p Params) Result {
	return Result{
		Handler: lazy.New(func() (handler.Handler, error) {
			pr, err := p.Processor.Get()
			if err != nil {
				return handler.Handler{}, err
			}
			return handler.New(
				processor.MessageName,
				func(ctx context.Context, job model.QueueJob) (err error) {
					msg := &processor.MessageParams{}
					if err := json.Unmarshal([]byte(job.Payload), msg); err != nil {
						return err
					}

					return pr.Process(ctx, *msg)
				},
				handler.JobTimeout(time.Second*60*10),
				handler.Concurrency(1),
			), nil
		}),
	}
}
