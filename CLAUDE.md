# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Enterprise-grade NFSv4.1-to-S3 gateway written in Go. Exposes S3-compatible object storage (MinIO, AWS S3, Dell ObjectScale) as an NFSv4 filesystem.

## Build and Test Commands

```bash
make build              # Build binary to bin/s3nfsgw
make test               # Run all unit tests (go test ./... -v)
make lint               # Run golangci-lint
make fmt                # Format code
make vet                # Run go vet
make all                # fmt + vet + lint + test + build
make docker             # Build Docker image
make up                 # Start gateway + MinIO via docker-compose
make down               # Stop docker-compose environment
make test-integration   # Run integration tests (requires Docker, builds containers)
make integration        # Run integration tests (uses existing containers)
make test-all           # Unit + integration tests

# Run a single test
go test ./internal/s3fs/ -run TestSpecificName -v

# Run tests for a single package
go test ./internal/cache/ -v

# Run the binary
./bin/s3nfsgw --config configs/default.yaml --data-dir /var/lib/s3nfsgw
```

Docker compose files live in `deployments/docker/` (`docker-compose.yml`, `docker-compose.test.yml`, `docker-compose.objectscale.yml`).

## Architecture

```
NFS Clients
    |
NFSv4.1 Server (libnfs-go, port 2049)
    |  creates per-session S3FS instances via vfsLoader closure
    |
S3 Filesystem (implements libnfs-go/fs.FS interface)
    |  POSIX ops -> S3 API calls, handle/inode management via bbolt
    |
+---+---+
|       |
Metadata Cache (in-memory LRU)    Data Cache (disk-based LRU)
|       |
S3 Client (minio-go, connection pooling)
    |
Object Storage (MinIO/S3/ObjectScale)

Health Server (port 9090): /health, /ready, /metrics
```

**Key data flow:** NFS server (`internal/nfs/`) creates an `S3FS` instance per session. Each `S3FS` (`internal/s3fs/`) translates POSIX operations to S3 API calls via the S3 client (`internal/s3/`). Two cache layers sit between: metadata cache (`internal/cache/metadata.go`) for stat/listing results, and data cache (`internal/cache/data.go`) for file content on disk.

**Session model:** `internal/nfs/server.go` uses a `vfsLoader` closure that creates a new `S3FS` per NFS session, sharing the metadata cache across sessions.

**Inode/handle management:** `internal/s3fs/handle.go` uses bbolt for persistent bidirectional inode <-> S3 key mapping with an in-memory cache layer. Inodes are synthetic monotonic counters, 8-byte big-endian encoded (following libnfs-go memfs pattern).

**Write path:** Files buffer to local temp file, upload to S3 on `Close()` (write-once/read-many optimized). POSIX metadata stored as S3 user-metadata headers (`x-amz-meta-uid/gid/mode`).

**Read path:** `chunkReader` (`internal/s3fs/reader.go`) does adaptive prefetch: 1MB -> 4MB (after 4 sequential reads) -> 16MB (after 12). Resets on seek.

**S3 abstraction:** `internal/s3/interface.go` defines `S3API` interface (HeadObject, GetObject, GetObjectRange, ListObjects, PutObject, DeleteObject, CopyObject, CreateDirMarker). Tests mock this interface rather than hitting real S3.

## Key Design Decisions

- **libnfs-go fs.FS interface** (not go-billy) -- reference `memfs` package in libnfs-go for implementation patterns
- **minio-go over aws-sdk-go** -- first-class MinIO/ObjectScale support
- **Directory handling** -- marker objects (trailing `/`) + implicit dirs from ListObjectsV2 prefixes
- **Cache invalidation** -- writes invalidate affected key + parent directory listing
- **Negative caching** -- non-existent paths cached for 10s to reduce S3 HEAD traffic
- **S3 limitations** -- no atomic rename (copy+delete), no symlinks/hardlinks, random writes require download-modify-upload

## Conventions

- **Module:** `github.com/vipurkumar/s3-filesystem-gateway`
- **Config:** `os.Getenv` with hardcoded defaults (no viper/cobra). See `internal/config/config.go` for all env vars.
- **Key env vars:** `S3_ENDPOINT`, `S3_ACCESS_KEY`, `S3_SECRET_KEY`, `S3_BUCKET`, `S3_REGION`, `S3_USE_SSL`, `S3_PATH_STYLE`, `NFS_PORT`, `HEALTH_PORT`, `CACHE_METADATA_TTL`, `CACHE_DATA_DIR`, `CACHE_DATA_MAX_SIZE`, `LOG_LEVEL`
- **CLI flags:** `--config` (YAML config path, default `configs/default.yaml`), `--data-dir` (bbolt DB location, default `/var/lib/s3nfsgw`)
- **Logging:** `log/slog` structured JSON to stdout
- **Error handling:** `fmt.Errorf` with `%w` wrapping, `os.ErrNotExist` for not-found
- **Concurrency:** `sync.Mutex` / `sync.RWMutex` per struct, no global locks
- **S3 key mapping:** root `/` maps to empty string `""`, paths stripped of leading `/`, dirs end with `/`
- **Code layout:** `internal/` (private packages), `cmd/s3nfsgw/` (CLI entry point)
- **Build vars:** `version`, `gitCommit`, `buildDate` set via `-ldflags` at build time

## Test Environment

- MinIO container on ports 9000 (API) / 9001 (console), creds: minioadmin / minioadmin
- Gateway on port 2049 (NFS), port 9090 (health/metrics)
- Mount: `sudo mount -t nfs4 localhost:/ /mnt/s3`

## S3 Compatibility Targets

1. **MinIO** -- primary test target (docker-compose included)
2. **AWS S3** -- standard compatibility
3. **Dell ObjectScale** -- ports 9020/9021, path-style addressing, S3 API subset (separate compose file: `docker-compose.objectscale.yml`)

## NFSv4 Library References

- **libnfs-go memfs:** reference implementation for fs.FS interface (`github.com/smallfz/libnfs-go/memfs/`)
- **buildbarn NFSv4:** reference for protocol edge cases (`github.com/buildbarn/bb-remote-execution/pkg/filesystem/virtual/nfsv4/`)
