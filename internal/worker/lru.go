package worker

import (
	"container/list"
	"sync"
)

// lruCache is a concurrent-safe LRU set with a fixed capacity.
type lruCache struct {
	items    map[string]*list.Element
	order    *list.List
	mu       sync.Mutex
	capacity int
}

func newLRUCache(capacity int) *lruCache { //nolint:unparam // capacity is configurable by design
	return &lruCache{
		capacity: capacity,
		items:    make(map[string]*list.Element, capacity),
		order:    list.New(),
	}
}

func (c *lruCache) contains(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.order.MoveToFront(el)
		return true
	}
	return false
}

func (c *lruCache) add(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.order.MoveToFront(el)
		return
	}
	if c.order.Len() >= c.capacity {
		oldest := c.order.Back()
		if oldest != nil {
			c.order.Remove(oldest)
			if evictKey, ok := oldest.Value.(string); ok {
				delete(c.items, evictKey)
			}
		}
	}
	el := c.order.PushFront(key)
	c.items[key] = el
}
