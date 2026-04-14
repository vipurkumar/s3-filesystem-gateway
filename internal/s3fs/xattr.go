package s3fs

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/smallfz/libnfs-go/fs"
)

// xattrMetaPrefix is prepended to every user.* xattr key before it is
// stashed in S3 user metadata. The full S3 header name ends up
// "x-amz-meta-xattr-<name>"; the "Xattr-" prefix keeps xattrs from
// colliding with the existing Uid / Gid / Mode / SymlinkTarget keys
// that Stream A already reserves at the top of the user metadata
// namespace.
const xattrMetaPrefix = "Xattr-"

// xattrValueMax is the hard cap on a single xattr value before
// base64 expansion. The NFS xattr layer enforces an upper bound too
// (currently the same value), but the two copies exist on purpose:
// this one protects the S3 round-trip (metadata headers are ASCII
// and capped well below ~2 KB per header by AWS), while the NFS one
// maps the error back to NFS4ERR_XATTR2BIG.
const xattrValueMax = 2048

// xattrMaxTotalBytes is a soft cap on the cumulative encoded size of
// all xattr metadata headers for one object. S3 caps all user
// metadata at ~8 KB total across keys; we stay a bit under that so
// existing non-xattr metadata (Uid/Gid/Mode/etc) still has headroom.
const xattrMaxTotalBytes = 6144

// _ compile-time assertion: S3FS must satisfy fs.XattrCapable.
var _ fs.XattrCapable = (*S3FS)(nil)

// stripUserPrefix turns "user.checksum" into "checksum". We validate
// the prefix at the NFS layer and defensively re-validate here so a
// misbehaving or future caller can't punch through to the trusted.*
// namespace via this method.
func stripUserPrefix(name string) (string, error) {
	if !strings.HasPrefix(name, "user.") {
		return "", os.ErrPermission
	}
	short := strings.TrimPrefix(name, "user.")
	if short == "" {
		return "", os.ErrInvalid
	}
	return short, nil
}

// xattrMetaKey is the S3 user-metadata map key for a given xattr name.
// Example: "user.checksum" -> "Xattr-checksum".
func xattrMetaKey(name string) (string, error) {
	short, err := stripUserPrefix(name)
	if err != nil {
		return "", err
	}
	return xattrMetaPrefix + short, nil
}

// addUserPrefix is the inverse of stripUserPrefix.
func addUserPrefix(shortName string) string {
	return "user." + shortName
}

// readObjectMeta resolves a POSIX path to an S3 object and returns a
// mutable copy of its user metadata. Falls back to the directory-
// marker key when the file key is not found, matching the Chmod/Chown
// pattern in filesystem.go.
func (fs *S3FS) readObjectMeta(ctx context.Context, path string) (string /*key*/, map[string]string, error) {
	key := s3KeyFromPath(path)
	info, err := fs.s3.HeadObject(ctx, key)
	if err != nil {
		dirKey := s3DirKey(key)
		info, err = fs.s3.HeadObject(ctx, dirKey)
		if err != nil {
			return "", nil, os.ErrNotExist
		}
		key = dirKey
	}
	meta := make(map[string]string, len(info.UserMetadata))
	for k, v := range info.UserMetadata {
		meta[k] = v
	}
	return key, meta, nil
}

// GetXattr returns the raw bytes of a single user.* xattr.
func (fs *S3FS) GetXattr(path, name string) ([]byte, error) {
	metaKey, err := xattrMetaKey(name)
	if err != nil {
		return nil, err
	}
	_, meta, err := fs.readObjectMeta(context.Background(), path)
	if err != nil {
		return nil, err
	}
	encoded, ok := findXattr(meta, metaKey)
	if !ok {
		return nil, os.ErrNotExist
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		// Stored value is not valid base64. Treat as corruption /
		// absent so callers see NOXATTR rather than a backend error.
		return nil, fmt.Errorf("xattr %s: decode: %w", name, os.ErrNotExist)
	}
	return decoded, nil
}

