// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"context"
	"net"
	"testing"
	"time"

	"nvpair-shared/discovery"
	"nvpair-shared/mdns"
)

const testDomain = "local."

// These tests exercise the first-party mDNS stack end to end inside the test
// process: a shared/mdns.Responder advertises on 5353 and a shared/discovery.
// Browser listens on the same local multicast group, so the two deliver to each
// other over loopback without a second host.

// startInProcessResponder advertises a single service instance in-process and
// starts the responder's run loop. It registers a cleanup that cancels the run
// (a TTL=0 goodbye plus stopping the periodic announcements) and returns the
// cancel so a test can withdraw the instance mid-flight.
func startInProcessResponder(t *testing.T, instance, service, domain string, port int, txt []string) context.CancelFunc {
	t.Helper()
	resp, err := mdns.NewResponder(instance, service, domain, port, txt)
	if err != nil {
		t.Fatalf("mdns responder %s: %v", instance, err)
	}
	respCtx, cancelResp := context.WithCancel(context.Background())
	t.Cleanup(cancelResp)
	go resp.Run(respCtx)
	return cancelResp
}

// startInProcessBrowser runs a discovery.Browser in-process (a shared receive
// socket on 5353 plus periodic query scans) and returns its event stream. The
// browser is bound to the same local multicast group the responder uses.
func startInProcessBrowser(t *testing.T, service, domain string, opts ...discovery.Option) <-chan discovery.Event {
	t.Helper()
	browser := discovery.New(service, domain, opts...)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	events := make(chan discovery.Event, 64)
	go browser.Run(ctx, events)
	return events
}

// waitNodeEvent blocks until an event of the given type for the given instance
// arrives on events, or until timeout elapses (which fails the test).
func waitNodeEvent(t *testing.T, events <-chan discovery.Event, wantType, instance string, timeout time.Duration) discovery.Node {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("browser event stream closed before a %q for %q", wantType, instance)
			}
			if ev.Type == wantType && ev.Node.ID == instance {
				return ev.Node
			}
		case <-deadline:
			t.Fatalf("timed out (%s) waiting for %q %q", timeout, wantType, instance)
		}
	}
}

func TestInProcessDiscovery(t *testing.T) {
	const (
		service  = "_test-disc._tcp"
		instance = "in-proc-test"
		port     = 55555
	)

	startInProcessResponder(t, instance, service, testDomain, port, []string{"env=test"})
	events := startInProcessBrowser(t, service, testDomain,
		discovery.WithInterval(200*time.Millisecond),
		discovery.WithScanTimeout(100*time.Millisecond))

	got := waitNodeEvent(t, events, discovery.Discovered, instance, 10*time.Second)

	if got.Port != port {
		t.Errorf("port = %d, want %d", got.Port, port)
	}
	if len(got.TXT) == 0 || got.TXT[0] != "env=test" {
		t.Errorf("txt = %v, want [env=test]", got.TXT)
	}
	if !hasAddress(got) {
		t.Error("no addresses resolved")
	}
	t.Logf("OK: %s @ %s:%d addrs=%v txt=%v", got.ID, got.Host, got.Port, got.Addresses, got.TXT)
}

func TestInProcessMultipleInstances(t *testing.T) {
	const service = "_test-multi._tcp"
	instances := []string{"node-alpha", "node-beta", "node-gamma"}

	for i, inst := range instances {
		startInProcessResponder(t, inst, service, testDomain, 50000+i, nil)
	}
	events := startInProcessBrowser(t, service, testDomain,
		discovery.WithInterval(200*time.Millisecond),
		discovery.WithScanTimeout(100*time.Millisecond))

	found := make(map[string]bool)
	deadline := time.After(10 * time.Second)
	for len(found) < len(instances) {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("browser event stream closed; found %d/%d instances", len(found), len(instances))
			}
			for _, inst := range instances {
				if ev.Type == discovery.Discovered && ev.Node.ID == inst {
					found[inst] = true
				}
			}
		case <-deadline:
			t.Fatalf("timed out; discovered %d/%d instances", len(found), len(instances))
		}
	}
	for _, inst := range instances {
		if !found[inst] {
			t.Errorf("instance %q not discovered", inst)
		}
	}
	t.Logf("OK: discovered %d/%d instances", len(found), len(instances))
}

func TestInProcessRemoval(t *testing.T) {
	const (
		service  = "_test-rmv._tcp"
		instance = "removable-node"
		port     = 55556
	)

	cancelResp := startInProcessResponder(t, instance, service, testDomain, port, nil)
	events := startInProcessBrowser(t, service, testDomain,
		discovery.WithInterval(200*time.Millisecond),
		discovery.WithScanTimeout(100*time.Millisecond),
		discovery.WithMissThreshold(2))

	// Phase 1: the instance must be discoverable while it is advertised.
	waitNodeEvent(t, events, discovery.Discovered, instance, 10*time.Second)
	t.Log("phase 1: service discovered")

	// Phase 2: withdraw it. The responder sends a TTL=0 goodbye (dropped by the
	// receiver) and stops announcing and answering queries, so the node then ages
	// out via the miss threshold and the browser emits a removal.
	cancelResp()
	waitNodeEvent(t, events, discovery.Removed, instance, 15*time.Second)
	t.Log("phase 2: service correctly evicted after withdrawal")
}

// hasAddress reports whether the node resolved at least one usable address: an
// IPv4 that is not 0.0.0.0, or an IPv6 that is not a link-local address.
func hasAddress(n discovery.Node) bool {
	for _, s := range n.Addresses {
		ip := net.ParseIP(s)
		if ip == nil {
			continue
		}
		if ip4 := ip.To4(); ip4 != nil && !ip4.IsUnspecified() {
			return true
		}
		if !ip.IsLinkLocalUnicast() {
			return true
		}
	}
	return false
}
