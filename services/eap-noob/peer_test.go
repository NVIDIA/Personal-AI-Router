// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eapnoob

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func peerNegotiationOffer(t *testing.T) (*Peer, *wireMessage) {
	t.Helper()
	server := NewServer(ServerConfig{}, nil)
	peer := NewPeer(PeerConfig{}, nil)
	offer, err := decode(negotiationRequest(t, server, peer))
	require.NoError(t, err)
	return peer, offer
}

func TestPeer_AdvertisesDefaultParameters(t *testing.T) {
	server := NewServer(ServerConfig{}, nil)
	info := PairingInfo{
		V:                   2,
		NodeUUID:            "peer-uuid",
		NodeID:              "peer-node",
		Name:                "peer",
		ClusterID:           "test-cluster",
		AdmissionEpoch:      9007199254740993,
		ClusterFriendlyName: "Test cluster",
		Addr:                "peer.example:14321",
		Cert:                "peer-cert",
	}
	peer := NewPeer(PeerConfig{PeerInfo: info}, nil)
	request, err := server.Start()
	require.NoError(t, err)
	discovery, err := peer.Receive(request)
	require.NoError(t, err)
	require.Nil(t, discovery.Err)
	identity, err := decode(discovery.Send)
	require.NoError(t, err)
	assert.Equal(t, json.RawMessage(`"noob@eap-noob.arpa"`), identity.NAI)

	negotiation, err := server.Receive(discovery.Send)
	require.NoError(t, err)
	require.Nil(t, negotiation.Err)
	selection, err := peer.Receive(negotiation.Send)
	require.NoError(t, err)
	require.Nil(t, selection.Err)
	response, err := decode(selection.Send)
	require.NoError(t, err)
	assert.Equal(t, json.RawMessage(`1`), response.Verp)
	assert.Equal(t, json.RawMessage(`2`), response.Cryptosuitep)
	assert.Equal(t, json.RawMessage(`2`), response.Dirp)
	var receivedInfo PairingInfo
	require.NoError(t, json.Unmarshal(response.PeerInfo, &receivedInfo))
	assert.Equal(t, info, receivedInfo)
}

func TestPeerNegotiation_SelectsSupportedParameters(t *testing.T) {
	test := func(name string, versions, suites json.RawMessage, wantSuite string) {
		t.Run(name, func(t *testing.T) {
			peer, offer := peerNegotiationOffer(t)
			offer.Vers = versions
			offer.Cryptosuites = suites
			message, err := json.Marshal(offer)
			require.NoError(t, err)

			out, err := peer.Receive(message)

			require.NoError(t, err)
			require.Nil(t, out.Err)
			response, err := decode(out.Send)
			require.NoError(t, err)
			assert.Equal(t, json.RawMessage(`1`), response.Verp)
			assert.Equal(t, json.RawMessage(wantSuite), response.Cryptosuitep)
		})
	}
	test("unsupported suite is skipped", json.RawMessage(`[1]`), json.RawMessage(`[99,1]`), "1")
	test("supported version is selected from a larger offer", json.RawMessage(`[99,1]`), json.RawMessage(`[2,1]`), "2")
}

func TestPeerNegotiation_RejectsUnsupportedOffers(t *testing.T) {
	test := func(name string, change func(*wireMessage), wantCode int) {
		t.Run(name, func(t *testing.T) {
			peer, offer := peerNegotiationOffer(t)
			change(offer)
			message, err := json.Marshal(offer)
			require.NoError(t, err)

			out, err := peer.Receive(message)

			require.NoError(t, err)
			require.NotNil(t, out.Err)
			assert.Equal(t, wantCode, out.Err.Code)
			assert.True(t, out.Done)
			assert.False(t, out.Success)
			assert.NotEqual(t, StateRegistered, peer.State())
		})
	}
	test("unsupported version", func(offer *wireMessage) {
		offer.Vers = json.RawMessage(`[99]`)
	}, ErrUnsupportedVersion)
	test("unsupported cryptosuite", func(offer *wireMessage) {
		offer.Cryptosuites = json.RawMessage(`[99]`)
	}, ErrUnsupportedCryptosuite)
	test("no OOB direction", func(offer *wireMessage) {
		offer.Dirs = json.RawMessage(`0`)
	}, ErrNoMutuallySupportedOOB)
}

