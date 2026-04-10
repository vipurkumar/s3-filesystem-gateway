package cache

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tempDataCache(t *testing.T, maxSize int64) *DataCache {
	t.Helper()
	dir := t.TempDir()
	dc, err := NewDataCache(DataCacheConfig{
		Dir:     dir,
		MaxSize: maxSize,
	})
	if err != nil {
		t.Fatalf("NewDataCache: %v", err)
	}
	t.Cleanup(dc.Stop)
	return dc
}

func TestDataCache_PutGet(t *testing.T) {
	dc := tempDataCache(t, 1<<20) // 1 MB

	data := []byte("hello, world")
	err := dc.Put("my/key.txt", "etag1", bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, ok := dc.Get("my/key.txt", "etag1")
	if !ok {
		t.Fatal("expected cache hit")
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("got %q, want %q", got, data)
	}
}

func TestDataCache_Miss(t *testing.T) {
	dc := tempDataCache(t, 1<<20)

	// Miss on empty cache.
	_, ok := dc.Get("no/such/key", "etag")
	if ok {
		t.Fatal("expected cache miss on empty cache")
	}

	// Put one key, miss on different etag.
	data := []byte("data")
	if err := dc.Put("key", "etag1", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	_, ok = dc.Get("key", "etag2")
	if ok {
		t.Fatal("expected cache miss for different etag")
	}
}

func TestDataCache_Eviction(t *testing.T) {
	// Max size = 100 bytes.
	dc := tempDataCache(t, 100)

	// Put 3 entries of 40 bytes each (total 120 > 100).
	for i, key := range []string{"a", "b", "c"} {
		data := bytes.Repeat([]byte{byte('A' + i)}, 40)
		if err := dc.Put(key, "e", bytes.NewReader(data), 40); err != nil {
			t.Fatalf("Put %s: %v", key, err)
		}
	}

	stats := dc.Stats()
	if stats.CurrentSize > 100 {
		t.Fatalf("cache size %d exceeds max 100", stats.CurrentSize)
	}

	// The oldest entry ("a") should have been evicted.
	_, ok := dc.Get("a", "e")
	if ok {
		t.Fatal("expected entry 'a' to be evicted")
	}

	// "b" or "c" should still be present.
	_, ok = dc.Get("c", "e")
	if !ok {
		t.Fatal("expected entry 'c' to still be cached")
	}
}

func TestDataCache_Invalidate(t *testing.T) {
	dc := tempDataCache(t, 1<<20)

	data := []byte("payload")
	// Put two versions of same key.
	if err := dc.Put("obj", "v1", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("Put v1: %v", err)
	}
	if err := dc.Put("obj", "v2", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("Put v2: %v", err)
	}

	dc.Invalidate("obj")

	if _, ok := dc.Get("obj", "v1"); ok {
		t.Fatal("expected v1 to be invalidated")
	}
	if _, ok := dc.Get("obj", "v2"); ok {
		t.Fatal("expected v2 to be invalidated")
	}

	stats := dc.Stats()
	if stats.EntryCount != 0 {
		t.Fatalf("expected 0 entries after invalidation, got %d", stats.EntryCount)
	}
}

func TestDataCache_Stats(t *testing.T) {
	dc := tempDataCache(t, 1<<20)

	data := []byte("stats-test")
	if err := dc.Put("k", "e", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// One hit.
	rc, ok := dc.Get("k", "e")
	if !ok {
		t.Fatal("expected hit")
	}
	rc.Close()

	// One miss.
	dc.Get("k", "wrong")

	stats := dc.Stats()
	if stats.Hits != 1 {
		t.Fatalf("hits = %d, want 1", stats.Hits)
	}
	if stats.Misses != 1 {
		t.Fatalf("misses = %d, want 1", stats.Misses)
	}
	if stats.EntryCount != 1 {
		t.Fatalf("entry count = %d, want 1", stats.EntryCount)
	}
	if stats.CurrentSize != int64(len(data)) {
		t.Fatalf("current size = %d, want %d", stats.CurrentSize, len(data))
	}
}

func TestCacheKey(t *testing.T) {
	k1 := cacheKey("a", "b")
	k2 := cacheKey("a", "c")
	if k1 == k2 {
		t.Fatal("different etags should produce different keys")
	}
	if len(k1) != 64 {
		t.Fatalf("expected 64 hex chars, got %d", len(k1))
	}
}

func TestCachePath_Sharding(t *testing.T) {
	dc := tempDataCache(t, 1<<20)
	key := cacheKey("test", "etag")
	path := dc.cachePath(key)

	// Path should contain the shard directory.
	if !strings.Contains(path, filepath.Join(key[:2], key)) {
		t.Fatalf("path %q does not contain expected shard structure", path)
	}
}

func TestDataCache_ScanDir(t *testing.T) {
	dir := t.TempDir()

	// Pre-populate the cache directory.
	key := cacheKey("scan/test", "etag")
	shardDir := filepath.Join(dir, key[:2])
	os.MkdirAll(shardDir, 0o755)
	os.WriteFile(filepath.Join(shardDir, key), []byte("scanned"), 0o644)

	dc, err := NewDataCache(DataCacheConfig{
		Dir:     dir,
		MaxSize: 1 << 20,
	})
	if err != nil {
		t.Fatalf("NewDataCache: %v", err)
	}
	defer dc.Stop()

	stats := dc.Stats()
	if stats.EntryCount != 1 {
		t.Fatalf("expected 1 entry after scan, got %d", stats.EntryCount)
	}
	if stats.CurrentSize != 7 { // len("scanned")
		t.Fatalf("expected size 7, got %d", stats.CurrentSize)
	}
}
