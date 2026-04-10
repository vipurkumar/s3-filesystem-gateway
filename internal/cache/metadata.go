// Copyright 2024 s3-filesystem-gateway contributors
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"container/list"
	"os"
	"strings"
	"sync"
	"time"
)

// CacheEntry stores cached metadata for an S3 object or directory.
type CacheEntry struct {
	S3Key    string
	Size     int64
	ModTime  time.Time
	Mode     os.FileMode
	UID      int
	GID      int
	IsDir    bool
	ETag     string
	Inode    uint64
	CachedAt time.Time

	// negative indicates this is a negative (not-found) cache entry.
	negative bool
}

// dirListingEntry holds a cached directory listing with its timestamp.
type dirListingEntry struct {
	entries  []CacheEntry
	cachedAt time.Time
}

// CacheConfig holds configuration for the metadata cache.
type CacheConfig struct {
	// MaxEntries is the maximum number of entries in the LRU cache.
	// Default: 10000
	MaxEntries int

	// FileTTL is how long file metadata stays valid. Default: 300s.
	FileTTL time.Duration

	// DirTTL is how long directory metadata stays valid. Default: 60s.
	DirTTL time.Duration

	// NegativeTTL is how long negative (not-found) entries stay valid. Default: 10s.
	NegativeTTL time.Duration

	// EvictionInterval is how often the background eviction runs. Default: 30s.
	EvictionInterval time.Duration
}

// DefaultCacheConfig returns a CacheConfig with sensible defaults.
func DefaultCacheConfig() CacheConfig {
	return CacheConfig{
		MaxEntries:       10000,
		FileTTL:          300 * time.Second,
		DirTTL:           60 * time.Second,
		NegativeTTL:      10 * time.Second,
		EvictionInterval: 30 * time.Second,
	}
}

// lruItem is the value stored in each list element.
type lruItem struct {
	key   string
	entry CacheEntry
}

// MetadataCache is a thread-safe in-memory LRU cache with TTL-based expiry
// for S3 object metadata and directory listings.
type MetadataCache struct {
	mu sync.RWMutex

	config CacheConfig

	// LRU for individual entries: key -> *list.Element (holding *lruItem)
	items map[string]*list.Element
	order *list.List // front = most recently used

	// Directory listing cache: prefix -> dirListingEntry
	dirListings map[string]dirListingEntry

	stopCh chan struct{}
	done   chan struct{}
}

// NewMetadataCache creates a new metadata cache and starts background eviction.
func NewMetadataCache(config CacheConfig) *MetadataCache {
	if config.MaxEntries <= 0 {
		config.MaxEntries = 10000
	}
	if config.FileTTL <= 0 {
		config.FileTTL = 300 * time.Second
	}
	if config.DirTTL <= 0 {
		config.DirTTL = 60 * time.Second
	}
	if config.NegativeTTL <= 0 {
		config.NegativeTTL = 10 * time.Second
	}
	if config.EvictionInterval <= 0 {
		config.EvictionInterval = 30 * time.Second
	}

	mc := &MetadataCache{
		config:      config,
		items:       make(map[string]*list.Element),
		order:       list.New(),
		dirListings: make(map[string]dirListingEntry),
		stopCh:      make(chan struct{}),
		done:        make(chan struct{}),
	}

	go mc.evictionLoop()
	return mc
}

// GetEntry returns the cached metadata for the given S3 key if it exists and
// has not expired. Returns (nil, false) on miss or expiry.
func (mc *MetadataCache) GetEntry(s3Key string) (*CacheEntry, bool) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	elem, ok := mc.items[s3Key]
	if !ok {
		return nil, false
	}

	item := elem.Value.(*lruItem)
	if mc.isExpired(item.entry) {
		mc.removeLocked(s3Key, elem)
		return nil, false
	}

	// Move to front (most recently used).
	mc.order.MoveToFront(elem)

	entry := item.entry
	return &entry, true
}

// PutEntry stores metadata for the given S3 key.
func (mc *MetadataCache) PutEntry(s3Key string, entry CacheEntry) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	entry.CachedAt = time.Now()
	entry.negative = false

	if elem, ok := mc.items[s3Key]; ok {
		// Update existing entry.
		elem.Value.(*lruItem).entry = entry
		mc.order.MoveToFront(elem)
		return
	}

	// Evict LRU if at capacity.
	if mc.order.Len() >= mc.config.MaxEntries {
		mc.evictOldestLocked()
	}

	item := &lruItem{key: s3Key, entry: entry}
	elem := mc.order.PushFront(item)
	mc.items[s3Key] = elem
}

