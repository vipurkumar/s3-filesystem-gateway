package s3fs

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vipurkumar/s3-filesystem-gateway/internal/cache"
)

// newTestS3FS creates an S3FS with a real HandleStore and optional MetadataCache.
// The S3 client is nil since we only test cache methods.
func newTestS3FS(t *testing.T, mc *cache.MetadataCache) *S3FS {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	handles, err := NewHandleStore(dbPath)
	if err != nil {
		t.Fatalf("NewHandleStore: %v", err)
	}
	t.Cleanup(func() { handles.Close() })
	return NewS3FS(nil, handles, mc, nil)
}

func newTestCache(t *testing.T) *cache.MetadataCache {
	t.Helper()
	mc := cache.NewMetadataCache(cache.CacheConfig{
		MaxEntries:       1000,
		FileTTL:          5 * time.Minute,
		DirTTL:           1 * time.Minute,
		NegativeTTL:      10 * time.Second,
		EvictionInterval: 1 * time.Hour, // long interval so it doesn't interfere
	})
	t.Cleanup(func() { mc.Stop() })
	return mc
}

// --- nil cache: all methods are no-ops ---

func TestCacheMethods_NilCache_NoOps(t *testing.T) {
	fs := newTestS3FS(t, nil)

	// None of these should panic
	fi, ok := fs.cacheGet("any-key")
	if fi != nil || ok {
		t.Error("cacheGet with nil cache should return (nil, false)")
	}

	fs.cachePut("key", &fileInfo{name: "x"})
	fs.cachePutNegative("key")
	fs.cacheInvalidate("key")
	fs.cacheInvalidateParent("foo/bar")

	entries, ok := fs.cacheGetDirListing("prefix/")
	if entries != nil || ok {
		t.Error("cacheGetDirListing with nil cache should return (nil, false)")
	}

	fs.cachePutDirListing("prefix/", []cache.CacheEntry{{S3Key: "x"}})

	if fs.Cache() != nil {
		t.Error("Cache() should return nil")
	}
}

// --- real cache tests ---

func TestCachePutAndGet(t *testing.T) {
	mc := newTestCache(t)
	fs := newTestS3FS(t, mc)

	now := time.Now()
	info := &fileInfo{
		name:    "test.txt",
		size:    1234,
		mode:    0644,
		modTime: now,
		isDir:   false,
		inode:   10,
	}

	fs.cachePut("foo/test.txt", info)

	got, ok := fs.cacheGet("foo/test.txt")
	if !ok {
		t.Fatal("cacheGet returned false after cachePut")
	}
	if got == nil {
		t.Fatal("cacheGet returned nil fileInfo")
	}
	if got.Name() != "test.txt" {
		t.Errorf("Name() = %q, want %q", got.Name(), "test.txt")
	}
	if got.Size() != 1234 {
		t.Errorf("Size() = %d, want 1234", got.Size())
	}
	if got.IsDir() {
		t.Error("should not be dir")
	}
}

func TestCachePutNegativeAndGet(t *testing.T) {
	mc := newTestCache(t)
	fs := newTestS3FS(t, mc)

	fs.cachePutNegative("missing/file.txt")

	got, ok := fs.cacheGet("missing/file.txt")
	if !ok {
		t.Fatal("cacheGet should return true for negative entry")
	}
	if got != nil {
		t.Fatal("cacheGet should return nil fileInfo for negative entry")
	}
}

func TestCacheGet_Miss(t *testing.T) {
	mc := newTestCache(t)
	fs := newTestS3FS(t, mc)

	got, ok := fs.cacheGet("never-stored")
	if ok {
		t.Fatal("cacheGet should return false for miss")
	}
	if got != nil {
		t.Fatal("cacheGet should return nil for miss")
	}
}

func TestCacheInvalidate(t *testing.T) {
	mc := newTestCache(t)
	fs := newTestS3FS(t, mc)

	fs.cachePut("to-invalidate", &fileInfo{name: "x", size: 1})
	fs.cacheInvalidate("to-invalidate")

	got, ok := fs.cacheGet("to-invalidate")
	if ok || got != nil {
		t.Error("entry should be gone after invalidate")
	}
}

func TestCacheInvalidateParent(t *testing.T) {
	mc := newTestCache(t)
	fs := newTestS3FS(t, mc)

	// cacheInvalidateParent computes parentPrefix of the key, then calls
	// cache.Invalidate on that prefix. cache.Invalidate removes the items
	// entry and the parent directory listing of the *given* key.
	// So cacheInvalidateParent("parent/child1.txt") calls
	// cache.Invalidate("parent/") which removes items["parent/"] and
	// dirListings[parentPrefix("parent/")] = dirListings[""].

	// Store a root-level dir listing.
	rootEntries := []cache.CacheEntry{
		{S3Key: "parent/", Size: 0, IsDir: true},
	}
	fs.cachePutDirListing("", rootEntries)

	// Also cache "parent/" as an item.
	fs.cachePut("parent/", &fileInfo{name: "parent", isDir: true, mode: 0755})

	// Invalidate the parent of "parent/child1.txt" -> invalidates "parent/"
	fs.cacheInvalidateParent("parent/child1.txt")

	// The items entry for "parent/" should be gone.
	got, ok := fs.cacheGet("parent/")
	if ok && got != nil {
		t.Error("parent/ entry should be invalidated from items cache")
	}

	// The root dir listing should also be gone (grandparent listing).
	rootGot, rootOk := fs.cacheGetDirListing("")
	if rootOk {
		t.Errorf("root dir listing should be invalidated, got %d entries", len(rootGot))
	}
}

