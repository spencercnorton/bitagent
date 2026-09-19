package llmcapture

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.uber.org/zap"
)

type captureExpiryStore interface {
	DeleteExpired(context.Context) (int64, error)
}

// Janitor deletes expired capture payloads and their cascaded raw-infohash
// admissions independently of Config.Enabled. Capture collection can be
// disabled immediately after a bounded window without suspending retention.
type Janitor struct {
	cfg    Config
	store  captureExpiryStore
	logger *zap.SugaredLogger

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewJanitor(
	cfg Config,
	store *PostgresStore,
	logger *zap.SugaredLogger,
) (*Janitor, error) {
	return newJanitor(cfg, store, logger)
}

func newJanitor(
	cfg Config,
	store captureExpiryStore,
	logger *zap.SugaredLogger,
) (*Janitor, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if store == nil {
		return nil, errors.New("capture expiry janitor has no store")
	}
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return &Janitor{
		cfg:    cfg,
		store:  store,
		logger: logger.Named("llm_evaluation_capture_janitor"),
	}, nil
}

func (j *Janitor) Start(context.Context) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.cancel != nil {
		return errors.New("capture expiry janitor is already started")
	}
	ctx, cancel := context.WithCancel(context.Background())
	j.cancel = cancel
	j.wg.Add(1)
	go j.loop(ctx)
	return nil
}

func (j *Janitor) Stop(context.Context) error {
	j.mu.Lock()
	cancel := j.cancel
	j.cancel = nil
	j.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	j.wg.Wait()
	return nil
}

func (j *Janitor) loop(ctx context.Context) {
	defer j.wg.Done()
	j.runCycle(ctx)
	ticker := time.NewTicker(j.cfg.CleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			j.runCycle(ctx)
		}
	}
}

func (j *Janitor) runCycle(ctx context.Context) {
	deleted, err := j.store.DeleteExpired(ctx)
	if err != nil {
		if ctx.Err() == nil {
			j.logger.Warnw("delete expired captures", "err", err)
		}
		return
	}
	if deleted > 0 {
		j.logger.Infow("deleted expired captures", "captures", deleted)
	}
}
