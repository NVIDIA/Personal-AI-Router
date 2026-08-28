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
	State  string  `json:"state"`
	Pin    *string `json:"pin"`
	Reason string  `json:"reason"`
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