func TestCacheDirListing_Roundtrip(t *testing.T) {
	mc := newTestCache(t)
	fs := newTestS3FS(t, mc)

	entries := []cache.CacheEntry{
		{S3Key: "dir/a.txt", Size: 100, IsDir: false},
		{S3Key: "dir/sub/", Size: 0, IsDir: true},
	}
	fs.cachePutDirListing("dir/", entries)

	got, ok := fs.cacheGetDirListing("dir/")
	if !ok {
		t.Fatal("expected cache hit for dir listing")
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if got[0].S3Key != "dir/a.txt" {
		t.Errorf("entry[0].S3Key = %q", got[0].S3Key)
	}
	if !got[1].IsDir {
		t.Error("entry[1] should be a dir")
	}
}

func TestCacheReturnsCache(t *testing.T) {
	mc := newTestCache(t)
	fs := newTestS3FS(t, mc)
	if fs.Cache() != mc {
		t.Error("Cache() should return the metadata cache")
	}
}

// --- helper function tests ---

func TestParentPrefix_CacheIntegration(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"foo/bar/baz.txt", "foo/bar/"},
		{"foo.txt", ""},
		{"", ""},
		{"foo/", ""},
		{"a/b/c/", "a/b/"},
	}
	for _, tc := range tests {
		got := parentPrefix(tc.input)
		if got != tc.want {
			t.Errorf("parentPrefix(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestNameFromS3Key_CacheIntegration(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"foo/bar/baz.txt", "baz.txt"},
		{"foo/", "foo"},
		{"", ""},
		{"simple.txt", "simple.txt"},
		{"a/b/c/", "c"},
	}
	for _, tc := range tests {
		got := nameFromS3Key(tc.input)
		if got != tc.want {
			t.Errorf("nameFromS3Key(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestCacheEntryToFileInfo(t *testing.T) {
	entry := cache.CacheEntry{
		Size:    512,
		ModTime: time.Now(),
		Mode:    0644,
		IsDir:   false,
		Inode:   99,
	}
	fi := cacheEntryToFileInfo(entry, "test.txt", 99)

	if fi.Name() != "test.txt" {
		t.Errorf("Name() = %q", fi.Name())
	}
	if fi.Size() != 512 {
		t.Errorf("Size() = %d", fi.Size())
	}
	if fi.Mode() != 0644 {
		t.Errorf("Mode() = %v", fi.Mode())
	}
	if fi.NumLinks() != 1 {
		t.Errorf("NumLinks() = %d, want 1", fi.NumLinks())
	}
}

func TestCacheEntryToFileInfo_Dir(t *testing.T) {
	entry := cache.CacheEntry{
		IsDir: true,
		Mode:  os.ModeDir | 0755,
	}
	fi := cacheEntryToFileInfo(entry, "mydir", 50)
	if fi.NumLinks() != 2 {
		t.Errorf("NumLinks() = %d, want 2", fi.NumLinks())
	}
	if !fi.IsDir() {
		t.Error("should be dir")
	}
}

func TestCacheEntryToFileInfo_ZeroMode_File(t *testing.T) {
	entry := cache.CacheEntry{
		Mode:  0,
		IsDir: false,
	}
	fi := cacheEntryToFileInfo(entry, "f.txt", 1)
	if fi.Mode() != DefaultFileMode {
		t.Errorf("Mode() = %v, want %v", fi.Mode(), DefaultFileMode)
	}
}

func TestCacheEntryToFileInfo_ZeroMode_Dir(t *testing.T) {
	entry := cache.CacheEntry{
		Mode:  0,
		IsDir: true,
	}
	fi := cacheEntryToFileInfo(entry, "d", 1)
	if fi.Mode() != os.ModeDir|DefaultDirMode {
		t.Errorf("Mode() = %v, want %v", fi.Mode(), os.ModeDir|DefaultDirMode)
	}
}

func TestDirListingToCacheEntries(t *testing.T) {
	now := time.Now()
	infos := []*fileInfo{
		{name: "a.txt", size: 100, mode: 0644, modTime: now, isDir: false, inode: 10},
		{name: "sub", size: 0, mode: os.ModeDir | 0755, modTime: now, isDir: true, inode: 20},
	}

	entries := dirListingToCacheEntries(infos)
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}

	if entries[0].Size != 100 || entries[0].IsDir {
		t.Errorf("entry[0] mismatch: size=%d isDir=%v", entries[0].Size, entries[0].IsDir)
	}
	if entries[1].Inode != 20 || !entries[1].IsDir {
		t.Errorf("entry[1] mismatch: inode=%d isDir=%v", entries[1].Inode, entries[1].IsDir)
	}
	if entries[0].CachedAt.IsZero() {
		t.Error("CachedAt should be set")
	}
}
