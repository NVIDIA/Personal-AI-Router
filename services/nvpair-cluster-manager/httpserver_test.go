// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"eapnoob"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Skipping negotiation previously panicked while holding the session lock,
// leaving a session that expiry could never reclaim.
func TestPairingInitial_RejectsKeyExchangeBeforeNegotiation(t *testing.T) {
	m := testManagerAt(t, t.TempDir(), 15211)
	env := &pairingEnvelope{InviteID: "skipped-negotiation", Phase: "initial"}
	discovery := httptest.NewRecorder()
	m.handlePairingInitial(discovery, env, []byte(`{"Type":1}`), "127.0.0.1")
	require.Equal(t, http.StatusOK, discovery.Code)
	_, exists := m.getSession(env.InviteID)
	require.True(t, exists)

	response := httptest.NewRecorder()
	m.handlePairingInitial(response, env, []byte(`{"Type":3,"PKs":{},"Ns":""}`), "127.0.0.1")

	require.Equal(t, http.StatusOK, response.Code)
	var reply pairingEnvelope
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &reply))
	message, err := base64.StdEncoding.DecodeString(reply.Msg)
	require.NoError(t, err)
	var failure struct {
		ErrorCode int `json:"ErrorCode"`
	}
	require.NoError(t, json.Unmarshal(message, &failure))
	assert.Equal(t, eapnoob.ErrStateMismatch, failure.ErrorCode)
	_, exists = m.getSession(env.InviteID)
	assert.False(t, exists, "a terminal protocol error must release the pending session slot")
}

func postPairingInitial(t *testing.T, m *Manager, inviteID string, blob []byte) []byte {
	t.Helper()
	response := httptest.NewRecorder()
	m.handlePairingInitial(response, &pairingEnvelope{InviteID: inviteID, Phase: "initial"}, blob, "127.0.0.1")
	require.Equal(t, http.StatusOK, response.Code)
	var reply pairingEnvelope
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &reply))
	message, err := base64.StdEncoding.DecodeString(reply.Msg)
	require.NoError(t, err)
	return message
}

// An invite published at Waiting must stay answerable: deleting its session
// would leave a pending invite that can be neither accepted, declined nor canceled.
func TestPairingInitial_KeepsPublishedSessionOnProtocolError(t *testing.T) {
	cases := []struct {
		name string
		blob []byte
	}{
		{name: "malformed JSON", blob: []byte("not json")},
		{name: "error message", blob: []byte(`{"Type":0,"ErrorCode":1001,"ErrorInfo":"x"}`)},
		{name: "completion message", blob: []byte(`{"Type":6}`)},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inviter := testManagerAt(t, t.TempDir(), 15220+2*i)
			activateTestCluster(t, inviter, "cid-1")
			joiner := testManagerAt(t, t.TempDir(), 15221+2*i)
			server := eapnoob.NewServer(eapnoob.ServerConfig{
				Dirs:       eapnoob.OOBServerToPeer,
				ServerInfo: inviter.localPairingInfo("127.0.0.1:15220"),
			}, nil)
			const inviteID = "published-invite"

			blob, err := server.Start()
			require.NoError(t, err)
			for len(blob) > 0 {
				reply := postPairingInitial(t, joiner, inviteID, blob)
				if len(reply) == 0 {
					break
				}
				out, err := server.Receive(reply)
				require.NoError(t, err)
				blob = out.Send
			}
			require.Equal(t, eapnoob.StateWaiting, server.State())
			invite, ok := joiner.getInvite(inviteID)
			require.True(t, ok)
			require.Equal(t, inviteStatePending, invite.State)

			postPairingInitial(t, joiner, inviteID, tc.blob)

			_, exists := joiner.getSession(inviteID)
			assert.True(t, exists, "a published invite must keep its session")
			invite, ok = joiner.getInvite(inviteID)
			require.True(t, ok)
			assert.Equal(t, inviteStatePending, invite.State)
		})
	}
}
