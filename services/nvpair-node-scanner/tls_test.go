// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestTLSClientOptionsValidate(t *testing.T) {
	cases := []struct {
		name string
		o    tlsClientOptions
		ok   bool
	}{
		{"none", tlsClientOptions{}, true},
		{"ca only", tlsClientOptions{CABundlePath: "ca.pem"}, true},
		{"cert+key", tlsClientOptions{CertPath: "c", KeyPath: "k"}, true},
		{"cert without key", tlsClientOptions{CertPath: "c"}, false},
		{"key without cert", tlsClientOptions{KeyPath: "k"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.o.validate()
			if tc.ok && err != nil {
				t.Fatalf("validate() = %v, want nil", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("validate() = nil, want error")
			}
		})
	}
}

// TestBuildTLSClientUnconfiguredIsNil is the dormant-by-default guarantee: with
// no flags set, there's no TLS client and the daemon falls back to plain HTTP.
func TestBuildTLSClientUnconfiguredIsNil(t *testing.T) {
	c, err := buildTLSClient(tlsClientOptions{}, nodeInfoFetchTimeout)
	if err != nil {
		t.Fatalf("buildTLSClient(unconfigured) err = %v", err)
	}
	if c != nil {
		t.Fatal("buildTLSClient(unconfigured) should return a nil client (plain-HTTP fallback)")
	}
}

func TestBuildTLSClientCABundle(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	dir := t.TempDir()

	good := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(good, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := buildTLSClient(tlsClientOptions{CABundlePath: good}, nodeInfoFetchTimeout)
	if err != nil || c == nil {
		t.Fatalf("buildTLSClient(valid CA) = (%v, %v), want non-nil client and nil err", c, err)
	}

	bad := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(bad, []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := buildTLSClient(tlsClientOptions{CABundlePath: bad}, nodeInfoFetchTimeout); err == nil {
		t.Fatal("buildTLSClient(garbage CA) should error")
	}
}

// TestFetchNodeInfoTLSClientSelection proves the flag-gated wiring: an HTTPS-only
// node-info endpoint is reachable only when the daemon holds a configured TLS
// client; the default plain-HTTP client cannot speak to it.
func TestFetchNodeInfoTLSClientSelection(t *testing.T) {
	want := NodeInfoResponse{GPUs: []GPUInfo{{}}}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/node-info" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer srv.Close()

	host, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)

	// With the TLS client (which trusts the test server), the HTTPS fetch works.
	dTLS := &daemon{http: &http.Client{Timeout: nodeInfoFetchTimeout}, tlsHTTP: srv.Client()}
	if _, ok := dTLS.fetchNodeInfo(host, port); !ok {
		t.Error("fetchNodeInfo with a TLS client should reach the HTTPS node-info endpoint")
	}

	// Without it (dormant default), plain HTTP can't reach an HTTPS-only endpoint.
	dPlain := &daemon{http: &http.Client{Timeout: nodeInfoFetchTimeout}}
	if _, ok := dPlain.fetchNodeInfo(host, port); ok {
		t.Error("fetchNodeInfo without a TLS client should not reach an HTTPS-only endpoint over plain HTTP")
	}
}
