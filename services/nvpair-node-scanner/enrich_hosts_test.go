// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/noderec"
)

// answering records which addresses were asked, so a test can assert on the fetches
// a sweep did and did not make.
type answering struct {
	mu     sync.Mutex
	accept map[string]bool
	asked  []string
	slow   map[string]time.Duration
}

func newAnswering(accepting ...string) *answering {
	a := &answering{accept: make(map[string]bool), slow: make(map[string]time.Duration)}
	for _, host := range accepting {
		a.accept[host] = true
	}
	return a
}

func (a *answering) ask(host string) (string, bool) {
	a.mu.Lock()
	a.asked = append(a.asked, host)
	ok := a.accept[host]
	delay := a.slow[host]
	a.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if !ok {
		return "", false
	}
	return "inventory from " + host, true
}

func (a *answering) askedAddresses() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.asked...)
}

func testKey() hostKey {
	return hostKey{hostUUID: "uuid-peer", service: noderec.ServiceNodeInfo}
}

// TestAskRememberedAsksOnlyTheAddressThatAnswered is why the memory exists: the
// sweep repeats every peerRefreshInterval for as long as a node is advertised, so
// starting at the top of its list each time paid the leading address's full fetch
// timeout forever to reach the one that had already worked.
func TestAskRemembered_AsksOnlyTheAddressThatAnswered(t *testing.T) {
	net := newAnswering("10.0.0.5")
	mem := &hostMemory{}
	hosts := []string{"192.168.240.1", "169.254.7.7", "10.0.0.5"}

	host, value, ok := askRemembered(mem, testKey(), hosts, net.ask)
	require.True(t, ok, "askRemembered (%v, %v, %v)", host, value, ok)
	require.Equal(t, "10.0.0.5", host, "askRemembered (%v, %v, %v)", host, value, ok)
	require.Equal(t, "inventory from 10.0.0.5", value, "askRemembered (%v, %v, %v)", host, value, ok)

	for range 5 {
		host, _, ok := askRemembered(mem, testKey(), hosts, net.ask)
		require.True(t, ok, "askRemembered")
		require.Equal(t, "10.0.0.5", host, "askRemembered")
	}
	asked := net.askedAddresses()
	require.Len(t, asked, len(hosts)+5, "asked")
	for _, host := range asked[len(hosts):] {
		assert.Equal(t, "10.0.0.5", host, "a later sweep asked")
	}
}

func TestAskRemembered_SkipsWithdrawnAddresses(t *testing.T) {
	var memory hostMemory
	key := hostKey{hostUUID: "peer", service: noderec.ServiceNodeInfo}
	memory.set(key, "old-address")
	host, _, ok := askRemembered(&memory, key, []string{"new-address"}, func(host string) (string, bool) { return host, true })
	require.True(t, ok)
	assert.Equal(t, "new-address", host)
}

// TestAskRememberedKeepsAWorkingAddressWhenABetterRankedOneComesUp: nothing is
// wrong with the address in use, and switching changes the address this node is
// enriched and reported at to learn nothing.
func TestAskRemembered_KeepsAWorkingAddressWhenABetterRankedOneComesUp(t *testing.T) {
	net := newAnswering("10.0.0.5")
	mem := &hostMemory{}
	hosts := []string{"192.168.240.1", "10.0.0.5"}
	host, _, ok := askRemembered(mem, testKey(), hosts, net.ask)
	require.True(t, ok, "askRemembered")
	require.Equal(t, "10.0.0.5", host, "askRemembered")

	net.mu.Lock()
	net.accept["192.168.240.1"] = true
	net.mu.Unlock()

	host, _, ok = askRemembered(mem, testKey(), hosts, net.ask)
	require.True(t, ok, "askRemembered")
	require.Equal(t, "10.0.0.5", host, "askRemembered")
}

