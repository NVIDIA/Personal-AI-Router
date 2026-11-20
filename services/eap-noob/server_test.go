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

// Completion requires authentication keys even after the server receives
// the pairing secret.
func TestServer_RejectsCompletionBeforeKeysAreDerived(t *testing.T) {
	test := func(name string, state State) {
		t.Run(name, func(t *testing.T) {
			server := NewServer(ServerConfig{Dirs: OOBPeerToServer}, nil)
			peer := NewPeer(PeerConfig{}, nil)
			require.Nil(t, driveConversation(t, server, peer).Err)
			require.Equal(t, StateWaiting, server.State())
			if state == StateOOBReceived {
				oob, err := peer.OOBOutput()
				require.NoError(t, err)
				require.NoError(t, server.OOBInput(oob))
			}
			require.Equal(t, state, server.State())
			require.Empty(t, server.keyM.Kmp)

			// The transcript is public, so an authentication code computed with
			// an empty key must not grant trust.
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
			assert.Equal(t, state, server.State())
			assert.Nil(t, server.Association())
		})
	}
	test("before receiving the secret", StateWaiting)
	test("after receiving the secret", StateOOBReceived)
}

// A node can answer discovery with a key-exchange response before negotiation
// selects an algorithm and creates the server's private key. Rejecting that
// response prevents a malformed pairing attempt from crashing the inviter.
func TestServer_RejectsKeyExchangeBeforeNegotiation(t *testing.T) {
	server := NewServer(ServerConfig{}, nil)
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

// Once the Initial Exchange has finished, a key-exchange response that arrives
// before the Completion Exchange must not replace the shared secret. Otherwise
// an attacker who later learns the PIN could pair with the stored secret.
func TestServer_RejectsKeyExchangeAfterInitialExchange(t *testing.T) {
	server := NewServer(ServerConfig{Dirs: OOBServerToPeer}, nil)
	peer := NewPeer(PeerConfig{}, nil)
	require.Nil(t, driveConversation(t, server, peer).Err)
	require.Equal(t, StateWaiting, server.State())
	require.NoError(t, server.OOBOutputWith(bytes.Repeat([]byte{7}, 16)))
	secretBefore := append([]byte(nil), server.z...)

	_, attackerKey, err := server.suite.generateKeypair()
	require.NoError(t, err)
	publicKey, err := jwkRaw(attackerKey)
	require.NoError(t, err)
	injected, err := json.Marshal(&wireMessage{
		Type:   intp(typeKeyExchange),
		PeerId: rawString(server.peerId),
		PKp:    publicKey,
		Np:     rawString(b64Encode(bytes.Repeat([]byte{9}, 32))),
	})
	require.NoError(t, err)

	out, err := server.Receive(injected)

	require.NoError(t, err)
	require.NotNil(t, out.Err)
	assert.Equal(t, ErrStateMismatch, out.Err.Code)
	assert.True(t, out.Done)
	assert.False(t, out.Success)
	assert.Equal(t, secretBefore, server.z)
}

// A registered server must not run the Completion handler again for a
// message captured from the exchange that registered it.
func TestServer_RejectsReplayedCompletionAfterRegistration(t *testing.T) {
	server := NewServer(ServerConfig{Dirs: OOBServerToPeer}, nil)
	peer := NewPeer(PeerConfig{}, nil)
	require.Nil(t, driveConversation(t, server, peer).Err)
	noob := bytes.Repeat([]byte{7}, 16)
	require.NoError(t, server.OOBOutputWith(noob))
	require.NoError(t, peer.OOBInputNoob(noob))

	var completion []byte
	message, err := server.Start()
	require.NoError(t, err)
	for peerTurn := true; len(message) != 0; peerTurn = !peerTurn {
		var out Outcome
		if peerTurn {
			out, err = peer.Receive(message)
			if wire, decodeErr := decode(out.Send); decodeErr == nil && wire.Type != nil && *wire.Type == typeCompletion {
				completion = out.Send
			}
		} else {
			out, err = server.Receive(message)
		}
		require.NoError(t, err)
		require.Nil(t, out.Err)
		message = out.Send
	}
	require.Equal(t, StateRegistered, server.State())
	require.NotEmpty(t, completion)

	out, err := server.Receive(completion)

	require.NoError(t, err)
	require.NotNil(t, out.Err)
	assert.Equal(t, ErrStateMismatch, out.Err.Code)
	assert.False(t, out.Success)
	assert.Equal(t, StateRegistered, server.State())
}
