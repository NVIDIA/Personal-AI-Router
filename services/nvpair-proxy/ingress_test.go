// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"nvpair-shared/cors"
	"strings"
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
	p := testProxy(anyProfile(t), disc, 11435) // mesh nil => unclustered

	cands := p.soleFacade().resolveCandidates("")
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
	p := testProxy(anyProfile(t), NewDiscovery(), 11435)
	req := httptest.NewRequest(http.MethodPost, "/api/generate", nil)
	req.RemoteAddr = "192.0.2.50:40000"
	rec := httptest.NewRecorder()

	p.soleFacade().handlePlain(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-loopback plaintext status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want no CORS header on the refusal", got)
	}
}

// TestAllowlistGateRejectsUnlistedOrigin covers the request-entry gate: a
// loopback request from an origin the operator allowlist does not name is
// refused with 403 before it can drive an engine. The Origin-less (non-browser)
// caller is unaffected. This runs before engine-policy intersection, which is
// what closes the blind-oracle path a simple cross-origin POST would otherwise
// reach with no preflight.
func TestAllowlistGateRejectsUnlistedOrigin(t *testing.T) {
	t.Setenv(cors.AllowedOriginsEnv, "https://ui.example")

	t.Run("unlisted origin is refused", func(t *testing.T) {
		p := testProxy(anyProfile(t), NewDiscovery(), 11435)
		req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
		req.RemoteAddr = "127.0.0.1:40000"
		req.Header.Set("Origin", "https://evil.example")
		rec := httptest.NewRecorder()
		p.soleFacade().handlePlain(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("unlisted origin status = %d, want %d", rec.Code, http.StatusForbidden)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Access-Control-Allow-Origin = %q, want no grant on a refusal", got)
		}
	})

	t.Run("allowlisted origin reaches routing", func(t *testing.T) {
		p := testProxy(anyProfile(t), NewDiscovery(), 11435)
		req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
		req.RemoteAddr = "127.0.0.1:40000"
		req.Header.Set("Origin", "https://ui.example")
		rec := httptest.NewRecorder()
		// No engine is available, so the request reaches routing and is
		// rejected there (502), not by the allowlist gate (403).
		p.soleFacade().handlePlain(rec, req)
		if rec.Code == http.StatusForbidden {
			t.Fatalf("allowlisted origin was denied by the gate (403); want it to pass through to routing")
		}
	})

	t.Run("no Origin is unaffected", func(t *testing.T) {
		p := testProxy(anyProfile(t), NewDiscovery(), 11435)
		req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
		req.RemoteAddr = "127.0.0.1:40000"
		rec := httptest.NewRecorder()
		p.soleFacade().handlePlain(rec, req)
		if rec.Code == http.StatusForbidden {
			t.Fatalf("Origin-less request was denied by the gate (403); want it unaffected")
		}
	})
}

// Preflight is subject to the same ingress gate as ordinary requests.
func TestHandlePlainRejectsPreflightAtLoopbackGate(t *testing.T) {
	p := testProxy(anyProfile(t), NewDiscovery(), 11435)
	req := httptest.NewRequest(http.MethodOptions, "/api/generate", nil)
	req.RemoteAddr = "192.0.2.50:40000"
	rec := httptest.NewRecorder()

	p.soleFacade().handlePlain(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("preflight status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want no CORS header", got)
	}
}

// engine-manager marks its own identity probes so the compatibility facade can
// never be adopted as the engine itself. The rejection has to name the engine
// this proxy fronts, or the operator reading it is sent to the wrong process —
// which is why this runs per engine rather than asserting only the status.
func TestHandlePlainRejectsEngineIdentityProbe(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		p := testProxy(tc.profile, NewDiscovery(), tc.profile.StandalonePort)
		req := httptest.NewRequest(http.MethodGet, "/api/version", nil)
		req.RemoteAddr = "127.0.0.1:40000"
		req.Header.Set(engineIdentityProbeHeader, "1")
		rec := httptest.NewRecorder()

		p.soleFacade().handlePlain(rec, req)
		if rec.Code != http.StatusConflict {
			t.Fatalf("identity probe status = %d, want %d", rec.Code, http.StatusConflict)
		}
		if body := rec.Body.String(); !strings.Contains(body, tc.profile.DisplayName) {
			t.Fatalf("rejection does not name %s: %s", tc.profile.DisplayName, body)
		}
	})
}

// TestHandleClusterIngressUnclusteredForbids proves the mTLS ingress fails
// closed on an unclustered node (nil mesh): with no cluster identity there are
// no pins, so every caller is rejected.
func TestHandleClusterIngressUnclusteredForbids(t *testing.T) {
	p := testProxy(anyProfile(t), NewDiscovery(), 11435)
	req := httptest.NewRequest(http.MethodPost, "/api/generate", nil) // no client cert
	rec := httptest.NewRecorder()

	p.soleFacade().handleClusterIngress(rec, req)
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
	p := testProxy(anyProfile(t), NewDiscovery(), 11435)
	shared := p.plainHTTPTransport()
	target := &url.URL{Scheme: "http", Host: "127.0.0.1:1"}
	rp := p.soleFacade().newLocalReverseProxy(target)
	tr, ok := rp.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport type = %T, want *http.Transport", rp.Transport)
	}
	if tr != shared {
		t.Fatal("ingress reverse proxy did not use the shared plain Transport")
	}
}
