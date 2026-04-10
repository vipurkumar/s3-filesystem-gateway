package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/vipurkumar/s3-filesystem-gateway/internal/config"
	"github.com/vipurkumar/s3-filesystem-gateway/internal/nfs"
	s3client "github.com/vipurkumar/s3-filesystem-gateway/internal/s3"
	"github.com/vipurkumar/s3-filesystem-gateway/internal/s3fs"
)

func main() {
	configPath := flag.String("config", "configs/default.yaml", "path to configuration file")
	dataDir := flag.String("data-dir", "/var/lib/s3nfsgw", "directory for persistent data (bbolt database)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load configuration", "error", err)
		os.Exit(1)
	}

	slog.Info("starting s3-filesystem-gateway",
		"s3_endpoint", cfg.S3.Endpoint,
		"s3_bucket", cfg.S3.Bucket,
		"nfs_port", cfg.NFS.Port,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		slog.Info("received signal, shutting down", "signal", sig)
		cancel()
	}()

	// 1. Initialize S3 client and verify bucket access
	s3c, err := s3client.NewClient(ctx, s3client.ClientConfig{
		Endpoint:  cfg.S3.Endpoint,
		AccessKey: cfg.S3.AccessKey,
		SecretKey: cfg.S3.SecretKey,
		Bucket:    cfg.S3.Bucket,
		Region:    cfg.S3.Region,
		UseSSL:    cfg.S3.UseSSL,
	})
	if err != nil {
		slog.Error("failed to connect to S3", "error", err)
		os.Exit(1)
	}
	slog.Info("connected to S3", "endpoint", cfg.S3.Endpoint, "bucket", cfg.S3.Bucket)

	// 2. Initialize bbolt metadata store
	if err := os.MkdirAll(*dataDir, 0755); err != nil {
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

	// 3. Start NFSv4 server
	nfsSrv, err := nfs.NewServer(nfs.ServerConfig{
		Port:    cfg.NFS.Port,
		S3:      s3c,
		Handles: handles,
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

	select {
	case err := <-errCh:
		if err != nil {
			slog.Error("NFS server error", "error", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		slog.Info("shutting down gracefully")
	}

	slog.Info("s3-filesystem-gateway stopped")
}