func TestPeer_RejectsSuccessBeforeVerifiedCompletion(t *testing.T) {
	test := func(name string, state State) {
		t.Run(name, func(t *testing.T) {
			srv := NewServer(ServerConfig{}, nil)
			peer := NewPeer(PeerConfig{}, nil)
			if state != StateUnregistered {
				require.Nil(t, driveConversation(t, srv, peer).Err)
			}
			if state == StateOOBReceived {
				require.NoError(t, peer.OOBInputNoob(make([]byte, 16)))
			}
			require.Equal(t, state, peer.State())

			out, err := peer.Receive(resultBytes(eapSuccess))

			require.NoError(t, err)
			require.NotNil(t, out.Err)
			assert.Equal(t, ErrStateMismatch, out.Err.Code)
			assert.True(t, out.Done)
			assert.False(t, out.Success)
			assert.Equal(t, state, peer.State())
			assert.Nil(t, peer.Association())
			secret, err := peer.Export("app", nil, 32)
			assert.ErrorIs(t, err, ErrNotRegistered)
			assert.Empty(t, secret)
		})
	}
	test("before initial exchange", StateUnregistered)
	test("before OOB input", StateWaiting)
	test("after OOB input without Completion", StateOOBReceived)
}

func TestPeer_RejectsChangedAuthenticatedCertificateInformation(t *testing.T) {
	test := func(name string, changedServerInfo, changedPeerInfo json.RawMessage) {
		t.Run(name, func(t *testing.T) {
			server := NewServer(ServerConfig{ServerInfo: PairingInfo{Cert: "original-server-cert"}}, nil)
			peer := NewPeer(PeerConfig{PeerInfo: PairingInfo{Cert: "original-peer-cert"}}, nil)

			offer, err := decode(negotiationRequest(t, server, peer))
			require.NoError(t, err)
			if len(changedServerInfo) != 0 {
				offer.ServerInfo = changedServerInfo
			}
			request, err := json.Marshal(offer)
			require.NoError(t, err)
			selection, err := peer.Receive(request)
			require.NoError(t, err)
			require.Nil(t, selection.Err)
			wire, err := decode(selection.Send)
			require.NoError(t, err)
			if len(changedPeerInfo) != 0 {
				wire.PeerInfo = changedPeerInfo
			}
			response, err := json.Marshal(wire)
			require.NoError(t, err)
			keyExchange, err := server.Receive(response)
			require.NoError(t, err)
			require.Nil(t, keyExchange.Err)
			require.Nil(t, driveMessages(t, server, peer, keyExchange.Send).Err)
			require.Equal(t, StateWaiting, server.State())
			require.Equal(t, StateWaiting, peer.State())

			noob := bytes.Repeat([]byte{0xAB}, 16)
			require.NoError(t, server.OOBOutputWith(noob))
			require.NoError(t, peer.OOBInputNoob(noob))
			out := driveConversation(t, server, peer)

			require.NotNil(t, out.Err)
			assert.Equal(t, StateOOBReceived, out.State, "peer must reject the server's transcript MAC")
			assert.Equal(t, ErrHMACVerificationFailed, out.Err.Code)
			assert.False(t, out.Success)
			assert.NotEqual(t, StateRegistered, peer.State())
			assert.NotEqual(t, StateRegistered, server.State())
		})
	}
	test("changed server certificate", json.RawMessage(`{"cert":"changed-server-cert"}`), nil)
	test("changed peer certificate", nil, json.RawMessage(`{"cert":"changed-peer-cert"}`))
}

// A sender can skip negotiation after discovery. Reject its key-exchange
// message before accessing the algorithm that negotiation would have selected.
func TestPeer_RejectsKeyExchangeBeforeNegotiation(t *testing.T) {
	peer := NewPeer(PeerConfig{}, nil)
	discovery, err := peer.Receive([]byte(`{"Type":1}`))
	require.NoError(t, err)
	require.Nil(t, discovery.Err)

	out, err := peer.Receive([]byte(`{"Type":3,"PKs":{},"Ns":""}`))

	require.NoError(t, err)
	require.NotNil(t, out.Err)
	assert.Equal(t, ErrStateMismatch, out.Err.Code)
	assert.True(t, out.Done)
	assert.False(t, out.Success)
	assert.Equal(t, StateUnregistered, peer.State())
}
