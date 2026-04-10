// Copyright 2024 s3-filesystem-gateway contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/vipurkumar/s3-filesystem-gateway/internal/config"
	"github.com/vipurkumar/s3-filesystem-gateway/internal/health"
	_ "github.com/vipurkumar/s3-filesystem-gateway/internal/metrics" // register Prometheus metrics
	"github.com/vipurkumar/s3-filesystem-gateway/internal/nfs"
	s3client "github.com/vipurkumar/s3-filesystem-gateway/internal/s3"
	"github.com/vipurkumar/s3-filesystem-gateway/internal/s3fs"
)

// Build-time variables set via -ldflags.
var (
	version   = "dev"
	gitCommit = "unknown"
	buildDate = "unknown"
)

func main() {
	configPath := flag.String("config", "configs/default.yaml", "path to configuration file")
	dataDir := flag.String("data-dir", "/var/lib/s3nfsgw", "directory for persistent data (bbolt database)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	// Startup banner
	slog.Info("starting s3-filesystem-gateway",
		"version", version,
		"commit", gitCommit,
		"built", buildDate,
	)

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load configuration", "error", err)
		os.Exit(1)
	}

	slog.Info("configuration loaded",
		"s3_endpoint", cfg.S3.Endpoint,
		"s3_bucket", cfg.S3.Bucket,
		"nfs_port", cfg.NFS.Port,
		"health_port", cfg.Health.Port,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Initialize S3 client and verify bucket access
	s3c, err := s3client.NewClient(ctx, s3client.ClientConfig{
		Endpoint:  cfg.S3.Endpoint,
		AccessKey: cfg.S3.AccessKey,
		SecretKey: cfg.S3.SecretKey,
		Bucket:    cfg.S3.Bucket,
		Region:    cfg.S3.Region,
		UseSSL:    cfg.S3.UseSSL,
		PathStyle: cfg.S3.PathStyle,
	})
	if err != nil {
		slog.Error("failed to connect to S3", "error", err)
		os.Exit(1)
	}
	slog.Info("connected to S3", "endpoint", cfg.S3.Endpoint, "bucket", cfg.S3.Bucket)

	// 2. Start health/metrics server
	healthAddr := fmt.Sprintf(":%d", cfg.Health.Port)
	healthSrv := health.NewHealthServer(healthAddr, func() error {
		checkCtx, checkCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer checkCancel()
		ok, err := s3c.BucketExists(checkCtx)
		if err != nil {
			return fmt.Errorf("S3 health check failed: %w", err)
		}
		if !ok {
			return fmt.Errorf("S3 bucket not found")
		}
		return nil
	})
	if err := healthSrv.Start(); err != nil {
		slog.Error("failed to start health server", "error", err)
		os.Exit(1)
	}
	slog.Info("health/metrics server started", "addr", healthAddr)

	// 3. Initialize bbolt metadata store
	if err := os.MkdirAll(*dataDir, 0700); err != nil {
		slog.Error("failed to create data directory", "error", err)
		os.Exit(1)
	}
	dbPath := filepath.Join(*dataDir, "handles.db")
	handles, err := s3fs.NewHandleStore(dbPath)
	if err != nil {
		slog.Error("failed to open handle store", "error", err)
		os.Exit(1)
	}
	defer handles.Close()
	slog.Info("handle store opened", "path", dbPath)

	// 4. Start NFSv4 server
	nfsSrv, err := nfs.NewServer(nfs.ServerConfig{
		Port:     cfg.NFS.Port,
		BindAddr: cfg.NFS.BindAddr,
		S3:       s3c,
		Handles:  handles,
	})
	if err != nil {
		slog.Error("failed to create NFS server", "error", err)
		os.Exit(1)
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("NFSv4 server starting", "port", cfg.NFS.Port)
		errCh <- nfsSrv.Serve()
	}()

	// 5. Wait for shutdown signal or server error
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil {
			slog.Error("NFS server error", "error", err)
			os.Exit(1)
		}
	case sig := <-sigCh:
		slog.Info("received signal, starting graceful shutdown", "signal", sig)
	}

	// Graceful shutdown with 30-second timeout
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	// Stop health server first
	slog.Info("stopping health server")
	if err := healthSrv.Stop(shutdownCtx); err != nil {
		slog.Error("health server shutdown error", "error", err)
	}

	// Cancel the main context to signal NFS server shutdown
	cancel()
	slog.Info("waiting for NFS server to stop")

	slog.Info("s3-filesystem-gateway stopped")
}
