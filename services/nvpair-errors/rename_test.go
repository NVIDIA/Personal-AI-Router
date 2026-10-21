// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/noderec"
)

// Error origin, peer eviction, and local self-checks share a stable host UUID.
// Keeping the hostname separate prevents renames from changing that identity.
func TestDirectoryToPeer_KeysByHostUUID(t *testing.T) {
	const hostUUID = "uuid-x"
	const hostName = "host-x"
	const primaryAddress = "10.0.0.4"
	const secondaryAddress = "192.168.1.4"
	n := noderec.DirectoryNode{
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
		HostUUID:             hostUUID,
		Name:                 hostName,
		IP:                   primaryAddress,
		IPs:                  []string{primaryAddress, secondaryAddress},
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceErrors: {Port: 14319},
		},
	}
	p, ok := directoryToPeer(n)
	require.True(t, ok, "node advertising er with an IP should project")
	assert.Equal(t, hostUUID, p.ID, "peer ID")
	assert.Equal(t, hostName, p.Host, "peer Host")
	assert.Equal(t, []string{primaryAddress, secondaryAddress}, p.Addresses, "peer addresses")
}

// The broker's --node-id override attributes local errors to a stable UUID.
func TestSetLocalNodeID_RekeysLocalErrors(t *testing.T) {
	m := NewManager(NewCodec(newCaptureRW()))
	m.SetLocalNodeID("uuid-self")
	assert.Equal(t, "uuid-self", m.LocalNodeID())
	// An empty override must not blank a known id.
	m.SetLocalNodeID("")
	assert.Equal(t, "uuid-self", m.LocalNodeID(), "empty override cleared the id")
}
