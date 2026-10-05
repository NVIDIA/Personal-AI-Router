// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetLoopbackAliasValidation(t *testing.T) {
	for _, tc := range []struct {
		address  string
		wantErr  bool
		wantAddr string
	}{
		{"127.0.0.1:11433", false, "127.0.0.1:11433"},
		{"127.0.0.2:11433", false, "127.0.0.2:11433"},
		{"localhost:11433", false, "127.0.0.1:11433"},
		{"[::1]:11433", false, "[::1]:11433"},
		{"0.0.0.0:11433", true, ""},
		{"192.168.1.20:11433", true, ""},
		{"localhost:11434", true, ""},
		{"localhost:0", true, ""},
		{"not-an-address", true, ""},
	} {
		p := newTestProxy(ollamaOnlyProfile(t), NewCodec(rwNop{}), NewDiscovery(), 11434)
		if err := p.soleFacade().setLoopbackAlias(tc.address); (err != nil) != tc.wantErr {
			assert.Fail(t, "test expectation failed", "setLoopbackAlias(%q) error = %v, wantErr %v", tc.address, err, tc.wantErr)
		} else {
			assert.False(t, err == nil && p.soleFacade().aliasAddr != tc.wantAddr, "setLoopbackAlias")
		}
	}
}

func TestLoopbackAliasUsesPrimaryRouterAndSurvivesPrimaryRebind(t *testing.T) {
	redirectConfigDir(t)
	rec := &recRW{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"message":"from-upstream"}`)
	}))
	defer upstream.Close()

	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "upstream", upstream.URL, "test"))
	primaryPort := freeTCPPort(t)
	aliasPort := freeTCPPort(t)
	p := newTestProxy(ollamaOnlyProfile(t), NewCodec(rec), disc, primaryPort)
	require.NoError(t, p.soleFacade().setLoopbackAlias(fmt.Sprintf("127.0.0.1:%d", aliasPort)))
	p.soleFacade().bindLoopbackAlias()
	primary, err := net.Listen("tcp", fmt.Sprintf(":%d", primaryPort))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.soleFacade().serveHTTP(ctx, primary)
	p.soleFacade().serveLoopbackAlias()
	defer p.shutdown(context.Background())

	assertRouted := func(port int) {
		t.Helper()
		resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/api/chat", port), "application/json", strings.NewReader(`{"model":"test"}`))
		require.NoError(t, err, "POST through port (%v, %v)", port, err)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.True(t, resp.StatusCode == http.StatusOK, "port (%v, %v)", port, body)
		require.Contains(t, string(body), "from-upstream", "port (%v, %v)", port, body)
	}
	assertRouted(primaryPort)
	assertRouted(aliasPort)
	require.True(t, rec.has("workload:started"), "alias inference did not use the workload-producing router")
	require.True(t, rec.has("workload:completed"), "alias inference did not use the workload-producing router")

	newPrimary := freeTCPPort(t)
	require.NoError(t, p.soleFacade().setPort(newPrimary), "rebind primary")
	assertRouted(newPrimary)
	assertRouted(aliasPort)

	p.soleFacade().httpMu.Lock()
	bound := p.soleFacade().aliasLn.Addr().(*net.TCPAddr).IP
	p.soleFacade().httpMu.Unlock()
	require.True(t, bound.IsLoopback(), "alias bound non-loopback address (%v)", bound)
}

func TestOccupiedLoopbackAliasLeavesOwnerAndPrimaryRunning(t *testing.T) {
	rec := &recRW{}
	owner := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "owner")
	}))
	owner.Listener = mustLoopbackListener(t)
	owner.Start()
	defer owner.Close()
	aliasPort := owner.Listener.Addr().(*net.TCPAddr).Port

	primaryPort := freeTCPPort(t)
	p := newTestProxy(ollamaOnlyProfile(t), NewCodec(rec), NewDiscovery(), primaryPort)
	require.NoError(t, p.soleFacade().setLoopbackAlias(fmt.Sprintf("127.0.0.1:%d", aliasPort)))
	p.soleFacade().bindLoopbackAlias()
	primary, err := net.Listen("tcp", fmt.Sprintf(":%d", primaryPort))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.soleFacade().serveHTTP(ctx, primary)
	p.soleFacade().serveLoopbackAlias()
	defer p.shutdown(context.Background())

	resp, err := http.Get(owner.URL)
	require.NoError(t, err, "existing owner was disrupted")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, "owner", string(body), "existing owner response (%v)", body)
	if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", primaryPort), time.Second); err != nil {
		require.FailNow(t, "test expectation failed", "primary stopped after alias conflict: %v", err)
	} else {
		_ = conn.Close()
	}
	if !rec.has(ollamaHostAliasBlockedID) || !rec.has(`"severity":"warning"`) || !rec.has(`"action":"none"`) || !rec.has("No process was stopped") {
		rec.mu.Lock()
		got := string(rec.b)
		rec.mu.Unlock()
		require.FailNow(t, "test expectation failed", "missing actionable alias warning: %s", got)
	}
}

func TestAliasPortIsAProxySelfTarget(t *testing.T) {
	primaryPort := freeTCPPort(t)
	aliasPort := freeTCPPort(t)
	for aliasPort == primaryPort {
		aliasPort = freeTCPPort(t)
	}
	disc := NewDiscovery()
	disc.AddManual(Node{ID: "alias-self", Addresses: []string{"127.0.0.1"}, Port: aliasPort})
	disc.AddManual(Node{ID: "real", Addresses: []string{"192.0.2.10"}, Port: primaryPort})
	p := newTestProxy(ollamaOnlyProfile(t), NewCodec(rwNop{}), disc, primaryPort)
	require.NoError(t, p.soleFacade().setLoopbackAlias(fmt.Sprintf("127.0.0.1:%d", aliasPort)))
	p.soleFacade().bindLoopbackAlias()
	defer p.soleFacade().closeLoopbackAlias()
	candidates := p.soleFacade().resolveCandidates("")
	for _, candidate := range candidates {
		require.NotEqual(t, "alias-self", candidate.id, "alias endpoint survived the proxy self-target guard (%v)", candidates)
	}
	require.Len(t, candidates, 1, "non-self candidate was lost")
	require.Equal(t, "real", candidates[0].id, "non-self candidate was lost (%v)", candidates)
}

func TestAliasSelfTargetMatchesBoundLoopbackAddressNotPortAlone(t *testing.T) {
	// Proving the guard matches the bound address rather than the port alone
	// needs a second loopback address. Linux aliases all of 127.0.0.0/8 to lo
	// so this runs there and in CI; macOS binds only 127.0.0.1 unless an alias
	// was added by hand, and there is no way to make the distinction without
	// one. Skip rather than fail, so the suite is green on a Mac.
	probe, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skipf("no second loopback address available on this host: %v", err)
	}
	aliasPort := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	disc := NewDiscovery()
	disc.AddManual(Node{ID: "alias-self", Addresses: []string{"127.0.0.2"}, Port: aliasPort})
	disc.AddManual(Node{ID: "same-port-other-address", Addresses: []string{"127.0.0.1"}, Port: aliasPort})
	primaryPort := freeTCPPort(t)
	for primaryPort == aliasPort {
		primaryPort = freeTCPPort(t)
	}
	p := newTestProxy(ollamaOnlyProfile(t), NewCodec(rwNop{}), disc, primaryPort)
	require.NoError(t, p.soleFacade().setLoopbackAlias(fmt.Sprintf("127.0.0.2:%d", aliasPort)))
	p.soleFacade().bindLoopbackAlias()
	defer p.soleFacade().closeLoopbackAlias()

	candidates := p.soleFacade().resolveCandidates("")
	require.Len(t, candidates, 1, "candidates")
	require.Equal(t, "same-port-other-address", candidates[0].id, "candidates (%v)", candidates)
}

func TestFailedAliasBindDoesNotClaimOwnersEndpointAsSelf(t *testing.T) {
	owner := mustLoopbackListener(t)
	defer owner.Close()
	aliasPort := owner.Addr().(*net.TCPAddr).Port

	disc := NewDiscovery()
	disc.AddManual(Node{ID: "owner", Addresses: []string{"127.0.0.1"}, Port: aliasPort})
	primaryPort := freeTCPPort(t)
	for primaryPort == aliasPort {
		primaryPort = freeTCPPort(t)
	}
	p := newTestProxy(ollamaOnlyProfile(t), NewCodec(rwNop{}), disc, primaryPort)
	require.NoError(t, p.soleFacade().setLoopbackAlias(fmt.Sprintf("127.0.0.1:%d", aliasPort)))
	p.soleFacade().bindLoopbackAlias()
	require.Nil(t, p.soleFacade().aliasLn, "alias unexpectedly bound over the existing owner")

	candidates := p.soleFacade().resolveCandidates("")
	require.Len(t, candidates, 1, "candidates")
	require.Equal(t, "owner", candidates[0].id, "candidates (%v)", candidates)
}

func TestAliasSelfTargetTreatsLocalhostAsCanonicalIPv6Loopback(t *testing.T) {
	target := &url.URL{Host: "localhost:11433"}
	require.True(t, isAliasSelfTarget(target, "[::1]:11433"), "localhost target did not match an owned IPv6 loopback alias")
	require.False(t, isAliasSelfTarget(target, "127.0.0.2:11433"), "localhost target incorrectly matched a distinct 127/8 alias")
}

func TestIPv6AliasCandidatePreservesAddressForSelfCheck(t *testing.T) {
	localAddrsMu.RLock()
	original := make(map[string]bool, len(localAddrs))
	for address, local := range localAddrs {
		original[address] = local
	}
	localAddrsMu.RUnlock()
	t.Cleanup(func() { setLocalAddrs(original) })
	setLocalAddrs(map[string]bool{"::1": true})

	const port = 11433
	targets := nodeCandidates(Node{Addresses: []string{"::1"}, Port: port})
	require.Len(t, targets, 1, "IPv6 loopback candidate was rewritten before alias ownership check")
	require.Equal(t, "[::1]:11433", targets[0], "IPv6 loopback candidate was rewritten before alias ownership check (%v)", targets)
	require.True(t, isAliasSelfTarget(&url.URL{Host: targets[0]}, "[::1]:11433"), "IPv6 alias candidate was not recognized as the owned endpoint")
}

func TestLoopbackAliasOwnsBothLocalhostFamilies(t *testing.T) {
	var port int
	for attempt := 0; attempt < 10 && port == 0; attempt++ {
		ipv6, err := net.Listen("tcp", "[::1]:0")
		if err != nil {
			t.Skipf("IPv6 loopback unavailable: %v", err)
		}
		candidate := ipv6.Addr().(*net.TCPAddr).Port
		_ = ipv6.Close()
		ipv4, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", candidate))
		if err == nil {
			port = candidate
			_ = ipv4.Close()
		}
	}
	require.NotEqual(t, 0, port, "could not find a port free on both loopback families")

	p := newTestProxy(ollamaOnlyProfile(t), NewCodec(rwNop{}), NewDiscovery(), freeTCPPort(t))
	require.NoError(t, p.soleFacade().setLoopbackAlias(fmt.Sprintf("127.0.0.1:%d", port)))
	require.NoError(t, p.soleFacade().setLoopbackAlias(fmt.Sprintf("[::1]:%d", port)))
	p.soleFacade().bindLoopbackAlias()
	defer p.soleFacade().closeLoopbackAlias()
	require.NotNil(t, p.soleFacade().aliasLn, "localhost alias did not reserve both IPv4 and IPv6 loopback")
	require.NotNil(t, p.soleFacade().aliasAltLn, "localhost alias did not reserve both IPv4 and IPv6 loopback")
	for _, address := range []string{
		fmt.Sprintf("127.0.0.1:%d", port),
		fmt.Sprintf("[::1]:%d", port),
	} {
		conn, err := net.DialTimeout("tcp", address, time.Second)
		require.NoError(t, err, "dial reserved alias (%v, %v)", address, err)
		_ = conn.Close()
	}
}

func TestDualAliasBindIsAtomic(t *testing.T) {
	owner, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	defer owner.Close()
	port := owner.Addr().(*net.TCPAddr).Port
	probe, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Skipf("matching IPv4 loopback port unavailable: %v", err)
	}
	_ = probe.Close()

	p := newTestProxy(ollamaOnlyProfile(t), NewCodec(rwNop{}), NewDiscovery(), freeTCPPort(t))
	require.NoError(t, p.soleFacade().setLoopbackAlias(fmt.Sprintf("127.0.0.1:%d", port)))
	require.NoError(t, p.soleFacade().setLoopbackAlias(fmt.Sprintf("[::1]:%d", port)))
	p.soleFacade().bindLoopbackAlias()
	require.Nil(t, p.soleFacade().aliasLn, "partial localhost ownership survived an alternate-family bind failure")
	require.Nil(t, p.soleFacade().aliasAltLn, "partial localhost ownership survived an alternate-family bind failure")
	rebound, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, err, "IPv4 alias was not released after atomic bind failure")
	_ = rebound.Close()
}

func mustLoopbackListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	return ln
}
