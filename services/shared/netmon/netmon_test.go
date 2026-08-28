// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package netmon

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestFingerprintOrderInsensitive(t *testing.T) {
	a := Snapshot{
		LocalIPs: map[string]bool{"10.0.0.1": true, "192.168.1.2": true},
		IfaceV4:  map[int][]net.IP{3: {net.IPv4(10, 0, 0, 1), net.IPv4(10, 0, 0, 2)}},
	}
	b := Snapshot{
		LocalIPs: map[string]bool{"192.168.1.2": true, "10.0.0.1": true},
		IfaceV4:  map[int][]net.IP{3: {net.IPv4(10, 0, 0, 2), net.IPv4(10, 0, 0, 1)}},
	}
	if fingerprint(a) != fingerprint(b) {
		t.Fatalf("fingerprints differ for equal-but-reordered snapshots:\n%q\n%q", fingerprint(a), fingerprint(b))
	}
}

func TestFingerprintDetectsChange(t *testing.T) {
	a := Snapshot{LocalIPs: map[string]bool{"10.0.0.1": true}, IfaceV4: map[int][]net.IP{}}
	b := Snapshot{LocalIPs: map[string]bool{"10.0.0.2": true}, IfaceV4: map[int][]net.IP{}}
	if fingerprint(a) == fingerprint(b) {
		t.Fatal("fingerprint should differ when an IP changes")
	}
}

func TestSnapshotCloneIsIndependent(t *testing.T) {
	orig := Snapshot{
		LocalIPs: map[string]bool{"10.0.0.1": true},
		IfaceV4:  map[int][]net.IP{1: {net.IPv4(10, 0, 0, 1)}},
	}
	cp := orig.clone()
	cp.LocalIPs["10.0.0.2"] = true
	cp.IfaceV4[1][0] = net.IPv4(8, 8, 8, 8)
	if orig.LocalIPs["10.0.0.2"] {
		t.Error("clone shares LocalIPs map with original")
	}
	if orig.IfaceV4[1][0].Equal(net.IPv4(8, 8, 8, 8)) {
		t.Error("clone shares IfaceV4 backing array with original")
	}
}

func TestWatchProvidesInitialSnapshot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mon, err := Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	// Snapshot should match a direct enumeration taken at roughly the same
	// time (interfaces don't change during the test).
	if got, want := fingerprint(mon.Snapshot()), fingerprint(Enumerate()); got != want {
		t.Errorf("monitor snapshot %q != enumerate %q", got, want)
	}
	// Subscribe must hand back a usable channel that closes on cancel.
	ch := mon.Subscribe()
	cancel()
	select {
	case <-ch:
		// closed (or signalled) — both acceptable
	case <-time.After(2 * time.Second):
		t.Error("subscription channel not closed after context cancel")
	}
}
