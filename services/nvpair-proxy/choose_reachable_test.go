// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/reach"
)

// waitForTarget polls targetURL until it settles on want.
//
// Routing never waits on a handshake — reach.Prefer answers with the node's own
// ranking and confirms behind the request — so the address a multi-homed peer
// settles on is what the requests after the first see.
func waitForTarget(t *testing.T, p *Proxy, n Node, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		u := p.soleFacade().targetURL(n)
		if u != nil && u.Host == want {
			return
		}
		if time.Now().After(deadline) {
			got := "<nil>"
			if u != nil {
				got = u.Host
			}
			require.FailNow(t, "test expectation failed", "targetURL settled on %s, want %s", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// countingChooser installs a target chooser that records how many connection
// attempts routing makes and whether they succeed, so a test can assert on
// confirmation behaviour without opening sockets.
func countingChooser(p *Proxy, accept bool) *atomic.Int32 {
	var dials atomic.Int32
	p.soleFacade().targets = reach.NewChooser(reach.WithDial(
		func(_, _ string, _ time.Duration) (net.Conn, error) {
			dials.Add(1)
			if !accept {
				return nil, net.ErrClosed
			}
			c1, c2 := net.Pipe()
			_ = c2.Close()
			return c1, nil
		}))
	return &dials
}

func TestChooseReachableFailsOverForPinnedPeer(t *testing.T) {
	const peerUUID = "principal-peer"
	const reachable = "192.0.2.11"
	clusterDir := filepath.Join(t.TempDir(), "cluster")
	clustertrusttest.Join(t, clusterDir, "cluster-xyz", "principal-self", peerUUID)

	p := testProxy(anyProfile(t), NewDiscovery(), 11435)
	p.mesh = clustertrust.Open(clusterDir)
	var dials atomic.Int32
	p.soleFacade().targets = reach.NewChooser(reach.WithDial(
		func(_, address string, _ time.Duration) (net.Conn, error) {
			dials.Add(1)
			host, _, _ := net.SplitHostPort(address)
			if host != reachable {
				return nil, net.ErrClosed
			}
			c1, c2 := net.Pipe()
			_ = c2.Close()
			return c1, nil
		}))

	n := Node{
		ID:          "peer-a",
		Port:        11434,
		Addresses:   []string{"192.0.2.10", "192.0.2.11"},
		ClusterUUID: peerUUID,
	}
	// The first request is not made to wait for the confirmation, so it uses the
	// node's own top-ranked address; the ones behind it use the one that answers.
	{
		u := p.soleFacade().targetURL(n)
		require.NotNil(t, u, "first selection")
		require.True(t, u.Host == net.JoinHostPort("192.0.2.10", "11434"), "first selection (%v)", u)
	}
	waitForTarget(t, p, n, net.JoinHostPort(reachable, "11434"))
	require.Equal(t, int32(2), dials.Load(), "pinned peer triggered")
}

func TestChooseReachableProbesPlainMultiHomed(t *testing.T) {
	p := testProxy(anyProfile(t), NewDiscovery(), 11435)
	dials := countingChooser(p, true)
	n := Node{
		ID:        "manual-a",
		Port:      11434,
		Addresses: []string{"192.0.2.10", "192.0.2.11"},
	}
	require.NotNil(t, p.soleFacade().targetURL(n), "targetURL returned nil")
	deadline := time.Now().Add(2 * time.Second)
	for dials.Load() == 0 {
		require.False(t, time.Now().After(deadline), "plain multi-homed target did not confirm reachability")
		time.Sleep(time.Millisecond)
	}
}

// TestTargetURLFailsOverToAReachableAddress is the reported defect at the routing
// layer: a peer whose canonical address is a direct-connect link this host cannot
// reach must still be routed to, at the address that answers.
func TestTargetURLFailsOverToAReachableAddress(t *testing.T) {
	const reachable = "10.172.55.129"
	p := testProxy(anyProfile(t), NewDiscovery(), 11435)
	p.soleFacade().targets = reach.NewChooser(reach.WithDial(
		func(_, address string, _ time.Duration) (net.Conn, error) {
			host, _, _ := net.SplitHostPort(address)
			if host != reachable {
				return nil, net.ErrClosed
			}
			c1, c2 := net.Pipe()
			_ = c2.Close()
			return c1, nil
		}))

	n := Node{
		ID:   "spark",
		Port: 11434,
		// The node's own ranking leads with a link only its cabled neighbour can
		// reach; this host is not that neighbour.
		Addresses: []string{"192.168.240.1", reachable},
		TXT:       []string{"ip=192.168.240.1", "ips=192.168.240.1," + reachable},
	}
	waitForTarget(t, p, n, net.JoinHostPort(reachable, "11434"))
}

// TestNodeCandidatesKeepsPublishedOrder: the node ranked its addresses from
// evidence no observer has, so routing must try them in that order rather than
// re-sorting by address class.
func TestNodeCandidatesKeepsPublishedOrder(t *testing.T) {
	n := Node{
		ID:        "spark",
		Port:      11434,
		Addresses: []string{"192.168.240.1", "10.172.55.129"},
		TXT:       []string{"ip=10.172.55.129", "ips=10.172.55.129,192.168.240.1"},
	}
	got := nodeCandidates(n)
	want := []string{
		net.JoinHostPort("10.172.55.129", "11434"),
		net.JoinHostPort("192.168.240.1", "11434"),
	}
	require.Len(t, got, len(want), "nodeCandidates (%v, %v)", got, want)
	require.True(t, got[0] == want[0], "nodeCandidates (%v, %v)", got, want)
	require.True(t, got[1] == want[1], "nodeCandidates (%v, %v)", got, want)
}

// fakeNetwork is a chooser dialer whose accepting address can be moved, so a test
// can describe an address that stops answering and another that starts. Probes run
// on background goroutines, so both fields are read concurrently with the test.
type fakeNetwork struct {
	mu        sync.Mutex
	accepting string
	dials     atomic.Int32
}

func (f *fakeNetwork) accept(address string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accepting = address
}

func (f *fakeNetwork) install(p *Proxy) {
	p.soleFacade().targets = reach.NewChooser(reach.WithDial(
		func(_, address string, _ time.Duration) (net.Conn, error) {
			f.dials.Add(1)
			host, _, _ := net.SplitHostPort(address)
			f.mu.Lock()
			accepting := f.accepting
			f.mu.Unlock()
			if host != accepting {
				return nil, net.ErrClosed
			}
			c1, c2 := net.Pipe()
			_ = c2.Close()
			return c1, nil
		}))
}

// confirmedDeadPeer builds a proxy whose only routing target is a multi-homed peer
// whose confirmed address no longer answers: the address is a loopback endpoint
// whose server is already closed, so a forwarded request fails at the transport
// the way an unplugged link does, while the chooser still believes in it.
func confirmedDeadPeer(t *testing.T, replacement string) (*Proxy, Node, *fakeNetwork) {
	t.Helper()
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	n := nodeForModel(t, "peer-a", dead.URL, "llama")
	dead.Close()
	confirmed := n.Addresses[0]
	n.Addresses = append(n.Addresses, replacement)

	disc := NewDiscovery()
	disc.AddManual(n)
	p := testProxy(anyProfile(t), disc, 11435)
	fake := &fakeNetwork{accepting: confirmed}
	fake.install(p)

	waitForTarget(t, p, n, net.JoinHostPort(confirmed, strconv.Itoa(n.Port)))
	return p, n, fake
}

// assertReprobed moves the accepting address and requires the selections after the
// failure to confirm again and land on the replacement. A cached winner that
// outlived the failure keeps answering with the old address and dials nothing.
func assertReprobed(t *testing.T, p *Proxy, n Node, fake *fakeNetwork, replacement string) {
	t.Helper()
	probesBefore := fake.dials.Load()
	fake.accept(replacement)

	waitForTarget(t, p, n, net.JoinHostPort(replacement, strconv.Itoa(n.Port)))
	require.True(t, fake.dials.Load() != probesBefore, "selection probed nothing: the failed address is still cached")
}

// TestUpstreamTransportFailureReprobesTheNextSelection: a dial failure against a
// multi-homed peer must retire the confirmed address. Without that, every later
// request keeps being sent to the address that just failed, and the peer's other
// published addresses are never tried — which is the whole reason routing confirms
// reachability in the first place.
func TestUpstreamTransportFailureReprobesTheNextSelection(t *testing.T) {
	const replacement = "192.0.2.11"
	p, n, fake := confirmedDeadPeer(t, replacement)

	rec := httptest.NewRecorder()
	p.soleFacade().handleHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"model":"llama"}`)))
	require.True(t, rec.Code == http.StatusBadGateway, "status")

	assertReprobed(t, p, n, fake, replacement)
}

// TestModelListTransportFailureReprobesTheNextSelection: the aggregated model list
// reaches every candidate directly, so it learns about a dead address before any
// inference request does, and must retire it on the same evidence.
func TestModelListTransportFailureReprobesTheNextSelection(t *testing.T) {
	const replacement = "192.0.2.11"
	p, n, fake := confirmedDeadPeer(t, replacement)

	rec := httptest.NewRecorder()
	p.soleFacade().handleHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tags", nil))
	require.True(t, rec.Code == http.StatusServiceUnavailable, "status")

	assertReprobed(t, p, n, fake, replacement)
}
