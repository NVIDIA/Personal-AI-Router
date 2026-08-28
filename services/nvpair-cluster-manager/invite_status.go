// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import "encoding/json"

type inviteStatusParams struct {
	InviteID string `json:"inviteId"`
}

// handleInviteStatus returns the current Invite for an id (§7.0). The PIN is
// surfaced only on the inviter's still-pending session; everywhere else it is
// nulled. Unknown inviteId -> -32001.
func (m *Manager) handleInviteStatus(msg *Message) {
	var p inviteStatusParams
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		m.codec.RespondErrorData(msg.ID, codeInvalidParams, "invalid params: "+err.Error(), nil)
		return
	}
	if p.InviteID == "" {
		m.codec.RespondErrorData(msg.ID, codeInvalidParams, "inviteId is required", map[string]string{"field": "inviteId"})
		return
	}
	inv, ok := m.getInvite(p.InviteID)
	if !ok {
		m.codec.RespondErrorData(msg.ID, codeUnknownInvite, "unknown inviteId", map[string]string{"inviteId": p.InviteID})
		return
	}
	sess, hasSess := m.getSession(p.InviteID)
	if !(hasSess && sess.role == roleInviter && inv.State == inviteStatePending) {
		inv.Pin = nil
	}
	m.codec.Respond(msg.ID, inv)
}
