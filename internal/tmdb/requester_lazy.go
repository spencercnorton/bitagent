package tmdb

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/spencercnorton/bitagent/internal/concurrency"
	"go.uber.org/zap"
	"golang.org/x/sync/semaphore"
	"golang.org/x/time/rate"
)

// requesterLazy defers instantiation of the requester (and possible failure) until the first request is made,
// avoiding failure when the TMDB client is not needed.
type requesterLazy struct {
	once      sync.Once
	config    Config
	logger    *zap.SugaredLogger
	err       error
	requester Requester
}

func (r *requesterLazy) Request(
	ctx context.Context,
	path string,
	queryParams map[string]string,
	result interface{},
) (*resty.Response, error) {
	r.once.Do(func() {
		r.requester, r.err = newRequester(ctx, r.config, r.logger)
	})

	if r.err != nil {
		return nil, r.err
	}

	return r.requester.Request(ctx, path, queryParams, result)
}

func newRequester(ctx context.Context, config Config, logger *zap.SugaredLogger) (Requester, error) {
	if !config.Enabled {
		return nil, errors.New("TMDB is disabled")
	}

	if config.APIKey == "" {
		return nil, errors.New("TMDB_API_KEY must be configured for metadata enrichment")
	}

	r := requesterLogger{
		requester: requesterFailFast{
			requester: requesterSemaphore{
				requester: requesterLimiter{
					requester: requester{resty: newRestyClient(config, logger)},
					limiter:   rate.NewLimiter(rate.Every(config.RateLimit), config.RateLimitBurst),
				},
				semaphore: semaphore.NewWeighted(2),
			},
			isUnauthorized: &concurrency.AtomicValue[bool]{},
		},
		logger: logger,
	}

	err := client{r}.ValidateAPIKey(ctx)
	if errors.Is(err, ErrUnauthorized) {
		return nil, fmt.Errorf("configured TMDB API key rejected: %w", err)
	}

	return r, err
}

func newRestyClient(config Config, logger *zap.SugaredLogger) *resty.Client {
	return resty.New().
		SetBaseURL(config.BaseURL).
		SetQueryParam("api_key", config.APIKey).
		SetRetryCount(3).
		SetRetryWaitTime(2 * time.Second).
		SetRetryMaxWaitTime(20 * time.Second).
		SetTimeout(10 * time.Second).
		EnableTrace().
		SetLogger(restyLogger{logger})
}
