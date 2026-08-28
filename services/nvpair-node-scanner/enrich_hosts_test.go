// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"sync"
	"testing"
	"time"

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
func TestAskRememberedAsksOnlyTheAddressThatAnswered(t *testing.T) {
	net := newAnswering("10.0.0.5")
	mem := &hostMemory{}
	hosts := []string{"192.168.240.1", "169.254.7.7", "10.0.0.5"}

	host, value, ok := askRemembered(mem, testKey(), hosts, net.ask)
	if !ok || host != "10.0.0.5" || value != "inventory from 10.0.0.5" {
		t.Fatalf("askRemembered = %q,%q,%v, want the address that answered", host, value, ok)
	}

	for range 5 {
		if host, _, ok := askRemembered(mem, testKey(), hosts, net.ask); !ok || host != "10.0.0.5" {
			t.Fatalf("askRemembered = %q,%v, want the remembered address", host, ok)
		}
	}
	asked := net.askedAddresses()
	if len(asked) != len(hosts)+5 {
		t.Fatalf("asked %v, want one walk then one address per sweep", asked)
	}
	for _, host := range asked[len(hosts):] {
		if host != "10.0.0.5" {
			t.Errorf("a later sweep asked %q, want only the remembered address", host)
		}
	}
}

// TestAskRememberedKeepsAWorkingAddressWhenABetterRankedOneComesUp: nothing is
// wrong with the address in use, and switching changes the address this node is
// enriched and reported at to learn nothing.
func TestAskRememberedKeepsAWorkingAddressWhenABetterRankedOneComesUp(t *testing.T) {
	net := newAnswering("10.0.0.5")
	mem := &hostMemory{}
	hosts := []string{"192.168.240.1", "10.0.0.5"}
	if host, _, ok := askRemembered(mem, testKey(), hosts, net.ask); !ok || host != "10.0.0.5" {
		t.Fatalf("askRemembered = %q,%v, want the address that answered", host, ok)
	}

	net.mu.Lock()
	net.accept["192.168.240.1"] = true
	net.mu.Unlock()

	if host, _, ok := askRemembered(mem, testKey(), hosts, net.ask); !ok || host != "10.0.0.5" {
		t.Fatalf("askRemembered = %q,%v, want the address already known to work", host, ok)
	}
}

// TestAskRememberedWalksAgainWhenTheRememberedAddressStops: its failure is the one
// reason to look elsewhere, and the new answer is what gets remembered.
func TestAskRememberedWalksAgainWhenTheRememberedAddressStops(t *testing.T) {
	net := newAnswering("10.0.0.5")
	mem := &hostMemory{}
	hosts := []string{"192.168.240.1", "10.0.0.5", "10.0.0.6"}
	askRemembered(mem, testKey(), hosts, net.ask)

	net.mu.Lock()
	net.accept["10.0.0.5"] = false
	net.accept["10.0.0.6"] = true
	net.mu.Unlock()

	if host, _, ok := askRemembered(mem, testKey(), hosts, net.ask); !ok || host != "10.0.0.6" {
		t.Fatalf("askRemembered = %q,%v, want the address that took over", host, ok)
	}
	net.asked = nil
	if host, _, ok := askRemembered(mem, testKey(), hosts, net.ask); !ok || host != "10.0.0.6" {
		t.Fatalf("askRemembered = %q,%v, want the new address remembered", host, ok)
	}
	if asked := net.askedAddresses(); len(asked) != 1 {
		t.Errorf("asked %v after settling on a new address, want one", asked)
	}
}

// TestAskRememberedForgetsAnAddressNobodyAnswersAt: with nothing answering there is
// no address to remember, so the next sweep is free to find a recovery anywhere on
// the list rather than being pinned to one that failed.
func TestAskRememberedForgetsAnAddressNobodyAnswersAt(t *testing.T) {
	net := newAnswering("10.0.0.5")
	mem := &hostMemory{}
	hosts := []string{"10.0.0.5", "10.0.0.6"}
	askRemembered(mem, testKey(), hosts, net.ask)

	net.mu.Lock()
	net.accept["10.0.0.5"] = false
	net.mu.Unlock()
	if _, _, ok := askRemembered(mem, testKey(), hosts, net.ask); ok {
		t.Fatal("askRemembered reported success with nothing answering")
	}
	if got := mem.get(testKey()); got != "" {
		t.Errorf("still remembers %q after it stopped answering", got)
	}
}

// TestAskTogetherPrefersRankOverArrivalOrder: the node's ranking decides, not the
// stopwatch. Taking the fastest responder would let two working addresses swap
// places between sweeps on nothing but timing noise.
func TestAskTogetherPrefersRankOverArrivalOrder(t *testing.T) {
	net := newAnswering("10.0.0.5", "10.0.0.6")
	net.slow["10.0.0.5"] = 40 * time.Millisecond

	host, _, ok := askTogether([]string{"10.0.0.5", "10.0.0.6"}, net.ask)
	if !ok || host != "10.0.0.5" {
		t.Fatalf("askTogether = %q,%v, want the top-ranked address despite answering later", host, ok)
	}
}

// TestAskTogetherPaysOneTimeoutForTheWholeList: an address that neither answers nor
// refuses costs a whole fetch timeout, and asking in sequence paid that per address
// — up to modelsFetchTimeout each — before reaching one that works.
func TestAskTogetherPaysOneTimeoutForTheWholeList(t *testing.T) {
	const stall = 150 * time.Millisecond
	net := newAnswering("10.0.0.9")
	for _, host := range []string{"192.168.240.1", "169.254.7.7", "172.17.0.1"} {
		net.slow[host] = stall
	}
	hosts := []string{"192.168.240.1", "169.254.7.7", "172.17.0.1", "10.0.0.9"}

	start := time.Now()
	host, _, ok := askTogether(hosts, net.ask)
	elapsed := time.Since(start)

	if !ok || host != "10.0.0.9" {
		t.Fatalf("askTogether = %q,%v, want the address that answered", host, ok)
	}
	if elapsed > 2*stall {
		t.Errorf("asking four addresses took %v with a %v stall each; they are not being asked together",
			elapsed, stall)
	}
}

// TestAskTogetherSkipsBlankHosts guards the published-list edge: a record can carry
// an empty entry, and it is not an address to fetch from.
func TestAskTogetherSkipsBlankHosts(t *testing.T) {
	net := newAnswering("10.0.0.5")
	host, _, ok := askTogether([]string{"", "10.0.0.5"}, net.ask)
	if !ok || host != "10.0.0.5" {
		t.Fatalf("askTogether = %q,%v, want the only real address", host, ok)
	}
	if asked := net.askedAddresses(); len(asked) != 1 {
		t.Errorf("asked %v, want the blank entry skipped", asked)
	}
}
