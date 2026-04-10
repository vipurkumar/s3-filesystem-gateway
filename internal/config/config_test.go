package config

import (
	"testing"
	"time"
)

func TestLoad_Defaults(t *testing.T) {
	// Credentials are required now; set them for the defaults test.
	t.Setenv("S3_ACCESS_KEY", "testkey")
	t.Setenv("S3_SECRET_KEY", "testsecret")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	// S3 defaults
	if cfg.S3.Endpoint != "localhost:9000" {
		t.Errorf("S3.Endpoint = %q, want %q", cfg.S3.Endpoint, "localhost:9000")
	}
	if cfg.S3.AccessKey != "testkey" {
		t.Errorf("S3.AccessKey = %q, want %q", cfg.S3.AccessKey, "testkey")
	}
	if cfg.S3.SecretKey != "testsecret" {
		t.Errorf("S3.SecretKey = %q, want %q", cfg.S3.SecretKey, "testsecret")
	}
	if cfg.S3.Bucket != "data" {
		t.Errorf("S3.Bucket = %q, want %q", cfg.S3.Bucket, "data")
	}
	if cfg.S3.Region != "us-east-1" {
		t.Errorf("S3.Region = %q, want %q", cfg.S3.Region, "us-east-1")
	}
	if cfg.S3.UseSSL != true {
		t.Errorf("S3.UseSSL = %v, want true", cfg.S3.UseSSL)
	}
	if cfg.S3.PathStyle != true {
		t.Errorf("S3.PathStyle = %v, want true", cfg.S3.PathStyle)
	}

	// NFS defaults
	if cfg.NFS.Port != 2049 {
		t.Errorf("NFS.Port = %d, want 2049", cfg.NFS.Port)
	}
	if cfg.NFS.BindAddr != "0.0.0.0" {
		t.Errorf("NFS.BindAddr = %q, want %q", cfg.NFS.BindAddr, "0.0.0.0")
	}

	// Health defaults
	if cfg.Health.Port != 9090 {
		t.Errorf("Health.Port = %d, want 9090", cfg.Health.Port)
	}

	// Cache defaults
	if cfg.Cache.MetadataTTL != 60*time.Second {
		t.Errorf("Cache.MetadataTTL = %v, want %v", cfg.Cache.MetadataTTL, 60*time.Second)
	}
	if cfg.Cache.DataDir != "/var/cache/s3gw" {
		t.Errorf("Cache.DataDir = %q, want %q", cfg.Cache.DataDir, "/var/cache/s3gw")
	}
	if cfg.Cache.DataMaxSize != 10*1024*1024*1024 {
		t.Errorf("Cache.DataMaxSize = %d, want %d", cfg.Cache.DataMaxSize, int64(10*1024*1024*1024))
	}

	// Log defaults
	if cfg.Log.Level != "info" {
		t.Errorf("Log.Level = %q, want %q", cfg.Log.Level, "info")
	}
}

