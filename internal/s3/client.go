package s3

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Client wraps minio-go with connection pooling and retry logic.
type Client struct {
	mc     *minio.Client
	bucket string
}

// ClientConfig holds S3 client configuration.
type ClientConfig struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
	Region    string
	UseSSL    bool
}

// NewClient creates a new S3 client and verifies bucket access.
func NewClient(ctx context.Context, cfg ClientConfig) (*Client, error) {
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	}

	mc, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:     credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:    cfg.UseSSL,
		Region:    cfg.Region,
		Transport: transport,
	})
	if err != nil {
		return nil, fmt.Errorf("create minio client: %w", err)
	}

	// Verify bucket exists
	exists, err := mc.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("check bucket %q: %w", cfg.Bucket, err)
	}
	if !exists {
		return nil, fmt.Errorf("bucket %q does not exist", cfg.Bucket)
	}

	return &Client{mc: mc, bucket: cfg.Bucket}, nil
}

// ObjectInfo holds metadata about an S3 object.
type ObjectInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
	ETag         string
	ContentType  string
	IsDir        bool
	UserMetadata map[string]string
}

// HeadObject returns metadata for a single object.
func (c *Client) HeadObject(ctx context.Context, key string) (*ObjectInfo, error) {
	info, err := c.mc.StatObject(ctx, c.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return nil, err
	}
	return &ObjectInfo{
		Key:          info.Key,
		Size:         info.Size,
		LastModified: info.LastModified,
		ETag:         info.ETag,
		ContentType:  info.ContentType,
		UserMetadata: info.UserMetadata,
	}, nil
}

// GetObject returns a reader for the object's data.
func (c *Client) GetObject(ctx context.Context, key string) (io.ReadCloser, *ObjectInfo, error) {
	obj, err := c.mc.GetObject(ctx, c.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, nil, err
	}
	info, err := obj.Stat()
	if err != nil {
		obj.Close()
		return nil, nil, err
	}
	return obj, &ObjectInfo{
		Key:          info.Key,
		Size:         info.Size,
		LastModified: info.LastModified,
		ETag:         info.ETag,
		ContentType:  info.ContentType,
		UserMetadata: info.UserMetadata,
	}, nil
}

// GetObjectRange returns a reader for a byte range of the object.
func (c *Client) GetObjectRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	opts := minio.GetObjectOptions{}
	if err := opts.SetRange(offset, offset+length-1); err != nil {
		return nil, fmt.Errorf("set range: %w", err)
	}
	obj, err := c.mc.GetObject(ctx, c.bucket, key, opts)
	if err != nil {
		return nil, err
	}
	return obj, nil
}

// ListEntry represents a single entry from a directory listing.
type ListEntry struct {
	Key          string
	Size         int64
	LastModified time.Time
	ETag         string
	IsDir        bool
}

// ListObjects lists objects under the given prefix with delimiter "/".
func (c *Client) ListObjects(ctx context.Context, prefix string) ([]ListEntry, error) {
	var entries []ListEntry

	opts := minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: false, // use delimiter to get directory-like listing
	}

	for obj := range c.mc.ListObjects(ctx, c.bucket, opts) {
		if obj.Err != nil {
			return nil, obj.Err
		}

		entry := ListEntry{
			Key:          obj.Key,
			Size:         obj.Size,
			LastModified: obj.LastModified,
			ETag:         obj.ETag,
		}

		// Common prefixes (directories) have no size and key ends with "/"
		if obj.Size == 0 && len(obj.Key) > 0 && obj.Key[len(obj.Key)-1] == '/' {
			entry.IsDir = true
		}

		entries = append(entries, entry)
	}

	return entries, nil
}

// PutObject uploads data as an S3 object.
func (c *Client) PutObject(ctx context.Context, key string, reader io.Reader, size int64, metadata map[string]string) error {
	opts := minio.PutObjectOptions{
		UserMetadata: metadata,
	}
	_, err := c.mc.PutObject(ctx, c.bucket, key, reader, size, opts)
	return err
}

// DeleteObject removes an object.
func (c *Client) DeleteObject(ctx context.Context, key string) error {
	return c.mc.RemoveObject(ctx, c.bucket, key, minio.RemoveObjectOptions{})
}

// CopyObject copies an object from src to dst key within the same bucket.
func (c *Client) CopyObject(ctx context.Context, srcKey, dstKey string) error {
	src := minio.CopySrcOptions{Bucket: c.bucket, Object: srcKey}
	dst := minio.CopyDestOptions{Bucket: c.bucket, Object: dstKey}
	_, err := c.mc.CopyObject(ctx, dst, src)
	return err
}

// CreateDirMarker creates a zero-byte object as a directory marker.
func (c *Client) CreateDirMarker(ctx context.Context, key string) error {
	_, err := c.mc.PutObject(ctx, c.bucket, key, nil, 0, minio.PutObjectOptions{})
	return err
}

// BucketExists checks whether the configured bucket is reachable.
func (c *Client) BucketExists(ctx context.Context) (bool, error) {
	return c.mc.BucketExists(ctx, c.bucket)
}
