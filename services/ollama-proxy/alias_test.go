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
		p := NewProxy(NewCodec(rwNop{}), NewDiscovery(), 11434)
		if err := p.setLoopbackAlias(tc.address); (err != nil) != tc.wantErr {
			t.Errorf("setLoopbackAlias(%q) error = %v, wantErr %v", tc.address, err, tc.wantErr)
		} else if err == nil && p.aliasAddr != tc.wantAddr {
			t.Errorf("setLoopbackAlias(%q) address = %q, want %q", tc.address, p.aliasAddr, tc.wantAddr)
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
	p := NewProxy(NewCodec(rec), disc, primaryPort)
	if err := p.setLoopbackAlias(fmt.Sprintf("127.0.0.1:%d", aliasPort)); err != nil {
		t.Fatal(err)
	}
	p.bindLoopbackAlias()
	primary, err := net.Listen("tcp", fmt.Sprintf(":%d", primaryPort))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.serveHTTP(ctx, primary)
	p.serveLoopbackAlias()
	defer p.shutdown(context.Background())

	assertRouted := func(port int) {
		t.Helper()
		resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/api/chat", port), "application/json", strings.NewReader(`{"model":"test"}`))
		if err != nil {
			t.Fatalf("POST through port %d: %v", port, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "from-upstream") {
			t.Fatalf("port %d response = %d %s", port, resp.StatusCode, body)
		}
	}
	assertRouted(primaryPort)
	assertRouted(aliasPort)
	if !rec.has("workload:started") || !rec.has("workload:completed") {
		t.Fatal("alias inference did not use the workload-producing router")
	}

	newPrimary := freeTCPPort(t)
	if err := p.setPort(newPrimary); err != nil {
		t.Fatalf("rebind primary: %v", err)
	}
	assertRouted(newPrimary)
	assertRouted(aliasPort)

	p.httpMu.Lock()
	bound := p.aliasLn.Addr().(*net.TCPAddr).IP
	p.httpMu.Unlock()
	if !bound.IsLoopback() {
		t.Fatalf("alias bound non-loopback address %s", bound)
	}
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
	p := NewProxy(NewCodec(rec), NewDiscovery(), primaryPort)
	if err := p.setLoopbackAlias(fmt.Sprintf("127.0.0.1:%d", aliasPort)); err != nil {
		t.Fatal(err)
	}
	p.bindLoopbackAlias()
	primary, err := net.Listen("tcp", fmt.Sprintf(":%d", primaryPort))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.serveHTTP(ctx, primary)
	p.serveLoopbackAlias()
	defer p.shutdown(context.Background())

	resp, err := http.Get(owner.URL)
	if err != nil {
		t.Fatalf("existing owner was disrupted: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "owner" {
		t.Fatalf("existing owner response = %q", body)
	}
	if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", primaryPort), time.Second); err != nil {
		t.Fatalf("primary stopped after alias conflict: %v", err)
	} else {
		_ = conn.Close()
	}
	if !rec.has(ollamaHostAliasBlockedID) || !rec.has(`"severity":"warning"`) || !rec.has(`"action":"none"`) || !rec.has("No process was stopped") {
		rec.mu.Lock()
		got := string(rec.b)
		rec.mu.Unlock()
		t.Fatalf("missing actionable alias warning: %s", got)
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
	p := NewProxy(NewCodec(rwNop{}), disc, primaryPort)
	if err := p.setLoopbackAlias(fmt.Sprintf("127.0.0.1:%d", aliasPort)); err != nil {
		t.Fatal(err)
	}
	p.bindLoopbackAlias()
	defer p.closeLoopbackAlias()
	candidates := p.resolveCandidates("")
	for _, candidate := range candidates {
		if candidate.id == "alias-self" {
			t.Fatalf("alias endpoint survived the proxy self-target guard: %+v", candidates)
		}
	}
	if len(candidates) != 1 || candidates[0].id != "real" {
		t.Fatalf("non-self candidate was lost: %+v", candidates)
	}
}

func TestAliasSelfTargetMatchesBoundLoopbackAddressNotPortAlone(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Fatalf("reserve 127.0.0.2 test port: %v", err)
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
	p := NewProxy(NewCodec(rwNop{}), disc, primaryPort)
	if err := p.setLoopbackAlias(fmt.Sprintf("127.0.0.2:%d", aliasPort)); err != nil {
		t.Fatal(err)
	}
	p.bindLoopbackAlias()
	defer p.closeLoopbackAlias()

	candidates := p.resolveCandidates("")
	if len(candidates) != 1 || candidates[0].id != "same-port-other-address" {
		t.Fatalf("candidates = %+v, want only same-port-other-address", candidates)
	}
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
	p := NewProxy(NewCodec(rwNop{}), disc, primaryPort)
	if err := p.setLoopbackAlias(fmt.Sprintf("127.0.0.1:%d", aliasPort)); err != nil {
		t.Fatal(err)
	}
	p.bindLoopbackAlias()
	if p.aliasLn != nil {
		t.Fatal("alias unexpectedly bound over the existing owner")
	}

	candidates := p.resolveCandidates("")
	if len(candidates) != 1 || candidates[0].id != "owner" {
		t.Fatalf("candidates = %+v, want existing owner retained", candidates)
	}
}

func TestAliasSelfTargetTreatsLocalhostAsCanonicalIPv6Loopback(t *testing.T) {
	target := &url.URL{Host: "localhost:11433"}
	if !isAliasSelfTarget(target, "[::1]:11433") {
		t.Fatal("localhost target did not match an owned IPv6 loopback alias")
	}
	if isAliasSelfTarget(target, "127.0.0.2:11433") {
		t.Fatal("localhost target incorrectly matched a distinct 127/8 alias")
	}
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
	if len(targets) != 1 || targets[0] != "[::1]:11433" {
		t.Fatalf("IPv6 loopback candidate was rewritten before alias ownership check: %v", targets)
	}
	if !isAliasSelfTarget(&url.URL{Host: targets[0]}, "[::1]:11433") {
		t.Fatal("IPv6 alias candidate was not recognized as the owned endpoint")
	}
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
	if port == 0 {
		t.Fatal("could not find a port free on both loopback families")
	}

	p := NewProxy(NewCodec(rwNop{}), NewDiscovery(), freeTCPPort(t))
	if err := p.setLoopbackAlias(fmt.Sprintf("127.0.0.1:%d", port)); err != nil {
		t.Fatal(err)
	}
	if err := p.setLoopbackAlias(fmt.Sprintf("[::1]:%d", port)); err != nil {
		t.Fatal(err)
	}
	p.bindLoopbackAlias()
	defer p.closeLoopbackAlias()
	if p.aliasLn == nil || p.aliasAltLn == nil {
		t.Fatal("localhost alias did not reserve both IPv4 and IPv6 loopback")
	}
	for _, address := range []string{
		fmt.Sprintf("127.0.0.1:%d", port),
		fmt.Sprintf("[::1]:%d", port),
	} {
		conn, err := net.DialTimeout("tcp", address, time.Second)
		if err != nil {
			t.Fatalf("dial reserved alias %s: %v", address, err)
		}
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

	p := NewProxy(NewCodec(rwNop{}), NewDiscovery(), freeTCPPort(t))
	if err := p.setLoopbackAlias(fmt.Sprintf("127.0.0.1:%d", port)); err != nil {
		t.Fatal(err)
	}
	if err := p.setLoopbackAlias(fmt.Sprintf("[::1]:%d", port)); err != nil {
		t.Fatal(err)
	}
	p.bindLoopbackAlias()
	if p.aliasLn != nil || p.aliasAltLn != nil {
		t.Fatal("partial localhost ownership survived an alternate-family bind failure")
	}
	rebound, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("IPv4 alias was not released after atomic bind failure: %v", err)
	}
	_ = rebound.Close()
}

func mustLoopbackListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}
