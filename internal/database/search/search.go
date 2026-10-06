package search

import (
	"fmt"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/serving"
	"go.uber.org/fx"
)

type Search interface {
	ContentSearch
	QueueJobSearch
	TorrentSearch
	TorrentContentSearch
	TorrentFilesSearch
}

// ServingSearch is the mandatory consumer view; internal processing retains Search.
type ServingSearch interface{ Search }

type search struct {
	q           *dao.Query
	adultPolicy *serving.Policy
}

type Params struct {
	fx.In
	Query         lazy.Lazy[*dao.Query]
	ServingPolicy *serving.Policy
}

type Result struct {
	fx.Out
	Search        lazy.Lazy[Search]
	ServingSearch lazy.Lazy[ServingSearch]
}

func New(params Params) Result {
	return Result{
		ServingSearch: lazy.New(func() (ServingSearch, error) {
			if params.ServingPolicy == nil {
				return nil, fmt.Errorf("consumer serving policy is required")
			}
			q, err := params.Query.Get()
			if err != nil {
				return nil, err
			}
			return &search{q: q, adultPolicy: params.ServingPolicy}, nil
		}),
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
