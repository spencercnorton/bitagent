package concurrency

import (
	"context"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2/expirable"
	"golang.org/x/time/rate"
)

type KeyedLimiter interface {
	Allow(key string) bool
	Wait(ctx context.Context, key string) error
}

type keyedLimiter struct {
	lru   *lru.LRU[string, *rate.Limiter]
	mu    sync.RWMutex
	rl    rate.Limit
	burst int
}

// defaultKeyedLimiterSize matches what every current caller passes; it exists
// so a zero cannot silently mean "unbounded".
const defaultKeyedLimiterSize = 1000

// NewKeyedLimiter builds a per-key rate limiter backed by an expiring LRU.
//
// size is clamped: expirable.NewLRU treats 0 as UNBOUNDED. This limiter is
// keyed on remote IP for the public DHT port, so an unbounded map here is a
// memory-exhaustion vector driven by whoever is sending packets — every three
// current call sites pass a literal 1000, but nothing in the signature said a
// zero would be catastrophic rather than inert.
func NewKeyedLimiter(rl rate.Limit, burst int, size int, ttl time.Duration) KeyedLimiter {
	if size <= 0 {
		size = defaultKeyedLimiterSize
	}
	i := &keyedLimiter{
		lru:   lru.NewLRU[string, *rate.Limiter](size, nil, ttl),
		rl:    rl,
		burst: burst,
	}

	return i
}

func (i *keyedLimiter) Allow(key string) bool {
	return i.getLimiter(key).Allow()
}

func (i *keyedLimiter) Wait(ctx context.Context, key string) error {
	return i.getLimiter(key).Wait(ctx)
}

func (i *keyedLimiter) getLimiter(key string) *rate.Limiter {
	i.mu.Lock()

	l, ok := i.lru.Get(key)
	if !ok {
		l = rate.NewLimiter(i.rl, i.burst)
		i.lru.Add(key, l)
	}
	i.mu.Unlock()

	return l
}
