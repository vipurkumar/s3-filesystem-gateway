package s3fs

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	nfs "github.com/smallfz/libnfs-go/fs"
	s3client "github.com/vipurkumar/s3-filesystem-gateway/internal/s3"
)

// S3FS implements the libnfs-go fs.FS interface backed by S3.
type S3FS struct {
	s3      *s3client.Client
	handles *HandleStore
	creds   nfs.Creds
}

var _ nfs.FS = (*S3FS)(nil)

// NewS3FS creates a new S3-backed filesystem.
func NewS3FS(s3 *s3client.Client, handles *HandleStore) *S3FS {
	return &S3FS{
		s3:      s3,
		handles: handles,
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
		info := newFileInfoFromS3(nameFromPath(path), objInfo.Size, objInfo.LastModified, false, inode, objInfo.UserMetadata)
		return &s3File{
			fs:    fs,
			path:  path,
			s3Key: s3Key,
			info:  info,
			isDir: false,
		}, nil
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

	// Try as file
	objInfo, err := fs.s3.HeadObject(context.Background(), s3Key)
	if err == nil {
		inode, err := fs.handles.GetOrCreateInode(s3Key)
		if err != nil {
			return nil, err
		}
		return newFileInfoFromS3(nameFromPath(path), objInfo.Size, objInfo.LastModified, false, inode, objInfo.UserMetadata), nil
	}

	// Try as directory
	dirKey := s3DirKey(s3Key)

	// Check for explicit directory marker
	if objInfo, err := fs.s3.HeadObject(context.Background(), dirKey); err == nil {
		inode, err := fs.handles.GetOrCreateInode(dirKey)
		if err != nil {
			return nil, err
		}
		return newFileInfoFromS3(nameFromPath(path), objInfo.Size, objInfo.LastModified, true, inode, objInfo.UserMetadata), nil
	}

	// Check for implicit directory (objects with this prefix exist)
	entries, err := fs.s3.ListObjects(context.Background(), dirKey)
	if err == nil && len(entries) > 0 {
		inode, err := fs.handles.GetOrCreateInode(dirKey)
		if err != nil {
			return nil, err
		}
		return newDirInfo(nameFromPath(path), inode), nil
	}

	return nil, os.ErrNotExist
}

// Chmod changes file permissions (stored as S3 metadata).
func (fs *S3FS) Chmod(path string, mode os.FileMode) error {
	slog.Debug("chmod not fully implemented", "path", path, "mode", mode)
	return nil // Silently succeed; Phase 3 will implement via metadata update
}

// Chown changes file ownership (stored as S3 metadata).
func (fs *S3FS) Chown(path string, uid, gid int) error {
	slog.Debug("chown not fully implemented", "path", path, "uid", uid, "gid", gid)
	return nil
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
	// Phase 3: CopyObject + DeleteObject
	return fmt.Errorf("rename not supported yet")
}

// Remove deletes a file or directory.
func (fs *S3FS) Remove(path string) error {
	// Phase 3: DeleteObject
	return fmt.Errorf("remove not supported yet")
}

// MkdirAll creates a directory (and parents).
func (fs *S3FS) MkdirAll(path string, perm os.FileMode) error {
	// Phase 3: create directory marker
	return fmt.Errorf("mkdir not supported yet")
}

// S3Client returns the underlying S3 client (for use by the NFS server layer).
func (fs *S3FS) S3Client() *s3client.Client {
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
