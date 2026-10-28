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
	server := NewServer(PairingInfo{})
	peer := NewPeer(PairingInfo{})
	selection, err := peer.Receive(negotiationRequest(t, server, peer))
	require.NoError(t, err)
	require.Nil(t, selection.Err)
	wire, err := decode(selection.Send)
	require.NoError(t, err)
	return server, wire
}

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
	offer, err := decode(negotiationRequest(t, server, peer))
	require.NoError(t, err)
	assert.Equal(t, json.RawMessage(`[1]`), offer.Vers)
	assert.Equal(t, json.RawMessage(`[2,1]`), offer.Cryptosuites)
	assert.Equal(t, json.RawMessage(`2`), offer.Dirs)
	var receivedInfo PairingInfo
	require.NoError(t, json.Unmarshal(offer.ServerInfo, &receivedInfo))
	assert.Equal(t, info, receivedInfo)
	assert.Empty(t, offer.NewNAI)
}

func TestServerDiscovery_BindsIncomingNAIToAuthenticatedTranscript(t *testing.T) {
	server := NewServer(PairingInfo{})
	negotiation, err := server.Receive([]byte(`{"Type":1,"PeerState":0,"NAI":"remote@pair.test"}`))
	require.NoError(t, err)
	require.Nil(t, negotiation.Err)
	offer, err := decode(negotiation.Send)
	require.NoError(t, err)
	selection, err := json.Marshal(&wireMessage{
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
			server, wire := serverNegotiationSelection(t)
			wire.Dirp = json.RawMessage(direction)
			message, err := json.Marshal(wire)
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

// A completion message can arrive while the server is waiting for the PIN,
// before its authentication keys exist. We should reject in that case
func TestServer_RejectsCompletionBeforeKeysAreDerived(t *testing.T) {
	test := func(name string, suite cryptosuiteID) {
		t.Run(name, func(t *testing.T) {
			server := NewServer(PairingInfo{})
			peer := NewPeer(PairingInfo{})
			driveInitialWithSuite(t, server, peer, suite)
			require.Equal(t, StateWaiting, server.State())
			require.Empty(t, server.keyM.Kmp)

			// Before PIN verification, the transcript is public and an empty-key
			// MAC is computable without knowing the PIN. It must not grant trust.
			macp := server.suite.computeMAC(nil, 1, server.in)
			message, err := json.Marshal(&wireMessage{
				Type:   intp(typeCompletion),
				PeerId: rawString(server.peerId),
				MACp:   rawString(b64Encode(macp)),
			})
			require.NoError(t, err)

			out, err := server.Receive(message)

			require.NoError(t, err)
			require.NotNil(t, out.Err)
			assert.Equal(t, ErrStateMismatch, out.Err.Code)
			assert.True(t, out.Done)
			assert.False(t, out.Success)
			assert.Equal(t, StateWaiting, server.State())
		})
	}
	test("X25519", cryptosuiteX25519)
	test("P-256", cryptosuiteP256)
}

// A node can answer discovery with a key-exchange response before negotiation
// selects an algorithm and creates the server's private key. Rejecting that
// response prevents a malformed pairing attempt from crashing the inviter.
func TestServer_RejectsKeyExchangeBeforeNegotiation(t *testing.T) {
	server := NewServer(PairingInfo{})
	_, err := server.Start()
	require.NoError(t, err)

	out, err := server.Receive([]byte(`{"Type":3,"PKp":{},"Np":""}`))

	require.NoError(t, err)
	require.NotNil(t, out.Err)
	assert.Equal(t, ErrStateMismatch, out.Err.Code)
	assert.True(t, out.Done)
	assert.False(t, out.Success)
	assert.Equal(t, StateUnregistered, server.State())
}
