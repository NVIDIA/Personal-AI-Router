// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"strings"
	"testing"

	"nvpair-tui/pairing"
)

// A request answered over the control socket must leave the Nodes tab, or the
// prompt goes on offering to answer an invite that is already settled.
func TestNodesView_RequestAnsweredFromTheCommandLineLeavesTheScreen(t *testing.T) {
	v := newNodesView(nil, pairing.NewService(nil))
	v.inbound = &clusterInvite{InviteID: "inv-1", FromNodeName: "gpu-box"}
	v.queued = []clusterInvite{{InviteID: "inv-2", FromNodeName: "laptop"}}

	v.applyPairingEvent(pairing.Event{
		Kind:   pairing.EventResponded,
		Accept: true,
		Invite: pairing.Invite{InviteID: "inv-1", State: inviteStatePaired},
	})

	if v.inbound == nil || v.inbound.InviteID != "inv-2" {
		t.Fatalf("inbound = %+v, want the queued request inv-2 brought forward", v.inbound)
	}
	if !strings.Contains(v.status.text, "paired with gpu-box from the command line") {
		t.Errorf("status = %q, want it to say the pairing happened from the command line", v.status.text)
	}

	// A queued request answered elsewhere is dropped without touching the one
	// on screen.
	v.queued = []clusterInvite{{InviteID: "inv-3"}}
	v.applyPairingEvent(pairing.Event{Kind: pairing.EventResponded, Invite: pairing.Invite{InviteID: "inv-3"}})
	if len(v.queued) != 0 || v.inbound.InviteID != "inv-2" {
		t.Errorf("queued = %+v, inbound = %+v; want inv-3 dropped and inv-2 kept", v.queued, v.inbound)
	}
}

// The service reports this view's own answers back too. Those settle through
// pairingResultMsg, so the event must not drop the request a second time.
func TestNodesView_OwnAnswerIsNotHandledTwice(t *testing.T) {
	v := newNodesView(nil, pairing.NewService(nil))
	v.inbound = &clusterInvite{InviteID: "inv-1", FromNodeName: "gpu-box"}
	v.answeredHere["inv-1"] = true

	v.applyPairingEvent(pairing.Event{Kind: pairing.EventResponded, Invite: pairing.Invite{InviteID: "inv-1"}})

	if v.inbound == nil || v.inbound.InviteID != "inv-1" {
		t.Fatalf("inbound = %+v, want inv-1 left for pairingResultMsg to settle", v.inbound)
	}
	if v.answeredHere["inv-1"] {
		t.Error("answeredHere still holds inv-1 after its event was consumed")
	}
}
