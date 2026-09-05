// Package cache provides in-memory read-through caching for shortened links.
package cache

import (
	"container/list"
	"context"
	"sync"
	"time"

	"github.com/mmuqiitf/url-shortener/internal/model"
)

// Cache defines the key-value cache interface for Link entities.
type Cache interface {
	Get(ctx context.Context, code string) (model.Link, bool)
	Set(ctx context.Context, link model.Link, ttl time.Duration)
	Delete(ctx context.Context, code string)
}

type cacheItem struct {
	key       string
	link      model.Link
	expiresAt time.Time
}

// MemoryCache is a thread-safe in-memory LRU cache with TTL support.
type MemoryCache struct {
	mu       sync.RWMutex
	capacity int
	items    map[string]*list.Element
	evictList *list.List
}

// NewMemoryCache creates a new MemoryCache with the specified item capacity.
func NewMemoryCache(capacity int) *MemoryCache {
	if capacity <= 0 {
		capacity = 10000
	}
	return &MemoryCache{
		capacity:  capacity,
		items:     make(map[string]*list.Element, capacity),
		evictList: list.New(),
	}
}

// Get retrieves a link from cache if present and not expired.
func (c *MemoryCache) Get(_ context.Context, code string) (model.Link, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	elem, exists := c.items[code]
	if !exists {
		return model.Link{}, false
	}

	item := elem.Value.(*cacheItem)
	if !item.expiresAt.IsZero() && time.Now().UTC().After(item.expiresAt) {
		// Expired item: evict and return miss
		c.removeElement(elem)
		return model.Link{}, false
	}

	c.evictList.MoveToFront(elem)
	return item.link, true
}

// Set stores a link in the cache with the given TTL.
func (c *MemoryCache) Set(_ context.Context, link model.Link, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var exp time.Time
	if ttl > 0 {
		exp = time.Now().UTC().Add(ttl)
	}

	// Update existing element if present
	if elem, exists := c.items[link.Code]; exists {
		c.evictList.MoveToFront(elem)
		item := elem.Value.(*cacheItem)
		item.link = link
		item.expiresAt = exp
		return
	}

	// Evict oldest if capacity is reached
	if c.evictList.Len() >= c.capacity {
		c.removeOldest()
	}

	item := &cacheItem{
		key:       link.Code,
		link:      link,
		expiresAt: exp,
	}
	elem := c.evictList.PushFront(item)
	c.items[link.Code] = elem
}

// Delete removes a link from the cache by code.
func (c *MemoryCache) Delete(_ context.Context, code string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, exists := c.items[code]; exists {
		c.removeElement(elem)
	}
}

func (c *MemoryCache) removeOldest() {
	elem := c.evictList.Back()
	if elem != nil {
		c.removeElement(elem)
	}
}

func (c *MemoryCache) removeElement(elem *list.Element) {
	c.evictList.Remove(elem)
	item := elem.Value.(*cacheItem)
	delete(c.items, item.key)
}
