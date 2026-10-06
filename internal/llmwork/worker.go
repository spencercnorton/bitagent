package llmwork

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Worker runs one bounded source case at a time. It is independent of normal
// ingestion and shuts down by canceling the owned execution, retaining its
// conservative lifecycle outcome through Engine.RunOne.
type Worker struct {
	engine Engine
	logger *zap.SugaredLogger
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

func NewWorker(store *Store, handler Handler, logger *zap.SugaredLogger) (*Worker, error) {
	if store == nil {
		return nil, fmt.Errorf("optional model worker requires a task store")
	}
	if err := store.cfg.Validate(); err != nil {
		return nil, err
	}
	if store.Enabled() && store.cfg.WorkerEnabled && handler == nil {
		return nil, fmt.Errorf("optional model worker requires a narrow application handler")
	}
	var owner [16]byte
	if _, err := rand.Read(owner[:]); err != nil {
		return nil, err
	}
	return &Worker{engine: Engine{Store: store, Backend: store, Handler: handler, Config: store.cfg, Owner: hex.EncodeToString(owner[:])}, logger: logger.Named("llm_work")}, nil
}

func (w *Worker) Start(context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancel != nil {
		return fmt.Errorf("optional model worker is already started")
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.done = make(chan struct{})
	go w.run(ctx, w.done)
	return nil
}

func (w *Worker) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	interval := w.engine.Config.PollInterval
	if !w.engine.Config.Enabled || !w.engine.Config.WorkerEnabled {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		worked, err := w.engine.RunOne(ctx)
		outcome := "idle"
		if worked {
			outcome = "worked"
		}
		if err != nil {
			outcome = "error"
		}
		w.engine.Store.metrics.cycles.WithLabelValues(outcome).Inc()
		if err != nil && ctx.Err() == nil {
			w.logger.Warnw("optional model task failed", "error", err)
		}
		if ctx.Err() != nil {
			return
		}
		if _, err := w.engine.Store.Cleanup(ctx); err != nil && ctx.Err() == nil {
			w.logger.Warnw("optional model cleanup failed", "error", err)
		}
		if err := w.engine.Store.RefreshMetrics(ctx); err != nil && ctx.Err() == nil {
			w.logger.Warnw("optional model metrics failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Worker) Stop(ctx context.Context) error {
	w.mu.Lock()
	cancel, done := w.cancel, w.done
	w.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		w.mu.Lock()
		w.cancel = nil
		w.done = nil
		w.mu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
