package cache

import (
	"os"
	"testing"
	"time"
)

func testConfig() CacheConfig {
	return CacheConfig{
		MaxEntries:       100,
		FileTTL:          200 * time.Millisecond,
		DirTTL:           100 * time.Millisecond,
		NegativeTTL:      50 * time.Millisecond,
		EvictionInterval: 1 * time.Hour, // disable background eviction for most tests
	}
}

func newTestCache() *MetadataCache {
	return NewMetadataCache(testConfig())
}

func TestPutGetEntry(t *testing.T) {
	mc := newTestCache()
	defer mc.Stop()

	entry := CacheEntry{
		S3Key:   "photos/cat.jpg",
		Size:    1024,
		ModTime: time.Now(),
		Mode:    0644,
		UID:     1000,
		GID:     1000,
		IsDir:   false,
		ETag:    "abc123",
		Inode:   42,
	}

	mc.PutEntry("photos/cat.jpg", entry)

	got, ok := mc.GetEntry("photos/cat.jpg")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if got.S3Key != "photos/cat.jpg" {
		t.Errorf("expected S3Key %q, got %q", "photos/cat.jpg", got.S3Key)
	}
	if got.Size != 1024 {
		t.Errorf("expected Size 1024, got %d", got.Size)
	}
	if got.ETag != "abc123" {
		t.Errorf("expected ETag %q, got %q", "abc123", got.ETag)
	}
	if got.CachedAt.IsZero() {
		t.Error("expected CachedAt to be set")
	}
}

func TestGetEntryMiss(t *testing.T) {
	mc := newTestCache()
	defer mc.Stop()

	_, ok := mc.GetEntry("nonexistent")
	if ok {
		t.Fatal("expected cache miss")
	}
}

func TestFileTTLExpiry(t *testing.T) {
	mc := newTestCache()
	defer mc.Stop()

	entry := CacheEntry{
		S3Key: "file.txt",
		Size:  10,
		IsDir: false,
	}
	mc.PutEntry("file.txt", entry)

	// Should be present immediately.
	if _, ok := mc.GetEntry("file.txt"); !ok {
		t.Fatal("expected cache hit before TTL")
	}

	time.Sleep(250 * time.Millisecond) // FileTTL is 200ms

	if _, ok := mc.GetEntry("file.txt"); ok {
		t.Fatal("expected cache miss after TTL expiry")
	}
}

func TestDirTTLExpiry(t *testing.T) {
	mc := newTestCache()
	defer mc.Stop()

	entry := CacheEntry{
		S3Key: "mydir/",
		IsDir: true,
		Mode:  os.ModeDir | 0755,
	}
	mc.PutEntry("mydir/", entry)

	if _, ok := mc.GetEntry("mydir/"); !ok {
		t.Fatal("expected cache hit before TTL")
	}

	time.Sleep(150 * time.Millisecond) // DirTTL is 100ms

	if _, ok := mc.GetEntry("mydir/"); ok {
		t.Fatal("expected cache miss after dir TTL expiry")
	}
}

func TestNegativeCache(t *testing.T) {
	mc := newTestCache()
	defer mc.Stop()

	mc.PutNegative("gone.txt")

	got, ok := mc.GetEntry("gone.txt")
	if !ok {
		t.Fatal("expected cache hit for negative entry")
	}
	if !got.IsNegative() {
		t.Fatal("expected entry to be negative")
	}

	time.Sleep(80 * time.Millisecond) // NegativeTTL is 50ms

	if _, ok := mc.GetEntry("gone.txt"); ok {
		t.Fatal("expected negative cache entry to expire")
	}
}

func TestDirListingCache(t *testing.T) {
	mc := newTestCache()
	defer mc.Stop()

	entries := []CacheEntry{
		{S3Key: "dir/a.txt", Size: 10},
		{S3Key: "dir/b.txt", Size: 20},
		{S3Key: "dir/sub/", IsDir: true},
	}

	mc.PutDirListing("dir/", entries)

	got, ok := mc.GetDirListing("dir/")
	if !ok {
		t.Fatal("expected dir listing cache hit")
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(got))
	}

	// Verify it's a copy (mutating result doesn't affect cache).
	got[0].Size = 999
	got2, _ := mc.GetDirListing("dir/")
	if got2[0].Size != 10 {
		t.Error("dir listing cache returned reference, not copy")
	}
}

func TestDirListingTTLExpiry(t *testing.T) {
	mc := newTestCache()
	defer mc.Stop()

	mc.PutDirListing("dir/", []CacheEntry{{S3Key: "dir/a.txt"}})

	time.Sleep(150 * time.Millisecond) // DirTTL is 100ms

	if _, ok := mc.GetDirListing("dir/"); ok {
		t.Fatal("expected dir listing to expire")
	}
}

