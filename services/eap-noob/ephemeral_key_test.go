// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eapnoob

import (
	"bytes"
	"testing"
)

// TestEphemeralKeyLifecycle covers the export the cluster manager's signal MAC
// derives from: it must error before a Key Exchange ran, agree between server
// and peer after the Initial Exchange, and hand out a copy so a caller cannot
// corrupt the live secret.
func TestEphemeralKeyLifecycle(t *testing.T) {
	srv := NewServer(ServerConfig{Dirs: 2, ServerInfo: map[string]any{"role": "server"}}, nil)
	peer := NewPeer(PeerConfig{PreferDir: 2, PeerInfo: map[string]any{"role": "peer"}}, nil)

	if _, err := srv.EphemeralKey(); err == nil {
		t.Fatal("server EphemeralKey succeeded before any Key Exchange")
	}
	if _, err := peer.EphemeralKey(); err == nil {
		t.Fatal("peer EphemeralKey succeeded before any Key Exchange")
	}

	driveConversation(t, srv, peer)
	if srv.State() != StateWaiting || peer.State() != StateWaiting {
		t.Fatalf("after Initial: server=%s peer=%s, want waiting on both", srv.State(), peer.State())
	}

	srvKey, err := srv.EphemeralKey()
	if err != nil {
		t.Fatalf("server EphemeralKey after Initial Exchange: %v", err)
	}
	peerKey, err := peer.EphemeralKey()
	if err != nil {
		t.Fatalf("peer EphemeralKey after Initial Exchange: %v", err)
	}
	if len(srvKey) == 0 {
		t.Fatal("server key is empty after a Key Exchange")
	}
	if !bytes.Equal(srvKey, peerKey) {
		t.Fatal("server and peer ephemeral keys differ after the same Initial Exchange")
	}

	srvKey[0] ^= 0xFF
	again, err := srv.EphemeralKey()
	if err != nil {
		t.Fatalf("server EphemeralKey re-read: %v", err)
	}
	if !bytes.Equal(again, peerKey) {
		t.Fatal("mutating a returned key changed the server's secret; EphemeralKey must return a copy")
	}
}
