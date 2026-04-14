# Security Policy

## Reporting Vulnerabilities

If you discover a security vulnerability in s3-filesystem-gateway, please report it responsibly.

**Primary channel:** Use GitHub Security Advisories private vulnerability reporting: <https://github.com/rupivbluegreen/s3-filesystem-gateway/security/advisories/new>

**Secondary (fallback) channel:** `security@s3-filesystem-gateway.dev`

**Do not** open a public GitHub issue for security vulnerabilities.

Please include:
- A description of the vulnerability
- Steps to reproduce
- Potential impact
- Suggested fix (if any)

### Response Timeline

| Stage | Timeframe |
|-------|-----------|
| Acknowledgement | Within 48 hours |
| Initial assessment | Within 5 business days |
| Fix development | Within 30 days for critical issues |
| Public disclosure | After fix is released, coordinated with reporter |

## Supported Versions

| Version | Supported |
|---------|-----------|
| Latest release | Yes |
| Previous minor release | Security fixes only |
| Older versions | No |

## Security Features

The gateway implements the following security measures:

### TLS for S3 Connections
TLS is enabled by default for all connections to S3-compatible backends. Disabling TLS requires explicit configuration and is not recommended for production use.

### NFS Transport TLS (RFC 9289, in-band STARTTLS)
The gateway implements [RFC 9289 RPC-with-TLS](https://datatracker.ietf.org/doc/html/rfc9289) on the NFSv4.2 transport. When `NFS_TLS_ENABLE=true` (with `NFS_TLS_CERT_FILE` / `NFS_TLS_KEY_FILE` pointing at a PEM-encoded server cert and key), the same TCP port speaks both plaintext NFS and TLS-wrapped NFS — Linux 6.5+ clients opt in with `mount -o xprtsec=tls,vers=4.2 ...`, plaintext clients continue unchanged. Defaults to TLS 1.3; set `NFS_TLS_MIN_VERSION=1.2` for legacy clients. Pointing `NFS_TLS_CLIENT_CA_FILE` at a CA bundle additionally requires the kernel to present a client certificate signed by one of those CAs (mutual TLS). All four ops in the v4.1 session-establishment dance run inside the encrypted tunnel after the AUTH_TLS upgrade, so client identifiers, file handles, and contents are protected end-to-end on the NFS link.

### Path Traversal Protection
All file paths are validated and sanitized to prevent directory traversal attacks (e.g., `../` sequences). Paths are resolved within the virtual filesystem root before being mapped to S3 keys.

### No Hardcoded Credentials
The gateway does not contain any hardcoded credentials. All AWS/S3 credentials are provided at runtime through environment variables, configuration files, or IAM roles.

### Non-Root Container Execution
The provided container image runs as a non-root user. The Dockerfile does not require or use root privileges at runtime.

### Restrictive File Permissions
Data directories are created with mode `0700`, ensuring only the owning user can read, write, or traverse them. This applies to local cache directories and any temporary storage.

### HTTP Server Timeouts (Slowloris Protection)
The health check and metrics HTTP servers are configured with read, write, and idle timeouts to mitigate slowloris and similar slow-connection denial-of-service attacks.

### Inode Counter Overflow Protection
The virtual inode allocator includes overflow detection to prevent inode number reuse or wrap-around, which could lead to file identity confusion.

## Known Limitations

### NFS Traffic Encryption Is Opt-In
> Plaintext is still the default for backwards compatibility with v0.1.0 deployments and for clients on kernels older than 6.5 that don't support `xprtsec=tls`. To get end-to-end NFS encryption set `NFS_TLS_ENABLE=true` (see "NFS Transport TLS" above and `deployments/docker/docker-compose.quickstart.tls.yml`) and mount with `-o xprtsec=tls,vers=4.2` from a Linux 6.5+ client with `ktls-utils` installed. Without TLS, deploy on a trusted network segment or wrap the link in a Wireguard mesh.

### NFS Client Authentication: AUTH_SYS by Default, mTLS Available
> The gateway uses AUTH_SYS (traditional UNIX UID/GID) for NFS authentication, which provides no cryptographic verification of client identity on its own. Pair `NFS_TLS_ENABLE=true` with `NFS_TLS_CLIENT_CA_FILE=...` to require clients to present an X.509 cert signed by your CA — the kernel's `xprtsec=mtls` mount option drives this. Kerberos (RPCSEC_GSS) is parked indefinitely; in-band TLS + mTLS covers the same threat model with much less operational burden.

### Rename Is Not Atomic
Rename operations are implemented as copy-then-delete on S3, which is not atomic. A failure during rename may result in the object existing at both the old and new keys. This is an inherent limitation of S3's object storage model.

## Dependency Security

Dependencies are tracked in `go.mod` and `go.sum`. To check for known vulnerabilities:

```bash
go install golang.org/dl/govulncheck@latest
govulncheck ./...
```

Keep dependencies up to date by running:

```bash
go get -u ./...
go mod tidy
```

Review dependency updates for security advisories before upgrading in production.

## EU/GDPR Compliance Notes

- **No personal data collected or stored.** The gateway itself does not collect, process, or store any personal data. It acts as a transparent protocol translation layer between NFS clients and an S3-compatible storage backend.

- **Transparent proxy model.** Data classification, retention policies, and GDPR compliance for stored data are the responsibility of the S3 storage backend operator, not the gateway.

- **Credentials are never logged.** AWS access keys, secret keys, session tokens, and other credentials are excluded from all log output regardless of log level.

- **Cache data follows backend access controls.** Locally cached data (metadata and content) is subject to the same access restrictions as the S3 backend. Cache directories use restrictive file permissions (0700).

- **Cache retention is bounded.** Cached data is automatically evicted based on TTL expiration and maximum cache size limits. No data is retained indefinitely.

- **No telemetry or third-party data sharing.** The gateway does not send telemetry, analytics, crash reports, or any data to third parties. All operations are strictly between the NFS client, the gateway, and the configured S3 backend.

- **Open source and auditable.** The gateway is released under the Apache License 2.0. The complete source code is available for security audit and compliance review.
