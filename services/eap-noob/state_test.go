// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eapnoob

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPeer_RejectsSuccessBeforeVerifiedCompletion(t *testing.T) {
	test := func(name string, state State) {
		t.Run(name, func(t *testing.T) {
			srv := NewServer(ServerConfig{Dirs: 2})
			peer := NewPeer(PeerConfig{PreferDir: 2})
			if state != StateUnregistered {
				driveConversation(t, srv, peer)
			}
			if state == StateOOBReceived {
				require.NoError(t, peer.OOBInputNoob(make([]byte, 16)))
			}
			require.Equal(t, state, peer.State())

			out, err := peer.Receive(resultBytes(eapSuccess))

			require.NoError(t, err)
			require.NotNil(t, out.Err)
			assert.Equal(t, ErrStateMismatch, out.Err.Code)
			assert.True(t, out.Done)
			assert.False(t, out.Success)
			assert.Equal(t, state, peer.State())
		})
	}
	test("before initial exchange", StateUnregistered)
	test("before OOB input", StateWaiting)
	test("after OOB input without Completion", StateOOBReceived)
}

func TestOOB_RejectsNoobOutsideWaiting(t *testing.T) {
	test := func(name string, registered bool) {
		t.Run(name, func(t *testing.T) {
			srv := NewServer(ServerConfig{Dirs: 2})
			peer := NewPeer(PeerConfig{PreferDir: 2})
			wantState := StateUnregistered
			if registered {
				srv, peer = pair(t, 2, 2)
				wantState = StateRegistered
			}

			assert.Error(t, srv.OOBOutputWith(make([]byte, 16)))
			assert.Error(t, peer.OOBInputNoob(make([]byte, 16)))
			assert.Equal(t, wantState, srv.State())
			assert.Equal(t, wantState, peer.State())
		})
	}
	test("before initial exchange", false)
	test("after successful Completion", true)
}
