package classifier

import (
	"context"

	"github.com/spencercnorton/bitagent/internal/model"
)

type localSearchSemaphore struct {
	search    LocalSearch
	semaphore chan struct{}
}

func (s localSearchSemaphore) ContentByID(ctx context.Context, ref model.ContentRef) (model.Content, error) {
	select {
	case <-ctx.Done():
		return model.Content{}, ctx.Err()
	case s.semaphore <- struct{}{}:
	}

	defer func() { <-s.semaphore }()

	return s.search.ContentByID(ctx, ref)
}

func (s localSearchSemaphore) ContentBySearch(
	ctx context.Context,
	ct model.ContentType,
	baseTitle string,
	year model.Year,
) (model.Content, error) {
	select {
	case <-ctx.Done():
		return model.Content{}, ctx.Err()
	case s.semaphore <- struct{}{}:
	}

	defer func() { <-s.semaphore }()

	return s.search.ContentBySearch(ctx, ct, baseTitle, year)
}

func (s localSearchSemaphore) ContentCandidatesBySearch(
	ctx context.Context,
	ct model.ContentType,
	title string,
	year model.Year,
	limit int,
) ([]model.Content, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case s.semaphore <- struct{}{}:
	}

	defer func() { <-s.semaphore }()

	return s.search.ContentCandidatesBySearch(ctx, ct, title, year, limit)
}