func TestInvalidate(t *testing.T) {
	mc := newTestCache()
	defer mc.Stop()

	mc.PutEntry("photos/cat.jpg", CacheEntry{S3Key: "photos/cat.jpg", Size: 100})
	mc.PutDirListing("photos/", []CacheEntry{{S3Key: "photos/cat.jpg"}})

	mc.Invalidate("photos/cat.jpg")

	if _, ok := mc.GetEntry("photos/cat.jpg"); ok {
		t.Fatal("expected entry to be invalidated")
	}
	if _, ok := mc.GetDirListing("photos/"); ok {
		t.Fatal("expected parent dir listing to be invalidated")
	}
}

func TestInvalidatePrefix(t *testing.T) {
	mc := newTestCache()
	defer mc.Stop()

	mc.PutEntry("data/a.txt", CacheEntry{S3Key: "data/a.txt"})
	mc.PutEntry("data/b.txt", CacheEntry{S3Key: "data/b.txt"})
	mc.PutEntry("other/c.txt", CacheEntry{S3Key: "other/c.txt"})
	mc.PutDirListing("data/", []CacheEntry{{S3Key: "data/a.txt"}})

	mc.InvalidatePrefix("data/")

	if _, ok := mc.GetEntry("data/a.txt"); ok {
		t.Fatal("expected data/a.txt to be invalidated")
	}
	if _, ok := mc.GetEntry("data/b.txt"); ok {
		t.Fatal("expected data/b.txt to be invalidated")
	}
	if _, ok := mc.GetDirListing("data/"); ok {
		t.Fatal("expected data/ dir listing to be invalidated")
	}

	// other/c.txt should still be cached.
	if _, ok := mc.GetEntry("other/c.txt"); !ok {
		t.Fatal("expected other/c.txt to remain cached")
	}
}

func TestLRUEviction(t *testing.T) {
	cfg := testConfig()
	cfg.MaxEntries = 3
	mc := NewMetadataCache(cfg)
	defer mc.Stop()

	mc.PutEntry("a", CacheEntry{S3Key: "a"})
	mc.PutEntry("b", CacheEntry{S3Key: "b"})
	mc.PutEntry("c", CacheEntry{S3Key: "c"})

	// Access "a" to make it recently used.
	mc.GetEntry("a")

	// Adding "d" should evict "b" (least recently used).
	mc.PutEntry("d", CacheEntry{S3Key: "d"})

	if _, ok := mc.GetEntry("b"); ok {
		t.Fatal("expected 'b' to be evicted (LRU)")
	}
	if _, ok := mc.GetEntry("a"); !ok {
		t.Fatal("expected 'a' to remain (recently accessed)")
	}
	if _, ok := mc.GetEntry("d"); !ok {
		t.Fatal("expected 'd' to be present")
	}
}

func TestUpdateExistingEntry(t *testing.T) {
	mc := newTestCache()
	defer mc.Stop()

	mc.PutEntry("file.txt", CacheEntry{S3Key: "file.txt", Size: 100})
	mc.PutEntry("file.txt", CacheEntry{S3Key: "file.txt", Size: 200})

	got, ok := mc.GetEntry("file.txt")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if got.Size != 200 {
		t.Errorf("expected updated size 200, got %d", got.Size)
	}

	// Should not have created a duplicate entry.
	mc.mu.RLock()
	count := mc.order.Len()
	mc.mu.RUnlock()
	if count != 1 {
		t.Errorf("expected 1 entry in LRU, got %d", count)
	}
}

func TestBackgroundEviction(t *testing.T) {
	cfg := testConfig()
	cfg.FileTTL = 50 * time.Millisecond
	cfg.EvictionInterval = 50 * time.Millisecond
	mc := NewMetadataCache(cfg)
	defer mc.Stop()

	mc.PutEntry("ephemeral.txt", CacheEntry{S3Key: "ephemeral.txt"})

	// Wait for TTL + eviction interval.
	time.Sleep(200 * time.Millisecond)

	mc.mu.RLock()
	_, exists := mc.items["ephemeral.txt"]
	mc.mu.RUnlock()
	if exists {
		t.Fatal("expected background eviction to remove expired entry")
	}
}

func TestParentPrefix(t *testing.T) {
	tests := []struct {
		key  string
		want string
	}{
		{"foo/bar/baz.txt", "foo/bar/"},
		{"foo/bar/", "foo/"},
		{"foo/", ""},
		{"toplevel.txt", ""},
		{"a/b/c/d.txt", "a/b/c/"},
	}

	for _, tt := range tests {
		got := parentPrefix(tt.key)
		if got != tt.want {
			t.Errorf("parentPrefix(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}
