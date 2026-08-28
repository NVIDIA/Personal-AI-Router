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

// tls.go is the node-scanner's OPTIONAL client-side TLS for enriching a node over
// its node-info (ni) port. It is dormant by default: with none of the
// --client-cert / --client-key / --ca-bundle flags set, buildTLSClient returns
// nil and the daemon fetches node-info over plain HTTP — the standard path, since
// node-info is plain per the transport policy. The scaffolding is kept so an
// operator can move node-info enrichment onto TLS in the future purely via these
// flags, with no code change and — deliberately — no per-node tls-port=/mtls= TXT
// self-report (that self-report was dropped to keep the transport policy static:
// a service owns its transport, the record carries only ports).

// tlsClientOptions captures the three cert-related CLI knobs. All three are
// optional: no flags -> plain HTTP (nil client); --ca-bundle alone -> verify
// server certs against that bundle (server-only TLS); adding --client-cert +
// --client-key on top -> mTLS.
type tlsClientOptions struct {
	CertPath     string
	KeyPath      string
	CABundlePath string
}

// validate enforces the cert/key pairing rule: both are given, or neither. A bare
// --ca-bundle is always fine — it just means "trust these CAs but don't
// authenticate myself".
func (o tlsClientOptions) validate() error {
	switch {
	case o.CertPath != "" && o.KeyPath == "":
		return errors.New("--client-cert requires --client-key")
	case o.KeyPath != "" && o.CertPath == "":
		return errors.New("--client-key requires --client-cert")
	}
	return nil
}

// configured reports whether any TLS knob is set, i.e. whether the operator has
// opted node-info enrichment onto HTTPS.
func (o tlsClientOptions) configured() bool {
	return o.CertPath != "" || o.KeyPath != "" || o.CABundlePath != ""
}

// buildTLSClient produces an *http.Client for HTTPS node-info fetches. It keeps
// the system trust store as a base; --ca-bundle adds roots on top of that so an
// operator deploying a private CA doesn't also have to remove the OS roots.
//
// Returns nil when nothing is configured — the caller falls back to its
// plain-HTTP client so plain nodes keep working out of the box.
func buildTLSClient(o tlsClientOptions, timeout time.Duration) (*http.Client, error) {
	if !o.configured() {
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if o.CABundlePath != "" {
		caPEM, err := os.ReadFile(o.CABundlePath)
		if err != nil {
			return nil, fmt.Errorf("read CA bundle: %w", err)
		}
		// Start from the system roots so we don't turn off WebPKI when the
		// operator only wants to *add* a private CA. SystemCertPool can fail on
		// some minimal Linux containers, in which case we fall back to an empty
		// pool.
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
		Timeout:   timeout,
		Transport: nodeInfoTransport(cfg),
	}, nil
}
