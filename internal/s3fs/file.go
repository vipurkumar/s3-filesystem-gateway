package s3fs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	nfs "github.com/smallfz/libnfs-go/fs"
	s3client "github.com/vipurkumar/s3-filesystem-gateway/internal/s3"
)

// s3File implements libnfs-go fs.File for S3 objects.
type s3File struct {
	mu       sync.Mutex
	fs       *S3FS
	path     string // full POSIX path
	s3Key    string // S3 object key
	info     *fileInfo
	isDir    bool
	offset   int64
	reader   io.ReadCloser // lazy-loaded from S3
	closed   bool
}

var _ nfs.File = (*s3File)(nil)

func (f *s3File) Name() string {
	return f.path
}

func (f *s3File) Stat() (nfs.FileInfo, error) {
	return f.info, nil
}

func (f *s3File) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return 0, os.ErrClosed
	}
	if f.isDir {
		return 0, fmt.Errorf("cannot read directory")
	}

	// Lazy-load reader from S3
	if f.reader == nil {
		reader, _, err := f.fs.s3.GetObject(context.Background(), f.s3Key)
		if err != nil {
			return 0, err
		}
		f.reader = reader

		// Skip to current offset if needed
		if f.offset > 0 {
			if _, err := io.CopyN(io.Discard, f.reader, f.offset); err != nil {
				return 0, fmt.Errorf("seek to offset: %w", err)
			}
		}
	}

	n, err := f.reader.Read(p)
	f.offset += int64(n)
	return n, err
}

func (f *s3File) Write(p []byte) (int, error) {
	// Phase 3: write support via temp file buffering
	return 0, fmt.Errorf("write not supported yet")
}

func (f *s3File) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var newOffset int64
	switch whence {
	case io.SeekStart:
		newOffset = offset
	case io.SeekCurrent:
		newOffset = f.offset + offset
	case io.SeekEnd:
		newOffset = f.info.Size() + offset
	default:
		return 0, fmt.Errorf("invalid whence: %d", whence)
	}

	if newOffset < 0 {
		return 0, fmt.Errorf("negative offset")
	}

	// If we need to seek backwards, close and re-open
	if f.reader != nil && newOffset != f.offset {
		f.reader.Close()
		f.reader = nil
	}

	f.offset = newOffset
	return f.offset, nil
}

func (f *s3File) Truncate() error {
	return fmt.Errorf("truncate not supported yet")
}

func (f *s3File) Sync() error {
	return nil // No-op for read-only; Phase 3 will flush write buffer
}

func (f *s3File) Readdir(count int) ([]nfs.FileInfo, error) {
	if !f.isDir {
		return nil, fmt.Errorf("not a directory")
	}

	prefix := f.s3Key
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	entries, err := f.fs.s3.ListObjects(context.Background(), prefix)
	if err != nil {
		return nil, err
	}

	var infos []nfs.FileInfo
	for _, entry := range entries {
		name := entry.Key
		// Strip the prefix to get the relative name
		name = strings.TrimPrefix(name, prefix)
		name = strings.TrimSuffix(name, "/")

		if name == "" {
			continue // skip self
		}

		isDir := entry.IsDir || strings.HasSuffix(entry.Key, "/")

		inode, err := f.fs.handles.GetOrCreateInode(entry.Key)
		if err != nil {
			return nil, fmt.Errorf("allocate inode for %q: %w", entry.Key, err)
		}

		var meta map[string]string
		if !isDir {
			// Fetch metadata for files to get POSIX attrs
			if objInfo, err := f.fs.s3.HeadObject(context.Background(), entry.Key); err == nil {
				meta = objInfo.UserMetadata
			}
		}

		fi := newFileInfoFromS3(name, entry.Size, entry.LastModified, isDir, inode, meta)
		infos = append(infos, fi)
	}

	if count > 0 && len(infos) > count {
		infos = infos[:count]
	}

	return infos, nil
}

func (f *s3File) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return nil
	}
	f.closed = true

	if f.reader != nil {
		return f.reader.Close()
	}
	return nil
}

// dirFile is a minimal File for directory entries that don't need S3 I/O.
type dirFile struct {
	info *fileInfo
	path string
}

var _ nfs.File = (*dirFile)(nil)

func (f *dirFile) Name() string                      { return f.path }
func (f *dirFile) Stat() (nfs.FileInfo, error)        { return f.info, nil }
func (f *dirFile) Read([]byte) (int, error)           { return 0, fmt.Errorf("cannot read directory") }
func (f *dirFile) Write([]byte) (int, error)          { return 0, fmt.Errorf("cannot write directory") }
func (f *dirFile) Seek(int64, int) (int64, error)     { return 0, nil }
func (f *dirFile) Truncate() error                     { return fmt.Errorf("cannot truncate directory") }
func (f *dirFile) Sync() error                         { return nil }
func (f *dirFile) Readdir(int) ([]nfs.FileInfo, error) { return nil, nil }
func (f *dirFile) Close() error                        { return nil }

// emptyReader is an io.ReadCloser that returns no data.
var emptyReader = io.NopCloser(bytes.NewReader(nil))

// s3KeyFromPath converts a POSIX path to an S3 key.
// Root "/" maps to "", "/foo/bar" maps to "foo/bar".
func s3KeyFromPath(path string) string {
	path = strings.TrimPrefix(path, "/")
	return path
}

// s3DirKey ensures an S3 key ends with "/" for directory operations.
func s3DirKey(key string) string {
	if key == "" {
		return ""
	}
	if !strings.HasSuffix(key, "/") {
		return key + "/"
	}
	return key
}

// nameFromPath returns the base name from a POSIX path.
func nameFromPath(path string) string {
	path = strings.TrimSuffix(path, "/")
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		return path[idx+1:]
	}
	return path
}

// Helper for creating an ObjectInfo from a ListEntry
func listEntryToObjectInfo(entry s3client.ListEntry) *s3client.ObjectInfo {
	return &s3client.ObjectInfo{
		Key:          entry.Key,
		Size:         entry.Size,
		LastModified: entry.LastModified,
		ETag:         entry.ETag,
		IsDir:        entry.IsDir,
	}
}
