package s3fs

import (
	"context"
	"fmt"
)

// Copy implements the optional fs.CopyCapable interface from libnfs-go.
// NFSv4.2 COPY handlers dispatch here for synchronous full-object
// copies; partial-range copies are filtered out at the NFS layer and
// never reach this method. The whole operation collapses to one S3
// CopyObject, which is the whole point of advertising v4.2 COPY on an
// object-storage-backed filesystem.
func (fs *S3FS) Copy(srcPath, dstPath string) error {
	ctx := context.Background()
	srcKey := s3KeyFromPath(srcPath)
	dstKey := s3KeyFromPath(dstPath)
	if err := fs.s3.CopyObject(ctx, srcKey, dstKey); err != nil {
		return fmt.Errorf("copy %s -> %s: %w", srcKey, dstKey, err)
	}
	fs.cacheInvalidate(dstKey)
	fs.cacheInvalidateParent(dstKey)
	return nil
}
