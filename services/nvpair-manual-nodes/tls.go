// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

// tlsClientOptions captures the cert paths the manager uses when
// probing TLS-enabled manual nodes. All three are optional. When
// they're all empty, the manager still gets a working tlsClient
// (system trust store, no client cert) so a probe of a public-CA
// HTTPS node works without any flags. When --client-cert and
// --client-key are set they're sent on every TLS probe — there's
// no per-node "send cert here, don't there" — because the
// originating node is the user's own machine and the cost of
// always presenting a cert is zero.
type tlsClientOptions struct {
	CertPath     string
	KeyPath      string
	CABundlePath string
}

func (o tlsClientOptions) validate() error {
	switch {
	case o.CertPath != "" && o.KeyPath == "":
		return errors.New("--client-cert requires --client-key")
	case o.KeyPath != "" && o.CertPath == "":
		return errors.New("--client-key requires --client-cert")
	}
	return nil
}

// buildTLSClient mirrors the scanner's helper of the same name —
// the two would live in shared/applog if Go modules made local
// shared code less cumbersome to import, but for two ~30-line
// helpers it's cheaper to duplicate. Returns nil when nothing
// is configured; the caller falls back to a default *http.Client
// (system trust store) so out-of-the-box HTTPS to public-CA nodes
// just works.
func buildTLSClient(o tlsClientOptions, timeout time.Duration) (*http.Client, error) {
	if o.CertPath == "" && o.KeyPath == "" && o.CABundlePath == "" {
		return nil, nil
	}
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}
	if o.CABundlePath != "" {
		caPEM, err := os.ReadFile(o.CABundlePath)
		if err != nil {
			return nil, fmt.Errorf("read CA bundle: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("CA bundle %s contains no PEM certificates", o.CABundlePath)
		}
		cfg.RootCAs = pool
	}
	if o.CertPath != "" {
		cert, err := tls.LoadX509KeyPair(o.CertPath, o.KeyPath)
		if err != nil {
			return nil, fmt.Errorf("load client cert/key: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return &http.Client{
		Timeout: timeout,
		// DisableKeepAlives so each probe re-resolves the entry's address
		// rather than reusing a pooled connection to a now-stale IP.
		Transport: &http.Transport{TLSClientConfig: cfg, DisableKeepAlives: true},
	}, nil
}
