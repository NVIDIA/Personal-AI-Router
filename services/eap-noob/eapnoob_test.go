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

// driveConversation starts one message exchange and returns its final outcome.
func driveConversation(t *testing.T, srv *Server, peer *Peer) Outcome {
	t.Helper()
	msg, err := srv.Start()
	require.NoError(t, err, "server Start:")
	return driveMessages(t, srv, peer, msg)
}

func driveMessages(t *testing.T, srv *Server, peer *Peer, msg []byte) Outcome {
	t.Helper()
	peerTurn := true
	for rounds := 0; ; rounds++ {
		require.Less(t, rounds, 16, "message exchange did not terminate")
		var out Outcome
		var err error
		if peerTurn {
			out, err = peer.Receive(msg)
		} else {
			out, err = srv.Receive(msg)
		}
		require.NoError(t, err, "Receive:")
		if out.Err != nil || len(out.Send) == 0 {
			return out
		}
		msg = out.Send
		peerTurn = !peerTurn
	}
}

// negotiationRequest runs discovery and returns the server's negotiation request.
func negotiationRequest(t *testing.T, srv *Server, peer *Peer) []byte {
	t.Helper()
	request, err := srv.Start()
	require.NoError(t, err)
	discovery, err := peer.Receive(request)
	require.NoError(t, err)
	require.Nil(t, discovery.Err)
	negotiation, err := srv.Receive(discovery.Send)
	require.NoError(t, err)
	require.Nil(t, negotiation.Err)
	return negotiation.Send
}

// driveInitialWithSuite supplies a single-suite offer as a remote-protocol
// fixture. Production always advertises both suites; tests can exercise each
// negotiated suite without adding configuration to the application API.
func driveInitialWithSuite(t *testing.T, srv *Server, peer *Peer, suite cryptosuiteID) {
	t.Helper()
	wire, err := decode(negotiationRequest(t, srv, peer))
	require.NoError(t, err)
	wire.Cryptosuites = rawIntSlice([]int{int(suite)})
	request, err := json.Marshal(wire)
	require.NoError(t, err)
	srv.in.Cryptosuites = wire.Cryptosuites
	require.Nil(t, driveMessages(t, srv, peer, request).Err)
}

func TestExport_RejectsUnregisteredPairing(t *testing.T) {
	peer := NewPeer(PeerConfig{}, nil)
	_, err := peer.Export("app", nil, 16)
	assert.ErrorIs(t, err, ErrNotRegistered, "Export before registration")
}

func TestPairing_RejectsTamperedMAC(t *testing.T) {
	srv := NewServer(ServerConfig{}, nil)
	peer := NewPeer(PeerConfig{}, nil)

	require.Nil(t, driveConversation(t, srv, peer).Err)
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
	tampered, err := json.Marshal(wm)
	require.NoError(t, err, "re-encode:")
	out, err := srv.Receive(tampered)
	require.NoError(t, err, "server receive tampered:")
	require.NotNil(t, out.Err, "expected HMAC verification failure, got %+v", out.Err)
	assert.Equal(t, ErrHMACVerificationFailed, out.Err.Code, "expected HMAC verification failure, got %+v", out.Err)
}

func TestPairing_RejectsIncorrectOutOfBandFingerprint(t *testing.T) {
	srv := NewServer(ServerConfig{}, nil)
	peer := NewPeer(PeerConfig{}, nil)
	require.Nil(t, driveConversation(t, srv, peer).Err)

	oob, err := srv.OOBOutput()
	require.NoError(t, err, "OOBOutput:")
	oob.Hoob = "AAAAAAAAAAAAAAAAAAAAAA" // wrong 16-byte fingerprint
	require.Error(t, peer.OOBInput(oob), "expected OOBInput to reject bad fingerprint")
	assert.Equal(t, StateWaiting, peer.State(), "peer state %s after rejected OOB, want WaitingForOOB", peer.State())
}

func TestOneStepKDF_DerivesRepeatableKeys(t *testing.T) {
	z := bytes.Repeat([]byte{0x01}, 32)
	np := bytes.Repeat([]byte{0x02}, 32)
	ns := bytes.Repeat([]byte{0x03}, 32)
	noob := bytes.Repeat([]byte{0x04}, 16)

	km1 := deriveCompletion(z, np, ns, noob)
	km2 := deriveCompletion(z, np, ns, noob)

	assert.Equal(t, km2.MSK, km1.MSK, "KDF not deterministic")
	assert.Equal(t, km2.Kz, km1.Kz, "KDF not deterministic")
	assert.Len(t, km1.MSK, 64, "MSK length")
	assert.Len(t, km1.EMSK, 64, "EMSK length")
	assert.Len(t, km1.AMSK, 64, "AMSK length")
	assert.Len(t, km1.MethodID, 32, "MethodID length")
	assert.Len(t, km1.Kms, 32, "Kms length")
	assert.Len(t, km1.Kmp, 32, "Kmp length")
	assert.Len(t, km1.Kz, 32, "Kz length")
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