// PutNegative stores a negative (not-found) cache entry for the given S3 key.
func (mc *MetadataCache) PutNegative(s3Key string) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	entry := CacheEntry{
		S3Key:    s3Key,
		CachedAt: time.Now(),
		negative: true,
	}

	if elem, ok := mc.items[s3Key]; ok {
		elem.Value.(*lruItem).entry = entry
		mc.order.MoveToFront(elem)
		return
	}

	if mc.order.Len() >= mc.config.MaxEntries {
		mc.evictOldestLocked()
	}

	item := &lruItem{key: s3Key, entry: entry}
	elem := mc.order.PushFront(item)
	mc.items[s3Key] = elem
}

// IsNegative returns true if the entry is a negative (not-found) cache entry.
func (e *CacheEntry) IsNegative() bool {
	return e.negative
}

// GetDirListing returns the cached directory listing for the given prefix,
// or (nil, false) if not cached or expired.
func (mc *MetadataCache) GetDirListing(prefix string) ([]CacheEntry, bool) {
	mc.mu.RLock()
	defer mc.mu.RUnlock()

	dl, ok := mc.dirListings[prefix]
	if !ok {
		return nil, false
	}

	if time.Since(dl.cachedAt) > mc.config.DirTTL {
		// Expired — will be cleaned up by eviction loop or next Put.
		return nil, false
	}

	// Return a copy to avoid data races.
	result := make([]CacheEntry, len(dl.entries))
	copy(result, dl.entries)
	return result, true
}

// PutDirListing caches a directory listing for the given prefix.
func (mc *MetadataCache) PutDirListing(prefix string, entries []CacheEntry) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	stored := make([]CacheEntry, len(entries))
	copy(stored, entries)

	mc.dirListings[prefix] = dirListingEntry{
		entries:  stored,
		cachedAt: time.Now(),
	}
}

// Invalidate removes the entry for s3Key and invalidates the parent directory
// listing that would contain it.
func (mc *MetadataCache) Invalidate(s3Key string) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	if elem, ok := mc.items[s3Key]; ok {
		mc.removeLocked(s3Key, elem)
	}

	// Invalidate parent directory listing.
	parent := parentPrefix(s3Key)
	delete(mc.dirListings, parent)
}

// InvalidatePrefix removes all cached entries whose key starts with the given
// prefix, and removes any directory listings under that prefix.
func (mc *MetadataCache) InvalidatePrefix(prefix string) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	// Remove matching entries.
	for key, elem := range mc.items {
		if strings.HasPrefix(key, prefix) {
			mc.removeLocked(key, elem)
		}
	}

	// Remove matching directory listings.
	for p := range mc.dirListings {
		if strings.HasPrefix(p, prefix) || p == prefix {
			delete(mc.dirListings, p)
		}
	}
}

// Stop stops the background eviction goroutine and waits for it to exit.
func (mc *MetadataCache) Stop() {
	close(mc.stopCh)
	<-mc.done
}

// --- internal helpers ---

// isExpired checks whether a cache entry has exceeded its TTL.
func (mc *MetadataCache) isExpired(entry CacheEntry) bool {
	age := time.Since(entry.CachedAt)
	if entry.negative {
		return age > mc.config.NegativeTTL
	}
	if entry.IsDir {
		return age > mc.config.DirTTL
	}
	return age > mc.config.FileTTL
}

// removeLocked removes an entry by key and list element. Caller must hold mu.
func (mc *MetadataCache) removeLocked(key string, elem *list.Element) {
	mc.order.Remove(elem)
	delete(mc.items, key)
}

// evictOldestLocked removes the least recently used entry. Caller must hold mu.
func (mc *MetadataCache) evictOldestLocked() {
	back := mc.order.Back()
	if back == nil {
		return
	}
	item := back.Value.(*lruItem)
	mc.removeLocked(item.key, back)
}

// evictionLoop runs periodically to remove expired entries.
func (mc *MetadataCache) evictionLoop() {
	defer close(mc.done)
	ticker := time.NewTicker(mc.config.EvictionInterval)
	defer ticker.Stop()

	for {
		select {
		case <-mc.stopCh:
			return
		case <-ticker.C:
			mc.evictExpired()
		}
	}
}

// evictExpired removes all expired entries and directory listings.
func (mc *MetadataCache) evictExpired() {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	// Evict expired individual entries.
	for key, elem := range mc.items {
		item := elem.Value.(*lruItem)
		if mc.isExpired(item.entry) {
			mc.removeLocked(key, elem)
		}
	}

	// Evict expired directory listings.
	now := time.Now()
	for prefix, dl := range mc.dirListings {
		if now.Sub(dl.cachedAt) > mc.config.DirTTL {
			delete(mc.dirListings, prefix)
		}
	}
}

// parentPrefix returns the parent directory prefix for an S3 key.
// For example, "foo/bar/baz.txt" -> "foo/bar/", "foo/" -> "".
func parentPrefix(s3Key string) string {
	key := strings.TrimSuffix(s3Key, "/")
	idx := strings.LastIndex(key, "/")
	if idx < 0 {
		return ""
	}
	return key[:idx+1]
}
