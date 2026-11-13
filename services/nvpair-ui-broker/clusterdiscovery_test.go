// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"nvpair-shared/noderec"
	"testing"
)

func TestClusterDiscovery_SeedsOnlyConfirmedRemoteMembers(t *testing.T) {
	b := &Broker{nodeID: "self"}
	b.updateClusterPeers(json.RawMessage(`{"nodes":[{"nodeUuid":"self","state":"member","ipAddress":"127.0.0.1"},{"nodeUuid":"peer","name":"remote","state":"member","ipAddress":"192.0.2.20"},{"nodeUuid":"pending","state":"pending-outbound","ipAddress":"192.0.2.21"}]}`))
	require.Equal(t, []noderec.ClusterPeer{{HostUUID: "peer", Name: "remote", Address: "192.0.2.20", NodeInfoPort: 14318}}, b.clusterPeers)
	b.updateClusterPeers(json.RawMessage(`{"nodes":[]}`))
	assert.Empty(t, b.clusterPeers)
}
func TestClusterDiscovery_DoesNotReplaceANewerRosterWithAStartupSnapshot(t *testing.T) {
	b := &Broker{nodeID: "self"}
	generation := b.clusterPeersGeneration
	b.updateClusterPeers(json.RawMessage(`{"nodes":[{"nodeUuid":"peer","state":"member","ipAddress":"192.0.2.20"}]}`))
	b.updateClusterPeersAt(json.RawMessage(`{"nodes":[]}`), &generation)
	require.Len(t, b.clusterPeers, 1)
	assert.Equal(t, "peer", b.clusterPeers[0].HostUUID)
}
