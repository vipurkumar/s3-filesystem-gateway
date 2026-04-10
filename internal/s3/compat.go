// Copyright 2024 s3-filesystem-gateway contributors
// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"context"
	"fmt"
	"strings"

	"github.com/minio/minio-go/v7"
)

// S3Backend identifies the type of S3-compatible backend.
type S3Backend string

const (
	// BackendMinIO is the MinIO object store.
	BackendMinIO S3Backend = "minio"
	// BackendAWS is Amazon S3.
	BackendAWS S3Backend = "aws"
	// BackendObjectScale is Dell ObjectScale.
	BackendObjectScale S3Backend = "objectscale"
)

// DetectBackend uses heuristics on the endpoint to guess which S3 backend is
// being targeted. The result is advisory (used for logging and tailored error
// messages) and does not change protocol behaviour.
func DetectBackend(endpoint string) S3Backend {
	// Strip scheme if present (endpoint may or may not include it).
	host := endpoint
	for _, prefix := range []string{"http://", "https://"} {
		host = strings.TrimPrefix(host, prefix)
	}

	// Check for well-known ports.
	if strings.HasSuffix(host, ":9020") || strings.HasSuffix(host, ":9021") {
		return BackendObjectScale
	}
	if strings.HasSuffix(host, ":9000") {
		return BackendMinIO
	}

	// Check for common hostname patterns.
	lower := strings.ToLower(host)
	if strings.Contains(lower, "objectscale") || strings.Contains(lower, "dell") {
		return BackendObjectScale
	}
	if strings.Contains(lower, "minio") {
		return BackendMinIO
	}
	if strings.Contains(lower, "amazonaws.com") || strings.Contains(lower, "s3.") {
		return BackendAWS
	}

	// Default to AWS-style when we cannot determine the backend.
	return BackendAWS
}

// ValidateConnection verifies that the S3 endpoint is reachable and the bucket
// exists, returning backend-specific error messages for common failure modes.
func ValidateConnection(ctx context.Context, mc *minio.Client, bucket string, backend S3Backend) error {
	exists, err := mc.BucketExists(ctx, bucket)
	if err != nil {
		return wrapConnectionError(err, backend)
	}
	if !exists {
		return fmt.Errorf("bucket %q does not exist on %s backend", bucket, string(backend))
	}
	return nil
}

// wrapConnectionError adds backend-specific troubleshooting hints to
// connection errors.
func wrapConnectionError(err error, backend S3Backend) error {
	base := fmt.Sprintf("S3 connection error (%s backend): %v", string(backend), err)

	switch backend {
	case BackendObjectScale:
		hints := []string{
			"verify the endpoint uses port 9020 (HTTP) or 9021 (HTTPS)",
			"ensure path-style addressing is enabled (S3_PATH_STYLE=true)",
			"check that the ObjectScale namespace and bucket exist",
			"if using HTTPS, verify the TLS certificate is trusted or set S3_USE_SSL=false for testing",
		}
		return fmt.Errorf("%s\nObjectScale troubleshooting hints:\n  - %s",
			base, strings.Join(hints, "\n  - "))

	case BackendMinIO:
		hints := []string{
			"verify MinIO is running and reachable at the configured endpoint",
			"check that access key and secret key are correct",
		}
		return fmt.Errorf("%s\nMinIO troubleshooting hints:\n  - %s",
			base, strings.Join(hints, "\n  - "))

	default:
		return fmt.Errorf("%s", base)
	}
}
