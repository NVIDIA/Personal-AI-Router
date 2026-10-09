// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/noderec"
)

func TestPeerDirectorySetAndLookup(t *testing.T) {
	d := newPeerDirectory()
	d.set([]noderec.DirectoryNode{
		{
			HostUUID: "uuid-b", Name: "nodeB", IP: "192.168.1.42",
			IPs: []string{"192.168.1.42", "10.0.0.42"}, ClusterUUID: "cuuid-b",
			Services: map[noderec.ServiceKey]noderec.ServiceStatus{
				noderec.ServiceEngineControl: {Port: 14323},
			},
		},
		{ // advertises em but not ec -> excluded
			HostUUID: "uuid-c", Name: "nodeC", IP: "192.168.1.43",
			Services: map[noderec.ServiceKey]noderec.ServiceStatus{
				noderec.ServiceEngineManager: {Port: 14322},
			},
		},
		{ // ec but no dialable IP -> excluded
			HostUUID: "uuid-d", Name: "nodeD",
			Services: map[noderec.ServiceKey]noderec.ServiceStatus{
				noderec.ServiceEngineControl: {Port: 14323},
			},
		},
	})

	p, ok := d.lookup("uuid-b")
	require.True(t, ok, "unexpected nodeB entry (%v, %v)", p, ok)
	assert.Equal(t, []string{"192.168.1.42", "10.0.0.42"}, p.addresses, "unexpected nodeB addresses")
	require.Equal(t, 14323, p.port, "unexpected nodeB entry (%v, %v)", p, ok)
	require.Equal(t, "cuuid-b", p.clusterUUID, "unexpected nodeB entry (%v, %v)", p, ok)
	_, ok = d.lookup("uuid-c")
	require.False(t, ok, "nodeC advertises no ec; should not be in directory")
	_, ok = d.lookup("uuid-d")
	require.False(t, ok, "nodeD has no dialable IP; should not be in directory")

	// A later snapshot replaces the set wholesale.
	d.set(nil)
	_, ok = d.lookup("uuid-b")
	require.False(t, ok, "empty snapshot should clear the directory")
}

// TestPeerDirectoryKeysByHostUUID: an ec peer with a stable hostUuid is keyed
// (and addressed) by it, not the hostname — so a remote-control target survives
// a rename and same-named peers stay distinct. A record without a
// hostUuid falls back to the instance name.
func TestPeerDirectoryKeysByHostUUID(t *testing.T) {
	d := newPeerDirectory()
	d.set([]noderec.DirectoryNode{{
		HostUUID: "uuid-b", Name: "nodeB", IP: "192.168.1.42", ClusterUUID: "cuuid-b",
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceEngineControl: {Port: 14323},
		},
	}})
	p, ok := d.lookup("uuid-b")
	require.True(t, ok, "expected lookup by hostUuid (%v, %v)", p, ok)
	require.Equal(t, "uuid-b", p.nodeID, "expected lookup by hostUuid (%v, %v)", p, ok)
	assert.Equal(t, []string{"192.168.1.42"}, p.addresses, "expected lookup by hostUuid")
	_, ok = d.lookup("nodeB")
	require.False(t, ok, "must not be addressable by hostname when a hostUuid is present")
}
