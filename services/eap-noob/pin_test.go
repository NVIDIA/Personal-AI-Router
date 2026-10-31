// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eapnoob

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCallerInjectedNoobPairs drives a full pairing in the server-to-peer
// direction using the caller-injected-Noob API (the path the cluster manager's
// PIN flow takes): the server supplies the Noob via OOBOutputWith and the peer
// records the same Noob via OOBInputNoob, with no OOBMessage/Hoob envelope.
func TestPairing_AcceptsMatchingCallerProvidedNoob(t *testing.T) {
	test := func(name string, suite cryptosuiteID) {
		t.Run(name, func(t *testing.T) {
			srv := NewServer(PairingInfo{Name: "server"})
			peer := NewPeer(PairingInfo{Name: "peer"})
			driveInitialWithSuite(t, srv, peer, suite)
			require.Equal(t, StateWaiting, srv.State())
			require.Equal(t, StateWaiting, peer.State())

			noob := bytes.Repeat([]byte{0xAB}, 16)
			require.NoError(t, srv.OOBOutputWith(noob))
			assert.Equal(t, StateWaiting, srv.State())
			require.NoError(t, peer.OOBInputNoob(noob))
			require.Equal(t, StateOOBReceived, peer.State())

			require.Nil(t, driveConversation(t, srv, peer).Err)
			require.Equal(t, StateRegistered, srv.State())
			require.Equal(t, StateRegistered, peer.State())
			assert.Equal(t, srv.serverInfo, peer.ServerInfo())
			assert.Equal(t, peer.peerInfo, srv.PeerInfo())
		})
	}
	test("suite 1", cryptosuiteX25519)
	test("suite 2", cryptosuiteP256)
}

// The cluster manager uses the peer's terminal protocol error to report a
// wrong PIN as "incorrect-pin". Neither side may become registered.
func TestPeerWrongPin_YieldsProtocolError(t *testing.T) {
	srv := NewServer(PairingInfo{})
	peer := NewPeer(PairingInfo{})

	// Initial Exchange.
	require.Nil(t, driveConversation(t, srv, peer).Err)

	// Supply different NOOB values to represent a wrong PIN.
	require.NoError(t, srv.OOBOutputWith(bytes.Repeat([]byte{0x01}, 16)), "server OOBOutputWith:")
	require.NoError(t, peer.OOBInputNoob(bytes.Repeat([]byte{0x02}, 16)), "peer OOBInputNoob:")

	// Drive the Completion Exchange exactly as the joiner does: the server
	// kicks off (Start), and the peer receives each blob as the HTTP client.
	msg, err := srv.Start()
	require.NoError(t, err, "server start completion:")
	var peerOut Outcome
	for {
		require.NotEmpty(t, msg, "server ended completion unexpectedly before the peer went terminal")
		out, rerr := peer.Receive(msg)
		// The classifier reads out.Err.
		require.NoError(t, rerr, "a wrong PIN must surface in out.Err")
		if out.Done {
			peerOut = out
			break
		}
		sout, serr := srv.Receive(out.Send)
		require.NoError(t, serr, "server.Receive:")
		msg = sout.Send
	}

	require.NotNil(t, peerOut.Err, "peer completion produced no ProtocolError on a wrong PIN; the classifier would degrade the reason to empty")
	assert.Contains(t, []int{ErrUnrecognizedOOBMsgID, ErrHMACVerificationFailed}, peerOut.Err.Code, "wrong PIN must produce an unrecognized NoobId or MAC mismatch")
	assert.NotEqual(t, StateRegistered, peer.State(), "peer reached Registered despite a wrong PIN")
	assert.NotEqual(t, StateRegistered, srv.State(), "server reached Registered despite a wrong PIN")
}

// TestOOBOutputWithRejectsBadLength guards the 16-byte contract.
func TestOOBOutputWith_RejectsBadLength(t *testing.T) {
	srv := NewServer(PairingInfo{})
	peer := NewPeer(PairingInfo{})
	require.Nil(t, driveConversation(t, srv, peer).Err)
	require.Error(t, srv.OOBOutputWith(bytes.Repeat([]byte{0x01}, 8)), "expected OOBOutputWith to reject an 8-byte Noob")
}
