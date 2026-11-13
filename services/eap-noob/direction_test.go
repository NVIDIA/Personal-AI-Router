// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eapnoob

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPairing_ExportsMatchingSecretsInEitherOOBDirection(t *testing.T) {
	test := func(name string, suite cryptosuiteID, direction OOBDirection) {
		t.Run(name, func(t *testing.T) {
			server := NewServer(ServerConfig{Dirs: direction}, nil)
			peer := NewPeer(PeerConfig{}, nil)
			driveInitialWithSuite(t, server, peer, suite)
			require.Equal(t, StateWaiting, server.State())
			require.Equal(t, StateWaiting, peer.State())
			if direction == OOBPeerToServer {
				oob, err := peer.OOBOutput()
				require.NoError(t, err)
				require.NoError(t, server.OOBInput(oob))
				require.Equal(t, StateOOBReceived, server.State())
				require.Equal(t, StateWaiting, peer.State())
			} else {
				oob, err := server.OOBOutput()
				require.NoError(t, err)
				require.NoError(t, peer.OOBInput(oob))
				require.Equal(t, StateWaiting, server.State())
				require.Equal(t, StateOOBReceived, peer.State())
			}

			out := driveConversation(t, server, peer)

			require.Nil(t, out.Err)
			require.True(t, out.Success)
			require.Equal(t, StateRegistered, server.State())
			require.Equal(t, StateRegistered, peer.State())
			serverSecret, err := server.Export("application", nil, 48)
			require.NoError(t, err)
			peerSecret, err := peer.Export("application", nil, 48)
			require.NoError(t, err)
			assert.Len(t, serverSecret, 48)
			assert.Equal(t, serverSecret, peerSecret)
			require.NotNil(t, server.Association())
			require.NotNil(t, peer.Association())
			assert.Equal(t, int(suite), server.Association().Cryptosuitep)
			assert.Equal(t, server.Association(), peer.Association())
		})
	}
	test("X25519 peer to server", cryptosuiteX25519, OOBPeerToServer)
	test("X25519 server to peer", cryptosuiteX25519, OOBServerToPeer)
	test("P-256 peer to server", cryptosuiteP256, OOBPeerToServer)
	test("P-256 server to peer", cryptosuiteP256, OOBServerToPeer)
}

func TestPeerNegotiation_SelectsPreferredOOBDirectionWhenBothAreOffered(t *testing.T) {
	test := func(name string, preference, expected OOBDirection) {
		t.Run(name, func(t *testing.T) {
			server := NewServer(ServerConfig{Dirs: OOBBoth}, nil)
			peer := NewPeer(PeerConfig{PreferDir: preference}, nil)

			out, err := peer.Receive(negotiationRequest(t, server, peer))

			require.NoError(t, err)
			require.Nil(t, out.Err)
			selection, err := decode(out.Send)
			require.NoError(t, err)
			direction, err := rawToInt(selection.Dirp)
			require.NoError(t, err)
			assert.Equal(t, int(expected), direction)
		})
	}
	test("peer supplies the secret", OOBPeerToServer, OOBPeerToServer)
	test("server supplies the secret", OOBServerToPeer, OOBServerToPeer)
	test("default preference", 0, OOBServerToPeer)
}
