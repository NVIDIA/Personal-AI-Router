// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
)

// tlsOptions captures the four cert-related CLI knobs the operator
// can flip on nvpair-node-info. Kept as a small value type so the
// validation and the actual config-build step can be unit-tested
// without having to wire up flag.Parse / os.Exit / log.Fatalf.
type tlsOptions struct {
	CertPath     string
	KeyPath      string
	ClientCAPath string
}

// Enabled reports whether the operator asked for an HTTPS listener
// at all. We require both --cert and --key to be set together; the
// validation step rejects half-configurations earlier so by the time
// anyone calls Enabled the pair is already known to be consistent.
func (o tlsOptions) Enabled() bool {
	return o.CertPath != "" && o.KeyPath != ""
}

// MutualAuth reports whether client-cert verification should be
// enforced. Implies Enabled() — the validation step rejects
// --client-ca without --cert/--key.
func (o tlsOptions) MutualAuth() bool {
	return o.ClientCAPath != ""
}

// validate normalizes the bring-your-own contract: cert and key go
// together, and a client CA only makes sense if the server side is
// already serving TLS. Any other combination is rejected before we
// touch the disk so the operator gets a clear error rather than an
// HTTPS server that silently downgrades to no client auth.
func (o tlsOptions) validate() error {
	switch {
	case o.CertPath != "" && o.KeyPath == "":
		return errors.New("--cert requires --key")
	case o.KeyPath != "" && o.CertPath == "":
		return errors.New("--key requires --cert")
	case o.ClientCAPath != "" && (o.CertPath == "" || o.KeyPath == ""):
		return errors.New("--client-ca requires --cert and --key")
	}
	return nil
}

// buildTLSConfig loads the cert/key pair from disk and, when a
// client CA is configured, the trust bundle to verify incoming
// client certs against. We pin MinVersion to TLS 1.2 because the
// scanner / manual-nodes clients all run modern Go and there
// is no reason to accept anything older. ClientAuth is set to
// RequireAndVerifyClientCert so a connection with no cert (or one
// signed by something not in our bundle) is dropped at handshake
// time and never reaches the HTTP layer.
func buildTLSConfig(o tlsOptions) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(o.CertPath, o.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("load cert/key: %w", err)
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if o.ClientCAPath != "" {
		caPEM, err := os.ReadFile(o.ClientCAPath)
		if err != nil {
			return nil, fmt.Errorf("read client CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("client CA file %s contains no PEM certificates", o.ClientCAPath)
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}
