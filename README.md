# S3 Filesystem Gateway

[![CI](https://github.com/rupivbluegreen/s3-filesystem-gateway/actions/workflows/ci.yml/badge.svg)](https://github.com/rupivbluegreen/s3-filesystem-gateway/actions/workflows/ci.yml)
[![License](https://img.shields.io/github/license/rupivbluegreen/s3-filesystem-gateway)](LICENSE)
[![Release](https://img.shields.io/github/v/release/rupivbluegreen/s3-filesystem-gateway)](https://github.com/rupivbluegreen/s3-filesystem-gateway/releases)
[![Go Report Card](https://goreportcard.com/badge/github.com/vipurkumar/s3-filesystem-gateway)](https://goreportcard.com/report/github.com/vipurkumar/s3-filesystem-gateway)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/rupivbluegreen/s3-filesystem-gateway/badge)](https://scorecard.dev/viewer/?uri=github.com/rupivbluegreen/s3-filesystem-gateway)
[![Docker Pulls (GHCR)](https://img.shields.io/badge/ghcr.io-rupivbluegreen%2Fs3--filesystem--gateway-blue?logo=github)](https://github.com/rupivbluegreen/s3-filesystem-gateway/pkgs/container/s3-filesystem-gateway)
[![Docker Pulls (Hub)](https://img.shields.io/docker/pulls/vipurkumar/s3-filesystem-gateway)](https://hub.docker.com/r/vipurkumar/s3-filesystem-gateway)

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

# Mount from another terminal (Linux). The kernel default (v4.2)
# works since v0.3.0; v4.1 and v4.0 also work for older clients.
sudo mount -t nfs4 -o port=2049,nolock localhost:/ /mnt/s3

# Use it
ls /mnt/s3
echo "hello" > /mnt/s3/test.txt
cat /mnt/s3/test.txt
mkdir /mnt/s3/subdir

# MinIO console at http://localhost:9001 (minioadmin / minioadmin)
# Gateway health at http://localhost:9090/health
```

### Encrypted NFS (RFC 9289 in-band TLS)

Since v0.3.0 the gateway supports [RFC 9289 RPC-with-TLS](https://datatracker.ietf.org/doc/html/rfc9289) — encrypted NFSv4.2 traffic on the same port as plaintext, with no stunnel or VPN required. The Linux kernel client opts in per-mount with `xprtsec=tls`; existing plaintext mounts keep working unchanged.

**Server side** (one extra compose file, generates a self-signed cert on first run):

```bash
curl -O https://raw.githubusercontent.com/rupivbluegreen/s3-filesystem-gateway/main/deployments/docker/docker-compose.quickstart.tls.yml
docker compose -f docker-compose.quickstart.tls.yml up -d
```

**Client side** (Linux 6.5+, requires `ktls-utils` for the kernel TLS handshake daemon):

```bash
# One-time host setup
sudo apt install -y ktls-utils nfs-common
sudo systemctl enable --now tlshd

# Trust the gateway's self-signed cert (only needed for the quickstart;
# real deployments use a CA-signed cert)
docker compose -f docker-compose.quickstart.tls.yml cp gateway:/certs/server.crt /tmp/sbst.crt
sudo cp /tmp/sbst.crt /usr/local/share/ca-certificates/sbst.crt
sudo update-ca-certificates

# Mount with TLS
sudo mount -t nfs4 -o vers=4.2,xprtsec=tls,port=2049 localhost:/ /mnt/s3

# Verify the wire is encrypted (no NFS RPCs visible in tcpdump)
sudo tcpdump -i lo -nn port 2049 -c 4 -X
```

Configurable env vars on the gateway:

| Env Variable | Default | Description |
|---|---|---|
| `NFS_TLS_ENABLE` | `false` | Set to `true` to advertise AUTH_TLS to clients. |
| `NFS_TLS_CERT_FILE` | (unset) | PEM-encoded server certificate. Required when enabled. |
| `NFS_TLS_KEY_FILE` | (unset) | PEM-encoded private key. Required when enabled. |
| `NFS_TLS_CLIENT_CA_FILE` | (unset) | If set, requires clients to present a cert signed by one of these CAs (mTLS). |
| `NFS_TLS_MIN_VERSION` | `1.3` | Minimum TLS version. Set to `1.2` for legacy clients. |

Plaintext clients continue to work against a TLS-enabled gateway — the AUTH_TLS NULL probe is only sent when the kernel mounts with `xprtsec=tls`. Operators can run a single port serving both populations during migration.

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
| `NFS_TLS_ENABLE` | `false` | Enable RFC 9289 in-band TLS on the NFS port |
| `NFS_TLS_CERT_FILE` | (unset) | Path to PEM-encoded server certificate |
| `NFS_TLS_KEY_FILE` | (unset) | Path to PEM-encoded private key |
| `NFS_TLS_CLIENT_CA_FILE` | (unset) | CA bundle for mutual TLS (clients must present a cert) |
| `NFS_TLS_MIN_VERSION` | `1.3` | Minimum TLS version (`1.2` or `1.3`) |

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

#### v0.2.0 — quality-of-life

- Dell ObjectScale compatibility testing
- Integration test suite for MinIO with a functional NFS-mount healthcheck
- YAML config file loading (currently defaults + env vars only)
- README quickstart against the published image (no clone required)

#### v0.3.0 — native NFSv4.1 / NFSv4.2 + RFC 9289 RPC-with-TLS

The marquee feature for v0.3.0 is **native in-transit encryption over NFS** via RFC 9289 STARTTLS — no stunnel, no Wireguard, no out-of-band tunnel. To get there we have to first ship NFSv4.1 session support (the Linux kernel only negotiates `xprtsec=tls` at minorversion ≥ 2), so v0.3.0 is one coherent "modern NFS" release covering all three:

- **NFSv4.1 session ops** — `EXCHANGE_ID`, `CREATE_SESSION`, `SEQUENCE`, `DESTROY_SESSION`, `DESTROY_CLIENTID`, `FREE_STATEID`, `RECLAIM_COMPLETE`. Modern Linux clients no longer need explicit `-o vers=4.0`; the gateway will be simultaneously v4.0/v4.1/v4.2 capable.
- **NFSv4.2 minorversion advertising** — kernel mounts default to v4.2 and will succeed against the gateway out of the box.
- **RFC 9289 RPC-with-TLS** — `mount -t nfs4 -o xprtsec=tls gateway:/ /mnt/s3` works on Linux 6.5+ with a server certificate. Configurable via `NFS_TLS_ENABLE`, `NFS_TLS_CERT_FILE`, `NFS_TLS_KEY_FILE`, `NFS_TLS_CLIENT_CA_FILE`, `NFS_TLS_MIN_VERSION` env vars.
- **NFSv4.2 `COPY` (op 60) → S3 `CopyObject`** — `cp /mnt/s3/big.bin /mnt/s3/big.bak` on intra-bucket copies becomes a single `S3 CopyObject` request. Bytes never leave the object store; an order-of-magnitude perf win for archive/backup workflows.
- **NFSv4.2 xattrs (RFC 8276)** — `getfattr` / `setfattr` map to S3 user-metadata (`x-amz-meta-xattr-*`). Lets clients store SELinux contexts, file checksums, and arbitrary `user.*` namespace attributes.
- **NFSv4.2 `SEEK_HOLE` / `SEEK_DATA` stub** — reports "no holes; data extends to EOF" so `lseek` and tools that probe for sparse-file support behave correctly on the non-sparse S3 backend.

Other v4.2 ops that have no useful S3 mapping (`ALLOCATE`, `DEALLOCATE`, `CLONE`, `READ_PLUS`, `WRITE_SAME`, pNFS layout ops) return `NFS4ERR_NOTSUPP` cleanly so userspace tools fall back gracefully.

This work requires a fork of `github.com/smallfz/libnfs-go` (which is currently NFSv4.0 only); the fork lives at `github.com/rupivbluegreen/libnfs-go` and is wired in via a `go.mod replace` directive during development.

#### Parked / on demand

- **Kerberos (`sec=krb5p`)** — RPCSEC_GSS support is parked indefinitely. RFC 9289 TLS covers the same in-transit-encryption threat model with a fraction of the operational burden (no KDC, no keytabs, no clock-skew handling). Will only be revisited if a specific deployment requires Kerberos identity propagation.
- **pNFS layouts** — out of scope. Fundamentally incompatible with a single-node S3 gateway architecture.
- **NFSv3** — not served, no plan to.

## License

Apache License 2.0
