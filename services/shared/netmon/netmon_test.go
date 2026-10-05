// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package netmon

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	assert.Equal(t, fingerprint(b), fingerprint(a), "fingerprints differ for equal-but-reordered snapshots")
}

func TestFingerprintDetectsChange(t *testing.T) {
	a := Snapshot{LocalIPs: map[string]bool{"10.0.0.1": true}, IfaceV4: map[int][]net.IP{}}
	b := Snapshot{LocalIPs: map[string]bool{"10.0.0.2": true}, IfaceV4: map[int][]net.IP{}}
	assert.NotEqual(t, fingerprint(b), fingerprint(a), "fingerprint should differ when an IP changes")
}

func TestSnapshotCloneIsIndependent(t *testing.T) {
	orig := Snapshot{
		LocalIPs: map[string]bool{"10.0.0.1": true},
		IfaceV4:  map[int][]net.IP{1: {net.IPv4(10, 0, 0, 1)}},
	}
	cp := orig.clone()
	cp.LocalIPs["10.0.0.2"] = true
	cp.IfaceV4[1][0] = net.IPv4(8, 8, 8, 8)
	assert.False(t, orig.LocalIPs["10.0.0.2"], "clone shares LocalIPs map with original")
	assert.False(t, orig.IfaceV4[1][0].Equal(net.IPv4(8, 8, 8, 8)), "clone shares IfaceV4 backing array with original")
}

func TestWatchProvidesInitialSnapshot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mon, err := Watch(ctx)
	require.NoError(t, err, "Watch")
	// Snapshot should match a direct enumeration taken at roughly the same
	// time (interfaces don't change during the test).
	{
		got, want := fingerprint(mon.Snapshot()), fingerprint(Enumerate())
		assert.Equal(t, want, got, "monitor snapshot")
	}
	// Subscribe must hand back a usable channel that closes on cancel.
	ch := mon.Subscribe()
	cancel()
	select {
	case <-ch:
		// closed (or signalled) — both acceptable
	case <-time.After(2 * time.Second):
		assert.Fail(t, "test expectation failed", "subscription channel not closed after context cancel")
	}
}
