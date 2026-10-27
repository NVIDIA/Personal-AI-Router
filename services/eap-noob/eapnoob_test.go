// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eapnoob

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// driveConversation relays messages between the server and peer for one EAP
// conversation, starting from the server's first request, until a side has
// nothing more to send.
func driveConversation(t *testing.T, srv *Server, peer *Peer) {
	t.Helper()
	msg, err := srv.Start()
	require.NoError(t, err, "server Start:")
	peerTurn := true
	for {
		var out Outcome
		if peerTurn {
			out, err = peer.Receive(msg)
		} else {
			out, err = srv.Receive(msg)
		}
		require.NoError(t, err, "Receive:")
		require.Nil(t, out.Err, "unexpected protocol error: %v", out.Err)
		if len(out.Send) == 0 {
			return
		}
		msg = out.Send
		peerTurn = !peerTurn
	}
}

// pair runs a full EAP-NOOB pairing (Initial + OOB + Completion) for the given
// cryptosuite and OOB direction, returning the registered server and peer.
func pair(t *testing.T, suite, dir int) (*Server, *Peer) {
	t.Helper()
	srv := NewServer(ServerConfig{
		Cryptosuites: []int{suite},
		Dirs:         dir,
		ServerInfo:   map[string]any{"ServerName": "test-server"},
	})
	peer := NewPeer(PeerConfig{
		PreferDir: dir,
		PeerInfo:  map[string]any{"PeerName": "test-peer"},
	})

	// Initial Exchange.
	driveConversation(t, srv, peer)
	require.Equal(t, StateWaiting, srv.State(), "server after Initial")
	require.Equal(t, StateWaiting, peer.State(), "peer after Initial")

	// OOB Step in the negotiated direction.
	switch dir {
	case 1: // peer-to-server
		msg, err := peer.OOBOutput()
		require.NoError(t, err, "peer OOBOutput:")
		require.NoError(t, srv.OOBInput(msg), "server OOBInput:")
		require.Equal(t, StateOOBReceived, srv.State())
	case 2: // server-to-peer
		msg, err := srv.OOBOutput()
		require.NoError(t, err, "server OOBOutput:")
		require.NoError(t, peer.OOBInput(msg), "peer OOBInput:")
		require.Equal(t, StateOOBReceived, peer.State())
	}

	// Completion Exchange.
	driveConversation(t, srv, peer)
	require.Equal(t, StateRegistered, srv.State(), "server after Completion")
	require.Equal(t, StateRegistered, peer.State(), "peer after Completion")
	return srv, peer
}

func TestPairing_AuthenticatesIdentityAcrossSuitesAndDirections(t *testing.T) {
	test := func(name string, suite, dir int) {
		t.Run(name, func(t *testing.T) {
			srv, peer := pair(t, suite, dir)
			assert.Equal(t, suite, srv.csp)
			assert.Equal(t, suite, peer.csp)
			assert.Equal(t, peer.peerId, srv.peerId)
			assert.JSONEq(t, `{"PeerName":"test-peer"}`, string(srv.PeerInfo()))
			assert.JSONEq(t, `{"ServerName":"test-server"}`, string(peer.ServerInfo()))
		})
	}
	test("suite 1 server-to-peer", 1, 2)
	test("suite 2 server-to-peer", 2, 2)
	test("suite 1 peer-to-server", 1, 1)
	test("suite 2 peer-to-server", 2, 1)
}

func TestPairing_RejectsTamperedMAC(t *testing.T) {
	srv := NewServer(ServerConfig{Cryptosuites: []int{1}, Dirs: 2})
	peer := NewPeer(PeerConfig{PreferDir: 2})

	driveConversation(t, srv, peer)
	oob, err := srv.OOBOutput()
	require.NoError(t, err, "OOBOutput:")
	require.NoError(t, peer.OOBInput(oob), "OOBInput:")

	// Run the Completion handshake manually and corrupt MACp before the server
	// verifies it.
	req1, err := srv.Start()
	require.NoError(t, err, "start completion discovery")
	resp1, err := peer.Receive(req1) // Type=1 response
	require.NoError(t, err, "peer discovery:")
	req5, err := srv.Receive(resp1.Send) // Type=5 request (NoobId)
	require.NoError(t, err, "server NoobId request:")
	resp5, err := peer.Receive(req5.Send)
	require.NoError(t, err, "peer NoobId response:")
	req6, err := srv.Receive(resp5.Send) // Type=6 request (MACs)
	require.NoError(t, err, "server completion request:")
	resp6, err := peer.Receive(req6.Send) // Type=6 response (MACp)
	require.NoError(t, err, "peer completion:")

	// Replace MACp with a valid-length (32-byte) but incorrect value so the
	// failure is the HMAC check rather than a structural error.
	wm, err := decode(resp6.Send)
	require.NoError(t, err, "decode completion response:")
	wm.MACp = rawString(b64Encode(make([]byte, 32)))
	tampered, _, err := encode(wm)
	require.NoError(t, err, "re-encode:")
	out, err := srv.Receive(tampered)
	require.NoError(t, err, "server receive tampered:")
	require.NotNil(t, out.Err, "expected HMAC verification failure, got %+v", out.Err)
	assert.Equal(t, ErrHMACVerificationFailed, out.Err.Code, "expected HMAC verification failure, got %+v", out.Err)
}

func TestPairing_RejectsIncorrectOutOfBandFingerprint(t *testing.T) {
	srv := NewServer(ServerConfig{Cryptosuites: []int{1}, Dirs: 2})
	peer := NewPeer(PeerConfig{PreferDir: 2})
	driveConversation(t, srv, peer)

	oob, err := srv.OOBOutput()
	require.NoError(t, err, "OOBOutput:")
	oob.Hoob = "AAAAAAAAAAAAAAAAAAAAAA" // wrong 16-byte fingerprint
	require.Error(t, peer.OOBInput(oob), "expected OOBInput to reject bad fingerprint")
	assert.Equal(t, StateWaiting, peer.State(), "peer state %s after rejected OOB, want WaitingForOOB", peer.State())
}

func TestJWK_RoundTripsPublicKeys(t *testing.T) {
	test := func(name string, id int) {
		t.Run(name, func(t *testing.T) {
			cs, err := suiteByID(id)
			require.NoError(t, err)
			priv, jwk, err := cs.generateKeypair()
			require.NoError(t, err, "generate keypair")
			pub, err := cs.decodeJWK(jwk)
			require.NoError(t, err, "decode JWK")
			assert.Equal(t, priv.PublicKey().Bytes(), pub.Bytes(), "JWK round-trip mismatch")
		})
	}
	test("suite 1", 1)
	test("suite 2", 2)
}
