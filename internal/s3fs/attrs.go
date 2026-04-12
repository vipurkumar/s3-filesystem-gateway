// Copyright 2024 s3-filesystem-gateway contributors
// SPDX-License-Identifier: Apache-2.0

package s3fs

import (
	"os"
	"strconv"
	"time"

	nfs "github.com/smallfz/libnfs-go/fs"
)

// Default POSIX attributes for S3 objects that lack metadata headers.
const (
	DefaultUID      = 1000
	DefaultGID      = 1000
	DefaultFileMode = 0644
	DefaultDirMode  = 0755
)

// S3 user-metadata header keys for POSIX attributes.
const (
	MetaKeyUID  = "Uid"
	MetaKeyGID  = "Gid"
	MetaKeyMode = "Mode"
)

// fileInfo implements libnfs-go fs.FileInfo (extends os.FileInfo with ATime, CTime, NumLinks).
type fileInfo struct {
	name     string
	size     int64
	mode     os.FileMode
	modTime  time.Time
	isDir    bool
	inode    uint64
	numLinks int
	etag     string
}

var _ nfs.FileInfo = (*fileInfo)(nil)

func (fi *fileInfo) Name() string        { return fi.name }
func (fi *fileInfo) Size() int64          { return fi.size }
func (fi *fileInfo) Mode() os.FileMode    { return fi.mode }
func (fi *fileInfo) ModTime() time.Time   { return fi.modTime }
func (fi *fileInfo) IsDir() bool          { return fi.isDir }
func (fi *fileInfo) Sys() interface{}     { return nil }
func (fi *fileInfo) ATime() time.Time     { return fi.modTime }
func (fi *fileInfo) CTime() time.Time     { return fi.modTime }
func (fi *fileInfo) NumLinks() int        { return fi.numLinks }

// newFileInfoFromS3 creates a fileInfo from S3 object metadata.
func newFileInfoFromS3(name string, size int64, lastModified time.Time, isDir bool, inode uint64, userMeta map[string]string) *fileInfo {
	mode := parseMode(userMeta, isDir)
	numLinks := 1
	if isDir {
		numLinks = 2
	}

	return &fileInfo{
		name:     name,
		size:     size,
		mode:     mode,
		modTime:  lastModified,
		isDir:    isDir,
		inode:    inode,
		numLinks: numLinks,
	}
}

// newFileInfoFromS3WithETag creates a fileInfo including the S3 ETag for cache coherency.
func newFileInfoFromS3WithETag(name string, size int64, lastModified time.Time, isDir bool, inode uint64, userMeta map[string]string, etag string) *fileInfo {
	fi := newFileInfoFromS3(name, size, lastModified, isDir, inode, userMeta)
	fi.etag = etag
	return fi
}

// newDirInfo creates a fileInfo for a directory.
func newDirInfo(name string, inode uint64) *fileInfo {
	return &fileInfo{
		name:     name,
		size:     0,
		mode:     os.ModeDir | DefaultDirMode,
		modTime:  time.Now(),
		isDir:    true,
		inode:    inode,
		numLinks: 2,
	}
}

// parseMode extracts the file mode from S3 user-metadata, falling back to defaults.
func parseMode(meta map[string]string, isDir bool) os.FileMode {
	if v, ok := meta[MetaKeyMode]; ok {
		if m, err := strconv.ParseUint(v, 8, 32); err == nil {
			mode := os.FileMode(m)
			if isDir {
				mode |= os.ModeDir
			}
			return mode
		}
	}
	if isDir {
		return os.ModeDir | DefaultDirMode
	}
	return DefaultFileMode
}

// posixMetadata returns S3 user-metadata headers for POSIX attributes.
func posixMetadata(uid, gid int, mode os.FileMode) map[string]string {
	return map[string]string{
		MetaKeyUID:  strconv.Itoa(uid),
		MetaKeyGID:  strconv.Itoa(gid),
		MetaKeyMode: strconv.FormatUint(uint64(mode.Perm()), 8),
	}
}
