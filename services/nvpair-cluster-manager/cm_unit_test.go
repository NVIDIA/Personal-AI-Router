// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdentity_MintsAndReloadsStableCredentials(t *testing.T) {
	dir := t.TempDir()
	first, err := loadOrMintIdentity(dir)
	require.NoError(t, err, "mint")
	require.NotEqual(t, "", first.NodeUUID, "expected a non-empty UUID and fingerprint")
	require.NotEqual(t, "", first.CertFingerprint, "expected a non-empty UUID and fingerprint")

	second, err := loadOrMintIdentity(dir)
	require.NoError(t, err, "reload")
	require.Equal(t, first.NodeUUID, second.NodeUUID, "UUID changed across reload")
	require.Equal(t, first.CertFingerprint, second.CertFingerprint, "fingerprint changed across reload")
}

func TestIdentity_RejectsReloadWhenPrivateKeyIsMissing(t *testing.T) {
	dir := t.TempDir()
	_, err := loadOrMintIdentity(dir)
	require.NoError(t, err, "mint")
	require.NoError(t, os.Remove(filepath.Join(dir, "node.key")), "remove key")
	_, err = loadOrMintIdentity(dir)
	require.Error(t, err, "expected a loud failure when identity.json exists but the key is gone")
}

func TestPIN_EncodesExactNoobBytes(t *testing.T) {
	test := func(name, pin string, want [16]byte) {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want[:], noobFromPIN(pin))
		})
	}
	test("all zeroes", "000000", [16]byte{})
	test("leading zeroes", "000123", [16]byte{15: 0x7b})
	test("ordinary PIN", "402199", [16]byte{13: 0x06, 14: 0x23, 15: 0x17})
	test("maximum PIN", "999999", [16]byte{13: 0x0f, 14: 0x42, 15: 0x3f})
}

func TestGeneratePIN_ReturnsMatchingNoob(t *testing.T) {
	pin, noob, err := generatePIN()
	require.NoError(t, err, "generatePIN")
	assert.Regexp(t, pinPattern, pin)
	assert.Equal(t, noobFromPIN(pin), noob)
}

func TestRespondToInvite_RejectsMalformedPIN(t *testing.T) {
	test := func(name, pinJSON string) {
		t.Run(name, func(t *testing.T) {
			m := newTestManager(t)
			var response bytes.Buffer
			m.codec = NewCodec(&response)
			m.putInvite(&Invite{InviteID: "inv-pin", State: inviteStatePending})
			m.putSession(&pairingSession{inviteID: "inv-pin", role: roleJoiner})
			params, err := json.Marshal(struct {
				InviteID string          `json:"inviteId"`
				Accept   bool            `json:"accept"`
				Pin      json.RawMessage `json:"pin,omitempty"`
			}{InviteID: "inv-pin", Accept: true, Pin: json.RawMessage(pinJSON)})
			require.NoError(t, err)
			id := json.RawMessage("1")

			m.handleRespondToInvite(&Message{ID: &id, Params: params})

			var reply Message
			require.NoError(t, json.Unmarshal(response.Bytes(), &reply))
			require.NotNil(t, reply.Error)
			assert.Equal(t, codeInvalidParams, reply.Error.Code)
			var data struct {
				Field string `json:"field"`
			}
			require.NoError(t, json.Unmarshal(reply.Error.Data, &data))
			assert.Equal(t, "pin", data.Field)
			invite, ok := m.getInvite("inv-pin")
			require.True(t, ok)
			assert.Equal(t, inviteStatePending, invite.State)
			_, ok = m.getSession("inv-pin")
			assert.True(t, ok, "malformed PIN must leave the pending pairing available")
		})
	}
	test("missing PIN", "")
	test("null PIN", "null")
	test("empty PIN", `""`)
	test("five digits", `"00123"`)
	test("seven digits", `"0000123"`)
	test("alphabetic character", `"00012a"`)
	test("leading space", `" 00123"`)
	test("trailing space", `"00123 "`)
	test("positive sign", `"+00123"`)
	test("negative sign", `"-00123"`)
	test("decimal point", `"001.23"`)
	test("non-ASCII digits", `"０００１２３"`)
	test("trailing newline", `"00123\n"`)
}
