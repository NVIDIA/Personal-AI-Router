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
	server := NewServer(PairingInfo{})
	peer := NewPeer(PairingInfo{})
	offer, err := decode(negotiationRequest(t, server, peer))
	require.NoError(t, err)
	return peer, offer
}

func TestPeer_AdvertisesPAIRDefaults(t *testing.T) {
	server := NewServer(PairingInfo{})
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
	peer := NewPeer(info)
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
	test := func(name string, versions, suites, directions json.RawMessage, wantSuite string) {
		t.Run(name, func(t *testing.T) {
			peer, offer := peerNegotiationOffer(t)
			offer.Vers = versions
			offer.Cryptosuites = suites
			offer.Dirs = directions
			message, _, err := encode(offer)
			require.NoError(t, err)

			out, err := peer.Receive(message)

			require.NoError(t, err)
			require.Nil(t, out.Err)
			response, err := decode(out.Send)
			require.NoError(t, err)
			assert.Equal(t, json.RawMessage(`1`), response.Verp)
			assert.Equal(t, json.RawMessage(wantSuite), response.Cryptosuitep)
			assert.Equal(t, json.RawMessage(`2`), response.Dirp)
		})
	}
	test("suite 1 remains supported", json.RawMessage(`[1]`), json.RawMessage(`[1]`), json.RawMessage(`2`), "1")
	test("suite 2 remains supported", json.RawMessage(`[1]`), json.RawMessage(`[2]`), json.RawMessage(`2`), "2")
	test("server preference selects suite 2", json.RawMessage(`[1]`), json.RawMessage(`[2,1]`), json.RawMessage(`2`), "2")
	test("unsupported suite is skipped", json.RawMessage(`[1]`), json.RawMessage(`[99,1]`), json.RawMessage(`2`), "1")
	test("supported version is selected from a larger offer", json.RawMessage(`[99,1]`), json.RawMessage(`[2,1]`), json.RawMessage(`2`), "2")
	test("both directions select server-to-peer", json.RawMessage(`[1]`), json.RawMessage(`[2,1]`), json.RawMessage(`3`), "2")
}

func TestPeerNegotiation_RejectsUnsupportedOffers(t *testing.T) {
	test := func(name string, change func(*wireMessage), wantCode int) {
		t.Run(name, func(t *testing.T) {
			peer, offer := peerNegotiationOffer(t)
			change(offer)
			message, _, err := encode(offer)
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
	test("inverse direction only", func(offer *wireMessage) {
		offer.Dirs = json.RawMessage(`1`)
	}, ErrNoMutuallySupportedOOB)
	test("no OOB direction", func(offer *wireMessage) {
		offer.Dirs = json.RawMessage(`0`)
	}, ErrNoMutuallySupportedOOB)
}

func TestPeerNegotiation_BindsAssignedNAIToAuthenticatedTranscript(t *testing.T) {
	peer, offer := peerNegotiationOffer(t)
	offer.NewNAI = json.RawMessage(`"assigned@pair.test"`)
	message, _, err := encode(offer)
	require.NoError(t, err)

	out, err := peer.Receive(message)

	require.NoError(t, err)
	require.Nil(t, out.Err)
	assert.Equal(t, json.RawMessage(`"assigned@pair.test"`), peer.in.NAI)
	discovery, err := peer.Receive([]byte(`{"Type":1}`))
	require.NoError(t, err)
	require.Nil(t, discovery.Err)
	identity, err := decode(discovery.Send)
	require.NoError(t, err)
	assert.Equal(t, json.RawMessage(`"assigned@pair.test"`), identity.NAI)
}

func TestPeer_RejectsSuccessBeforeVerifiedCompletion(t *testing.T) {
	test := func(name string, state State) {
		t.Run(name, func(t *testing.T) {
			srv := NewServer(PairingInfo{})
			peer := NewPeer(PairingInfo{})
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
		})
	}
	test("before initial exchange", StateUnregistered)
	test("before OOB input", StateWaiting)
	test("after OOB input without Completion", StateOOBReceived)
}

func TestPeer_RejectsNoobOutsideWaiting(t *testing.T) {
	test := func(name string, registered bool) {
		t.Run(name, func(t *testing.T) {
			peer := NewPeer(PairingInfo{})
			wantState := StateUnregistered
			if registered {
				_, peer = pair(t, 2)
				wantState = StateRegistered
			}

			assert.Error(t, peer.OOBInputNoob(make([]byte, 16)))
			assert.Equal(t, wantState, peer.State())
		})
	}
	test("before initial exchange", false)
	test("after successful Completion", true)
}

func TestPeer_RejectsChangedAuthenticatedCertificateInformation(t *testing.T) {
	test := func(name string, changedServerInfo, changedPeerInfo json.RawMessage) {
		t.Run(name, func(t *testing.T) {
			server := NewServer(PairingInfo{Cert: "original-server-cert"})
			peer := NewPeer(PairingInfo{Cert: "original-peer-cert"})

			offer, err := decode(negotiationRequest(t, server, peer))
			require.NoError(t, err)
			if len(changedServerInfo) != 0 {
				offer.ServerInfo = changedServerInfo
			}
			request, _, err := encode(offer)
			require.NoError(t, err)
			selection, err := peer.Receive(request)
			require.NoError(t, err)
			require.Nil(t, selection.Err)
			wire, err := decode(selection.Send)
			require.NoError(t, err)
			if len(changedPeerInfo) != 0 {
				wire.PeerInfo = changedPeerInfo
			}
			response, _, err := encode(wire)
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

func TestPeer_RejectsMalformedJSON(t *testing.T) {
	out, err := NewPeer(PairingInfo{}).Receive([]byte(`{"Type":`))

	require.NoError(t, err)
	require.NotNil(t, out.Err)
	assert.Equal(t, ErrInvalidMessageStructure, out.Err.Code)
	assert.True(t, out.Done)
	assert.False(t, out.Success)
}
