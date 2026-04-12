// Copyright 2024 s3-filesystem-gateway contributors
// SPDX-License-Identifier: Apache-2.0

package s3fs

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"time"

	nfs "github.com/smallfz/libnfs-go/fs"
	"github.com/vipurkumar/s3-filesystem-gateway/internal/cache"
	s3client "github.com/vipurkumar/s3-filesystem-gateway/internal/s3"
)

// S3FS implements the libnfs-go fs.FS interface backed by S3.
type S3FS struct {
	s3        s3client.S3API
	handles   *HandleStore
	cache     *cache.MetadataCache
	dataCache *cache.DataCache
	creds     nfs.Creds
}

var _ nfs.FS = (*S3FS)(nil)

// NewS3FS creates a new S3-backed filesystem. The mc parameter may be nil to
// disable metadata caching. The dc parameter may be nil to disable data caching.
func NewS3FS(s3 s3client.S3API, handles *HandleStore, mc *cache.MetadataCache, dc *cache.DataCache) *S3FS {
	return &S3FS{
		s3:        s3,
		handles:   handles,
		cache:     mc,
		dataCache: dc,
	}
}

// SetCreds is called by libnfs-go before each operation with client credentials.
func (fs *S3FS) SetCreds(creds nfs.Creds) {
	fs.creds = creds
}

// Attributes returns the filesystem's NFSv4 attributes.
func (fs *S3FS) Attributes() *nfs.Attributes {
	return &nfs.Attributes{
		LinkSupport:     false, // S3 doesn't support hard links
		SymlinkSupport:  false, // S3 doesn't support symlinks
		ChownRestricted: true,
		MaxName:         1024,    // S3 key max is 1024 bytes
		MaxRead:         1048576, // 1MB
		MaxWrite:        1048576, // 1MB
		NoTrunc:         true,
	}
}

// GetRootHandle returns the NFS handle for the root directory.
func (fs *S3FS) GetRootHandle() []byte {
	return RootHandle()
}

// GetHandle returns the NFS handle for a file identified by FileInfo.
func (fs *S3FS) GetHandle(fi nfs.FileInfo) ([]byte, error) {
	id := fs.GetFileId(fi)
	if id == 0 {
		return nil, os.ErrNotExist
	}
	return InodeToHandle(id), nil
}

// ResolveHandle translates an NFS handle to a full POSIX path.
func (fs *S3FS) ResolveHandle(handle []byte) (string, error) {
	inode, err := HandleToInode(handle)
	if err != nil {
		return "", err
	}

	key, ok := fs.handles.GetKey(inode)
	if !ok {
		return "", os.ErrNotExist
	}

	if key == "" {
		return "/", nil
	}

	path := "/" + strings.TrimSuffix(key, "/")
	return path, nil
}

// GetFileId returns the unique inode number for a file.
func (fs *S3FS) GetFileId(fi nfs.FileInfo) uint64 {
	if info, ok := fi.(*fileInfo); ok {
		return info.inode
	}
	return 0
}

// Open opens a file for reading.
func (fs *S3FS) Open(path string) (nfs.File, error) {
	return fs.OpenFile(path, os.O_RDONLY, 0)
}

// OpenFile opens a file with the given flags and mode.
func (fs *S3FS) OpenFile(path string, flags int, perm os.FileMode) (nfs.File, error) {
	s3Key := s3KeyFromPath(path)
	writable := flags&(os.O_WRONLY|os.O_RDWR|os.O_CREATE) != 0

	// Root directory
	if s3Key == "" {
		inode, _ := fs.handles.GetOrCreateInode("")
		info := newDirInfo("/", inode)
		return &s3File{
			fs:    fs,
			path:  "/",
			s3Key: "",
			info:  info,
			isDir: true,
		}, nil
	}

	// Try as file first
	objInfo, err := fs.s3.HeadObject(context.Background(), s3Key)
	if err == nil {
		inode, err := fs.handles.GetOrCreateInode(s3Key)
		if err != nil {
			return nil, err
		}
		info := newFileInfoFromS3WithETag(nameFromPath(path), objInfo.Size, objInfo.LastModified, false, inode, objInfo.UserMetadata, objInfo.ETag)
		fs.cachePut(s3Key, info)
		if writable {
			return newWritableFile(fs, path, s3Key, info)
		}
		return &s3File{
			fs:    fs,
			path:  path,
			s3Key: s3Key,
			info:  info,
			isDir: false,
			etag:  objInfo.ETag,
		}, nil
	}

	// If creating a new file, return a writable file
	if writable {
		inode, err := fs.handles.GetOrCreateInode(s3Key)
		if err != nil {
			return nil, err
		}
		info := newFileInfoFromS3(nameFromPath(path), 0, now(), false, inode, nil)
		return newWritableFile(fs, path, s3Key, info)
	}

	// Try as directory (with trailing slash)
	dirKey := s3DirKey(s3Key)
	entries, err := fs.s3.ListObjects(context.Background(), dirKey)
	if err == nil && len(entries) > 0 {
		inode, err := fs.handles.GetOrCreateInode(dirKey)
		if err != nil {
			return nil, err
		}
		info := newDirInfo(nameFromPath(path), inode)
		fs.cachePut(dirKey, info)
		return &s3File{
			fs:    fs,
			path:  path,
			s3Key: dirKey,
			info:  info,
			isDir: true,
		}, nil
	}

	// Also try explicit directory marker
	if objInfo, err := fs.s3.HeadObject(context.Background(), dirKey); err == nil {
		inode, err := fs.handles.GetOrCreateInode(dirKey)
		if err != nil {
			return nil, err
		}
		info := newFileInfoFromS3(nameFromPath(path), objInfo.Size, objInfo.LastModified, true, inode, objInfo.UserMetadata)
		fs.cachePut(dirKey, info)
		return &s3File{
			fs:    fs,
			path:  path,
			s3Key: dirKey,
			info:  info,
			isDir: true,
		}, nil
	}

	return nil, os.ErrNotExist
}

