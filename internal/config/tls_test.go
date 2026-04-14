// Copyright 2024 s3-filesystem-gateway contributors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// generateSelfSignedCert writes a fresh self-signed cert + key into
// dir and returns the file paths. Used by the BuildTLSConfig tests so
// they don't depend on any checked-in fixture.
func generateSelfSignedCert(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	certPath = filepath.Join(dir, "cert.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	keyDer, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	keyPath = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDer}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func TestBuildTLSConfig_Disabled(t *testing.T) {
	cfg, err := TLSConfig{Enable: false}.BuildTLSConfig()
	if err != nil || cfg != nil {
		t.Fatalf("expected nil/nil, got %v / %v", cfg, err)
	}
}

func TestBuildTLSConfig_DefaultMinVersion13(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := generateSelfSignedCert(t, dir)
	cfg, err := TLSConfig{
		Enable:   true,
		CertFile: certPath,
		KeyFile:  keyPath,
	}.BuildTLSConfig()
	if err != nil {
		t.Fatalf("BuildTLSConfig: %v", err)
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Fatalf("MinVersion = 0x%04x, want TLSv1.3 (0x%04x)", cfg.MinVersion, tls.VersionTLS13)
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("expected 1 certificate, got %d", len(cfg.Certificates))
	}
	if cfg.ClientAuth != tls.NoClientCert {
		t.Fatalf("ClientAuth = %v, want NoClientCert", cfg.ClientAuth)
	}
}

func TestBuildTLSConfig_MinVersion12(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := generateSelfSignedCert(t, dir)
	cfg, err := TLSConfig{
		Enable:     true,
		CertFile:   certPath,
		KeyFile:    keyPath,
		MinVersion: "1.2",
	}.BuildTLSConfig()
	if err != nil {
		t.Fatalf("BuildTLSConfig: %v", err)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = 0x%04x, want TLSv1.2", cfg.MinVersion)
	}
}

func TestBuildTLSConfig_MissingCertFile(t *testing.T) {
	_, err := TLSConfig{
		Enable:   true,
		CertFile: "/nope/cert.pem",
		KeyFile:  "/nope/key.pem",
	}.BuildTLSConfig()
	if err == nil {
		t.Fatal("expected error for missing cert/key, got nil")
	}
}

func TestBuildTLSConfig_MutualTLS(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := generateSelfSignedCert(t, dir)
	// Reuse the server cert as the client CA bundle — for the
	// purposes of this test we only need to exercise the load path.
	cfg, err := TLSConfig{
		Enable:       true,
		CertFile:     certPath,
		KeyFile:      keyPath,
		ClientCAFile: certPath,
	}.BuildTLSConfig()
	if err != nil {
		t.Fatalf("BuildTLSConfig: %v", err)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("ClientAuth = %v, want RequireAndVerifyClientCert", cfg.ClientAuth)
	}
	if cfg.ClientCAs == nil {
		t.Fatal("ClientCAs not populated")
	}
}

func TestLoad_TLSEnvVars(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := generateSelfSignedCert(t, dir)

	t.Setenv("S3_ACCESS_KEY", "k")
	t.Setenv("S3_SECRET_KEY", "s")
	t.Setenv("NFS_TLS_ENABLE", "true")
	t.Setenv("NFS_TLS_CERT_FILE", certPath)
	t.Setenv("NFS_TLS_KEY_FILE", keyPath)
	t.Setenv("NFS_TLS_MIN_VERSION", "1.2")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.TLS.Enable || cfg.TLS.CertFile != certPath || cfg.TLS.KeyFile != keyPath || cfg.TLS.MinVersion != "1.2" {
		t.Fatalf("TLS env vars not applied: %+v", cfg.TLS)
	}
}

func TestLoad_TLSEnabledWithoutCertFails(t *testing.T) {
	t.Setenv("S3_ACCESS_KEY", "k")
	t.Setenv("S3_SECRET_KEY", "s")
	t.Setenv("NFS_TLS_ENABLE", "true")
	if _, err := Load(""); err == nil {
		t.Fatal("expected Load to fail when NFS_TLS_ENABLE is set without cert paths")
	}
}

func TestLoad_TLSInvalidMinVersion(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := generateSelfSignedCert(t, dir)

	t.Setenv("S3_ACCESS_KEY", "k")
	t.Setenv("S3_SECRET_KEY", "s")
	t.Setenv("NFS_TLS_ENABLE", "true")
	t.Setenv("NFS_TLS_CERT_FILE", certPath)
	t.Setenv("NFS_TLS_KEY_FILE", keyPath)
	t.Setenv("NFS_TLS_MIN_VERSION", "1.0")

	if _, err := Load(""); err == nil {
		t.Fatal("expected Load to fail on TLS 1.0")
	}
}
