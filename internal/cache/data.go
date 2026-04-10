package cache

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultDataCacheDir     = "/var/cache/s3gw"
	defaultDataCacheMaxSize = 10 * 1024 * 1024 * 1024 // 10 GB
)

// DataCacheConfig holds configuration for the disk-based data cache.
type DataCacheConfig struct {
	// Dir is the directory where cached data files are stored.
	// Default: "/var/cache/s3gw"
	Dir string

	// MaxSize is the maximum total size of cached data in bytes.
	// Default: 10 GB
	MaxSize int64
}

// DefaultDataCacheConfig returns a DataCacheConfig with sensible defaults.
func DefaultDataCacheConfig() DataCacheConfig {
	return DataCacheConfig{
		Dir:     defaultDataCacheDir,
		MaxSize: defaultDataCacheMaxSize,
	}
}

// DataCacheStats holds statistics about the data cache.
type DataCacheStats struct {
	Hits        int64
	Misses      int64
	CurrentSize int64
	EntryCount  int
}

// dataEntry tracks a cached file in the LRU list.
type dataEntry struct {
	key        string // SHA256 hex of s3Key+etag
	s3Key      string // original S3 key (for invalidation by prefix)
	size       int64
	accessTime time.Time
}

// DataCache is a thread-safe disk-based LRU cache for S3 object data.
type DataCache struct {
	mu sync.RWMutex

	config DataCacheConfig

	// LRU tracking: key -> *list.Element (holding *dataEntry)
	items map[string]*list.Element
	order *list.List // front = most recently used

	currentSize int64

	hits   atomic.Int64
	misses atomic.Int64

	stopCh chan struct{}
	done   chan struct{}
}

// NewDataCache creates a new disk-based data cache. It creates the cache
// directory if needed and scans it to rebuild the in-memory LRU index.
func NewDataCache(config DataCacheConfig) (*DataCache, error) {
	if config.Dir == "" {
		config.Dir = defaultDataCacheDir
	}
	if config.MaxSize <= 0 {
		config.MaxSize = defaultDataCacheMaxSize
	}

	if err := os.MkdirAll(config.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("create cache dir %q: %w", config.Dir, err)
	}

	dc := &DataCache{
		config: config,
		items:  make(map[string]*list.Element),
		order:  list.New(),
		stopCh: make(chan struct{}),
		done:   make(chan struct{}),
	}

	dc.scanDir()

	// Evict if we're over limit after scanning existing files.
	dc.evict()

	go dc.evictionLoop()
	return dc, nil
}

// Get returns a ReadCloser for the cached data if it exists. The caller must
// close the returned reader. Returns (nil, false) on cache miss.
func (dc *DataCache) Get(s3Key, etag string) (io.ReadCloser, bool) {
	key := cacheKey(s3Key, etag)
	path := dc.cachePath(key)

	dc.mu.Lock()
	elem, ok := dc.items[key]
	if !ok {
		dc.mu.Unlock()
		dc.misses.Add(1)
		return nil, false
	}

	// Update access time and move to front.
	entry := elem.Value.(*dataEntry)
	entry.accessTime = time.Now()
	dc.order.MoveToFront(elem)
	dc.mu.Unlock()

	f, err := os.Open(path)
	if err != nil {
		// File disappeared from disk; remove from index.
		dc.mu.Lock()
		dc.currentSize -= elem.Value.(*dataEntry).size
		dc.order.Remove(elem)
		delete(dc.items, key)
		dc.mu.Unlock()
		dc.misses.Add(1)
		return nil, false
	}

	dc.hits.Add(1)
	return f, true
}