// Stat returns file information for the given path.
func (fs *S3FS) Stat(path string) (nfs.FileInfo, error) {
	s3Key := s3KeyFromPath(path)

	// Root directory
	if s3Key == "" {
		inode, _ := fs.handles.GetOrCreateInode("")
		return newDirInfo("/", inode), nil
	}

	// Check cache first — try as file key.
	if fi, hit := fs.cacheGet(s3Key); hit {
		if fi == nil {
			// Negative cache hit for file key — still try directory below.
		} else {
			fi.name = nameFromPath(path)
			return fi, nil
		}
	}

	// Check cache — try as directory key.
	dirKey := s3DirKey(s3Key)
	if fi, hit := fs.cacheGet(dirKey); hit {
		if fi == nil {
			// Negative hit for dir key too — still check S3 to be safe.
		} else {
			fi.name = nameFromPath(path)
			return fi, nil
		}
	}

	// Try as file via S3.
	objInfo, err := fs.s3.HeadObject(context.Background(), s3Key)
	if err == nil {
		isDir := objInfo.IsDir || strings.HasSuffix(s3Key, "/")
		inode, err := fs.handles.GetOrCreateInode(s3Key)
		if err != nil {
			return nil, err
		}
		info := newFileInfoFromS3(nameFromPath(path), objInfo.Size, objInfo.LastModified, isDir, inode, objInfo.UserMetadata)
		fs.cachePut(s3Key, info)
		return info, nil
	}

	// Check for explicit directory marker.
	if objInfo, err := fs.s3.HeadObject(context.Background(), dirKey); err == nil {
		inode, err := fs.handles.GetOrCreateInode(dirKey)
		if err != nil {
			return nil, err
		}
		info := newFileInfoFromS3(nameFromPath(path), objInfo.Size, objInfo.LastModified, true, inode, objInfo.UserMetadata)
		fs.cachePut(dirKey, info)
		return info, nil
	}

	// Check for implicit directory (objects with this prefix exist).
	entries, err := fs.s3.ListObjects(context.Background(), dirKey)
	if err == nil && len(entries) > 0 {
		inode, err := fs.handles.GetOrCreateInode(dirKey)
		if err != nil {
			return nil, err
		}
		info := newDirInfo(nameFromPath(path), inode)
		fs.cachePut(dirKey, info)
		return info, nil
	}

	// Not found — cache negative entries for both file and dir keys.
	fs.cachePutNegative(s3Key)
	fs.cachePutNegative(dirKey)

	return nil, os.ErrNotExist
}

// Chmod changes file permissions (stored as S3 metadata).
func (fs *S3FS) Chmod(path string, mode os.FileMode) error {
	slog.Debug("chmod not supported", "path", path, "mode", mode)
	return syscall.ENOTSUP
}

// Chown changes file ownership (stored as S3 metadata).
func (fs *S3FS) Chown(path string, uid, gid int) error {
	slog.Debug("chown not supported", "path", path, "uid", uid, "gid", gid)
	return syscall.ENOTSUP
}

// Symlink creates a symbolic link (not supported on S3).
func (fs *S3FS) Symlink(oldname, newname string) error {
	return fmt.Errorf("symlinks not supported on S3")
}

