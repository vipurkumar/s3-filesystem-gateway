// Copyright 2024 s3-filesystem-gateway contributors
// SPDX-License-Identifier: Apache-2.0

package s3fs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	nfs "github.com/smallfz/libnfs-go/fs"
	"github.com/vipurkumar/s3-filesystem-gateway/internal/cache"
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
	chunked  *chunkReader // ranged-read reader with adaptive prefetch
	closed   bool
	etag     string
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

	// Lazy-init the chunk reader.
	if f.chunked == nil {
		f.chunked = newChunkReader(f.fs.s3, f.s3Key, f.info.Size(), f.fs.dataCache, f.etag)
	}

	n, err := f.chunked.ReadAt(p, f.offset)
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

	// Reposition the chunk reader (invalidates buffer only if needed).
	if f.chunked != nil && newOffset != f.offset {
		if err := f.chunked.Seek(newOffset); err != nil {
			return 0, err
		}
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

	// Check directory listing cache first.
	if cachedEntries, ok := f.fs.cacheGetDirListing(prefix); ok {
		var infos []nfs.FileInfo
		for _, ce := range cachedEntries {
			name := nameFromS3Key(ce.S3Key)
			if name == "" {
				continue
			}
			inode, err := f.fs.handles.GetOrCreateInode(ce.S3Key)
			if err != nil {
				return nil, fmt.Errorf("allocate inode for %q: %w", ce.S3Key, err)
			}
			fi := cacheEntryToFileInfo(ce, name, inode)
			infos = append(infos, fi)
		}
		if count > 0 && len(infos) > count {
			infos = infos[:count]
		}
		return infos, nil
	}

	entries, err := f.fs.s3.ListObjects(context.Background(), prefix)
	if err != nil {
		return nil, err
	}

	var infos []nfs.FileInfo
	var cacheEntries []cache.CacheEntry
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

		// Also populate individual entry cache and build dir listing cache entry.
		f.fs.cachePut(entry.Key, fi)
		cacheEntries = append(cacheEntries, cache.CacheEntry{
			S3Key:   entry.Key,
			Size:    fi.size,
			ModTime: fi.modTime,
			Mode:    fi.mode,
			IsDir:   fi.isDir,
			Inode:   fi.inode,
		})
	}

	// Store the directory listing in cache.
	f.fs.cachePutDirListing(prefix, cacheEntries)

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

	if f.chunked != nil {
		return f.chunked.Close()
	}
	return nil
}

// s3WritableFile buffers writes to a local temp file and uploads to S3 on Close.
type s3WritableFile struct {
	mu      sync.Mutex
	fs      *S3FS
	path    string // full POSIX path
	s3Key   string // S3 object key
	info    *fileInfo
	tmp     *os.File // local temp file for buffering writes
	closed  bool
}

var _ nfs.File = (*s3WritableFile)(nil)

func newWritableFile(fsys *S3FS, path, s3Key string, info *fileInfo) (*s3WritableFile, error) {
	tmp, err := os.CreateTemp(os.TempDir(), "s3gw-*")
	if err != nil {
		return nil, fmt.Errorf("create temp file: %w", err)
	}
	return &s3WritableFile{
		fs:    fsys,
		path:  path,
		s3Key: s3Key,
		info:  info,
		tmp:   tmp,
	}, nil
}

func (f *s3WritableFile) Name() string {
	return f.path
}

func (f *s3WritableFile) Stat() (nfs.FileInfo, error) {
	return f.info, nil
}

func (f *s3WritableFile) Read(p []byte) (int, error) {
	return 0, fmt.Errorf("cannot read a write-only file")
}

func (f *s3WritableFile) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return 0, os.ErrClosed
	}
	return f.tmp.Write(p)
}

func (f *s3WritableFile) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return 0, os.ErrClosed
	}
	return f.tmp.Seek(offset, whence)
}

func (f *s3WritableFile) Truncate() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return os.ErrClosed
	}
	return f.tmp.Truncate(0)
}

func (f *s3WritableFile) Sync() error {
	return nil // upload happens on Close
}

func (f *s3WritableFile) Readdir(count int) ([]nfs.FileInfo, error) {
	return nil, fmt.Errorf("not a directory")
}

func (f *s3WritableFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return nil
	}
	f.closed = true

	defer func() {
		f.tmp.Close()
		os.Remove(f.tmp.Name())
	}()

	// Get file size
	stat, err := f.tmp.Stat()
	if err != nil {
		return fmt.Errorf("stat temp file: %w", err)
	}
	size := stat.Size()

	// Seek to beginning for upload
	if _, err := f.tmp.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek temp file: %w", err)
	}

	// Build POSIX metadata
	meta := posixMetadata(DefaultUID, DefaultGID, DefaultFileMode)

	// Upload to S3
	if err := f.fs.s3.PutObject(context.Background(), f.s3Key, f.tmp, size, meta); err != nil {
		return fmt.Errorf("upload to S3: %w", err)
	}

	// Invalidate cache so subsequent reads see the new data.
	f.fs.cacheInvalidate(f.s3Key)
	f.fs.cacheInvalidateParent(f.s3Key)
	f.fs.dataCacheInvalidate(f.s3Key)

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
	cleaned := filepath.Clean(path)
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "." {
		return ""
	}
	// Reject path traversal attempts
	for _, part := range strings.Split(cleaned, "/") {
		if part == ".." {
			return ""
		}
	}
	return cleaned
}

// s3DirKey ensures an S3 key ends with "/" for directory operations.
func s3DirKey(key string) string {
	if key == "" {
		return ""
	}
	// Reject path traversal attempts
	for _, part := range strings.Split(key, "/") {
		if part == ".." {
			return ""
		}
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
