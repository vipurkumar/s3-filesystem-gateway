// Copyright 2024 s3-filesystem-gateway contributors
// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"context"
	"io"
)

// S3API defines the interface for S3 operations used by the filesystem layer.
// The concrete *Client satisfies this interface; tests can provide a mock.
type S3API interface {
	HeadObject(ctx context.Context, key string) (*ObjectInfo, error)
	GetObject(ctx context.Context, key string) (io.ReadCloser, *ObjectInfo, error)
	GetObjectRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error)
	ListObjects(ctx context.Context, prefix string) ([]ListEntry, error)
	PutObject(ctx context.Context, key string, reader io.Reader, size int64, metadata map[string]string) error
	DeleteObject(ctx context.Context, key string) error
	CopyObject(ctx context.Context, srcKey, dstKey string) error
	CreateDirMarker(ctx context.Context, key string) error
}

// Compile-time check: *Client implements S3API.
var _ S3API = (*Client)(nil)
