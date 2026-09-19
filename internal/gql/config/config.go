package config

import (
	"github.com/spencercnorton/bitagent/internal/gql"
	"github.com/spencercnorton/bitagent/internal/gql/resolvers"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"go.uber.org/fx"
)

type Params struct {
	fx.In
	ResolverRoot lazy.Lazy[*resolvers.Resolver]
}

func New(p Params) lazy.Lazy[gql.Config] {
	return lazy.New(func() (gql.Config, error) {
		root, err := p.ResolverRoot.Get()
		if err != nil {
			return gql.Config{}, err
		}

		return gql.Config{Resolvers: root}, nil
	})
}
