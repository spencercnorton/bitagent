package tmdbhealthcheck

import (
	"context"
	"time"

	"github.com/spencercnorton/bitagent/internal/health"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/tmdb"
)

func NewCheck(
	enabled bool,
	client lazy.Lazy[tmdb.Client],
) health.Check {
	return health.Check{
		Name:    "tmdb",
		Timeout: time.Second * 30,
		IsActive: func() bool {
			return enabled
		},
		Check: func(ctx context.Context) error {
			c, err := client.Get()
			if err != nil {
				return err
			}
			return c.ValidateAPIKey(ctx)
		},
	}
}