// Readlink reads a symbolic link (not supported on S3).
func (fs *S3FS) Readlink(name string) (string, error) {
	return "", fmt.Errorf("symlinks not supported on S3")
}

// Link creates a hard link (not supported on S3).
func (fs *S3FS) Link(oldname, newname string) error {
	return fmt.Errorf("hard links not supported on S3")
}

// Rename moves a file or directory.
func (fs *S3FS) Rename(oldpath, newpath string) error {
	ctx := context.Background()
	oldKey := s3KeyFromPath(oldpath)
	newKey := s3KeyFromPath(newpath)

	// Try as file first
	if _, err := fs.s3.HeadObject(ctx, oldKey); err == nil {
		if err := fs.s3.CopyObject(ctx, oldKey, newKey); err != nil {
			return fmt.Errorf("copy object: %w", err)
		}
		if err := fs.s3.DeleteObject(ctx, oldKey); err != nil {
			return fmt.Errorf("delete old object: %w", err)
		}
		_ = fs.handles.RenameKey(oldKey, newKey)
		fs.cacheInvalidate(oldKey)
		fs.cacheInvalidate(newKey)
		fs.cacheInvalidateParent(oldKey)
		fs.cacheInvalidateParent(newKey)
		fs.dataCacheInvalidate(oldKey)
		fs.dataCacheInvalidate(newKey)
		return nil
	}

	// Try as directory
	oldDirKey := s3DirKey(oldKey)
	newDirKey := s3DirKey(newKey)
	if _, err := fs.s3.HeadObject(ctx, oldDirKey); err == nil {
		if err := fs.s3.CopyObject(ctx, oldDirKey, newDirKey); err != nil {
			return fmt.Errorf("copy dir marker: %w", err)
		}
		if err := fs.s3.DeleteObject(ctx, oldDirKey); err != nil {
			return fmt.Errorf("delete old dir marker: %w", err)
		}
		_ = fs.handles.RenameKey(oldDirKey, newDirKey)
		fs.cacheInvalidate(oldDirKey)
		fs.cacheInvalidate(newDirKey)
		fs.cacheInvalidateParent(oldDirKey)
		fs.cacheInvalidateParent(newDirKey)
		fs.dataCacheInvalidate(oldDirKey)
		fs.dataCacheInvalidate(newDirKey)
		return nil
	}

	return os.ErrNotExist
}

// Remove deletes a file or directory.
func (fs *S3FS) Remove(path string) error {
	ctx := context.Background()
	s3Key := s3KeyFromPath(path)

	// Try as file first
	if _, err := fs.s3.HeadObject(ctx, s3Key); err == nil {
		if err := fs.s3.DeleteObject(ctx, s3Key); err != nil {
			return fmt.Errorf("delete object: %w", err)
		}
		_ = fs.handles.RemoveByKey(s3Key)
		fs.cacheInvalidate(s3Key)
		fs.cacheInvalidateParent(s3Key)
		fs.dataCacheInvalidate(s3Key)
		return nil
	}

	// Try as directory
	dirKey := s3DirKey(s3Key)
	if _, err := fs.s3.HeadObject(ctx, dirKey); err == nil {
		if err := fs.s3.DeleteObject(ctx, dirKey); err != nil {
			return fmt.Errorf("delete dir marker: %w", err)
		}
		_ = fs.handles.RemoveByKey(dirKey)
		fs.cacheInvalidate(dirKey)
		fs.cacheInvalidateParent(dirKey)
		fs.dataCacheInvalidate(dirKey)
		return nil
	}

	return os.ErrNotExist
}

// MkdirAll creates a directory (and parents).
func (fs *S3FS) MkdirAll(path string, perm os.FileMode) error {
	ctx := context.Background()
	s3Key := s3KeyFromPath(path)
	dirKey := s3DirKey(s3Key)

	if err := fs.s3.CreateDirMarker(ctx, dirKey); err != nil {
		return fmt.Errorf("create dir marker: %w", err)
	}

	if _, err := fs.handles.GetOrCreateInode(dirKey); err != nil {
		return fmt.Errorf("allocate inode: %w", err)
	}

	// Invalidate cache so subsequent Stat/Readdir see the new directory.
	fs.cacheInvalidate(dirKey)
	fs.cacheInvalidate(s3Key)
	fs.cacheInvalidateParent(dirKey)

	return nil
}

// S3Client returns the underlying S3 client (for use by the NFS server layer).
func (fs *S3FS) S3Client() s3client.S3API {
	return fs.s3
}

// Handles returns the handle store (for use by the NFS server layer).
func (fs *S3FS) Handles() *HandleStore {
	return fs.handles
}

// now is a helper for consistent timestamps.
func now() time.Time {
	return time.Now().UTC()
}
