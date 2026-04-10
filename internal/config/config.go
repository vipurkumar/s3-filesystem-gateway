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
}

// S3Config holds S3 backend configuration.
type S3Config struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
	Region    string
	UseSSL    bool
}

// NFSConfig holds NFS server configuration.
type NFSConfig struct {
	Port int
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

// Load reads configuration from the given YAML file path,
// with environment variable overrides.
func Load(path string) (*Config, error) {
	// Start with defaults
	cfg := &Config{
		S3: S3Config{
			Endpoint:  "localhost:9000",
			AccessKey: "minioadmin",
			SecretKey: "minioadmin",
			Bucket:    "data",
			Region:    "us-east-1",
			UseSSL:    false,
		},
		NFS: NFSConfig{
			Port: 2049,
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

	return cfg, nil
}
