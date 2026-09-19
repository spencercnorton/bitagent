package classifier

import (
	"context"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/model"
)

type runnerSemaphore struct {
	runner    Runner
	semaphore chan struct{}
}

func (r runnerSemaphore) Run(
	ctx context.Context,
	workflow string,
	flags Flags,
	t model.Torrent,
) (classification.Result, error) {
	select {
	case <-ctx.Done():
		return classification.Result{}, ctx.Err()
	case r.semaphore <- struct{}{}:
	}

	defer func() { <-r.semaphore }()

	return r.runner.Run(ctx, workflow, flags, t)
}

// EvalMatch delegates without taking the workflow semaphore: the eval command
// is an offline measurement path that manages its own concurrency, and the
// matcher's own LLM/TMDB limits already bound it.
func (r runnerSemaphore) EvalMatch(ctx context.Context, t model.Torrent, ct model.NullContentType) (MatchDecision, error) {
	return r.runner.EvalMatch(ctx, t, ct)
}
