// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eapnoob

import (
	"bytes"
	"testing"
)

// driveAllowFail relays one EAP conversation, returning true if either side
// produced a protocol error (e.g. a Completion MAC mismatch) rather than t-Fatal.
func driveAllowFail(srv *Server, peer *Peer) bool {
	msg, err := srv.Start()
	if err != nil {
		return true
	}
	peerTurn := true
	for {
		var out Outcome
		if peerTurn {
			out, err = peer.Receive(msg)
		} else {
			out, err = srv.Receive(msg)
		}
		if err != nil || out.Err != nil {
			return true
		}
		if len(out.Send) == 0 {
			return false
		}
		msg = out.Send
		peerTurn = !peerTurn
	}
}

// TestCallerInjectedNoobPairs drives a full pairing in the server-to-peer
// direction using the caller-injected-Noob API (the path the cluster manager's
// PIN flow takes): the server supplies the Noob via OOBOutputWith and the peer
// records the same Noob via OOBInputNoob, with no OOBMessage/Hoob envelope.
func TestCallerInjectedNoobPairs(t *testing.T) {
	srv := NewServer(ServerConfig{Dirs: 2, ServerInfo: map[string]any{"role": "server"}}, nil)
	peer := NewPeer(PeerConfig{PreferDir: 2, PeerInfo: map[string]any{"role": "peer"}}, nil)

	driveConversation(t, srv, peer)
	if srv.State() != StateWaiting || peer.State() != StateWaiting {
		t.Fatalf("after Initial: server=%s peer=%s, want WaitingForOOB", srv.State(), peer.State())
	}

	noob := bytes.Repeat([]byte{0xAB}, 16)
	if _, err := srv.OOBOutputWith(noob); err != nil {
		t.Fatalf("server OOBOutputWith: %v", err)
	}
	if err := peer.OOBInputNoob(noob); err != nil {
		t.Fatalf("peer OOBInputNoob: %v", err)
	}
	if peer.State() != StateOOBReceived {
		t.Fatalf("peer state %s, want OOBReceived", peer.State())
	}

	driveConversation(t, srv, peer)
	if srv.State() != StateRegistered || peer.State() != StateRegistered {
		t.Fatalf("after Completion: server=%s peer=%s, want Registered", srv.State(), peer.State())
	}
	if !bytes.Equal(srv.Association().Kz, peer.Association().Kz) {
		t.Fatal("Kz mismatch between server and peer")
	}
	s, err := srv.Export("cluster-mtls", nil, 32)
	if err != nil {
		t.Fatalf("server Export: %v", err)
	}
	p, err := peer.Export("cluster-mtls", nil, 32)
	if err != nil {
		t.Fatalf("peer Export: %v", err)
	}
	if !bytes.Equal(s, p) {
		t.Fatal("exported secret mismatch")
	}
}

// TestMismatchedInjectedNoobFails is the wrong-PIN case: the two sides inject
// different Noobs, so the Completion MACs cannot agree and pairing must fail.
func TestMismatchedInjectedNoobFails(t *testing.T) {
	srv := NewServer(ServerConfig{Dirs: 2}, nil)
	peer := NewPeer(PeerConfig{PreferDir: 2}, nil)

	driveConversation(t, srv, peer)

	if _, err := srv.OOBOutputWith(bytes.Repeat([]byte{0x01}, 16)); err != nil {
		t.Fatalf("server OOBOutputWith: %v", err)
	}
	if err := peer.OOBInputNoob(bytes.Repeat([]byte{0x02}, 16)); err != nil {
		t.Fatalf("peer OOBInputNoob: %v", err)
	}

	if !driveAllowFail(srv, peer) {
		t.Fatal("expected the Completion Exchange to fail with mismatched Noobs")
	}
	if peer.State() == StateRegistered || srv.State() == StateRegistered {
		t.Fatal("a side reached Registered despite mismatched Noobs")
	}
}

// TestPeerWrongPinYieldsProtocolError pins the exact contract the cluster
// manager's wrong-PIN classification depends on (nvpair-cluster-manager
// runCompletionExchange): when the joiner (Peer) drives the Completion Exchange
// against a server whose Noob differs (a wrong PIN), the Peer must surface the
// failure as a terminal Outcome carrying a *ProtocolError in out.Err — NOT as
// the returned error and NOT as a silent EAP-Failure (out.Err == nil) — with a
// code the classifier recognizes (2003 unrecognized NoobId, checked first, or
// 4001 MAC mismatch). If a future change moved detection so the Peer no longer
// reports it this way, the cluster manager would silently degrade a wrong PIN to
// a generic failure (empty reason) instead of reason:"incorrect-pin".
func TestPeerWrongPinYieldsProtocolError(t *testing.T) {
	srv := NewServer(ServerConfig{Dirs: 2}, nil)
	peer := NewPeer(PeerConfig{PreferDir: 2}, nil)

	// Initial Exchange.
	driveConversation(t, srv, peer)

	// Server-to-peer OOB with MISMATCHED Noobs — the wrong-PIN condition.
	if _, err := srv.OOBOutputWith(bytes.Repeat([]byte{0x01}, 16)); err != nil {
		t.Fatalf("server OOBOutputWith: %v", err)
	}
	if err := peer.OOBInputNoob(bytes.Repeat([]byte{0x02}, 16)); err != nil {
		t.Fatalf("peer OOBInputNoob: %v", err)
	}

	// Drive the Completion Exchange exactly as the joiner does: the server
	// kicks off (Start), and the peer receives each blob as the HTTP client.
	msg, err := srv.Start()
	if err != nil {
		t.Fatalf("server start completion: %v", err)
	}
	var peerOut Outcome
	for {
		if len(msg) == 0 {
			t.Fatal("server ended completion unexpectedly before the peer went terminal")
		}
		out, rerr := peer.Receive(msg)
		// A wrong PIN must NOT come back as the returned error — the classifier
		// only inspects out.Err.
		if rerr != nil {
			t.Fatalf("peer.Receive returned err %v; a wrong PIN must surface in out.Err, not the returned error", rerr)
		}
		if out.Done {
			peerOut = out
			break
		}
		sout, serr := srv.Receive(out.Send)
		if serr != nil {
			t.Fatalf("server.Receive: %v", serr)
		}
		msg = sout.Send
	}

	if peerOut.Err == nil {
		t.Fatal("peer completion produced no ProtocolError on a wrong PIN; the classifier would degrade the reason to empty")
	}
	if peerOut.Err.Code != ErrUnrecognizedOOBMsgID && peerOut.Err.Code != ErrHMACVerificationFailed {
		t.Fatalf("peer ProtocolError code = %d, want %d (unrecognized NoobId) or %d (MAC mismatch)",
			peerOut.Err.Code, ErrUnrecognizedOOBMsgID, ErrHMACVerificationFailed)
	}
	if peer.State() == StateRegistered {
		t.Fatal("peer reached Registered despite a wrong PIN")
	}
}

// TestOOBOutputWithRejectsBadLength guards the 16-byte contract.
func TestOOBOutputWithRejectsBadLength(t *testing.T) {
	srv := NewServer(ServerConfig{Dirs: 2}, nil)
	peer := NewPeer(PeerConfig{PreferDir: 2}, nil)
	driveConversation(t, srv, peer)
	if _, err := srv.OOBOutputWith(bytes.Repeat([]byte{0x01}, 8)); err == nil {
		t.Fatal("expected OOBOutputWith to reject an 8-byte Noob")
	}
}
