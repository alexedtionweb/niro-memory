// Package cache defines the optional cache connector interface and an
// in-process LRU. Caches are never authoritative: everything cached can be
// recomputed from the store, and every entry has a TTL.
package cache

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// Cache is the connector interface (in-process LRU, Redis, …).
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, bool)
	Set(ctx context.Context, key string, val []byte, ttl time.Duration)
}

// LRU is a size-bounded, TTL-aware in-process cache. Safe for concurrent use.
type LRU struct {
	mu    sync.Mutex
	max   int
	ll    *list.List
	items map[string]*list.Element
	now   func() time.Time
}

type entry struct {
	key string
	val []byte
	exp time.Time
}

// NewLRU returns a cache holding at most max entries.
func NewLRU(max int) *LRU {
	return &LRU{max: max, ll: list.New(), items: map[string]*list.Element{}, now: time.Now}
}

// Get returns a live entry and marks it recently used.
func (c *LRU) Get(_ context.Context, key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	e := el.Value.(*entry)
	if c.now().After(e.exp) {
		c.ll.Remove(el)
		delete(c.items, key)
		return nil, false
	}
	c.ll.MoveToFront(el)
	return e.val, true
}

// Set stores val for ttl, evicting the least recently used entry when full.
func (c *LRU) Set(_ context.Context, key string, val []byte, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	exp := c.now().Add(ttl)
	if el, ok := c.items[key]; ok {
		el.Value = &entry{key, val, exp}
		c.ll.MoveToFront(el)
		return
	}
	c.items[key] = c.ll.PushFront(&entry{key, val, exp})
	for c.ll.Len() > c.max {
		old := c.ll.Back()
		c.ll.Remove(old)
		delete(c.items, old.Value.(*entry).key)
	}
}
