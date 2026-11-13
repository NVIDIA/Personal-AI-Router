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
