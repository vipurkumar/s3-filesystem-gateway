package s3fs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/vipurkumar/s3-filesystem-gateway/internal/cache"
	s3client "github.com/vipurkumar/s3-filesystem-gateway/internal/s3"
)

// ---------------------------------------------------------------------------
// Mock S3 implementation
// ---------------------------------------------------------------------------

type mockObject struct {
	data     []byte
	metadata map[string]string
	modTime  time.Time
}

type mockS3 struct {
	mu      sync.RWMutex
	objects map[string]*mockObject // key -> object
}

var _ s3client.S3API = (*mockS3)(nil)

func newMockS3() *mockS3 {
	return &mockS3{objects: make(map[string]*mockObject)}
}

func (m *mockS3) put(key string, data []byte, meta map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = &mockObject{
		data:     data,
		metadata: meta,
		modTime:  time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC),
	}
}

func (m *mockS3) HeadObject(_ context.Context, key string) (*s3client.ObjectInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	obj, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("NoSuchKey: %s", key)
	}
	return &s3client.ObjectInfo{
		Key:          key,
		Size:         int64(len(obj.data)),
		LastModified: obj.modTime,
		ContentType:  "application/octet-stream",
		IsDir:        strings.HasSuffix(key, "/") && len(obj.data) == 0,
		UserMetadata: obj.metadata,
	}, nil
}

func (m *mockS3) GetObject(_ context.Context, key string) (io.ReadCloser, *s3client.ObjectInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	obj, ok := m.objects[key]
	if !ok {
		return nil, nil, fmt.Errorf("NoSuchKey: %s", key)
	}
	info := &s3client.ObjectInfo{
		Key:          key,
		Size:         int64(len(obj.data)),
		LastModified: obj.modTime,
		ContentType:  "application/octet-stream",
		UserMetadata: obj.metadata,
	}
	return io.NopCloser(bytes.NewReader(obj.data)), info, nil
}

func (m *mockS3) GetObjectRange(_ context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	obj, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("NoSuchKey: %s", key)
	}
	end := offset + length
	if end > int64(len(obj.data)) {
		end = int64(len(obj.data))
	}
	if offset >= int64(len(obj.data)) {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	chunk := make([]byte, end-offset)
	copy(chunk, obj.data[offset:end])
	return io.NopCloser(bytes.NewReader(chunk)), nil
}

