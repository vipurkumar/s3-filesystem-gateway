// Copyright 2024 s3-filesystem-gateway contributors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds the gateway configuration.
type Config struct {
	S3     S3Config
	NFS    NFSConfig
	Health HealthConfig
	Cache  CacheConfig
	Log    LogConfig
	TLS    TLSConfig
}

// S3Config holds S3 backend configuration.
type S3Config struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
	Region    string
	UseSSL    bool
	PathStyle bool
}

// NFSConfig holds NFS server configuration.
type NFSConfig struct {
	Port     int
	BindAddr string
}

// HealthConfig holds health/metrics server configuration.
type HealthConfig struct {
	Port int
}

// CacheConfig holds caching configuration.
type CacheConfig struct {
	MetadataTTL time.Duration
	DataDir     string
	DataMaxSize int64 // bytes
}

// LogConfig holds logging configuration.
type LogConfig struct {
	Level string
}

// TLSConfig holds in-band TLS (RFC 9289) configuration for the NFS
// server. When Enable is true the server advertises AUTH_TLS in
// response to the kernel's STARTTLS probe and upgrades the connection
// in place. CertFile / KeyFile point to a PEM-encoded server cert and
// key. ClientCAFile, if set, enables mutual TLS — clients must
// present a cert signed by one of the CAs in the file. MinVersion
// accepts "1.2" or "1.3" (default 1.3).
type TLSConfig struct {
	Enable       bool
	CertFile     string
	KeyFile      string
	ClientCAFile string
	MinVersion   string
}

// Load reads configuration from the given YAML file path,
// with environment variable overrides.
func Load(path string) (*Config, error) {
	// Start with defaults
	cfg := &Config{
		S3: S3Config{
			Endpoint:  "localhost:9000",
			AccessKey: "",
			SecretKey: "",
			Bucket:    "data",
			Region:    "us-east-1",
			UseSSL:    true,
			PathStyle: true,
		},
		NFS: NFSConfig{
			Port:     2049,
			BindAddr: "0.0.0.0",
		},
		Health: HealthConfig{
			Port: 9090,
		},
		Cache: CacheConfig{
			MetadataTTL: 60 * time.Second,
			DataDir:     "/var/cache/s3gw",
			DataMaxSize: 10 * 1024 * 1024 * 1024, // 10GB
		},
		Log: LogConfig{
			Level: "info",
		},
		TLS: TLSConfig{
			Enable:     false,
			MinVersion: "1.3",
		},
	}

	// TODO: Load YAML file if it exists (viper integration in Phase 2)

	// Environment variable overrides
	if v := os.Getenv("S3_ENDPOINT"); v != "" {
		cfg.S3.Endpoint = v
	}
	if v := os.Getenv("S3_ACCESS_KEY"); v != "" {
		cfg.S3.AccessKey = v
	}
	if v := os.Getenv("S3_SECRET_KEY"); v != "" {
		cfg.S3.SecretKey = v
	}
	if v := os.Getenv("S3_BUCKET"); v != "" {
		cfg.S3.Bucket = v
	}
	if v := os.Getenv("S3_REGION"); v != "" {
		cfg.S3.Region = v
	}
	if v := os.Getenv("S3_USE_SSL"); v != "" {
		cfg.S3.UseSSL = v == "true" || v == "1"
	}
	if v := os.Getenv("S3_PATH_STYLE"); v != "" {
		cfg.S3.PathStyle = v == "true" || v == "1"
	}
	if v := os.Getenv("NFS_BIND_ADDR"); v != "" {
		cfg.NFS.BindAddr = v
	}
	if v := os.Getenv("NFS_PORT"); v != "" {
		port, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid NFS_PORT: %w", err)
		}
		cfg.NFS.Port = port
	}
	if v := os.Getenv("HEALTH_PORT"); v != "" {
		port, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid HEALTH_PORT: %w", err)
		}
		cfg.Health.Port = port
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		cfg.Log.Level = v
	}
	if v := os.Getenv("CACHE_DATA_DIR"); v != "" {
		cfg.Cache.DataDir = v
	}
	if v := os.Getenv("CACHE_DATA_MAX_SIZE"); v != "" {
		size, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid CACHE_DATA_MAX_SIZE: %w", err)
		}
		cfg.Cache.DataMaxSize = size
	}
	if v := os.Getenv("CACHE_METADATA_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid CACHE_METADATA_TTL: %w", err)
		}
		cfg.Cache.MetadataTTL = d
	}
	if v := os.Getenv("NFS_TLS_ENABLE"); v != "" {
		cfg.TLS.Enable = v == "true" || v == "1"
	}
	if v := os.Getenv("NFS_TLS_CERT_FILE"); v != "" {
		cfg.TLS.CertFile = v
	}
	if v := os.Getenv("NFS_TLS_KEY_FILE"); v != "" {
		cfg.TLS.KeyFile = v
	}
	if v := os.Getenv("NFS_TLS_CLIENT_CA_FILE"); v != "" {
		cfg.TLS.ClientCAFile = v
	}
	if v := os.Getenv("NFS_TLS_MIN_VERSION"); v != "" {
		cfg.TLS.MinVersion = v
	}
	if cfg.TLS.Enable {
		if cfg.TLS.CertFile == "" || cfg.TLS.KeyFile == "" {
			return nil, fmt.Errorf("NFS_TLS_ENABLE=true requires NFS_TLS_CERT_FILE and NFS_TLS_KEY_FILE")
		}
		switch cfg.TLS.MinVersion {
		case "", "1.2", "1.3":
		default:
			return nil, fmt.Errorf("invalid NFS_TLS_MIN_VERSION %q (want \"1.2\" or \"1.3\")", cfg.TLS.MinVersion)
		}
	}

	// Validate required credentials
	if cfg.S3.AccessKey == "" || cfg.S3.SecretKey == "" {
		return nil, fmt.Errorf("S3_ACCESS_KEY and S3_SECRET_KEY environment variables are required")
	}

	return cfg, nil
}
