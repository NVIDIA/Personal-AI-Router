// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"nvpair-shared/noderec"
)

// Give the broker a verified peer, an unverified record claiming that peer's
// UUID, and an explicitly configured manual target. Read the scheduler's actual
// notification and check that it contains only the peer and manual target.
// A manual configuration authorizes routing without authorizing hardware data.
func TestFanDiscoveryToScheduler_ExcludesUnverifiedDiscoveries(t *testing.T) {
	const peerUUID = "peer"
	const verifiedName = "verified"
	const manualID = "configured"
	brokerSide, schedulerSide := net.Pipe()
	t.Cleanup(func() {
		assert.NoError(t, brokerSide.Close())
		assert.NoError(t, schedulerSide.Close())
	})
	broker := newManualTestBroker()
	broker.setScheduler(&rpcWorker{peer: NewPeer(NewCodec(brokerSide))})
	broker.store.Upsert(EnrichedNode{
		HostUUID:             peerUUID,
		ID:                   verifiedName,
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
	}, sourceScanner)
	broker.store.Upsert(EnrichedNode{
		HostUUID:             peerUUID,
		ID:                   "rogue",
		DiscoveryRecordID:    "candidate:rogue",
		IdentityVerification: noderec.IdentityUnverified,
	}, sourceScanner)
	broker.store.Upsert(manualToEnriched(manualNodeStatus{
		ID:      manualID,
		Address: "192.0.2.30",
	}), sourceManual)
	// net.Pipe writes wait for a reader, so fanout must run alongside the read
	// below. The completion channel lets us join the writer before pipe cleanup.
	done := make(chan struct{})
	go func() {
		broker.fanDiscoveryToScheduler()
		close(done)
	}()
	require.NoError(t, schedulerSide.SetReadDeadline(time.Now().Add(2*time.Second)))

	message, err := NewCodec(schedulerSide).Read()
	require.NoError(t, err)
	var nodes []AvailableNode
	require.NoError(t, json.Unmarshal(message.Params, &nodes))
	require.Len(t, nodes, 2)
	assert.Equal(t, manualID, nodes[0].ID)
	assert.Equal(t, "manual:configured", nodes[0].HostUUID)
	assert.Equal(t, verifiedName, nodes[1].ID)
	<-done
}
