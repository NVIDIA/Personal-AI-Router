// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"

	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.Len(t, cands, 1, "unclustered candidate set")
	require.Equal(t, "manual-x", cands[0].id, "unclustered candidate")
	require.Equal(t, "", cands[0].peerUUID, "unclustered candidate")
	require.Equal(t, "http", cands[0].url.Scheme, "unclustered candidate")
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
	require.True(t, rec.Code == http.StatusForbidden, "non-loopback plaintext status")
	{
		got := rec.Header().Get("Access-Control-Allow-Origin")
		assert.Equal(t, "", got, "Access-Control-Allow-Origin")
	}
}

// Preflight is subject to the same ingress gate as ordinary requests.
func TestHandlePlainRejectsPreflightAtLoopbackGate(t *testing.T) {
	p := testProxy(anyProfile(t), NewDiscovery(), 11435)
	req := httptest.NewRequest(http.MethodOptions, "/api/generate", nil)
	req.RemoteAddr = "192.0.2.50:40000"
	rec := httptest.NewRecorder()

	p.soleFacade().handlePlain(rec, req)
	require.True(t, rec.Code == http.StatusForbidden, "preflight status")
	{
		got := rec.Header().Get("Access-Control-Allow-Origin")
		assert.Equal(t, "", got, "Access-Control-Allow-Origin")
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
		require.True(t, rec.Code == http.StatusConflict, "identity probe status")
		{
			body := rec.Body.String()
			require.Contains(t, body, tc.profile.DisplayName, "rejection does not name")
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
	require.True(t, rec.Code == http.StatusForbidden, "unclustered ingress status")
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
		{
			got := isLoopbackRemote(c.addr)
			assert.True(t, got == c.want, "isLoopbackRemote (%v)", got)
		}
	}
}

func TestLocalReverseProxyUsesSharedPlainTransport(t *testing.T) {
	p := testProxy(anyProfile(t), NewDiscovery(), 11435)
	shared := p.plainHTTPTransport()
	target := &url.URL{Scheme: "http", Host: "127.0.0.1:1"}
	rp := p.soleFacade().newLocalReverseProxy(target)
	tr, ok := rp.Transport.(*http.Transport)
	require.True(t, ok, "Transport type")
	require.True(t, tr == shared, "ingress reverse proxy did not use the shared plain Transport")
}