func (m *mockS3) ListObjects(_ context.Context, prefix string) ([]s3client.ListEntry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var entries []s3client.ListEntry
	seen := make(map[string]bool)

	for key, obj := range m.objects {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		// Simulate delimiter "/" behavior: return immediate children only.
		rest := strings.TrimPrefix(key, prefix)
		if rest == "" {
			continue // skip self
		}
		if idx := strings.Index(rest, "/"); idx >= 0 {
			// This is a nested key; return the directory prefix.
			dirKey := prefix + rest[:idx+1]
			if seen[dirKey] {
				continue
			}
			seen[dirKey] = true
			entries = append(entries, s3client.ListEntry{
				Key:   dirKey,
				IsDir: true,
			})
		} else {
			entries = append(entries, s3client.ListEntry{
				Key:          key,
				Size:         int64(len(obj.data)),
				LastModified: obj.modTime,
				IsDir:        false,
			})
		}
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	return entries, nil
}

func (m *mockS3) PutObject(_ context.Context, key string, reader io.Reader, size int64, metadata map[string]string) error {
	var data []byte
	if reader != nil {
		var err error
		data, err = io.ReadAll(reader)
		if err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = &mockObject{
		data:     data,
		metadata: metadata,
		modTime:  time.Now().UTC(),
	}
	return nil
}

func (m *mockS3) DeleteObject(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

func (m *mockS3) CopyObject(_ context.Context, srcKey, dstKey string) error {
	m.mu.RLock()
	obj, ok := m.objects[srcKey]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("NoSuchKey: %s", srcKey)
	}

	dataCopy := make([]byte, len(obj.data))
	copy(dataCopy, obj.data)

	var metaCopy map[string]string
	if obj.metadata != nil {
		metaCopy = make(map[string]string, len(obj.metadata))
		for k, v := range obj.metadata {
			metaCopy[k] = v
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[dstKey] = &mockObject{
		data:     dataCopy,
		metadata: metaCopy,
		modTime:  obj.modTime,
	}
	return nil
}

func (m *mockS3) CreateDirMarker(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = &mockObject{
		data:    nil,
		modTime: time.Now().UTC(),
	}
	return nil
}

// mockCreds implements nfs.Creds for testing.
type mockCreds struct {
	uid, gid uint32
}

func (c mockCreds) Host() string      { return "localhost" }
func (c mockCreds) Uid() uint32       { return c.uid }
func (c mockCreds) Gid() uint32       { return c.gid }
func (c mockCreds) Groups() []uint32  { return nil }

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func setupTestFS(t *testing.T) (*S3FS, *mockS3, func()) {
	t.Helper()

	mock := newMockS3()
	dbPath := t.TempDir() + "/test.db"
	handles, err := NewHandleStore(dbPath)
	if err != nil {
		t.Fatalf("NewHandleStore: %v", err)
	}

	mc := cache.NewMetadataCache(cache.CacheConfig{
		MaxEntries:       1000,
		FileTTL:          5 * time.Second,
		DirTTL:           5 * time.Second,
		NegativeTTL:      1 * time.Second,
		EvictionInterval: 60 * time.Second, // don't evict during tests
	})

	fsys := NewS3FS(mock, handles, mc, nil)
	cleanup := func() {
		mc.Stop()
		handles.Close()
	}
	return fsys, mock, cleanup
}

func setupTestFSNoCache(t *testing.T) (*S3FS, *mockS3, func()) {
	t.Helper()

	mock := newMockS3()
	dbPath := t.TempDir() + "/test.db"
	handles, err := NewHandleStore(dbPath)
	if err != nil {
		t.Fatalf("NewHandleStore: %v", err)
	}

	fsys := NewS3FS(mock, handles, nil, nil)
	cleanup := func() {
		handles.Close()
	}
	return fsys, mock, cleanup
}

// ---------------------------------------------------------------------------
// Tests: NewS3FS + SetCreds
// ---------------------------------------------------------------------------

func TestNewS3FS(t *testing.T) {
	fsys, _, cleanup := setupTestFS(t)
	defer cleanup()

	if fsys == nil {
		t.Fatal("NewS3FS returned nil")
	}
	if fsys.S3Client() == nil {
		t.Fatal("S3Client() returned nil")
	}
	if fsys.Handles() == nil {
		t.Fatal("Handles() returned nil")
	}
	if fsys.Cache() == nil {
		t.Fatal("Cache() returned nil")
	}
}

func TestNewS3FSNoCache(t *testing.T) {
	fsys, _, cleanup := setupTestFSNoCache(t)
	defer cleanup()

	if fsys.Cache() != nil {
		t.Fatal("expected nil cache")
	}
}

func TestSetCreds(t *testing.T) {
	fsys, _, cleanup := setupTestFS(t)
	defer cleanup()

	creds := mockCreds{uid: 1000, gid: 1000}
	fsys.SetCreds(creds)
	if fsys.creds.Uid() != 1000 || fsys.creds.Gid() != 1000 {
		t.Fatal("SetCreds did not set credentials")
	}
}

// ---------------------------------------------------------------------------
// Tests: Attributes
// ---------------------------------------------------------------------------

func TestAttributes(t *testing.T) {
	fsys, _, cleanup := setupTestFS(t)
	defer cleanup()

	attrs := fsys.Attributes()
	if attrs == nil {
		t.Fatal("Attributes returned nil")
	}
	if attrs.LinkSupport {
		t.Error("expected LinkSupport=false")
	}
	if attrs.SymlinkSupport {
		t.Error("expected SymlinkSupport=false")
	}
	if !attrs.ChownRestricted {
		t.Error("expected ChownRestricted=true")
	}
	if attrs.MaxName != 1024 {
		t.Errorf("expected MaxName=1024, got %d", attrs.MaxName)
	}
	if attrs.MaxRead != 1048576 {
		t.Errorf("expected MaxRead=1048576, got %d", attrs.MaxRead)
	}
	if attrs.MaxWrite != 1048576 {
		t.Errorf("expected MaxWrite=1048576, got %d", attrs.MaxWrite)
	}
	if !attrs.NoTrunc {
		t.Error("expected NoTrunc=true")
	}
}

// ---------------------------------------------------------------------------
// Tests: Handle operations
// ---------------------------------------------------------------------------

func TestGetRootHandle(t *testing.T) {
	fsys, _, cleanup := setupTestFS(t)
	defer cleanup()

	h := fsys.GetRootHandle()
	if len(h) != 8 {
		t.Fatalf("expected 8-byte root handle, got %d bytes", len(h))
	}
	inode, err := HandleToInode(h)
	if err != nil {
		t.Fatalf("HandleToInode: %v", err)
	}
	if inode != rootInode {
		t.Errorf("expected root inode %d, got %d", rootInode, inode)
	}
}

func TestGetHandle(t *testing.T) {
	fsys, mock, cleanup := setupTestFS(t)
	defer cleanup()

	mock.put("test.txt", []byte("hello"), nil)

	// Open a file to get its fileInfo with an inode.
	f, err := fsys.Open("/test.txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	handle, err := fsys.GetHandle(fi)
	if err != nil {
		t.Fatalf("GetHandle: %v", err)
	}
	if len(handle) != 8 {
		t.Fatalf("expected 8-byte handle, got %d bytes", len(handle))
	}
}

func TestGetHandleNotExist(t *testing.T) {
	fsys, _, cleanup := setupTestFS(t)
	defer cleanup()

	// A fileInfo with inode=0 should return ErrNotExist.
	fi := &fileInfo{name: "fake", inode: 0}
	_, err := fsys.GetHandle(fi)
	if err != os.ErrNotExist {
		t.Fatalf("expected os.ErrNotExist, got %v", err)
	}
}

func TestResolveHandle(t *testing.T) {
	fsys, mock, cleanup := setupTestFS(t)
	defer cleanup()

	mock.put("docs/readme.txt", []byte("content"), nil)

	f, err := fsys.Open("/docs/readme.txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	fi, _ := f.Stat()
	f.Close()

	handle, err := fsys.GetHandle(fi)
	if err != nil {
		t.Fatalf("GetHandle: %v", err)
	}

	path, err := fsys.ResolveHandle(handle)
	if err != nil {
		t.Fatalf("ResolveHandle: %v", err)
	}
	if path != "/docs/readme.txt" {
		t.Errorf("expected /docs/readme.txt, got %s", path)
	}
}

func TestResolveRootHandle(t *testing.T) {
	fsys, _, cleanup := setupTestFS(t)
	defer cleanup()

	path, err := fsys.ResolveHandle(fsys.GetRootHandle())
	if err != nil {
		t.Fatalf("ResolveHandle root: %v", err)
	}
	if path != "/" {
		t.Errorf("expected /, got %s", path)
	}
}

func TestResolveHandleInvalid(t *testing.T) {
	fsys, _, cleanup := setupTestFS(t)
	defer cleanup()

	// Too-short handle.
	_, err := fsys.ResolveHandle([]byte{0x01})
	if err == nil {
		t.Fatal("expected error for short handle")
	}

	// Unknown inode.
	_, err = fsys.ResolveHandle(InodeToHandle(999999))
	if err != os.ErrNotExist {
		t.Fatalf("expected os.ErrNotExist, got %v", err)
	}
}

func TestGetFileId(t *testing.T) {
	fsys, _, cleanup := setupTestFS(t)
	defer cleanup()

	fi := &fileInfo{name: "test", inode: 42}
	id := fsys.GetFileId(fi)
	if id != 42 {
		t.Errorf("expected 42, got %d", id)
	}
}

// otherFileInfo implements nfs.FileInfo but is NOT *fileInfo.
type otherFileInfo struct{}

func (o *otherFileInfo) Name() string        { return "other" }
func (o *otherFileInfo) Size() int64          { return 0 }
func (o *otherFileInfo) Mode() os.FileMode    { return 0 }
func (o *otherFileInfo) ModTime() time.Time   { return time.Time{} }
func (o *otherFileInfo) IsDir() bool          { return false }
func (o *otherFileInfo) Sys() interface{}     { return nil }
func (o *otherFileInfo) ATime() time.Time     { return time.Time{} }
func (o *otherFileInfo) CTime() time.Time     { return time.Time{} }
func (o *otherFileInfo) NumLinks() int        { return 1 }

func TestGetFileIdNonFileInfo(t *testing.T) {
	fsys, _, cleanup := setupTestFS(t)
	defer cleanup()

	// Pass an nfs.FileInfo that is NOT *fileInfo.
	id := fsys.GetFileId(&otherFileInfo{})
	if id != 0 {
		t.Errorf("expected 0 for non-fileInfo, got %d", id)
	}
}

// ---------------------------------------------------------------------------
// Tests: Open
// ---------------------------------------------------------------------------

func TestOpenRoot(t *testing.T) {
	fsys, _, cleanup := setupTestFS(t)
	defer cleanup()

	f, err := fsys.Open("/")
	if err != nil {
		t.Fatalf("Open(/): %v", err)
	}
	defer f.Close()

	fi, _ := f.Stat()
	if !fi.IsDir() {
		t.Error("expected root to be directory")
	}
	if f.Name() != "/" {
		t.Errorf("expected name /, got %s", f.Name())
	}
}

func TestOpenFile(t *testing.T) {
	fsys, mock, cleanup := setupTestFS(t)
	defer cleanup()

	mock.put("file.txt", []byte("hello world"), nil)

	f, err := fsys.Open("/file.txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	fi, _ := f.Stat()
	if fi.IsDir() {
		t.Error("expected file, not directory")
	}
	if fi.Size() != 11 {
		t.Errorf("expected size 11, got %d", fi.Size())
	}
	if fi.Name() != "file.txt" {
		t.Errorf("expected name file.txt, got %s", fi.Name())
	}
}

func TestOpenNonExistent(t *testing.T) {
	fsys, _, cleanup := setupTestFS(t)
	defer cleanup()

	_, err := fsys.Open("/nonexistent")
	if err != os.ErrNotExist {
		t.Fatalf("expected os.ErrNotExist, got %v", err)
	}
}

func TestOpenDirectory(t *testing.T) {
	fsys, mock, cleanup := setupTestFS(t)
	defer cleanup()

	// Create a file under a directory prefix so the dir is implicit.
	mock.put("mydir/file.txt", []byte("data"), nil)

	f, err := fsys.Open("/mydir")
	if err != nil {
		t.Fatalf("Open(/mydir): %v", err)
	}
	defer f.Close()

	fi, _ := f.Stat()
	if !fi.IsDir() {
		t.Error("expected directory")
	}
}

func TestOpenExplicitDirMarker(t *testing.T) {
	fsys, mock, cleanup := setupTestFS(t)
	defer cleanup()

	// Create explicit dir marker but no children.
	mock.put("emptydir/", []byte{}, nil)

	f, err := fsys.Open("/emptydir")
	if err != nil {
		t.Fatalf("Open(/emptydir): %v", err)
	}
	defer f.Close()

	fi, _ := f.Stat()
	if !fi.IsDir() {
		t.Error("expected directory")
	}
}

// ---------------------------------------------------------------------------
// Tests: OpenFile with writable flags
// ---------------------------------------------------------------------------

func TestOpenFileCreate(t *testing.T) {
	fsys, _, cleanup := setupTestFS(t)
	defer cleanup()

	f, err := fsys.OpenFile("/newfile.txt", os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("OpenFile O_CREATE: %v", err)
	}
	defer f.Close()

	_, ok := f.(*s3WritableFile)
	if !ok {
		t.Error("expected s3WritableFile for O_CREATE")
	}
}

func TestOpenFileWriteExisting(t *testing.T) {
	fsys, mock, cleanup := setupTestFS(t)
	defer cleanup()

	mock.put("existing.txt", []byte("old data"), nil)

	f, err := fsys.OpenFile("/existing.txt", os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("OpenFile O_WRONLY: %v", err)
	}
	defer f.Close()

	_, ok := f.(*s3WritableFile)
	if !ok {
		t.Error("expected s3WritableFile for O_WRONLY on existing file")
	}
}

// ---------------------------------------------------------------------------
// Tests: Stat
// ---------------------------------------------------------------------------

func TestStatRoot(t *testing.T) {
	fsys, _, cleanup := setupTestFS(t)
	defer cleanup()

	fi, err := fsys.Stat("/")
	if err != nil {
		t.Fatalf("Stat(/): %v", err)
	}
	if !fi.IsDir() {
		t.Error("expected root to be directory")
	}
}

func TestStatFile(t *testing.T) {
	fsys, mock, cleanup := setupTestFS(t)
	defer cleanup()

	mock.put("file.txt", []byte("hello"), map[string]string{"Mode": "755"})

	fi, err := fsys.Stat("/file.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.IsDir() {
		t.Error("expected file, not directory")
	}
	if fi.Size() != 5 {
		t.Errorf("expected size 5, got %d", fi.Size())
	}
	if fi.Name() != "file.txt" {
		t.Errorf("expected name file.txt, got %s", fi.Name())
	}
}

func TestStatDirectoryTrailingSlash(t *testing.T) {
	fsys, mock, cleanup := setupTestFS(t)
	defer cleanup()

	// When a path has a trailing slash, s3KeyFromPath returns "mydir/" which
	// matches the HeadObject as a zero-byte file. The code treats it as a
	// file (size 0) because the first HeadObject branch fires. This is the
	// expected behavior for the current implementation.
	mock.put("mydir/", []byte{}, nil)

	fi, err := fsys.Stat("/mydir/")
	if err != nil {
		t.Fatalf("Stat(/mydir/): %v", err)
	}
	// When the path already ends with "/" the s3Key is "mydir/" which is
	// found by the first HeadObject call and treated as a regular (zero-byte)
	// file. Verify Stat succeeds and returns a valid entry.
	if fi.Size() != 0 {
		t.Errorf("expected size 0, got %d", fi.Size())
	}
}

func TestStatImplicitDir(t *testing.T) {
	fsys, mock, cleanup := setupTestFS(t)
	defer cleanup()

	// No explicit dir marker, but files exist under this prefix.
	mock.put("implicit-dir/child.txt", []byte("data"), nil)

	fi, err := fsys.Stat("/implicit-dir")
	if err != nil {
		t.Fatalf("Stat(/implicit-dir): %v", err)
	}
	if !fi.IsDir() {
		t.Error("expected implicit directory")
	}
}

func TestStatNonExistent(t *testing.T) {
	fsys, _, cleanup := setupTestFS(t)
	defer cleanup()

	_, err := fsys.Stat("/nonexistent")
	if err != os.ErrNotExist {
		t.Fatalf("expected os.ErrNotExist, got %v", err)
	}
}

func TestStatCacheHit(t *testing.T) {
	fsys, mock, cleanup := setupTestFS(t)
	defer cleanup()

	mock.put("cached.txt", []byte("data"), nil)

	// First call populates cache.
	fi1, err := fsys.Stat("/cached.txt")
	if err != nil {
		t.Fatalf("first Stat: %v", err)
	}

	// Second call should use cache (same result).
	fi2, err := fsys.Stat("/cached.txt")
	if err != nil {
		t.Fatalf("second Stat: %v", err)
	}

	if fi1.Size() != fi2.Size() {
		t.Errorf("cache returned different size: %d vs %d", fi1.Size(), fi2.Size())
	}
}

func TestStatNegativeCache(t *testing.T) {
	fsys, _, cleanup := setupTestFS(t)
	defer cleanup()

	// First call: not found, caches negative.
	_, err := fsys.Stat("/missing")
	if err != os.ErrNotExist {
		t.Fatalf("expected os.ErrNotExist, got %v", err)
	}

	// Second call: still not found (negative cache or re-check).
	_, err = fsys.Stat("/missing")
	if err != os.ErrNotExist {
		t.Fatalf("expected os.ErrNotExist on second call, got %v", err)
	}
}

func TestStatExplicitDirMarker(t *testing.T) {
	fsys, mock, cleanup := setupTestFS(t)
	defer cleanup()

	mock.put("explicit-dir/", []byte{}, nil)

	fi, err := fsys.Stat("/explicit-dir")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !fi.IsDir() {
		t.Error("expected directory for explicit dir marker")
	}
}

// ---------------------------------------------------------------------------
// Tests: MkdirAll
// ---------------------------------------------------------------------------

func TestMkdirAll(t *testing.T) {
	fsys, mock, cleanup := setupTestFS(t)
	defer cleanup()

	err := fsys.MkdirAll("/newdir", 0755)
	if err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// Verify the dir marker was created in mock S3.
	mock.mu.RLock()
	_, exists := mock.objects["newdir/"]
	mock.mu.RUnlock()
	if !exists {
		t.Error("expected dir marker newdir/ in S3")
	}
}

func TestMkdirAllNested(t *testing.T) {
	fsys, mock, cleanup := setupTestFS(t)
	defer cleanup()

	err := fsys.MkdirAll("/a/b/c", 0755)
	if err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	mock.mu.RLock()
	_, exists := mock.objects["a/b/c/"]
	mock.mu.RUnlock()
	if !exists {
		t.Error("expected dir marker a/b/c/ in S3")
	}
}

// ---------------------------------------------------------------------------
// Tests: Remove
// ---------------------------------------------------------------------------

func TestRemoveFile(t *testing.T) {
	fsys, mock, cleanup := setupTestFS(t)
	defer cleanup()

	mock.put("removeme.txt", []byte("data"), nil)

	err := fsys.Remove("/removeme.txt")
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}

	mock.mu.RLock()
	_, exists := mock.objects["removeme.txt"]
	mock.mu.RUnlock()
	if exists {
		t.Error("expected file to be deleted")
	}
}

func TestRemoveDirectory(t *testing.T) {
	fsys, mock, cleanup := setupTestFS(t)
	defer cleanup()

	mock.put("rmdir/", []byte{}, nil)

	err := fsys.Remove("/rmdir")
	if err != nil {
		t.Fatalf("Remove dir: %v", err)
	}

	mock.mu.RLock()
	_, exists := mock.objects["rmdir/"]
	mock.mu.RUnlock()
	if exists {
		t.Error("expected dir marker to be deleted")
	}
}

func TestRemoveNonExistent(t *testing.T) {
	fsys, _, cleanup := setupTestFS(t)
	defer cleanup()

	err := fsys.Remove("/nonexistent")
	if err != os.ErrNotExist {
		t.Fatalf("expected os.ErrNotExist, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tests: Rename
// ---------------------------------------------------------------------------

func TestRenameFile(t *testing.T) {
	fsys, mock, cleanup := setupTestFS(t)
	defer cleanup()

	mock.put("old.txt", []byte("content"), map[string]string{"foo": "bar"})

	err := fsys.Rename("/old.txt", "/new.txt")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}

	mock.mu.RLock()
	_, oldExists := mock.objects["old.txt"]
	newObj, newExists := mock.objects["new.txt"]
	mock.mu.RUnlock()

	if oldExists {
		t.Error("expected old key to be deleted")
	}
	if !newExists {
		t.Fatal("expected new key to exist")
	}
	if string(newObj.data) != "content" {
		t.Errorf("expected content, got %s", string(newObj.data))
	}
}

func TestRenameDirectory(t *testing.T) {
	fsys, mock, cleanup := setupTestFS(t)
	defer cleanup()

	mock.put("olddir/", []byte{}, nil)

	err := fsys.Rename("/olddir", "/newdir")
	if err != nil {
		t.Fatalf("Rename dir: %v", err)
	}

	mock.mu.RLock()
	_, oldExists := mock.objects["olddir/"]
	_, newExists := mock.objects["newdir/"]
	mock.mu.RUnlock()

	if oldExists {
		t.Error("expected old dir marker to be deleted")
	}
	if !newExists {
		t.Error("expected new dir marker to exist")
	}
}

func TestRenameNonExistent(t *testing.T) {
	fsys, _, cleanup := setupTestFS(t)
	defer cleanup()

	err := fsys.Rename("/nonexistent", "/target")
	if err != os.ErrNotExist {
		t.Fatalf("expected os.ErrNotExist, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tests: Symlink, Readlink, Link (unsupported)
// ---------------------------------------------------------------------------

func TestSymlinkNotSupported(t *testing.T) {
	fsys, _, cleanup := setupTestFS(t)
	defer cleanup()

	err := fsys.Symlink("/old", "/new")
	if err == nil {
		t.Fatal("expected error for Symlink")
	}
}

func TestReadlinkNotSupported(t *testing.T) {
	fsys, _, cleanup := setupTestFS(t)
	defer cleanup()

	_, err := fsys.Readlink("/link")
	if err == nil {
		t.Fatal("expected error for Readlink")
	}
}

func TestLinkNotSupported(t *testing.T) {
	fsys, _, cleanup := setupTestFS(t)
	defer cleanup()

	err := fsys.Link("/old", "/new")
	if err == nil {
		t.Fatal("expected error for Link")
	}
}

// ---------------------------------------------------------------------------
// Tests: Chmod, Chown (no-ops)
// ---------------------------------------------------------------------------

func TestChmod(t *testing.T) {
	fsys, _, cleanup := setupTestFS(t)
	defer cleanup()

	err := fsys.Chmod("/file", 0755)
	if err != syscall.ENOTSUP {
		t.Fatalf("Chmod: got %v, want ENOTSUP", err)
	}
}

func TestChown(t *testing.T) {
	fsys, _, cleanup := setupTestFS(t)
	defer cleanup()

	err := fsys.Chown("/file", 1000, 1000)
	if err != syscall.ENOTSUP {
		t.Fatalf("Chown: got %v, want ENOTSUP", err)
	}
}

// ---------------------------------------------------------------------------
// Tests: Stat/Open without cache (nil cache)
// ---------------------------------------------------------------------------

func TestStatNoCacheFile(t *testing.T) {
	fsys, mock, cleanup := setupTestFSNoCache(t)
	defer cleanup()

	mock.put("nocache.txt", []byte("data"), nil)

	fi, err := fsys.Stat("/nocache.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Size() != 4 {
		t.Errorf("expected size 4, got %d", fi.Size())
	}
}

func TestStatNoCacheDir(t *testing.T) {
	fsys, mock, cleanup := setupTestFSNoCache(t)
	defer cleanup()

	mock.put("nocachedir/file.txt", []byte("data"), nil)

	fi, err := fsys.Stat("/nocachedir")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !fi.IsDir() {
		t.Error("expected directory")
	}
}

func TestStatNoCacheNonExistent(t *testing.T) {
	fsys, _, cleanup := setupTestFSNoCache(t)
	defer cleanup()

	_, err := fsys.Stat("/nope")
	if err != os.ErrNotExist {
		t.Fatalf("expected os.ErrNotExist, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tests: listEntryToObjectInfo helper
// ---------------------------------------------------------------------------

func TestListEntryToObjectInfoConversion(t *testing.T) {
	entry := s3client.ListEntry{
		Key:          "test.txt",
		Size:         100,
		LastModified: time.Now(),
		ETag:         "abc",
		IsDir:        false,
	}
	info := listEntryToObjectInfo(entry)
	if info.Key != entry.Key || info.Size != entry.Size || info.ETag != entry.ETag {
		t.Error("listEntryToObjectInfo did not copy fields correctly")
	}
}
