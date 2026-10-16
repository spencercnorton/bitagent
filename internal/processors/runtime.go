package processors

import (
	"context"
	"sync"
	"time"

	"github.com/spencercnorton/bitagent/internal/worker"
	"go.uber.org/fx"
)

// Worker never makes optional availability a core startup/readiness condition.
func (s *Store) Worker() worker.Worker {
	var cancel context.CancelFunc
	var wg sync.WaitGroup
	return worker.NewWorker("processors", fx.Hook{OnStart: func(context.Context) error {
		ctx, c := context.WithCancel(context.Background())
		cancel = c
		launch := func(interval time.Duration, f func(context.Context)) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				tick := time.NewTicker(interval)
				defer tick.Stop()
				for {
					f(ctx)
					select {
					case <-ctx.Done():
						return
					case <-tick.C:
					}
				}
			}()
		}
		launch(s.cfg.PollInterval, func(ctx context.Context) {
			if _, err := s.Scan(ctx); err != nil {
				s.metrics.admission.WithLabelValues("unavailable").Inc()
			}
		})
		launch(30*time.Second, func(ctx context.Context) { _ = s.Refresh(ctx); s.updateGauges(ctx) })
		for range s.cfg.Concurrency {
			launch(s.cfg.PollInterval, func(ctx context.Context) { _, _ = s.RunOne(ctx) })
		}
		return nil
	}, OnStop: func(ctx context.Context) error {
		if cancel == nil {
			return nil
		}
		cancel()
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
}