// Put writes data to the cache. If the cache exceeds its maximum size after
// writing, LRU eviction is triggered.
func (dc *DataCache) Put(s3Key, etag string, reader io.Reader, size int64) error {
	key := cacheKey(s3Key, etag)
	path := dc.cachePath(key)

	// Ensure shard directory exists.
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create shard dir: %w", err)
	}

	// Write to a temp file first, then rename for atomicity.
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()

	written, err := io.Copy(tmp, reader)
	if cerr := tmp.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("write cache file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename cache file: %w", err)
	}

	dc.mu.Lock()
	// If key already exists, update it.
	if elem, ok := dc.items[key]; ok {
		old := elem.Value.(*dataEntry)
		dc.currentSize -= old.size
		old.size = written
		old.accessTime = time.Now()
		dc.currentSize += written
		dc.order.MoveToFront(elem)
	} else {
		entry := &dataEntry{
			key:        key,
			s3Key:      s3Key,
			size:       written,
			accessTime: time.Now(),
		}
		elem := dc.order.PushFront(entry)
		dc.items[key] = elem
		dc.currentSize += written
	}
	dc.mu.Unlock()

	dc.evict()
	return nil
}

// Invalidate removes all cached versions of the given S3 key.
func (dc *DataCache) Invalidate(s3Key string) {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	// Collect keys to remove (can't modify map while iterating safely with delete
	// in the same loop when also removing list elements, but Go allows it).
	for key, elem := range dc.items {
		entry := elem.Value.(*dataEntry)
		if entry.s3Key == s3Key {
			path := dc.cachePath(key)
			os.Remove(path)
			dc.currentSize -= entry.size
			dc.order.Remove(elem)
			delete(dc.items, key)
		}
	}
}

// Stats returns current cache statistics.
func (dc *DataCache) Stats() DataCacheStats {
	dc.mu.RLock()
	defer dc.mu.RUnlock()

	return DataCacheStats{
		Hits:        dc.hits.Load(),
		Misses:      dc.misses.Load(),
		CurrentSize: dc.currentSize,
		EntryCount:  dc.order.Len(),
	}
}

// Stop stops the background eviction goroutine and waits for it to exit.
func (dc *DataCache) Stop() {
	close(dc.stopCh)
	<-dc.done
}

// --- internal helpers ---

// cacheKey returns the SHA256 hex digest of s3Key + etag.
func cacheKey(s3Key, etag string) string {
	h := sha256.Sum256([]byte(s3Key + etag))
	return hex.EncodeToString(h[:])
}

// cachePath returns the file path for a cache key, using the first two hex
// characters as a shard directory.
func (dc *DataCache) cachePath(key string) string {
	return filepath.Join(dc.config.Dir, key[:2], key)
}

// evict removes least recently used entries until the cache is under MaxSize.
func (dc *DataCache) evict() {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	for dc.currentSize > dc.config.MaxSize && dc.order.Len() > 0 {
		back := dc.order.Back()
		if back == nil {
			break
		}
		entry := back.Value.(*dataEntry)
		path := dc.cachePath(entry.key)
		os.Remove(path)
		dc.currentSize -= entry.size
		dc.order.Remove(back)
		delete(dc.items, entry.key)
	}
}

// scanDir scans the cache directory on startup to rebuild the in-memory LRU
// index from existing files on disk.
func (dc *DataCache) scanDir() {
	shards, err := os.ReadDir(dc.config.Dir)
	if err != nil {
		return
	}

	for _, shard := range shards {
		if !shard.IsDir() || len(shard.Name()) != 2 {
			continue
		}
		shardPath := filepath.Join(dc.config.Dir, shard.Name())
		files, err := os.ReadDir(shardPath)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || strings.HasPrefix(f.Name(), ".tmp-") {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			key := f.Name()
			entry := &dataEntry{
				key:        key,
				s3Key:      "", // unknown after restart; invalidation by s3Key won't match
				size:       info.Size(),
				accessTime: info.ModTime(),
			}
			elem := dc.order.PushBack(entry) // older files go to back
			dc.items[key] = elem
			dc.currentSize += info.Size()
		}
	}
}

// evictionLoop periodically checks if the cache exceeds its max size.
func (dc *DataCache) evictionLoop() {
	defer close(dc.done)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-dc.stopCh:
			return
		case <-ticker.C:
			dc.evict()
		}
	}
}
