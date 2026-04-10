// Copyright 2024 s3-filesystem-gateway contributors
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// NFS operation metrics.
	nfsOpsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "s3gw_nfs_operations_total",
		Help: "Total number of NFS operations.",
	}, []string{"operation", "status"})

	nfsOpDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "s3gw_nfs_operation_duration_seconds",
		Help:    "Duration of NFS operations in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"operation"})

	// S3 request metrics.
	s3RequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "s3gw_s3_requests_total",
		Help: "Total number of S3 requests.",
	}, []string{"method", "status"})

	s3RequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "s3gw_s3_request_duration_seconds",
		Help:    "Duration of S3 requests in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method"})

	// Cache metrics.
	cacheHitsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "s3gw_cache_hits_total",
		Help: "Total number of cache hits.",
	}, []string{"cache_type"})

	cacheMissesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "s3gw_cache_misses_total",
		Help: "Total number of cache misses.",
	}, []string{"cache_type"})

	// Connection metrics.
	activeConnections = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "s3gw_active_connections",
		Help: "Number of active NFS connections.",
	})

	// Byte transfer metrics.
	bytesTransferredTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "s3gw_bytes_transferred_total",
		Help: "Total bytes transferred.",
	}, []string{"direction"})
)

// RecordNFSOp records an NFS operation's outcome and duration.
func RecordNFSOp(operation string, duration time.Duration, err error) {
	status := "success"
	if err != nil {
		status = "error"
	}
	nfsOpsTotal.WithLabelValues(operation, status).Inc()
	nfsOpDuration.WithLabelValues(operation).Observe(duration.Seconds())
}

// RecordS3Request records an S3 request's outcome and duration.
func RecordS3Request(method string, duration time.Duration, err error) {
	status := "success"
	if err != nil {
		status = "error"
	}
	s3RequestsTotal.WithLabelValues(method, status).Inc()
	s3RequestDuration.WithLabelValues(method).Observe(duration.Seconds())
}

// RecordCacheHit records a cache hit for the given cache type.
func RecordCacheHit(cacheType string) {
	cacheHitsTotal.WithLabelValues(cacheType).Inc()
}

// RecordCacheMiss records a cache miss for the given cache type.
func RecordCacheMiss(cacheType string) {
	cacheMissesTotal.WithLabelValues(cacheType).Inc()
}

// RecordBytesTransferred records bytes transferred in the given direction.
func RecordBytesTransferred(direction string, bytes int64) {
	bytesTransferredTotal.WithLabelValues(direction).Add(float64(bytes))
}

// IncrConnections increments the active connection count.
func IncrConnections() {
	activeConnections.Inc()
}

// DecrConnections decrements the active connection count.
func DecrConnections() {
	activeConnections.Dec()
}
