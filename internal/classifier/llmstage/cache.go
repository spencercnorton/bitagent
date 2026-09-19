package llmstage

import (
	"container/list"
	"sync"
)

// lruCache is a small, thread-safe LRU keyed by the sha256 hex
// derived from the exact request, infohash, endpoint and application policy.
// Values include the original durable response receipt, not just a prediction.
//
// Capacity protects memory; eviction is strict oldest-first. When a
// prompt change invalidates semantic correctness of cached entries,
// the operator bumps Config.PromptVersion which appears in every key,
// so the cache effectively resets without explicit invalidation.
type lruCache struct {
	mu     sync.Mutex
	cap    int
	list   *list.List
	lookup map[string]*list.Element
}

type cacheEntry struct {
	key string
	val Decision
}

func newLRU(cap int) *lruCache {
	if cap <= 0 {
		cap = 1
	}
	return &lruCache{
		cap:    cap,
		list:   list.New(),
		lookup: make(map[string]*list.Element, cap),
	}
}

func (c *lruCache) Get(key string) (Decision, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.lookup[key]
	if !ok {
		return Decision{}, false
	}
	c.list.MoveToFront(el)
	return el.Value.(cacheEntry).val, true
}

func (c *lruCache) Put(key string, val Decision) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.lookup[key]; ok {
		el.Value = cacheEntry{key: key, val: val}
		c.list.MoveToFront(el)
		return
	}
	el := c.list.PushFront(cacheEntry{key: key, val: val})
	c.lookup[key] = el
	for c.list.Len() > c.cap {
		tail := c.list.Back()
		if tail == nil {
			break
		}
		c.list.Remove(tail)
		delete(c.lookup, tail.Value.(cacheEntry).key)
	}
}

func (c *lruCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.list.Len()
}
