// Copyright 2024 s3-filesystem-gateway contributors
// SPDX-License-Identifier: Apache-2.0

package nfs

import (
	"fmt"
	"log/slog"

	nfs "github.com/smallfz/libnfs-go/fs"
	nfsbackend "github.com/smallfz/libnfs-go/backend"
	nfsserver "github.com/smallfz/libnfs-go/server"
	"github.com/vipurkumar/s3-filesystem-gateway/internal/cache"
	s3client "github.com/vipurkumar/s3-filesystem-gateway/internal/s3"
	"github.com/vipurkumar/s3-filesystem-gateway/internal/s3fs"
)

// Server wraps the libnfs-go NFSv4 server.
type Server struct {
	srv     *nfsserver.Server
	s3      *s3client.Client
	handles *s3fs.HandleStore
}

// ServerConfig holds NFS server configuration.
type ServerConfig struct {
	Port     int
	BindAddr string
	S3       *s3client.Client
	Handles  *s3fs.HandleStore
}

// NewServer creates a new NFSv4 server backed by S3.
func NewServer(cfg ServerConfig) (*Server, error) {
	s3c := cfg.S3
	handles := cfg.Handles

	// Create a shared metadata cache for all sessions.
	mc := cache.NewMetadataCache(cache.DefaultCacheConfig())

	// vfsLoader creates a fresh S3 filesystem per client session
	vfsLoader := func() nfs.FS {
		slog.Debug("creating new S3 filesystem session")
		return s3fs.NewS3FS(s3c, handles, mc)
	}

	backend := nfsbackend.New(vfsLoader, nil)

	addr := fmt.Sprintf("%s:%d", cfg.BindAddr, cfg.Port)
	srv, err := nfsserver.NewServerTCP(addr, backend)
	if err != nil {
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
