// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"nvpair-tui/rpc"

	tea "github.com/charmbracelet/bubbletea"
)

// inviteNodeResult is the decoded cluster:invite-node result the UI acts on: a
// PIN to display on success, or an explicit rejection (e.g. the target is
// already clustered) carrying its reason. No PIN accompanies a rejection.
type inviteNodeResult struct {
	// InviteID identifies this pairing session. The manager mints one per
	// invite and stamps it on every terminal notification, so it is what lets a
	// view tell its own invite's outcome from a concurrent one's.
	InviteID string  `json:"inviteId"`
	State    string  `json:"state"`
	Pin      *string `json:"pin"`
	Reason   string  `json:"reason"`
}

// inviteResolution is how the UI reports an invite that is no longer pending.
type inviteResolution struct {
	kind  toastKind
	label string
}

// inviteRef is the invite a terminal notification refers to. The manager
// supports concurrent pairings, so an event has to be matched against the
// session it belongs to; acting on the method alone let an outbound decline
// erase an unrelated inbound PIN prompt.
type inviteRef struct {
	InviteID string `json:"inviteId"`
}

// inviteOutcome maps a cluster-manager terminal invite event to its report.
// These notifications are the only signal that a sent invite has stopped being
// pending — the synchronous cluster:invite-node result only covers the handoff —
// so a view that displays a PIN must consume them or leave a dead invite on
// screen looking live.
func inviteOutcome(method string) (inviteResolution, bool) {
	switch method {
	case "cluster:invite-declined":
		return inviteResolution{kind: toastError, label: "declined by the other node"}, true
	case "cluster:invite-expired":
		return inviteResolution{kind: toastError, label: "expired before it was accepted"}, true
	case "cluster:invite-canceled":
		return inviteResolution{kind: toastInfo, label: "canceled"}, true
	case "cluster:invite-failed":
		return inviteResolution{kind: toastError, label: "failed - check the Logs tab"}, true
	default:
		return inviteResolution{}, false
	}
}

// inviteNodeCmd issues a single cluster:invite-node request and maps the
// decoded result (or error) into the caller's view message (shared by the
// Cluster and Nodes tabs so the decode lives in one place).
//
// There is no separate "create cluster" step: the backend auto-founds a
// cluster of one when this node isn't clustered yet, so the invite is
// the one authoritative call and the UI carries no membership orchestration.
func inviteNodeCmd(client *rpc.Client, params map[string]any, finish func(res inviteNodeResult, err error) tea.Msg) tea.Cmd {
	return call(client, "cluster:invite-node", params, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return finish(inviteNodeResult{}, err)
		}
		var r inviteNodeResult
		_ = decodeParams(msg.Result, &r)
		return finish(r, nil)
	})
}
