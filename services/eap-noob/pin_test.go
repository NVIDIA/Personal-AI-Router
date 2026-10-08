// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eapnoob

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	assert.Equal(t, StateWaiting, srv.State(), "after Initial: server=%s peer=%s, want WaitingForOOB", srv.State(), peer.State())
	assert.Equal(t, StateWaiting, peer.State(), "after Initial: server=%s peer=%s, want WaitingForOOB", srv.State(), peer.State())

	noob := bytes.Repeat([]byte{0xAB}, 16)
	{
		_, err := srv.OOBOutputWith(noob)
		require.NoError(t, err, "server OOBOutputWith:")
	}
	require.NoError(t, peer.OOBInputNoob(noob), "peer OOBInputNoob:")
	assert.Equal(t, StateOOBReceived, peer.State(), "peer state %s, want OOBReceived", peer.State())

	driveConversation(t, srv, peer)
	assert.Equal(t, StateRegistered, srv.State(), "after Completion: server=%s peer=%s, want Registered", srv.State(), peer.State())
	assert.Equal(t, StateRegistered, peer.State(), "after Completion: server=%s peer=%s, want Registered", srv.State(), peer.State())
	assert.Equal(t, peer.Association().Kz, srv.Association().Kz, "Kz mismatch between server and peer")
	s, err := srv.Export("cluster-mtls", nil, 32)
	require.NoError(t, err, "server Export:")
	p, err := peer.Export("cluster-mtls", nil, 32)
	require.NoError(t, err, "peer Export:")
	assert.Equal(t, p, s, "exported secret mismatch")
}

// TestMismatchedInjectedNoobFails is the wrong-PIN case: the two sides inject
// different Noobs, so the Completion MACs cannot agree and pairing must fail.
func TestMismatchedInjectedNoobFails(t *testing.T) {
	srv := NewServer(ServerConfig{Dirs: 2}, nil)
	peer := NewPeer(PeerConfig{PreferDir: 2}, nil)

	driveConversation(t, srv, peer)

	{
		_, err := srv.OOBOutputWith(bytes.Repeat([]byte{0x01}, 16))
		require.NoError(t, err, "server OOBOutputWith:")
	}
	require.NoError(t, peer.OOBInputNoob(bytes.Repeat([]byte{0x02}, 16)), "peer OOBInputNoob:")

	assert.True(t, driveAllowFail(srv, peer), "expected the Completion Exchange to fail with mismatched Noobs")
	assert.NotEqual(t, StateRegistered, peer.State(), "a side reached Registered despite mismatched Noobs")
	assert.NotEqual(t, StateRegistered, srv.State(), "a side reached Registered despite mismatched Noobs")
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
	{
		_, err := srv.OOBOutputWith(bytes.Repeat([]byte{0x01}, 16))
		require.NoError(t, err, "server OOBOutputWith:")
	}
	require.NoError(t, peer.OOBInputNoob(bytes.Repeat([]byte{0x02}, 16)), "peer OOBInputNoob:")

	// Drive the Completion Exchange exactly as the joiner does: the server
	// kicks off (Start), and the peer receives each blob as the HTTP client.
	msg, err := srv.Start()
	require.NoError(t, err, "server start completion:")
	var peerOut Outcome
	for {
		assert.NotEmpty(t, msg, "server ended completion unexpectedly before the peer went terminal")
		out, rerr := peer.Receive(msg)
		// A wrong PIN must NOT come back as the returned error — the classifier
		// only inspects out.Err.
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
}

// TestOOBOutputWithRejectsBadLength guards the 16-byte contract.
func TestOOBOutputWithRejectsBadLength(t *testing.T) {
	srv := NewServer(ServerConfig{Dirs: 2}, nil)
	peer := NewPeer(PeerConfig{PreferDir: 2}, nil)
	driveConversation(t, srv, peer)
	{
		_, err := srv.OOBOutputWith(bytes.Repeat([]byte{0x01}, 8))
		require.Error(t, err, "expected OOBOutputWith to reject an 8-byte Noob")
	}
}
