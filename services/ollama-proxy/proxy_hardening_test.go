// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBufferBodyAndModelRejectsOversizedBody: the inbound body cap is the
// fix for the unauthenticated OOM — handleHTTP buffers the whole body for
// failover replay, so an uncapped read lets any loopback client (or any
// visited web page) grow the proxy until it dies.
func TestBufferBodyAndModelRejectsOversizedBody(t *testing.T) {
	big := strings.NewReader(strings.Repeat("x", maxInferenceBodyBytes+1))
	req := httptest.NewRequest(http.MethodPost, "/api/chat", big)
	if _, _, err := bufferBodyAndModel(req); err != errBodyTooLarge {
		t.Fatalf("oversized body err = %v, want errBodyTooLarge", err)
	}

	// Exactly at the cap still passes.
	exact := strings.NewReader(strings.Repeat("x", maxInferenceBodyBytes))
	req = httptest.NewRequest(http.MethodPost, "/api/chat", exact)
	body, _, err := bufferBodyAndModel(req)
	if err != nil {
		t.Fatalf("at-cap body err = %v, want nil", err)
	}
	if len(body) != maxInferenceBodyBytes {
		t.Fatalf("at-cap body len = %d, want %d", len(body), maxInferenceBodyBytes)
	}

	// Small bodies still parse the model field.
	req = httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"model":"llama3"}`))
	_, model, err := bufferBodyAndModel(req)
	if err != nil || model != "llama3" {
		t.Fatalf("small body model = %q, err = %v; want %q, nil", model, err, "llama3")
	}
}

// TestHandleHTTPRejectsOversizedBody: an over-cap POST is answered 413 with
// a JSON error before any routing or engine work, and the over-cap read
// consumes at most cap+1 bytes from the client.
func TestHandleHTTPRejectsOversizedBody(t *testing.T) {
	p := testProxy(NewDiscovery(), 11435)

	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte(strings.Repeat("x", maxInferenceBodyBytes+1024)))
		_ = pw.Close()
	}()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", pr)
	rec := httptest.NewRecorder()

	p.handleHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized POST status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if !strings.Contains(rec.Body.String(), "request body exceeds") {
		t.Errorf("413 body = %q, want it to name the limit", rec.Body.String())
	}
}

// TestHandleHTTPAcceptsNormalBody: a small body is not 413'd (it takes the
// normal no-candidate rejection path in this fixture).
func TestHandleHTTPAcceptsNormalBody(t *testing.T) {
	p := testProxy(NewDiscovery(), 11435)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"model":"llama3"}`))
	rec := httptest.NewRecorder()

	p.handleHTTP(rec, req)

	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("normal POST status = 413, want the ordinary rejection path")
	}
}

// TestLocalBackendTargetRejectsNonLoopback: the documented "always loopback"
// invariant is now enforced — a non-loopback host from node/set-local-backend
// is refused instead of dialed, so the mTLS ingress can never be turned into
// a forwarder to an arbitrary LAN address.
func TestLocalBackendTargetRejectsNonLoopback(t *testing.T) {
	p := testProxy(NewDiscovery(), 11435)

	for _, host := range []string{"192.168.1.5", "10.0.0.2", "example.com", "::ffff:192.168.1.5"} {
		p.setLocalBackend(localBackend{Engine: "ollama", Host: host, Port: 11436, Healthy: true})
		if _, ok := p.localBackendTarget(); ok {
			t.Errorf("localBackendTarget accepted non-loopback host %q", host)
		}
	}
	for _, host := range []string{"", "127.0.0.1", "127.0.0.2", "::1"} {
		p.setLocalBackend(localBackend{Engine: "ollama", Host: host, Port: 11436, Healthy: true})
		u, ok := p.localBackendTarget()
		if !ok {
			t.Errorf("localBackendTarget rejected loopback host %q", host)
			continue
		}
		if u.Scheme != "http" {
			t.Errorf("localBackendTarget scheme = %q, want http", u.Scheme)
		}
	}
}

// TestPeerHTTPTransportFailsClosedWithoutPin: with no live certificate pin
// the transport refuses every dial instead of falling back to an unpinned
// transport — a pin that vanishes between candidate selection and dial time
// can never silently downgrade to an unauthenticated connection.
func TestPeerHTTPTransportFailsClosedWithoutPin(t *testing.T) {
	p := testProxy(NewDiscovery(), 11435) // mesh nil => unclustered

	tr := p.peerHTTPTransport("no-such-peer")
	if tr == nil {
		t.Fatal("peerHTTPTransport returned nil; want a fail-closed transport")
	}
	if tr.TLSClientConfig != nil {
		t.Error("fail-closed transport must not carry a TLS config")
	}
	conn, err := tr.DialContext(t.Context(), "tcp", "192.0.2.10:443")
	if err != errPeerUnpinned {
		t.Errorf("dial err = %v, want errPeerUnpinned", err)
	}
	if conn != nil {
		_ = conn.Close()
		t.Error("fail-closed transport dialed successfully")
	}
}
