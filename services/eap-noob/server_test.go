// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eapnoob

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServer_AdvertisesPAIRDefaults(t *testing.T) {
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
	server := NewServer(info)
	peer := NewPeer(PairingInfo{})
	request, err := server.Start()
	require.NoError(t, err)
	discovery, err := peer.Receive(request)
	require.NoError(t, err)
	require.Nil(t, discovery.Err)
	negotiation, err := server.Receive(discovery.Send)
	require.NoError(t, err)
	require.Nil(t, negotiation.Err)
	offer, err := decode(negotiation.Send)
	require.NoError(t, err)
	assert.Equal(t, json.RawMessage(`[1]`), offer.Vers)
	assert.Equal(t, json.RawMessage(`[2,1]`), offer.Cryptosuites)
	assert.Equal(t, json.RawMessage(`2`), offer.Dirs)
	var receivedInfo PairingInfo
	require.NoError(t, json.Unmarshal(offer.ServerInfo, &receivedInfo))
	assert.Equal(t, info, receivedInfo)
	assert.Empty(t, offer.NewNAI)
	assert.Nil(t, offer.SleepTime)
}

func TestServerDiscovery_BindsIncomingNAIToAuthenticatedTranscript(t *testing.T) {
	server := NewServer(PairingInfo{})
	negotiation, err := server.Receive([]byte(`{"Type":1,"PeerState":0,"NAI":"remote@pair.test"}`))
	require.NoError(t, err)
	require.Nil(t, negotiation.Err)
	offer, err := decode(negotiation.Send)
	require.NoError(t, err)
	selection, _, err := encode(&wireMessage{
		Type:         intp(typeNegotiation),
		PeerId:       offer.PeerId,
		Verp:         json.RawMessage(`1`),
		Cryptosuitep: json.RawMessage(`2`),
		Dirp:         json.RawMessage(`2`),
		PeerInfo:     json.RawMessage(`{}`),
	})
	require.NoError(t, err)

	out, err := server.Receive(selection)

	require.NoError(t, err)
	require.Nil(t, out.Err)
	assert.Equal(t, json.RawMessage(`"remote@pair.test"`), server.in.NAI)
}

func TestServerNegotiation_AcceptsSelectionsIncludingServerToPeer(t *testing.T) {
	test := func(name, direction string) {
		t.Run(name, func(t *testing.T) {
			server := NewServer(PairingInfo{})
			peer := NewPeer(PairingInfo{})
			request, err := server.Start()
			require.NoError(t, err)
			discovery, err := peer.Receive(request)
			require.NoError(t, err)
			require.Nil(t, discovery.Err)
			negotiation, err := server.Receive(discovery.Send)
			require.NoError(t, err)
			require.Nil(t, negotiation.Err)
			selection, err := peer.Receive(negotiation.Send)
			require.NoError(t, err)
			require.Nil(t, selection.Err)
			wire, err := decode(selection.Send)
			require.NoError(t, err)
			wire.Dirp = json.RawMessage(direction)
			message, _, err := encode(wire)
			require.NoError(t, err)

			out, err := server.Receive(message)

			require.NoError(t, err)
			require.Nil(t, out.Err)
			keyExchange, err := decode(out.Send)
			require.NoError(t, err)
			require.NotNil(t, keyExchange.Type)
			assert.Equal(t, typeKeyExchange, *keyExchange.Type)
		})
	}
	test("server-to-peer selection", "2")
	test("both-direction selection still includes server-to-peer", "3")
}

func TestServerNegotiation_RejectsUnsupportedOrMismatchedSelections(t *testing.T) {
	test := func(name string, change func(*wireMessage), wantCode int) {
		t.Run(name, func(t *testing.T) {
			server := NewServer(PairingInfo{})
			peer := NewPeer(PairingInfo{})
			request, err := server.Start()
			require.NoError(t, err)
			discovery, err := peer.Receive(request)
			require.NoError(t, err)
			require.Nil(t, discovery.Err)
			negotiation, err := server.Receive(discovery.Send)
			require.NoError(t, err)
			require.Nil(t, negotiation.Err)
			response, err := peer.Receive(negotiation.Send)
			require.NoError(t, err)
			require.Nil(t, response.Err)
			wire, err := decode(response.Send)
			require.NoError(t, err)
			change(wire)
			changed, _, err := encode(wire)
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
	test("mismatched peer identifier", func(wire *wireMessage) {
		wire.PeerId = json.RawMessage(`"another-peer"`)
	}, ErrUnexpectedPeerIdentier)
}

func TestServer_RejectsNoobOutsideWaiting(t *testing.T) {
	test := func(name string, registered bool) {
		t.Run(name, func(t *testing.T) {
			srv := NewServer(PairingInfo{})
			wantState := StateUnregistered
			if registered {
				srv, _ = pair(t, 2)
				wantState = StateRegistered
			}

			assert.Error(t, srv.OOBOutputWith(make([]byte, 16)))
			assert.Equal(t, wantState, srv.State())
		})
	}
	test("before initial exchange", false)
	test("after successful Completion", true)
}

func TestServer_RejectsMalformedJSON(t *testing.T) {
	out, err := NewServer(PairingInfo{}).Receive([]byte(`{"Type":`))

	require.NoError(t, err)
	require.NotNil(t, out.Err)
	assert.Equal(t, ErrInvalidMessageStructure, out.Err.Code)
	assert.True(t, out.Done)
	assert.False(t, out.Success)
}
