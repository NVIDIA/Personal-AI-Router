// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestResolveCandidatesUnclusteredDropsRelayPeers is the core isolation
// assertion: an unclustered node (nil mesh) must not route inference to
// relay-discovered peers — even one advertising a cluster principal — because it
// holds no pin to reach them over mTLS. Only an explicit, user-added manual node
// survives, dialed plaintext.
func TestResolveCandidatesUnclusteredDropsRelayPeers(t *testing.T) {
	disc := NewDiscovery()
	disc.SetSubscribed([]Node{{
		ID: "peer-a", Host: "peer-a", Port: 11434,
		Addresses:   []string{"192.0.2.10"},
		IP:          "192.0.2.10",
		ClusterUUID: "cluster-uuid-a",
	}})
	disc.AddManual(Node{
		ID: "manual-x", Host: "manual-x", Port: 11434,
		Addresses: []string{"192.0.2.20"}, IP: "192.0.2.20",
	})
	p := testProxy(disc, 11435) // mesh nil => unclustered

	cands := p.resolveCandidates("")
	if len(cands) != 1 {
		t.Fatalf("unclustered candidate set = %+v, want exactly the manual node", cands)
	}
	if cands[0].id != "manual-x" || cands[0].peerUUID != "" || cands[0].url.Scheme != "http" {
		t.Fatalf("unclustered candidate = %+v, want plaintext manual-x with no peerUUID", cands[0])
	}
}

// TestHandlePlainRejectsNonLoopback proves the plaintext personality is
// loopback-only: a LAN caller is refused (closing the former open-relay), so
// peers cannot use the plaintext path at all.
func TestHandlePlainRejectsNonLoopback(t *testing.T) {
	p := testProxy(NewDiscovery(), 11435)
	req := httptest.NewRequest(http.MethodPost, "/api/generate", nil)
	req.RemoteAddr = "192.0.2.50:40000"
	rec := httptest.NewRecorder()

	p.handlePlain(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-loopback plaintext status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	// No CORS grant on the refusal: the caller sent no Origin and the proxy
	// writes grants only for allowlisted browser origins. A non-browser LAN
	// caller ignores CORS anyway; a cross-origin browser is gated by the
	// origin check that follows the loopback gate.
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want no grant on the refusal", got)
	}
}

// TestHandlePlainAnswersPreflightBeforeLoopbackGate: the preflight is answered
// even for a caller the gate will refuse. It authorizes nothing — the request
// that follows is still rejected — but without it the browser never sends that
// request and reports the refusal as a generic CORS failure.
func TestHandlePlainAnswersPreflightBeforeLoopbackGate(t *testing.T) {
	p := testProxy(NewDiscovery(), 11435)
	req := httptest.NewRequest(http.MethodOptions, "/api/generate", nil)
	req.RemoteAddr = "192.0.2.50:40000"
	rec := httptest.NewRecorder()

	p.handlePlain(rec, req)
	// The preflight is still answered (204) — it authorizes nothing and this
	// Origin-less caller gets no grant — and the request that follows would
	// hit the loopback gate's 403.
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want no grant", got)
	}
}

func TestHandlePlainRejectsEngineIdentityProbe(t *testing.T) {
	p := testProxy(NewDiscovery(), 11435)
	req := httptest.NewRequest(http.MethodGet, "/api/version", nil)
	req.RemoteAddr = "127.0.0.1:40000"
	req.Header.Set(engineIdentityProbeHeader, "1")
	rec := httptest.NewRecorder()

	p.handlePlain(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("identity probe status = %d, want %d", rec.Code, http.StatusConflict)
	}
}

// TestHandleClusterIngressUnclusteredForbids proves the mTLS ingress fails
// closed on an unclustered node (nil mesh): with no cluster identity there are
// no pins, so every caller is rejected.
func TestHandleClusterIngressUnclusteredForbids(t *testing.T) {
	p := testProxy(NewDiscovery(), 11435)
	req := httptest.NewRequest(http.MethodPost, "/api/generate", nil) // no client cert
	rec := httptest.NewRecorder()

	p.handleClusterIngress(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unclustered ingress status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestIsLoopbackRemote(t *testing.T) {
	for _, c := range []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:5000", true},
		{"[::1]:5000", true},
		{"192.168.1.10:5000", false},
		{"10.0.0.5:80", false},
		{"", false},
		{"garbage", false},
	} {
		if got := isLoopbackRemote(c.addr); got != c.want {
			t.Errorf("isLoopbackRemote(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

func TestLocalReverseProxyUsesSharedPlainTransport(t *testing.T) {
	p := testProxy(NewDiscovery(), 11435)
	shared := p.plainHTTPTransport()
	target := &url.URL{Scheme: "http", Host: "127.0.0.1:1"}
	rp := p.newLocalReverseProxy(target)
	tr, ok := rp.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport type = %T, want *http.Transport", rp.Transport)
	}
	if tr != shared {
		t.Fatal("ingress reverse proxy did not use the shared plain Transport")
	}
}
