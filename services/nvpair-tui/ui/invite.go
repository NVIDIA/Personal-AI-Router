// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"

	"nvpair-tui/pairing"

	tea "github.com/charmbracelet/bubbletea"
)

// inviteCmd starts one outbound pairing through the shared pairing service —
// the same call the control socket's pair:invite makes, so the Cluster tab and
// a script cannot diverge. It maps the outcome into the caller's view message
// (the Cluster and Nodes tabs each word their status line differently).
//
// There is no separate "create cluster" step: the backend auto-founds a
// cluster of one when this node isn't clustered yet, so the invite is
// the one authoritative call and the UI carries no membership orchestration.
//
// The Cluster tab's status line is driven by the service's event stream rather
// than by this result, so an invite started from the control socket lands
// there exactly like one started with `i`.
func inviteCmd(svc *pairing.Service, req pairing.InviteRequest, finish func(inv pairing.Invite, err error) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		res, err := svc.Invite(ctx, req)
		if err != nil {
			return finish(pairing.Invite{}, err)
		}
		return finish(res.Invite, nil)
	}
}

// respondCmd answers an inbound invite through the shared pairing service.
// inviteID may be empty, in which case the service resolves the single pending
// invite or reports why it could not.
//
// Nothing is returned into the update loop: the outcome, success or failure,
// reaches the view as a pairing event, which is also how an accept made over
// the control socket clears this tab's prompt.
func respondCmd(svc *pairing.Service, inviteID string, accept bool, pin string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		_, _ = svc.Respond(ctx, inviteID, accept, pin)
		return nil
	}
}

// waitForPairingEvent blocks on the next pairing event and re-arms itself, the
// same shape as waitForNotification. A closed channel ends the pump.
func waitForPairingEvent(events <-chan pairing.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-events
		if !ok {
			return nil
		}
		return pairingEventMsg{Event: ev}
	}
}

// pairingEventMsg carries one pairing state change into the update loop,
// whether this TUI's keyboard or the control socket caused it.
type pairingEventMsg struct{ Event pairing.Event }
