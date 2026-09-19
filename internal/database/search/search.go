package search

import (
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"go.uber.org/fx"
)

type Search interface {
	ContentSearch
	QueueJobSearch
	TorrentSearch
	TorrentContentSearch
	TorrentFilesSearch
}

type search struct {
	q *dao.Query
}

type Params struct {
	fx.In
	Query lazy.Lazy[*dao.Query]
}

type Result struct {
	fx.Out
	Search lazy.Lazy[Search]
}

func New(params Params) Result {
	return Result{
		Search: lazy.New(func() (Search, error) {
			q, err := params.Query.Get()
			if err != nil {
				return nil, err
			}
			return &search{
				q: q,
			}, nil
		}),
	}
}