// SetXattr sets or updates a user.* xattr, honoring SETXATTR4_CREATE /
// _REPLACE / _EITHER semantics.
func (fs *S3FS) SetXattr(path, name string, value []byte, option uint32) error {
	if len(value) > xattrValueMax {
		return fmt.Errorf("xattr value too large: %w", os.ErrInvalid)
	}
	metaKey, err := xattrMetaKey(name)
	if err != nil {
		return err
	}
	ctx := context.Background()
	key, meta, err := fs.readObjectMeta(ctx, path)
	if err != nil {
		return err
	}
	_, exists := findXattr(meta, metaKey)
	const (
		either  = uint32(0)
		create  = uint32(1)
		replace = uint32(2)
	)
	switch option {
	case create:
		if exists {
			return os.ErrExist
		}
	case replace:
		if !exists {
			return os.ErrNotExist
		}
	case either:
		// no constraint
	default:
		return os.ErrInvalid
	}
	encoded := base64.StdEncoding.EncodeToString(value)
	meta[metaKey] = encoded

	if totalXattrBytes(meta) > xattrMaxTotalBytes {
		return fmt.Errorf("xattr total too large: %w", os.ErrInvalid)
	}
	if err := fs.s3.CopyObjectWithMetadata(ctx, key, meta); err != nil {
		return fmt.Errorf("xattr set: %w", err)
	}
	fs.cacheInvalidate(key)
	return nil
}

// ListXattrs returns fully-qualified xattr names (e.g.
// "user.checksum") for the object at path. We only expose user.*
// names; other S3 user-metadata keys are filtered out.
func (fs *S3FS) ListXattrs(path string) ([]string, error) {
	_, meta, err := fs.readObjectMeta(context.Background(), path)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for k := range meta {
		if short, ok := shortFromMetaKey(k); ok {
			out = append(out, addUserPrefix(short))
		}
	}
	return out, nil
}

// RemoveXattr deletes a user.* xattr.
func (fs *S3FS) RemoveXattr(path, name string) error {
	metaKey, err := xattrMetaKey(name)
	if err != nil {
		return err
	}
	ctx := context.Background()
	key, meta, err := fs.readObjectMeta(ctx, path)
	if err != nil {
		return err
	}
	if _, ok := findXattr(meta, metaKey); !ok {
		return os.ErrNotExist
	}
	// Remove the key in whichever case the S3 backend returned it.
	for k := range meta {
		if strings.EqualFold(k, metaKey) {
			delete(meta, k)
		}
	}
	if err := fs.s3.CopyObjectWithMetadata(ctx, key, meta); err != nil {
		return fmt.Errorf("xattr remove: %w", err)
	}
	fs.cacheInvalidate(key)
	return nil
}

// findXattr looks up an xattr meta key case-insensitively. S3 servers
// (especially MinIO) may normalise header case on the way back out.
func findXattr(meta map[string]string, metaKey string) (string, bool) {
	if v, ok := meta[metaKey]; ok {
		return v, true
	}
	for k, v := range meta {
		if strings.EqualFold(k, metaKey) {
			return v, true
		}
	}
	return "", false
}

// shortFromMetaKey returns the xattr short name if the given user-
// metadata key is an xattr slot, or ok=false otherwise.
func shortFromMetaKey(metaKey string) (string, bool) {
	// Case-insensitive prefix match against "Xattr-".
	if len(metaKey) <= len(xattrMetaPrefix) {
		return "", false
	}
	if !strings.EqualFold(metaKey[:len(xattrMetaPrefix)], xattrMetaPrefix) {
		return "", false
	}
	return metaKey[len(xattrMetaPrefix):], true
}

// totalXattrBytes sums the encoded length of all xattr slots in the
// metadata map. Non-xattr keys are ignored so the cumulative cap
// applies only to xattrs, not to the reserved Uid/Gid/Mode/etc keys.
func totalXattrBytes(meta map[string]string) int {
	total := 0
	for k, v := range meta {
		if _, ok := shortFromMetaKey(k); ok {
			total += len(k) + len(v)
		}
	}
	return total
}

// ErrXattrInvalid is returned in the rare case a caller asks for an
// xattr option we don't understand. Kept public as a convenience for
// future unit tests; the NFS xattr layer maps os.ErrInvalid to
// NFS4ERR_INVAL which is the correct wire-level response.
var ErrXattrInvalid = errors.New("xattr: invalid request")
