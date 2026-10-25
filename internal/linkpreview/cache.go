package linkpreview

import (
	"sync"
	"time"
)

// Cache is a bounded in-process cache with a lifetime per entry. A fetch result
// is keyed by canonical URL so the same link pasted by every member in a group
// is fetched once; a negative result lives shorter than a card.
//
// It is a plain map under a mutex and not a library: the bound is small and the
// only policy is "full means drop what expires first".
type Cache[V any] struct {
	mu  sync.Mutex
	max int
	now func() time.Time
	m   map[string]cacheEntry[V]
}

type cacheEntry[V any] struct {
	v       V
	expires time.Time
}

// NewCache returns a cache of at most max entries. now is time.Now unless a
// test supplies a clock.
func NewCache[V any](max int, now func() time.Time) *Cache[V] {
	if now == nil {
		now = time.Now
	}
	return &Cache[V]{max: max, now: now, m: map[string]cacheEntry[V]{}}
}

// Get returns the live entry for key.
func (c *Cache[V]) Get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if ok && !c.now().Before(e.expires) {
		delete(c.m, key)
		ok = false
	}
	if !ok {
		var zero V
		return zero, false
	}
	return e.v, true
}

// Put stores v for ttl.
func (c *Cache[V]) Put(key string, v V, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if _, exists := c.m[key]; !exists && len(c.m) >= c.max {
		var oldest string
		var at time.Time
		for k, e := range c.m {
			if !now.Before(e.expires) {
				delete(c.m, k)
				continue
			}
			if oldest == "" || e.expires.Before(at) {
				oldest, at = k, e.expires
			}
		}
		if len(c.m) >= c.max && oldest != "" {
			delete(c.m, oldest)
		}
	}
	c.m[key] = cacheEntry[V]{v: v, expires: now.Add(ttl)}
}
