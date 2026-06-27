package cdx_s3

import (
	"container/list"
	"sync"
	"time"
)

// CacheItem holds metadata and optional byte content of an S3 object.
type CacheItem struct {
	Key          string
	ETag         string
	LastModified time.Time
	Size         int64
	ContentType  string
	Content      []byte    // Nil if only metadata is cached
	ExpiredAt    time.Time
	Exists       bool      // Negative caching support (for 404s)
}

// cacheEntry is the internal struct stored in the list.
type cacheEntry struct {
	key   string
	value *CacheItem
}

// LRUCache is a thread-safe LRU cache with TTL expiration.
type LRUCache struct {
	mu        sync.RWMutex
	capacity  int
	items     map[string]*list.Element
	evictList *list.List
}

// NewLRUCache creates a new LRUCache with the given capacity.
func NewLRUCache(capacity int) *LRUCache {
	if capacity <= 0 {
		capacity = 1000
	}
	return &LRUCache{
		capacity:  capacity,
		items:     make(map[string]*list.Element),
		evictList: list.New(),
	}
}

// Get retrieves an item from the cache. If the item has expired, it is evicted and nil, false is returned.
func (c *LRUCache) Get(key string) (*CacheItem, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	elem, ok := c.items[key]
	if !ok {
		return nil, false
	}

	entry := elem.Value.(*cacheEntry)
	if time.Now().After(entry.value.ExpiredAt) {
		// Item has expired, remove it from cache
		c.evictList.Remove(elem)
		delete(c.items, key)
		return nil, false
	}

	c.evictList.MoveToFront(elem)
	return entry.value, true
}

// Set adds or updates an item in the cache with the specified TTL.
func (c *LRUCache) Set(key string, value *CacheItem, ttl time.Duration) {
	if ttl <= 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	value.ExpiredAt = time.Now().Add(ttl)

	// If item already exists, update it and move to front
	if elem, ok := c.items[key]; ok {
		c.evictList.MoveToFront(elem)
		entry := elem.Value.(*cacheEntry)
		entry.value = value
		return
	}

	// Add new item
	entry := &cacheEntry{key: key, value: value}
	elem := c.evictList.PushFront(entry)
	c.items[key] = elem

	// Evict oldest if capacity exceeded
	if c.evictList.Len() > c.capacity {
		c.evictOldest()
	}
}

// Delete removes an item from the cache.
func (c *LRUCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.items[key]; ok {
		c.evictList.Remove(elem)
		delete(c.items, key)
	}
}

// Clear flushes all items from the cache.
func (c *LRUCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.items = make(map[string]*list.Element)
	c.evictList.Init()
}

func (c *LRUCache) evictOldest() {
	elem := c.evictList.Back()
	if elem != nil {
		c.evictList.Remove(elem)
		entry := elem.Value.(*cacheEntry)
		delete(c.items, entry.key)
	}
}
