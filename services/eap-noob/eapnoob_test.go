// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eapnoob

import (
	"bytes"
	"testing"
)

// driveConversation relays messages between the server and peer for one EAP
// conversation, starting from the server's first request, until a side has
// nothing more to send.
func driveConversation(t *testing.T, srv *Server, peer *Peer) {
	t.Helper()
	msg, err := srv.Start()
	if err != nil {
		t.Fatalf("server Start: %v", err)
	}
	peerTurn := true
	for {
		var out Outcome
		if peerTurn {
			out, err = peer.Receive(msg)
		} else {
			out, err = srv.Receive(msg)
		}
		if err != nil {
			t.Fatalf("Receive: %v", err)
		}
		if out.Err != nil {
			t.Fatalf("unexpected protocol error: %v", out.Err)
		}
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
	if srv.State() != StateWaiting || peer.State() != StateWaiting {
		t.Fatalf("after Initial: server=%s peer=%s, want WaitingForOOB", srv.State(), peer.State())
	}

	// OOB Step in the negotiated direction.
	switch dir {
	case 1: // peer-to-server
		msg, err := peer.OOBOutput()
		if err != nil {
			t.Fatalf("peer OOBOutput: %v", err)
		}
		if err := srv.OOBInput(msg); err != nil {
			t.Fatalf("server OOBInput: %v", err)
		}
		if srv.State() != StateOOBReceived {
			t.Fatalf("server state %s, want OOBReceived", srv.State())
		}
	case 2: // server-to-peer
		msg, err := srv.OOBOutput()
		if err != nil {
			t.Fatalf("server OOBOutput: %v", err)
		}
		if err := peer.OOBInput(msg); err != nil {
			t.Fatalf("peer OOBInput: %v", err)
		}
		if peer.State() != StateOOBReceived {
			t.Fatalf("peer state %s, want OOBReceived", peer.State())
		}
	}

	// Completion Exchange.
	driveConversation(t, srv, peer)
	if srv.State() != StateRegistered || peer.State() != StateRegistered {
		t.Fatalf("after Completion: server=%s peer=%s, want Registered", srv.State(), peer.State())
	}
	return srv, peer
}

func TestPairingAllSuitesAndDirections(t *testing.T) {
	for _, suite := range []int{1, 2} {
		for _, dir := range []int{1, 2} {
			suite, dir := suite, dir
			t.Run("", func(t *testing.T) {
				srv, peer := pair(t, suite, dir)

				if srv.Association().Cryptosuitep != suite {
					t.Fatalf("server negotiated cryptosuite %d, want %d", srv.Association().Cryptosuitep, suite)
				}
				if !bytes.Equal(srv.Association().Kz, peer.Association().Kz) {
					t.Fatalf("Kz mismatch between server and peer")
				}
				if srv.Association().PeerId != peer.Association().PeerId {
					t.Fatalf("PeerId mismatch: %q vs %q", srv.Association().PeerId, peer.Association().PeerId)
				}

				// The exported secret must agree for several lengths and differ
				// across labels/contexts.
				for _, n := range []int{1, 16, 32, 100, 1000} {
					sSecret, err := srv.Export("app", nil, n)
					if err != nil {
						t.Fatalf("server Export(%d): %v", n, err)
					}
					pSecret, err := peer.Export("app", nil, n)
					if err != nil {
						t.Fatalf("peer Export(%d): %v", n, err)
					}
					if len(sSecret) != n {
						t.Fatalf("export length %d, want %d", len(sSecret), n)
					}
					if !bytes.Equal(sSecret, pSecret) {
						t.Fatalf("exported secret mismatch at length %d", n)
					}
				}

				a, _ := srv.Export("label-a", nil, 32)
				b, _ := srv.Export("label-b", nil, 32)
				if bytes.Equal(a, b) {
					t.Fatalf("different labels produced identical secrets")
				}
				c1, _ := srv.Export("app", []byte("ctx1"), 32)
				c2, _ := srv.Export("app", []byte("ctx2"), 32)
				if bytes.Equal(c1, c2) {
					t.Fatalf("different contexts produced identical secrets")
				}
			})
		}
	}
}

func TestExportBeforeRegistrationFails(t *testing.T) {
	peer := NewPeer(PeerConfig{}, nil)
	if _, err := peer.Export("app", nil, 16); err != ErrNotRegistered {
		t.Fatalf("Export before registration: got %v, want ErrNotRegistered", err)
	}
}

func TestTamperedMACFails(t *testing.T) {
	srv := NewServer(ServerConfig{Cryptosuites: []int{1}, Dirs: 1}, nil)
	peer := NewPeer(PeerConfig{PreferDir: 1}, nil)

	driveConversation(t, srv, peer)
	oob, err := peer.OOBOutput()
	if err != nil {
		t.Fatalf("OOBOutput: %v", err)
	}
	if err := srv.OOBInput(oob); err != nil {
		t.Fatalf("OOBInput: %v", err)
	}

	// Run the Completion handshake manually and corrupt MACp before the server
	// verifies it.
	req1, _ := srv.Start()
	resp1, err := peer.Receive(req1) // Type=1 response
	if err != nil {
		t.Fatalf("peer discovery: %v", err)
	}
	req6, err := srv.Receive(resp1.Send) // Type=6 request (MACs)
	if err != nil {
		t.Fatalf("server completion request: %v", err)
	}
	resp6, err := peer.Receive(req6.Send) // Type=6 response (MACp)
	if err != nil {
		t.Fatalf("peer completion: %v", err)
	}

	// Replace MACp with a valid-length (32-byte) but incorrect value so the
	// failure is the HMAC check rather than a structural error.
	wm, err := decode(resp6.Send)
	if err != nil {
		t.Fatalf("decode completion response: %v", err)
	}
	wm.MACp = rawString(b64Encode(make([]byte, 32)))
	tampered, _, err := encode(wm)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	out, err := srv.Receive(tampered)
	if err != nil {
		t.Fatalf("server receive tampered: %v", err)
	}
	if out.Err == nil || out.Err.Code != ErrHMACVerificationFailed {
		t.Fatalf("expected HMAC verification failure, got %+v", out.Err)
	}
}

func TestRejectBadOOBFingerprint(t *testing.T) {
	srv := NewServer(ServerConfig{Cryptosuites: []int{1}, Dirs: 2}, nil)
	peer := NewPeer(PeerConfig{PreferDir: 2}, nil)
	driveConversation(t, srv, peer)

	oob, err := srv.OOBOutput()
	if err != nil {
		t.Fatalf("OOBOutput: %v", err)
	}
	oob.Hoob = "AAAAAAAAAAAAAAAAAAAAAA" // wrong 16-byte fingerprint
	if err := peer.OOBInput(oob); err == nil {
		t.Fatalf("expected OOBInput to reject bad fingerprint")
	}
	if peer.State() != StateWaiting {
		t.Fatalf("peer state %s after rejected OOB, want WaitingForOOB", peer.State())
	}
}

func TestOneStepKDFDeterministic(t *testing.T) {
	z := bytes.Repeat([]byte{0x01}, 32)
	np := bytes.Repeat([]byte{0x02}, 32)
	ns := bytes.Repeat([]byte{0x03}, 32)
	noob := bytes.Repeat([]byte{0x04}, 16)

	km1 := deriveCompletion(z, np, ns, noob)
	km2 := deriveCompletion(z, np, ns, noob)

	if !bytes.Equal(km1.MSK, km2.MSK) || !bytes.Equal(km1.Kz, km2.Kz) {
		t.Fatalf("KDF not deterministic")
	}
	lengths := map[string]int{"MSK": len(km1.MSK), "EMSK": len(km1.EMSK), "AMSK": len(km1.AMSK), "MethodID": len(km1.MethodID), "Kms": len(km1.Kms), "Kmp": len(km1.Kmp), "Kz": len(km1.Kz)}
	want := map[string]int{"MSK": 64, "EMSK": 64, "AMSK": 64, "MethodID": 32, "Kms": 32, "Kmp": 32, "Kz": 32}
	for k, v := range want {
		if lengths[k] != v {
			t.Fatalf("%s length %d, want %d", k, lengths[k], v)
		}
	}
}

func TestJWKRoundTrip(t *testing.T) {
	for _, id := range []int{1, 2} {
		cs, err := suiteByID(id)
		if err != nil {
			t.Fatal(err)
		}
		priv, jwk, err := cs.generateKeypair()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		pub, err := cs.decodeJWK(jwk)
		if err != nil {
			t.Fatalf("decode JWK: %v", err)
		}
		if !bytes.Equal(pub.Bytes(), priv.PublicKey().Bytes()) {
			t.Fatalf("suite %d: JWK round-trip mismatch", id)
		}
	}
}
