// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"eapnoob"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// joinerStub answers Initial Exchange messages with a real EAP-NOOB peer and
// calls onRequest before each answer, so a test can act while the inviter's
// exchange is still running.
func joinerStub(t *testing.T, onRequest func()) (addr string) {
	t.Helper()
	peer := eapnoob.NewPeer(eapnoob.PeerConfig{}, nil)
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc(pairingPath, func(w http.ResponseWriter, r *http.Request) {
		var env pairingEnvelope
		require.NoError(t, json.NewDecoder(r.Body).Decode(&env))
		msg, err := base64.StdEncoding.DecodeString(env.Msg)
		require.NoError(t, err)
		onRequest()
		mu.Lock()
		defer mu.Unlock()
		out, err := peer.Receive(msg)
		require.NoError(t, err)
		respondPairing(w, out.Send)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func postCompletion(m *Manager, inviteID string, msg []byte) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	m.handlePairingCompletion(response, &pairingEnvelope{InviteID: inviteID, Phase: "completion"}, msg, "127.0.0.1")
	return response
}

// The inviter registers its session and publishes the invite before its Initial
// Exchange runs, so a Completion message can arrive while the exchange still
// drives the same EAP-NOOB server. It must be refused until the exchange is done.
func TestPairingCompletion_RefusedUntilInitialExchangeFinishes(t *testing.T) {
	m := newTestManager(t)
	m.addSelfMember()
	cid, _ := m.clusterIdentity()
	const inviteID = "inv-completion-early"
	publishPendingInvite(t, m, inviteID)

	var duringExchange []int
	addr := joinerStub(t, func() {
		duringExchange = append(duringExchange,
			postCompletion(m, inviteID, nil).Code,
			postCompletion(m, inviteID, []byte(`{"Type":3,"PKp":{},"Np":""}`)).Code)
	})

	sess, pin, err := m.runInitialExchange(inviteID, addr, cid, m.sessGen.Load())

	require.NoError(t, err)
	require.NotEmpty(t, pin)
	require.NotEmpty(t, duringExchange)
	for _, code := range duringExchange {
		assert.Equal(t, http.StatusConflict, code, "Completion message accepted during the Initial Exchange")
	}
	assert.Equal(t, eapnoob.StateWaiting, sess.server.State())
	assert.Equal(t, http.StatusOK, postCompletion(m, inviteID, nil).Code, "Completion kickoff after the Initial Exchange")
}
