// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eapnoob

import (
	"bytes"
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
	}, nil)
	peer := NewPeer(PeerConfig{
		PreferDir: dir,
		PeerInfo:  map[string]any{"PeerName": "test-peer"},
	}, nil)

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

func TestPairingAllSuitesAndDirections(t *testing.T) {
	for _, suite := range []int{1, 2} {
		for _, dir := range []int{1, 2} {
			suite, dir := suite, dir
			t.Run("", func(t *testing.T) {
				srv, peer := pair(t, suite, dir)

				assert.Equal(t, suite, srv.Association().Cryptosuitep, "server negotiated cryptosuite %d, want %d", srv.Association().Cryptosuitep, suite)
				assert.Equal(t, peer.Association().Kz, srv.Association().Kz, "Kz mismatch between server and peer")
				assert.Equal(t, peer.Association().PeerId, srv.Association().PeerId, "PeerId mismatch: %q vs %q", srv.Association().PeerId, peer.Association().PeerId)

				// The exported secret must agree for several lengths and differ
				// across labels/contexts.
				for _, n := range []int{1, 16, 32, 100, 1000} {
					sSecret, err := srv.Export("app", nil, n)
					require.NoError(t, err, "server Export(%d): %v", n, err)
					pSecret, err := peer.Export("app", nil, n)
					require.NoError(t, err, "peer Export(%d): %v", n, err)
					require.Len(t, sSecret, n, "export length %d, want %d", len(sSecret), n)
					assert.Equal(t, pSecret, sSecret, "exported secret mismatch at length %d", n)
				}

				a, _ := srv.Export("label-a", nil, 32)
				b, _ := srv.Export("label-b", nil, 32)
				assert.NotEqual(t, b, a, "different labels produced identical secrets")
				c1, _ := srv.Export("app", []byte("ctx1"), 32)
				c2, _ := srv.Export("app", []byte("ctx2"), 32)
				assert.NotEqual(t, c2, c1, "different contexts produced identical secrets")
			})
		}
	}
}

func TestExportBeforeRegistrationFails(t *testing.T) {
	peer := NewPeer(PeerConfig{}, nil)
	_, err := peer.Export("app", nil, 16)
	assert.ErrorIs(t, err, ErrNotRegistered, "Export before registration")
}

func TestTamperedMACFails(t *testing.T) {
	srv := NewServer(ServerConfig{Cryptosuites: []int{1}, Dirs: 1}, nil)
	peer := NewPeer(PeerConfig{PreferDir: 1}, nil)

	driveConversation(t, srv, peer)
	oob, err := peer.OOBOutput()
	require.NoError(t, err, "OOBOutput:")
	require.NoError(t, srv.OOBInput(oob), "OOBInput:")

	// Run the Completion handshake manually and corrupt MACp before the server
	// verifies it.
	req1, _ := srv.Start()
	resp1, err := peer.Receive(req1) // Type=1 response
	require.NoError(t, err, "peer discovery:")
	req6, err := srv.Receive(resp1.Send) // Type=6 request (MACs)
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

func TestRejectBadOOBFingerprint(t *testing.T) {
	srv := NewServer(ServerConfig{Cryptosuites: []int{1}, Dirs: 2}, nil)
	peer := NewPeer(PeerConfig{PreferDir: 2}, nil)
	driveConversation(t, srv, peer)

	oob, err := srv.OOBOutput()
	require.NoError(t, err, "OOBOutput:")
	oob.Hoob = "AAAAAAAAAAAAAAAAAAAAAA" // wrong 16-byte fingerprint
	require.Error(t, peer.OOBInput(oob), "expected OOBInput to reject bad fingerprint")
	assert.Equal(t, StateWaiting, peer.State(), "peer state %s after rejected OOB, want WaitingForOOB", peer.State())
}

func TestOneStepKDFDeterministic(t *testing.T) {
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

func TestJWKRoundTrip(t *testing.T) {
	for _, id := range []int{1, 2} {
		cs, err := suiteByID(id)
		require.NoError(t, err)
		priv, jwk, err := cs.generateKeypair()
		require.NoError(t, err, "generate:")
		pub, err := cs.decodeJWK(jwk)
		require.NoError(t, err, "decode JWK:")
		assert.Equal(t, priv.PublicKey().Bytes(), pub.Bytes(), "suite %d: JWK round-trip mismatch", id)
	}
}
