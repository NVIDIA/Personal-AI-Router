// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"

	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/noderec"
)

// nopReadWriter is a codec transport that reads EOF and discards writes, for
// tests that drive the manager's helpers directly.
type nopReadWriter struct{}

func (nopReadWriter) Read([]byte) (int, error)    { return 0, io.EOF }
func (nopReadWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestDirectoryToPeerKeysByHostUUID: a peer is keyed by its stable hostUuid (the
// same identity it stamps as the origin of its errors and uses as its own
// localNodeID), so the self-check and EvictNode survive a peer's PC rename and
// never conflate two same-named peers. Host stays the hostname for dialing /
// display.
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

// TestSetLocalNodeID: the broker's --node-id override replaces the hostname
// default so local errors are attributed to this node's stable UUID.
func TestSetLocalNodeID_RekeysLocalErrors(t *testing.T) {
	m := NewManager(NewCodec(nopReadWriter{}))
	m.SetLocalNodeID("uuid-self")
	assert.Equal(t, "uuid-self", m.LocalNodeID())
	// An empty override must not blank a known id.
	m.SetLocalNodeID("")
	assert.Equal(t, "uuid-self", m.LocalNodeID(), "empty override cleared the id")
}
