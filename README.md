# S3 Filesystem Gateway

Enterprise-grade NFSv4.1-to-S3 gateway written in Go. Exposes S3-compatible object storage as an NFS filesystem.

## Overview

S3 Filesystem Gateway bridges the gap between NFS clients and S3-compatible object storage. Applications mount an S3 bucket over NFS and use standard filesystem operations -- the gateway translates them to S3 API calls transparently.

```
NFS Clients --> [NFSv4.1 Server] --> [S3 Filesystem] --> [Metadata Cache] --> [S3 API] --> Object Storage
                     |                     |                                      ^
                     |                     +------> [Data Cache (disk LRU)] ------+
                     |
                     +--> [Prometheus Metrics] --> /metrics
```

## Features

- **NFSv4.1 protocol** -- single port (2049), built-in locking, compound operations, sessions
- **S3-compatible backends** -- MinIO, AWS S3, Dell ObjectScale, any S3-compatible storage
- **Read path** -- chunked ranged reads with adaptive prefetch (1MB -> 4MB -> 16MB)
- **Write path** -- local temp file buffering with upload-on-close semantics
- **Full POSIX operations** -- create, mkdir, remove, rename (copy+delete), stat, readdir, chmod, chown, truncate, symlink
- **Read-after-write consistency** -- cache refresh on write close
- **Rate limiting** -- token-bucket limiter for S3 request shaping
- **Metadata cache** -- in-memory LRU with TTL-based expiry, negative caching, directory listing cache
- **Data cache** -- disk-based LRU with ETag coherency, SHA256-keyed shard storage, configurable max size
- **POSIX metadata** -- uid/gid/mode stored as S3 user-metadata headers (`x-amz-meta-uid/gid/mode`)
- **Inode management** -- bbolt-backed persistent inode-to-S3-key mapping with in-memory cache
- **Prometheus metrics** -- NFS ops, S3 requests, cache hit/miss rates, byte transfer, active connections
- **Health endpoints** -- `/health` (liveness), `/ready` (S3 reachability check), `/metrics` (Prometheus)
- **Structured logging** -- JSON via `log/slog`
- **Graceful shutdown** -- SIGINT/SIGTERM handling with 30-second timeout

## Quick Start

### Prerequisites

- Docker and Docker Compose
- Linux host with `nfs-common` (for mounting)

### One-shot dev/test environment (published image)

No clone required — download a single compose file that pulls the gateway image from GHCR and bundles MinIO:

```bash
curl -O https://raw.githubusercontent.com/rupivbluegreen/s3-filesystem-gateway/main/deployments/docker/docker-compose.quickstart.yml
docker compose -f docker-compose.quickstart.yml up -d

# Mount from another terminal (Linux)
sudo mount -t nfs4 -o port=2049,nolock localhost:/ /mnt/s3

# Use it
ls /mnt/s3
echo "hello" > /mnt/s3/test.txt
cat /mnt/s3/test.txt
mkdir /mnt/s3/subdir

# MinIO console at http://localhost:9001 (minioadmin / minioadmin)
# Gateway health at http://localhost:9090/health
```

### Published images

Images are built multi-arch (`linux/amd64` + `linux/arm64`) on each `v*` git tag and published to both registries:

- `ghcr.io/rupivbluegreen/s3-filesystem-gateway`
- `docker.io/vipurkumar/s3-filesystem-gateway`

Tag conventions: `:latest` (latest stable release), `:0.1.0` (exact semver), `:0.1` (minor), `:0` (major).

### Build from Source

```bash
git clone https://github.com/rupivbluegreen/s3-filesystem-gateway.git
cd s3-filesystem-gateway
docker compose -f deployments/docker/docker-compose.yml up   # builds locally
# or
make build
./bin/s3nfsgw --config configs/default.yaml --data-dir /var/lib/s3nfsgw
```

## Configuration

Configuration uses defaults with environment variable overrides:

| Env Variable | Default | Description |
|---|---|---|
| `S3_ENDPOINT` | `localhost:9000` | S3 endpoint |
| `S3_ACCESS_KEY` | `minioadmin` | Access key |
| `S3_SECRET_KEY` | `minioadmin` | Secret key |
| `S3_BUCKET` | `data` | Bucket name |
| `S3_REGION` | `us-east-1` | S3 region |
| `S3_USE_SSL` | `false` | Enable TLS |
| `S3_PATH_STYLE` | `true` | Use path-style addressing (required for MinIO) |
| `NFS_PORT` | `2049` | NFS listen port |
| `HEALTH_PORT` | `9090` | Health/metrics HTTP server port |
| `CACHE_METADATA_TTL` | `60s` | Metadata cache TTL |
| `CACHE_DATA_DIR` | `/var/cache/s3gw` | Data cache directory |
| `CACHE_DATA_MAX_SIZE` | `10GB` | Data cache max size |
| `LOG_LEVEL` | `info` | Log level (debug, info, warn, error) |

CLI flags:

| Flag | Default | Description |
|---|---|---|
| `--config` | `configs/default.yaml` | Path to configuration file |
| `--data-dir` | `/var/lib/s3nfsgw` | Directory for persistent data (bbolt database) |

## Architecture

### Layers

