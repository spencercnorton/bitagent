package llmmatch

import (
	"container/list"
	"sync"
)

// lruCache is a small mutex-guarded LRU of stringly-typed decisions. Values
// are opaque (extraction or rerank result) so one cache serves both stages,
// keyed with a stage prefix. Restart wipes it — LLM calls are rare once the
// evidence ingestor has coverage.
type lruCache struct {
	mu     sync.Mutex
	cap    int
	ll     *list.List
	lookup map[string]*list.Element
}

type cacheEntry struct {
	key string
	val any
}

func newLRU(capacity int) *lruCache {
	if capacity <= 0 {
		capacity = 1
	}
	return &lruCache{
		cap:    capacity,
		ll:     list.New(),
		lookup: make(map[string]*list.Element, capacity),
	}
}

func (c *lruCache) Get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.lookup[key]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*cacheEntry).val, true
	}
	return nil, false
}

func (c *lruCache) Put(key string, val any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.lookup[key]; ok {
		c.ll.MoveToFront(el)
		el.Value.(*cacheEntry).val = val
		return
	}
	el := c.ll.PushFront(&cacheEntry{key: key, val: val})
	c.lookup[key] = el
	for c.ll.Len() > c.cap {
		oldest := c.ll.Back()
		if oldest == nil {
			break
		}
		c.ll.Remove(oldest)
		delete(c.lookup, oldest.Value.(*cacheEntry).key)
	}
}
