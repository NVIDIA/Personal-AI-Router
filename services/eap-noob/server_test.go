// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eapnoob

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serverNegotiationSelection returns a fresh server and a valid peer selection.
func serverNegotiationSelection(t *testing.T) (*Server, *wireMessage) {
	t.Helper()
	server := NewServer(ServerConfig{Dirs: OOBServerToPeer}, nil)
	peer := NewPeer(PeerConfig{}, nil)
	selection, err := peer.Receive(negotiationRequest(t, server, peer))
	require.NoError(t, err)
	require.Nil(t, selection.Err)
	wire, err := decode(selection.Send)
	require.NoError(t, err)
	return server, wire
}

func TestServer_AdvertisesDefaultParameters(t *testing.T) {
	info := PairingInfo{
		V:                   2,
		NodeUUID:            "server-uuid",
		NodeID:              "server-node",
		Name:                "server",
		ClusterID:           "test-cluster",
		AdmissionEpoch:      9007199254740993,
		ClusterFriendlyName: "Test cluster",
		Addr:                "server.example:14321",
		Cert:                "server-cert",
	}
	server := NewServer(ServerConfig{ServerInfo: info}, nil)
	peer := NewPeer(PeerConfig{}, nil)
	offer, err := decode(negotiationRequest(t, server, peer))
	require.NoError(t, err)
	assert.Equal(t, json.RawMessage(`[1]`), offer.Vers)
	assert.Equal(t, json.RawMessage(`[2,1]`), offer.Cryptosuites)
	assert.Equal(t, json.RawMessage(`3`), offer.Dirs)
	var receivedInfo PairingInfo
	require.NoError(t, json.Unmarshal(offer.ServerInfo, &receivedInfo))
	assert.Equal(t, info, receivedInfo)
	assert.Empty(t, offer.NewNAI)
}

func TestServerNegotiation_RejectsUnsupportedOrMismatchedSelections(t *testing.T) {
	test := func(name string, change func(*wireMessage), wantCode int) {
		t.Run(name, func(t *testing.T) {
			server, wire := serverNegotiationSelection(t)
			change(wire)
			changed, err := json.Marshal(wire)
			require.NoError(t, err)

			out, err := server.Receive(changed)

			require.NoError(t, err)
			require.NotNil(t, out.Err)
			assert.Equal(t, wantCode, out.Err.Code)
			assert.True(t, out.Done)
			assert.False(t, out.Success)
			assert.NotEqual(t, StateRegistered, server.State())
		})
	}
	test("unsupported version", func(wire *wireMessage) {
		wire.Verp = json.RawMessage(`99`)
	}, ErrUnsupportedVersion)
	test("unsupported cryptosuite", func(wire *wireMessage) {
		wire.Cryptosuitep = json.RawMessage(`99`)
	}, ErrUnsupportedCryptosuite)
	test("inverse OOB direction", func(wire *wireMessage) {
		wire.Dirp = json.RawMessage(`1`)
	}, ErrNoMutuallySupportedOOB)
}
