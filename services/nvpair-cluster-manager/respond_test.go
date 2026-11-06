// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
