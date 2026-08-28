// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/grandcat/zeroconf"
)

const testDomain = "local."

func TestInProcessDiscovery(t *testing.T) {
	const (
		service  = "_test-disc._tcp"
		instance = "in-proc-test"
		port     = 55555
	)

	server, err := zeroconf.Register(instance, service, testDomain, port, []string{"env=test"}, nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer server.Shutdown()
	time.Sleep(500 * time.Millisecond)

	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}

	entries := make(chan *zeroconf.ServiceEntry)
	found := make(chan *zeroconf.ServiceEntry, 1)
	go func() {
		for e := range entries {
			if e.Instance == instance {
				select {
				case found <- e:
				default:
				}
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := resolver.Browse(ctx, service, testDomain, entries); err != nil {
		t.Fatalf("browse: %v", err)
	}

	select {
	case entry := <-found:
		if entry.Port != port {
			t.Errorf("port = %d, want %d", entry.Port, port)
		}
		if len(entry.Text) == 0 || entry.Text[0] != "env=test" {
			t.Errorf("txt = %v, want [env=test]", entry.Text)
		}
		if !hasAddress(entry) {
			t.Error("no addresses resolved")
		}
		t.Logf("OK: %s @ %s:%d addrs=%v txt=%v",
			entry.Instance, entry.HostName, entry.Port, entry.AddrIPv4, entry.Text)
	case <-ctx.Done():
		t.Fatal("timed out waiting for discovery")
	}
}

func TestInProcessMultipleInstances(t *testing.T) {
	const service = "_test-multi._tcp"
	instances := []string{"node-alpha", "node-beta", "node-gamma"}

	var servers []*zeroconf.Server
	for i, inst := range instances {
		s, err := zeroconf.Register(inst, service, testDomain, 50000+i, nil, nil)
		if err != nil {
			t.Fatalf("register %s: %v", inst, err)
		}
		servers = append(servers, s)
		defer s.Shutdown()
	}
	time.Sleep(500 * time.Millisecond)

	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}

	entries := make(chan *zeroconf.ServiceEntry)
	foundSet := make(map[string]bool)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for e := range entries {
			for _, inst := range instances {
				if e.Instance == inst {
					foundSet[inst] = true
				}
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := resolver.Browse(ctx, service, testDomain, entries); err != nil {
		t.Fatalf("browse: %v", err)
	}
	<-ctx.Done()
	<-done

	for _, inst := range instances {
		if !foundSet[inst] {
			t.Errorf("instance %q not discovered", inst)
		}
	}
	t.Logf("OK: discovered %d/%d instances", len(foundSet), len(instances))
}

func TestInProcessRemoval(t *testing.T) {
	const (
		service  = "_test-rmv._tcp"
		instance = "removable-node"
		port     = 55556
	)

	server, err := zeroconf.Register(instance, service, testDomain, port, nil, nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// Phase 1: verify discoverable
	entry := browseForInstance(t, service, instance, 5*time.Second)
	if entry == nil {
		t.Fatal("service not found before removal")
	}
	t.Log("phase 1: service discovered")

	// Shut down the service
	server.Shutdown()
	time.Sleep(time.Second)

	// Phase 2: verify no longer discoverable
	entry = browseForInstance(t, service, instance, 5*time.Second)
	if entry != nil {
		t.Error("service still discoverable after shutdown")
	} else {
		t.Log("phase 2: service correctly absent after removal")
	}
}

// --- helpers ---

func browseForInstance(t *testing.T, service, instance string, timeout time.Duration) *zeroconf.ServiceEntry {
	t.Helper()
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}

	entries := make(chan *zeroconf.ServiceEntry)
	found := make(chan *zeroconf.ServiceEntry, 1)
	go func() {
		for e := range entries {
			if e.Instance == instance {
				select {
				case found <- e:
				default:
				}
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := resolver.Browse(ctx, service, testDomain, entries); err != nil {
		t.Fatalf("browse: %v", err)
	}

	select {
	case e := <-found:
		return e
	case <-ctx.Done():
		return nil
	}
}

func hasAddress(e *zeroconf.ServiceEntry) bool {
	for _, ip := range e.AddrIPv4 {
		if ip != nil && !ip.IsUnspecified() {
			return true
		}
	}
	for _, ip := range e.AddrIPv6 {
		if ip != nil && !net.IP(ip).IsLinkLocalUnicast() {
			return true
		}
	}
	return false
}