func TestLoad_MissingCredentials(t *testing.T) {
	_, err := Load("")
	if err == nil {
		t.Fatal("Load() should return error when S3 credentials are not set")
	}
	want := "S3_ACCESS_KEY and S3_SECRET_KEY environment variables are required"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestLoad_EnvOverrides(t *testing.T) {
	tests := []struct {
		name    string
		envKey  string
		envVal  string
		check   func(*Config) bool
		desc    string
	}{
		{
			name:   "S3_ENDPOINT",
			envKey: "S3_ENDPOINT",
			envVal: "s3.example.com:443",
			check:  func(c *Config) bool { return c.S3.Endpoint == "s3.example.com:443" },
			desc:   "S3.Endpoint should be overridden",
		},
		{
			name:   "S3_ACCESS_KEY",
			envKey: "S3_ACCESS_KEY",
			envVal: "mykey",
			check:  func(c *Config) bool { return c.S3.AccessKey == "mykey" },
			desc:   "S3.AccessKey should be overridden",
		},
		{
			name:   "S3_SECRET_KEY",
			envKey: "S3_SECRET_KEY",
			envVal: "mysecret",
			check:  func(c *Config) bool { return c.S3.SecretKey == "mysecret" },
			desc:   "S3.SecretKey should be overridden",
		},
		{
			name:   "S3_BUCKET",
			envKey: "S3_BUCKET",
			envVal: "mybucket",
			check:  func(c *Config) bool { return c.S3.Bucket == "mybucket" },
			desc:   "S3.Bucket should be overridden",
		},
		{
			name:   "S3_REGION",
			envKey: "S3_REGION",
			envVal: "eu-west-1",
			check:  func(c *Config) bool { return c.S3.Region == "eu-west-1" },
			desc:   "S3.Region should be overridden",
		},
		{
			name:   "S3_USE_SSL_true",
			envKey: "S3_USE_SSL",
			envVal: "true",
			check:  func(c *Config) bool { return c.S3.UseSSL == true },
			desc:   "S3.UseSSL should be true when set to 'true'",
		},
		{
			name:   "S3_USE_SSL_1",
			envKey: "S3_USE_SSL",
			envVal: "1",
			check:  func(c *Config) bool { return c.S3.UseSSL == true },
			desc:   "S3.UseSSL should be true when set to '1'",
		},
		{
			name:   "S3_USE_SSL_false",
			envKey: "S3_USE_SSL",
			envVal: "false",
			check:  func(c *Config) bool { return c.S3.UseSSL == false },
			desc:   "S3.UseSSL should be false when set to 'false'",
		},
		{
			name:   "S3_PATH_STYLE_true",
			envKey: "S3_PATH_STYLE",
			envVal: "true",
			check:  func(c *Config) bool { return c.S3.PathStyle == true },
			desc:   "S3.PathStyle should be true when set to 'true'",
		},
		{
			name:   "S3_PATH_STYLE_1",
			envKey: "S3_PATH_STYLE",
			envVal: "1",
			check:  func(c *Config) bool { return c.S3.PathStyle == true },
			desc:   "S3.PathStyle should be true when set to '1'",
		},
		{
			name:   "S3_PATH_STYLE_false",
			envKey: "S3_PATH_STYLE",
			envVal: "false",
			check:  func(c *Config) bool { return c.S3.PathStyle == false },
			desc:   "S3.PathStyle should be false when set to 'false'",
		},
		{
			name:   "NFS_PORT",
			envKey: "NFS_PORT",
			envVal: "3049",
			check:  func(c *Config) bool { return c.NFS.Port == 3049 },
			desc:   "NFS.Port should be overridden",
		},
		{
			name:   "HEALTH_PORT",
			envKey: "HEALTH_PORT",
			envVal: "8080",
			check:  func(c *Config) bool { return c.Health.Port == 8080 },
			desc:   "Health.Port should be overridden",
		},
		{
			name:   "LOG_LEVEL",
			envKey: "LOG_LEVEL",
			envVal: "debug",
			check:  func(c *Config) bool { return c.Log.Level == "debug" },
			desc:   "Log.Level should be overridden",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Ensure credentials are always set so Load doesn't fail.
			t.Setenv("S3_ACCESS_KEY", "testkey")
			t.Setenv("S3_SECRET_KEY", "testsecret")
			t.Setenv(tt.envKey, tt.envVal)
			cfg, err := Load("")
			if err != nil {
				t.Fatalf("Load() returned error: %v", err)
			}
			if !tt.check(cfg) {
				t.Errorf("%s", tt.desc)
			}
		})
	}
}

func TestLoad_InvalidNFSPort(t *testing.T) {
	t.Setenv("S3_ACCESS_KEY", "testkey")
	t.Setenv("S3_SECRET_KEY", "testsecret")
	t.Setenv("NFS_PORT", "notanumber")
	_, err := Load("")
	if err == nil {
		t.Fatal("Load() should return error for non-numeric NFS_PORT")
	}
	if got := err.Error(); got == "" {
		t.Error("error message should not be empty")
	}
}

func TestLoad_InvalidHealthPort(t *testing.T) {
	t.Setenv("S3_ACCESS_KEY", "testkey")
	t.Setenv("S3_SECRET_KEY", "testsecret")
	t.Setenv("HEALTH_PORT", "notanumber")
	_, err := Load("")
	if err == nil {
		t.Fatal("Load() should return error for non-numeric HEALTH_PORT")
	}
	if got := err.Error(); got == "" {
		t.Error("error message should not be empty")
	}
}
