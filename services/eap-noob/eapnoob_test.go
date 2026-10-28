// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eapnoob

import (
	"encoding/json"
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
	driveMessages(t, srv, peer, msg)
}

func driveMessages(t *testing.T, srv *Server, peer *Peer, msg []byte) {
	t.Helper()
	peerTurn := true
	for {
		var out Outcome
		var err error
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

// driveInitialWithSuite supplies a single-suite offer as a remote-protocol
// fixture. Production always advertises both suites; tests can exercise each
// negotiated suite without adding configuration to the application API.
func driveInitialWithSuite(t *testing.T, srv *Server, peer *Peer, suite cryptosuiteID) {
	t.Helper()
	request, err := srv.Start()
	require.NoError(t, err)
	discovery, err := peer.Receive(request)
	require.NoError(t, err)
	require.Nil(t, discovery.Err)
	negotiation, err := srv.Receive(discovery.Send)
	require.NoError(t, err)
	require.Nil(t, negotiation.Err)
	wire, err := decode(negotiation.Send)
	require.NoError(t, err)
	wire.Cryptosuites = rawIntSlice([]int{int(suite)})
	request, parsed, err := encode(wire)
	require.NoError(t, err)
	srv.in.Cryptosuites = parsed.Cryptosuites
	driveMessages(t, srv, peer, request)
}

// pair runs a full server-to-peer pairing for the given cryptosuite.
func pair(t *testing.T, suite cryptosuiteID) (*Server, *Peer) {
	t.Helper()
	srv := NewServer(PairingInfo{Name: "test-server"})
	peer := NewPeer(PairingInfo{Name: "test-peer"})

	// Initial Exchange.
	driveInitialWithSuite(t, srv, peer, suite)
	require.Equal(t, StateWaiting, srv.State(), "server after Initial")
	require.Equal(t, StateWaiting, peer.State(), "peer after Initial")

	msg, err := srv.OOBOutput()
	require.NoError(t, err, "server OOBOutput:")
	require.NoError(t, peer.OOBInput(msg), "peer OOBInput:")
	require.Equal(t, StateWaiting, srv.State())
	require.Equal(t, StateOOBReceived, peer.State())

	// Completion Exchange.
	driveConversation(t, srv, peer)
	require.Equal(t, StateRegistered, srv.State(), "server after Completion")
	require.Equal(t, StateRegistered, peer.State(), "peer after Completion")
	return srv, peer
}

func TestPairing_AuthenticatesIdentityWithBothSuites(t *testing.T) {
	test := func(name string, suite cryptosuiteID) {
		t.Run(name, func(t *testing.T) {
			srv, peer := pair(t, suite)
			assert.Equal(t, suite, srv.suite.id)
			assert.Equal(t, suite, peer.suite.id)
			assert.Equal(t, peer.peerId, srv.peerId)
			var serverInfo, peerInfo PairingInfo
			require.NoError(t, json.Unmarshal(peer.ServerInfo(), &serverInfo))
			require.NoError(t, json.Unmarshal(srv.PeerInfo(), &peerInfo))
			assert.Equal(t, PairingInfo{Name: "test-server"}, serverInfo)
			assert.Equal(t, PairingInfo{Name: "test-peer"}, peerInfo)
		})
	}
	test("suite 1", cryptosuiteX25519)
	test("suite 2", cryptosuiteP256)
}

func TestPairing_RejectsTamperedMAC(t *testing.T) {
	srv := NewServer(PairingInfo{})
	peer := NewPeer(PairingInfo{})

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
	srv := NewServer(PairingInfo{})
	peer := NewPeer(PairingInfo{})
	driveConversation(t, srv, peer)

	oob, err := srv.OOBOutput()
	require.NoError(t, err, "OOBOutput:")
	oob.Hoob = "AAAAAAAAAAAAAAAAAAAAAA" // wrong 16-byte fingerprint
	require.Error(t, peer.OOBInput(oob), "expected OOBInput to reject bad fingerprint")
	assert.Equal(t, StateWaiting, peer.State(), "peer state %s after rejected OOB, want WaitingForOOB", peer.State())
}

func TestJWK_RoundTripsPublicKeys(t *testing.T) {
	test := func(name string, id cryptosuiteID) {
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
	test("suite 1", cryptosuiteX25519)
	test("suite 2", cryptosuiteP256)
}
