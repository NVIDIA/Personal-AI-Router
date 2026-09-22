// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"time"

	"nvpair-tui/rpc"

	tea "github.com/charmbracelet/bubbletea"
)

// callTimeout bounds ordinary broker requests, not long engine mutations.
const callTimeout = 35 * time.Second

// Engine Manager allows 30 minutes for acquisition and model operations.
// Keep its terminal response observable, with a little relay headroom.
const engineOperationTimeout = 31 * time.Minute

// NotificationMsg carries one broker server-push frame into the Bubble
// Tea update loop. Every view receives it.
type NotificationMsg struct{ Msg *rpc.Message }

// DisconnectedMsg is emitted once the broker's notification stream closes
// (broker exited or the connection dropped).
type DisconnectedMsg struct{}

// LogLineMsg carries one line of the broker's (and its workers') stderr
// into the update loop for the Logs view.
type LogLineMsg struct{ Line string }

// LogClosedMsg is emitted once the broker's stderr stream ends.
type LogClosedMsg struct{}

// waitForLog blocks on the next captured stderr line, re-arming itself
// after each one. A closed channel yields LogClosedMsg.
func waitForLog(lines <-chan string) tea.Cmd {
	return func() tea.Msg {
		line, ok := <-lines
		if !ok {
			return LogClosedMsg{}
		}
		return LogLineMsg{Line: line}
	}
}

// waitForNotification blocks on the next broker push and delivers it as a
// NotificationMsg, re-arming itself after each one (the model returns this
// command again from Update). A closed channel yields DisconnectedMsg.
func waitForNotification(client *rpc.Client) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-client.Notifications()
		if !ok {
			return DisconnectedMsg{}
		}
		return NotificationMsg{Msg: msg}
	}
}

// call issues a broker request on a background goroutine and feeds the
// outcome back into the update loop via decode, which maps the response
// (or error) to a view-specific message.
func call(client *rpc.Client, method string, params any, decode func(*rpc.Message, error) tea.Msg, budget ...time.Duration) tea.Cmd {
	timeout := callTimeout
	if len(budget) > 0 {
		timeout = budget[0]
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		msg, err := client.Call(ctx, method, params)
		return decode(msg, err)
	}
}
