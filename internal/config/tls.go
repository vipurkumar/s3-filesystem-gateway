// Copyright 2024 s3-filesystem-gateway contributors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// BuildTLSConfig materialises a *tls.Config from a TLSConfig. Returns
// nil if TLS is disabled, or an error if the cert/key/CA files don't
// load. Suitable for passing straight into the libnfs-go server's
// WithTLSConfig option.
func (t TLSConfig) BuildTLSConfig() (*tls.Config, error) {
	if !t.Enable {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS cert/key: %w", err)
	}
	min := uint16(tls.VersionTLS13)
	if t.MinVersion == "1.2" {
		min = tls.VersionTLS12
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   min,
	}
	if t.ClientCAFile != "" {
		caBytes, err := os.ReadFile(t.ClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("read client CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caBytes) {
			return nil, fmt.Errorf("client CA file %s contains no usable certs", t.ClientCAFile)
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}