```
cmd/s3nfsgw/main.go          Entry point, signal handling, health server startup
internal/nfs/server.go        NFSv4.1 server (libnfs-go), creates per-session S3FS
internal/s3fs/                S3 filesystem implementing libnfs-go fs.FS interface
internal/cache/metadata.go    In-memory LRU metadata cache with TTL + negative caching
internal/cache/data.go        Disk-based LRU data cache with ETag coherency
internal/s3/client.go         S3 client (minio-go) with connection pooling
internal/metrics/             Prometheus metric definitions and recording helpers
internal/health/              HTTP health/readiness/metrics endpoints
internal/config/              Configuration loading with env var overrides
```

- **NFS Layer** (`smallfz/libnfs-go`) -- Pure Go NFSv4 server handling protocol, sessions, locking. Creates a fresh `S3FS` instance per client session.
- **S3 Filesystem** -- Implements libnfs-go `fs.FS` interface (~15 methods). Translates POSIX operations to S3 API calls. Handles directory markers, implicit directories, file creation, rename (copy+delete), and remove.
- **Metadata Cache** -- In-memory LRU (default 10,000 entries) with separate TTLs for files (300s), directories (60s), and negative entries (10s). Background eviction goroutine. Caches individual entries and directory listings.
- **Data Cache** -- Disk-based LRU with SHA256-keyed shard directories. Atomic writes via temp+rename. Rebuilds index from disk on startup. Configurable max size with background eviction.
- **S3 Backend** (`minio/minio-go/v7`) -- S3 client with connection pooling (100 idle conns), ranged reads, multipart uploads, copy, delete, and directory marker management.
- **Handle Store** (`go.etcd.io/bbolt`) -- Persistent bidirectional mapping between S3 keys and synthetic inode numbers. In-memory cache for fast lookups, ACID persistence for crash recovery.

### Key Design Decisions

| Decision | Rationale |
|---|---|
| NFSv4.1 over NFSv3 | Single port, built-in locking, sessions, compound ops |
| libnfs-go over go-nfs | Pure Go NFSv4 with clean fs.FS interface, zero deps |
| minio-go over aws-sdk-go | First-class MinIO support, lighter weight |
| bbolt for handle persistence | ACID, embedded, zero-config, read-optimized |
| Upload on close | Optimized for write-once/read-many workloads |
| Adaptive prefetch | 1MB -> 4MB -> 16MB chunks based on sequential access patterns |
| Negative caching | Reduces S3 HEAD requests for non-existent paths (10s TTL) |

### S3 Limitations (by design)

- **No atomic rename** -- implemented as CopyObject + DeleteObject
- **No symlinks/hardlinks** -- S3 has no equivalent
- **Random writes are expensive** -- requires download-modify-upload cycle
- **Eventual consistency** -- configurable cache TTL trades freshness for performance

## Monitoring

### Prometheus Metrics

Available at `http://localhost:9090/metrics` (default). Key metrics:

| Metric | Type | Labels | Description |
|---|---|---|---|
| `s3gw_nfs_operations_total` | Counter | `operation`, `status` | Total NFS operations |
| `s3gw_nfs_operation_duration_seconds` | Histogram | `operation` | NFS operation latency |
| `s3gw_s3_requests_total` | Counter | `method`, `status` | Total S3 API requests |
| `s3gw_s3_request_duration_seconds` | Histogram | `method` | S3 request latency |
| `s3gw_cache_hits_total` | Counter | `cache_type` | Cache hits (metadata/data) |
| `s3gw_cache_misses_total` | Counter | `cache_type` | Cache misses |
| `s3gw_active_connections` | Gauge | -- | Active NFS connections |
| `s3gw_bytes_transferred_total` | Counter | `direction` | Bytes read/written |

### Health Endpoints

| Endpoint | Description |
|---|---|
| `GET /health` | Liveness check. Always returns `200 {"status": "ok"}`. |
| `GET /ready` | Readiness check. Verifies S3 bucket is reachable. Returns `503` if unhealthy. |
| `GET /metrics` | Prometheus metrics scrape endpoint. |

## S3 Compatibility

| Backend | Status |
|---|---|
| MinIO | Primary test target |
| AWS S3 | Supported |
| Dell ObjectScale | Planned (path-style addressing, ports 9020/9021) |

## Development

```bash
make build          # Build binary to bin/s3nfsgw
make test           # Run unit tests
make lint           # Run golangci-lint
make fmt            # Format code
make vet            # Run go vet
make docker         # Build Docker image
make up             # Start docker-compose environment
make down           # Stop docker-compose environment
make integration    # Run integration tests
make all            # fmt + vet + lint + test + build
```

## Project Status

### Implemented

- Phase 1: NFS handle/inode management, S3 filesystem read path, NFSv4 server wiring
- Phase 2: Metadata cache (in-memory LRU + bbolt), ranged reads with adaptive prefetch
- Phase 3: Write operations (create, mkdir, remove, rename), cache invalidation on writes
- Phase 4: Disk-based data cache with ETag coherency, wired into the read path with TTL expiry
- Phase 5: Prometheus metrics, health/readiness endpoints, graceful shutdown
- Phase 6 (AWS parity P0-P3): chmod/chown via S3 metadata replace, truncate via empty upload, symlinks via marker objects, read-after-write consistency, token-bucket rate limiter, Grafana dashboard template

### Planned

- Dell ObjectScale compatibility testing
- Integration test suite for MinIO
- YAML config file loading (currently defaults + env vars only)

## License

Apache License 2.0
