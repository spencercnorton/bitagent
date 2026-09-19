package search

import (
	"context"

	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/database/query"
	"github.com/spencercnorton/bitagent/internal/model"
)

type QueueJobResult = query.GenericResult[model.QueueJob]

type QueueJobSearch interface {
	QueueJobs(ctx context.Context, options ...query.Option) (result QueueJobResult, err error)
}

func (s search) QueueJobs(ctx context.Context, options ...query.Option) (result QueueJobResult, err error) {
	return query.GenericQuery[model.QueueJob](
		ctx,
		s.q,
		query.Options(append([]query.Option{query.SelectAll()}, options...)...),
		model.TableNameQueueJob,
		func(ctx context.Context, q *dao.Query) query.SubQuery {
			return query.GenericSubQuery[dao.IQueueJobDo]{
				SubQuery: q.QueueJob.WithContext(ctx).ReadDB(),
			}
		},
	)
}
