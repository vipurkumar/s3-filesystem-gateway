// Copyright 2024 s3-filesystem-gateway contributors
// SPDX-License-Identifier: Apache-2.0

package nfs

import (
	"crypto/tls"
	"fmt"
	"log/slog"

	libauth "github.com/smallfz/libnfs-go/auth"
	nfsbackend "github.com/smallfz/libnfs-go/backend"
	nfs "github.com/smallfz/libnfs-go/fs"
	libnfs "github.com/smallfz/libnfs-go/nfs"
	nfsserver "github.com/smallfz/libnfs-go/server"

	"github.com/vipurkumar/s3-filesystem-gateway/internal/cache"
	s3client "github.com/vipurkumar/s3-filesystem-gateway/internal/s3"
	"github.com/vipurkumar/s3-filesystem-gateway/internal/s3fs"
)

// acceptAuth accepts AUTH_NONE probes and decodes AUTH_SYS credentials for
// real operations. libnfs-go's backend requires a non-nil AuthenticationHandler;
// passing nil causes a SIGSEGV in Muxv4.Authenticate on the first client compound.
func acceptAuth(cred, verf *libnfs.Auth) (*libnfs.Auth, nfs.Creds, error) {
	if cred == nil || cred.Flavor == libnfs.AUTH_FLAVOR_NULL {
		return libauth.Null(cred, verf)
	}
	return libauth.Unix(cred, verf)
}

// Server wraps the libnfs-go NFSv4 server.
type Server struct {
	srv     *nfsserver.Server
	s3      *s3client.Client
	handles *s3fs.HandleStore
}

// ServerConfig holds NFS server configuration.
type ServerConfig struct {
	Port         int
	BindAddr     string
	S3           *s3client.Client
	Handles      *s3fs.HandleStore
	DataCacheDir string
	DataCacheMax int64
	// TLS, if non-nil, enables RFC 9289 in-band STARTTLS on the same
	// port as plaintext NFS. Clients opt in with `mount -o xprtsec=tls`
	// (Linux 6.5+); clients that don't ask continue to get plaintext.
	TLS *tls.Config
}

// NewServer creates a new NFSv4 server backed by S3.
func NewServer(cfg ServerConfig) (*Server, error) {
	s3c := cfg.S3
	handles := cfg.Handles

	// Create a shared metadata cache for all sessions.
	mc := cache.NewMetadataCache(cache.DefaultCacheConfig())

	// Create shared data cache if configured.
	var dc *cache.DataCache
	if cfg.DataCacheDir != "" {
		var err error
		dcConfig := cache.DataCacheConfig{
			Dir:     cfg.DataCacheDir,
			MaxSize: cfg.DataCacheMax,
		}
		dc, err = cache.NewDataCache(dcConfig)
		if err != nil {
			return nil, fmt.Errorf("create data cache: %w", err)
		}
	}

	// vfsLoader creates a fresh S3 filesystem per client session
	vfsLoader := func() nfs.FS {
		slog.Debug("creating new S3 filesystem session")
		return s3fs.NewS3FS(s3c, handles, mc, dc)
	}

	backend := nfsbackend.New(vfsLoader, acceptAuth)

	addr := fmt.Sprintf("%s:%d", cfg.BindAddr, cfg.Port)
	var serverOpts []nfsserver.Option
	if cfg.TLS != nil {
		serverOpts = append(serverOpts, nfsserver.WithTLSConfig(cfg.TLS))
		slog.Info("NFS in-band TLS enabled (RFC 9289)",
			"min_version", cfg.TLS.MinVersion,
			"client_auth", cfg.TLS.ClientAuth)
	}
	srv, err := nfsserver.NewServerTCP(addr, backend, serverOpts...)
	if err != nil {
		if dc != nil {
			dc.Stop()
		}
		return nil, fmt.Errorf("create NFS server: %w", err)
	}

	return &Server{
		srv:     srv,
		s3:      s3c,
		handles: handles,
	}, nil
}

// Serve starts the NFS server (blocking).
func (s *Server) Serve() error {
	slog.Info("NFSv4 server listening", "port", s.srv)
	return s.srv.Serve()
}