// TestAskRememberedWalksAgainWhenTheRememberedAddressStops: its failure is the one
// reason to look elsewhere, and the new answer is what gets remembered.
func TestAskRemembered_WalksAgainWhenTheRememberedAddressStops(t *testing.T) {
	net := newAnswering("10.0.0.5")
	mem := &hostMemory{}
	hosts := []string{"192.168.240.1", "10.0.0.5", "10.0.0.6"}
	askRemembered(mem, testKey(), hosts, net.ask)

	net.mu.Lock()
	net.accept["10.0.0.5"] = false
	net.accept["10.0.0.6"] = true
	net.mu.Unlock()

	host, _, ok := askRemembered(mem, testKey(), hosts, net.ask)
	require.True(t, ok, "askRemembered")
	require.Equal(t, "10.0.0.6", host, "askRemembered")
	net.asked = nil
	host, _, ok = askRemembered(mem, testKey(), hosts, net.ask)
	require.True(t, ok, "askRemembered")
	require.Equal(t, "10.0.0.6", host, "askRemembered")
	assert.Len(t, net.askedAddresses(), 1, "asked")
}

// TestAskRememberedForgetsAnAddressNobodyAnswersAt: with nothing answering there is
// no address to remember, so the next sweep is free to find a recovery anywhere on
// the list rather than being pinned to one that failed.
func TestAskRemembered_ForgetsAnAddressNobodyAnswersAt(t *testing.T) {
	net := newAnswering("10.0.0.5")
	mem := &hostMemory{}
	hosts := []string{"10.0.0.5", "10.0.0.6"}
	askRemembered(mem, testKey(), hosts, net.ask)

	net.mu.Lock()
	net.accept["10.0.0.5"] = false
	net.mu.Unlock()
	_, _, ok := askRemembered(mem, testKey(), hosts, net.ask)
	require.False(t, ok, "askRemembered reported success with nothing answering")
	assert.Equal(t, "", mem.get(testKey()), "still remembers")
}

// TestAskTogetherPrefersRankOverArrivalOrder: the node's ranking decides, not the
// stopwatch. Taking the fastest responder would let two working addresses swap
// places between sweeps on nothing but timing noise.
func TestAskTogether_PrefersRankOverArrivalOrder(t *testing.T) {
	net := newAnswering("10.0.0.5", "10.0.0.6")
	net.slow["10.0.0.5"] = 40 * time.Millisecond

	host, _, ok := askTogether([]string{"10.0.0.5", "10.0.0.6"}, net.ask)
	require.True(t, ok, "askTogether (%v, %v)", host, ok)
	require.Equal(t, "10.0.0.5", host, "askTogether (%v, %v)", host, ok)
}

// Every address must be asked before any response is released. Sequential
// probing would wait forever on the first address instead of reaching the rest.
func TestAskTogether_ProbesAddressesConcurrently(t *testing.T) {
	hosts := []string{"192.168.240.1", "169.254.7.7", "172.17.0.1", "10.0.0.9"}
	entered := make(chan string, len(hosts))
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	type probeResult struct {
		host string
		ok   bool
	}
	finished := make(chan probeResult, 1)
	go func() {
		host, _, ok := askTogether(hosts, func(host string) (string, bool) {
			entered <- host
			<-release
			return "inventory", host == "10.0.0.9"
		})
		finished <- probeResult{host: host, ok: ok}
	}()

	var asked []string
	for range hosts {
		select {
		case host := <-entered:
			asked = append(asked, host)
		case <-time.After(2 * time.Second):
			require.FailNow(t, "addresses were not probed concurrently")
		}
	}
	assert.ElementsMatch(t, hosts, asked)
	releaseOnce.Do(func() { close(release) })

	select {
	case result := <-finished:
		assert.True(t, result.ok)
		assert.Equal(t, "10.0.0.9", result.host)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "address probing did not finish after responses were released")
	}
}

// TestAskTogetherSkipsBlankHosts guards the published-list edge: a record can carry
// an empty entry, and it is not an address to fetch from.
func TestAskTogether_SkipsBlankHosts(t *testing.T) {
	net := newAnswering("10.0.0.5")
	host, _, ok := askTogether([]string{"", "10.0.0.5"}, net.ask)
	require.True(t, ok, "askTogether (%v, %v)", host, ok)
	require.Equal(t, "10.0.0.5", host, "askTogether (%v, %v)", host, ok)
	assert.Len(t, net.askedAddresses(), 1, "asked")
}
