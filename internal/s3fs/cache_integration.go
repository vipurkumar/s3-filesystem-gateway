// Copyright 2024 s3-filesystem-gateway contributors
// SPDX-License-Identifier: Apache-2.0

package s3fs

import (
	"os"
	"strings"
	"time"

	"github.com/vipurkumar/s3-filesystem-gateway/internal/cache"
)

// cacheGet looks up s3Key in the metadata cache and returns a fileInfo if found.
// Returns (nil, false) on cache miss. Returns (nil, true) for negative entries
// (caller should treat as os.ErrNotExist).
func (fs *S3FS) cacheGet(s3Key string) (*fileInfo, bool) {
	if fs.cache == nil {
		return nil, false
	}

	entry, ok := fs.cache.GetEntry(s3Key)
	if !ok {
		return nil, false
	}

	if entry.IsNegative() {
		// Negative cache hit — object does not exist.
		return nil, true
	}

	inode, err := fs.handles.GetOrCreateInode(s3Key)
	if err != nil {
		return nil, false
	}

	numLinks := 1
	if entry.IsDir {
		numLinks = 2
	}

	fi := &fileInfo{
		name:     nameFromS3Key(s3Key),
		size:     entry.Size,
		mode:     entry.Mode,
		modTime:  entry.ModTime,
		isDir:    entry.IsDir,
		inode:    inode,
		numLinks: numLinks,
	}
	return fi, true
}

// cachePut stores file metadata in the cache.
func (fs *S3FS) cachePut(s3Key string, info *fileInfo) {
	if fs.cache == nil {
		return
	}

	fs.cache.PutEntry(s3Key, cache.CacheEntry{
		S3Key:   s3Key,
		Size:    info.size,
		ModTime: info.modTime,
		Mode:    info.mode,
		IsDir:   info.isDir,
		Inode:   info.inode,
	})
}

// cachePutNegative stores a negative (not-found) entry in the cache.
func (fs *S3FS) cachePutNegative(s3Key string) {
	if fs.cache == nil {
		return
	}
	fs.cache.PutNegative(s3Key)
}

// cacheInvalidate removes the cached entry for the given S3 key.
func (fs *S3FS) cacheInvalidate(s3Key string) {
	if fs.cache == nil {
		return
	}
	fs.cache.Invalidate(s3Key)
}

// dataCacheInvalidate removes all cached data for the given S3 key.
func (fs *S3FS) dataCacheInvalidate(s3Key string) {
	if fs.dataCache == nil {
		return
	}
	fs.dataCache.Invalidate(s3Key)
}

// cacheInvalidateParent invalidates the parent directory listing for the given
// S3 key so that subsequent Readdir calls will re-fetch from S3.
func (fs *S3FS) cacheInvalidateParent(s3Key string) {
	if fs.cache == nil {
		return
	}
	parent := parentPrefix(s3Key)
	fs.cache.Invalidate(parent)
}

// cacheGetDirListing returns a cached directory listing for the given prefix.
func (fs *S3FS) cacheGetDirListing(prefix string) ([]cache.CacheEntry, bool) {
	if fs.cache == nil {
		return nil, false
	}
	return fs.cache.GetDirListing(prefix)
}

// cachePutDirListing stores a directory listing in the cache.
func (fs *S3FS) cachePutDirListing(prefix string, entries []cache.CacheEntry) {
	if fs.cache == nil {
		return
	}
	fs.cache.PutDirListing(prefix, entries)
}

// parentPrefix returns the parent directory's S3 prefix for the given key.
// For example: "foo/bar/baz.txt" -> "foo/bar/", "foo/" -> "".
func parentPrefix(s3Key string) string {
	key := strings.TrimSuffix(s3Key, "/")
	idx := strings.LastIndex(key, "/")
	if idx < 0 {
		return ""
	}
	return key[:idx+1]
}

// nameFromS3Key extracts the base name from an S3 key.
func nameFromS3Key(s3Key string) string {
	key := strings.TrimSuffix(s3Key, "/")
	if idx := strings.LastIndex(key, "/"); idx >= 0 {
		return key[idx+1:]
	}
	return key
}

// Cache returns the metadata cache (may be nil).
func (fs *S3FS) Cache() *cache.MetadataCache {
	return fs.cache
}

// dirListingToCacheEntries converts directory listing fileInfos to cache entries
// for storage in the directory listing cache.
func dirListingToCacheEntries(infos []*fileInfo) []cache.CacheEntry {
	entries := make([]cache.CacheEntry, len(infos))
	for i, fi := range infos {
		entries[i] = cache.CacheEntry{
			Size:     fi.size,
			ModTime:  fi.modTime,
			Mode:     fi.mode,
			IsDir:    fi.isDir,
			Inode:    fi.inode,
			CachedAt: time.Now(),
		}
	}
	return entries
}

// cacheEntryToFileInfo converts a CacheEntry to a fileInfo with the given name.
func cacheEntryToFileInfo(entry cache.CacheEntry, name string, inode uint64) *fileInfo {
	numLinks := 1
	if entry.IsDir {
		numLinks = 2
	}
	mode := entry.Mode
	if mode == 0 {
		if entry.IsDir {
			mode = os.ModeDir | DefaultDirMode
		} else {
			mode = DefaultFileMode
		}
	}
	return &fileInfo{
		name:     name,
		size:     entry.Size,
		mode:     mode,
		modTime:  entry.ModTime,
		isDir:    entry.IsDir,
		inode:    inode,
		numLinks: numLinks,
	}
}
