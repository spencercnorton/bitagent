package cache

import (
	caches "github.com/mgdigital/gorm-cache/v2"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"go.uber.org/fx"
	"gorm.io/gorm"
)

type DecoratorParams struct {
	fx.In
	Plugin *caches.Caches
	DB     lazy.Lazy[*gorm.DB]
}

type DecoratorResult struct {
	fx.Out
	DB lazy.Lazy[*gorm.DB]
}

func NewDecorator(p DecoratorParams) DecoratorResult {
	return DecoratorResult{
		DB: lazy.New(func() (*gorm.DB, error) {
			db, err := p.DB.Get()
			if err != nil {
				return nil, err
			}
			if err := db.Use(p.Plugin); err != nil {
				return nil, err
			}
			return db, nil
		}),
	}
}
