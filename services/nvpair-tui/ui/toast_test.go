// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"nvpair-tui/rpc"

	tea "github.com/charmbracelet/bubbletea"
)

// TestToastExpires is the regression guard for status lines that never went
// away: an outcome must stop rendering once its TTL has passed.
func TestToastExpires(t *testing.T) {
	var s toast
	s.ok("accept invite ok")
	if s.render() == "" {
		t.Fatal("a message just set should render")
	}

	s.at = time.Now().Add(-toastTTL - time.Second)
	if s.render() != "" {
		t.Error("message outlived its TTL and is still rendering")
	}
}

// TestToastErrorsOutliveSuccesses checks a failure stays readable for longer
// than a success, since the operator needs time to act on it.
func TestToastErrorsOutliveSuccesses(t *testing.T) {
	var ok, bad toast
	ok.ok("done")
	bad.error("it broke")

	aged := time.Now().Add(-toastTTL - time.Second)
	ok.at, bad.at = aged, aged

	if ok.render() != "" {
		t.Error("success should have expired by now")
	}
	if bad.render() == "" {
		t.Error("error expired at the success TTL; it should last longer")
	}

	bad.at = time.Now().Add(-toastErrorTTL - time.Second)
	if bad.render() != "" {
		t.Error("error outlived even the error TTL")
	}
}

// TestPinnedToastNeverExpires covers the pairing PIN. The operator reads it
// aloud to someone at another machine, so a timer must not remove it.
func TestPinnedToastNeverExpires(t *testing.T) {
	var s toast
	s.pin("invite sent - PIN 123456")
	s.at = time.Now().Add(-24 * time.Hour)

	if s.render() == "" {
		t.Error("pinned message expired; the PIN must stay until the invite resolves")
	}
	// It is replaced by the outcome, which is how a pinned message ends: the
	// views set a new one rather than clearing to nothing.
	s.ok("peer joined the cluster")
	if contains(s.render(), "123456") {
		t.Error("the PIN survived the message that replaced it")
	}
}

// TestSetReplacesPinned checks a later ordinary message drops the sticky flag,
// so a pinned PIN cannot make every subsequent message permanent.
func TestSetReplacesPinned(t *testing.T) {
	var s toast
	s.pin("invite sent - PIN 123456")
	s.info("inviting other-host...")

	s.at = time.Now().Add(-toastTTL - time.Second)
	if s.render() != "" {
		t.Error("message set after a pin inherited its stickiness")
	}
}

func TestInviteOutcome(t *testing.T) {
	resolved := []string{
		"cluster:invite-declined",
		"cluster:invite-expired",
		"cluster:invite-canceled",
		"cluster:invite-failed",
	}
	for _, method := range resolved {
		outcome, ok := inviteOutcome(method)
		if !ok {
			t.Errorf("%s is not recognised as a terminal invite event", method)
			continue
		}
		if outcome.label == "" {
			t.Errorf("%s has no operator-facing label", method)
		}
	}

	// An invite arriving is not an invite resolving.
	if _, ok := inviteOutcome("cluster:invite-received"); ok {
		t.Error("invite-received treated as terminal")
	}
	if _, ok := inviteOutcome("discovery:nodes-changed"); ok {
		t.Error("unrelated notification treated as a terminal invite event")
	}
}

// notify builds the broker push a view would receive for method, with no params.
func notify(method string) NotificationMsg {
	return NotificationMsg{Msg: &rpc.Message{Method: method}}
}

// TestNodesViewRetiresPinOnInviteDeclined is the regression guard for the
// pairing dead end: a PIN pinned on the Nodes tab must be replaced once the
// cluster manager reports the invite is no longer pending.
func TestNodesViewRetiresPinOnInviteDeclined(t *testing.T) {
	v := newNodesView(nil)
	v.invitedKey = "peer-uuid"
	v.outboundInviteID = "inv-1"
	v.status.pin("invite sent to peer - PIN 123456")

	v.Update(inviteEvent("cluster:invite-declined", "inv-1"))

	if v.invitedKey != "" {
		t.Error("pending invite still tracked after it was declined")
	}
	rendered := v.status.render()
	if rendered == "" {
		t.Fatal("declined invite produced no status at all")
	}
	if contains(rendered, "123456") {
		t.Errorf("PIN still on screen after the invite was declined: %q", rendered)
	}
}

// TestNodesViewRetiresPinOnPairingSuccess covers the one outcome with no
// terminal notification: success shows up as the invited node becoming a member.
func TestNodesViewRetiresPinOnPairingSuccess(t *testing.T) {
	v := newNodesView(nil)
	v.invitedKey = "peer-uuid"
	v.status.pin("invite sent to peer - PIN 123456")

	v.feeds.discovered = []availableNode{{HostUUID: "peer-uuid", Name: "peer", Trusted: true}}
	v.rebuild()

	if v.invitedKey != "" {
		t.Error("pending invite still tracked after the peer joined")
	}
	if rendered := v.status.render(); contains(rendered, "123456") {
		t.Errorf("PIN still on screen after pairing completed: %q", rendered)
	}
}

// TestNodesViewKeepsPinWhileInvitePending checks an unrelated snapshot does not
// retire a PIN that is still live.
func TestNodesViewKeepsPinWhileInvitePending(t *testing.T) {
	v := newNodesView(nil)
	v.invitedKey = "peer-uuid"
	v.status.pin("invite sent to peer - PIN 123456")

	v.feeds.discovered = []availableNode{
		{HostUUID: "peer-uuid", Name: "peer", Trusted: false},
		{HostUUID: "other-uuid", Name: "other", Trusted: true},
	}
	v.rebuild()

	if v.invitedKey != "peer-uuid" {
		t.Error("pending invite dropped while still unanswered")
	}
	if !contains(v.status.render(), "123456") {
		t.Error("PIN removed while the invite was still pending")
	}
}

// inviteEvent builds a terminal invite notification carrying an inviteId.
func inviteEvent(method, inviteID string) NotificationMsg {
	params, _ := json.Marshal(map[string]string{"inviteId": inviteID})
	return NotificationMsg{Msg: &rpc.Message{Method: method, Params: params}}
}

// inviteReceived is an inbound pairing request, as the cluster manager pushes it.
func inviteReceived(inviteID, from string) NotificationMsg {
	params, _ := json.Marshal(map[string]string{
		"inviteId": inviteID, "fromNodeName": from,
	})
	return NotificationMsg{Msg: &rpc.Message{
		Method: "cluster:invite-received", Params: params,
	}}
}

// TestInboundPairingIsPromptedOnce is the regression guard for the same
// request being announced twice, in two wordings.
//
// It was both a pinned status line and a row of the frame. Two prompts for one
// fact read as two requests, and the pinned one also outranked the status
// line, so nothing that happened next could be reported there.
func TestInboundPairingIsPromptedOnce(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(120, 30)
	v.Update(inviteReceived("inv-1", "M2GT9CR405"))

	prompt := v.inboundPrompt()
	if !contains(prompt, "M2GT9CR405") {
		t.Fatalf("the prompt does not name the machine asking: %q", prompt)
	}
	if got := v.status.render(); contains(got, "pairing request") {
		t.Errorf("the request is announced on the status line as well: %q", got)
	}
	// And once in the rendered frame, not twice.
	if n := strings.Count(v.View(), "pairing request from"); n != 1 {
		t.Errorf("the frame carries the prompt %d times, want 1", n)
	}
}

// TestInboundInviteDoesNotHideOutboundPIN checks a second live pairing request
// does not make the PIN for our still-pending outbound invite unreadable.
//
// Both used to be pinned to the status line, which holds one message, so the
// inbound request replaced the PIN the operator was about to read out.
func TestInboundInviteDoesNotHideOutboundPIN(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(100, 30)
	v.Update(nodeInviteMsg{name: "peer", inviteID: "outbound", pin: "123456"})
	if !contains(v.View(), "PIN 123456") {
		t.Fatal("outbound PIN was not visible before the inbound invite arrived")
	}

	v.Update(inviteReceived("inbound", "other peer"))

	if v.outboundInviteID != "outbound" {
		t.Fatalf("outbound invite is no longer pending: %q", v.outboundInviteID)
	}
	if v.inbound == nil || v.inbound.InviteID != "inbound" {
		t.Fatalf("inbound invite was not recorded: %#v", v.inbound)
	}
	if got := v.View(); !contains(got, "PIN 123456") {
		t.Errorf("live outbound PIN disappeared after an unrelated inbound invite: %q", got)
	}
}

// TestAcceptingPairingChangesThePrompt checks the prompt follows the request
// into its second state.
//
// Pressing accept does not finish anything — it opens the PIN field — so a
// prompt still offering "a to accept" told the operator to do what they had
// just done, while the PIN it actually wanted sat on the line below.
func TestAcceptingPairingChangesThePrompt(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(120, 30)
	v.Update(inviteReceived("inv-2", "M2GT9CR405"))
	v.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(nodePairKey.Help().Key)})

	if v.mode != nodesInputPin {
		t.Fatalf("accept did not open the PIN field (mode %v)", v.mode)
	}
	prompt := v.inboundPrompt()
	if contains(prompt, "to accept") {
		t.Errorf("still offering accept after it was pressed: %q", prompt)
	}
	if !contains(prompt, "PIN") {
		t.Errorf("prompt %q does not say what is wanted now", prompt)
	}
	// The request is still pending until the PIN is submitted, so the machine
	// it came from stays named.
	if !contains(prompt, "M2GT9CR405") {
		t.Errorf("prompt %q lost track of who is pairing", prompt)
	}
}

// TestInviteOutcomeMatchesBySession is the regression guard for concurrent
// pairings clobbering each other. The manager supports several at once and
// stamps an inviteId on every terminal event, so an unrelated invite's decline
// must not clear the PIN or prompt belonging to a different session.
func TestInviteOutcomeMatchesBySession(t *testing.T) {
	v := newNodesView(nil)
	v.outboundInviteID = "mine"
	v.invitedKey = "peer-uuid"
	v.status.pin("invite sent to peer - PIN 123456")
	v.inbound = &clusterInvite{InviteID: "theirs", FromNodeName: "other"}

	// An event carrying no invite at all belongs to no session and is ignored.
	// Every terminal notification the manager emits carries one, so this is a
	// malformed frame rather than an older sender to be accommodated.
	v.Update(notify("cluster:invite-declined"))
	if v.outboundInviteID != "mine" || v.inbound == nil {
		t.Error("an unattributable event cleared a live pairing session")
	}

	// A decline for a third, unrelated session touches neither.
	v.Update(inviteEvent("cluster:invite-declined", "somebody-else"))
	if v.outboundInviteID != "mine" || v.invitedKey == "" {
		t.Error("an unrelated invite's decline cleared our outbound session")
	}
	if v.inbound == nil {
		t.Error("an unrelated invite's decline cleared the inbound prompt")
	}

	// A decline for our outbound invite clears that, and leaves the inbound
	// request alone.
	v.Update(inviteEvent("cluster:invite-declined", "mine"))
	if v.outboundInviteID != "" || v.invitedKey != "" {
		t.Error("our own decline did not clear the outbound session")
	}
	if v.inbound == nil {
		t.Error("our outbound decline also cleared the unrelated inbound prompt")
	}

	// And the inbound one resolves on its own id.
	v.Update(inviteEvent("cluster:invite-expired", "theirs"))
	if v.inbound != nil {
		t.Error("the inbound prompt survived its own expiry")
	}
}

// TestInviteByAddressRetiresItsPin is the regression guard for the by-address
// path: it has no node UUID, so without matching on the address the PIN it
// pinned stayed on screen forever after the peer joined.
func TestInviteByAddressRetiresItsPin(t *testing.T) {
	v := newNodesView(nil)
	v.Update(nodeInviteMsg{
		name: "10.0.0.7", address: "10.0.0.7", inviteID: "inv", pin: "123456",
	})
	if v.invitedAddress != "10.0.0.7" {
		t.Fatalf("invitedAddress = %q, want the invited host", v.invitedAddress)
	}
	if !contains(v.status.render(), "123456") {
		t.Fatal("PIN was not pinned")
	}

	// The peer joins; discovery reports it with that address.
	v.feeds.discovered = []availableNode{{
		HostUUID: "peer-uuid", Name: "peer", IPAddress: "10.0.0.7", Trusted: true,
	}}
	v.rebuild()

	if v.invitedAddress != "" {
		t.Error("pending by-address invite still tracked after the peer joined")
	}
	if contains(v.status.render(), "123456") {
		t.Error("PIN still on screen after the by-address peer joined")
	}
}

// TestAddressMatches checks the host comparison used to recognise a peer invited
// by address, including a typed host:port form and the node's other addresses.
func TestAddressMatches(t *testing.T) {
	row := nodeRow{
		name:      "host-a",
		address:   "10.0.0.7",
		addresses: []string{"10.0.0.7", "192.168.1.9"},
	}
	for _, in := range []string{"10.0.0.7", "10.0.0.7:14321", "192.168.1.9", "HOST-A"} {
		if !addressMatches(row, in) {
			t.Errorf("addressMatches(%q) = false, want true", in)
		}
	}
	for _, in := range []string{"", "10.0.0.8", "other-host"} {
		if addressMatches(row, in) {
			t.Errorf("addressMatches(%q) = true, want false", in)
		}
	}
}

// TestRemoveMemberRequiresConfirmation checks un-pairing a peer is armed rather
// than immediate, matching leave-cluster. It acts on someone else's row, so a
// stray keystroke is worse there, not better.
func TestRemoveMemberRequiresConfirmation(t *testing.T) {
	v := newNodesView(nil)
	v.feeds.discovered = []availableNode{{
		HostUUID: "peer", Name: "peer", IPAddress: "10.0.0.2", Trusted: true,
	}}
	v.rebuild()
	v.selectedKey = "peer"

	if cmd := v.removeSelected(); cmd != nil {
		t.Error("removal was dispatched without confirmation")
	}
	if v.confirmRemove != "peer" {
		t.Fatalf("confirmRemove = %q, want the selected node", v.confirmRemove)
	}

	// Any other key cancels.
	v.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if v.confirmRemove != "" {
		t.Error("a non-confirming key left the removal armed")
	}

	// Re-arm and confirm.
	v.removeSelected()
	if cmd := v.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}); cmd == nil {
		t.Error("confirmation produced no removal command")
	}
}

// TestNodesViewExplainsAnEmptyTable is the guard for the worst first
// impression this tab can give: no rows and no reason.
//
// The four feeds behind it each ignored their error, so a broker that could not
// answer produced a tab identical to a quiet network. Those need opposite
// responses from the operator — wait, or go look at the service — so the screen
// has to say which one it is.
func TestNodesViewExplainsAnEmptyTable(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(100, 30)

	// Nothing wrong, just nothing found yet.
	if got := v.View(); !contains(got, "Discovery is browsing") {
		t.Errorf("a quiet network does not read as one:\n%s", got)
	}

	v.Update(clusterMembersMsg{err: errors.New("worker not running")})
	got := v.View()
	if contains(got, "Discovery is browsing") {
		t.Error("a failed read still claims discovery is simply looking")
	}
	if !contains(got, "cluster members") {
		t.Errorf("the failing feed is not named:\n%s", got)
	}
	if !contains(got, "worker not running") {
		t.Errorf("the reason is not shown:\n%s", got)
	}
}

// TestNodesViewWarnsWhenPopulatedButIncomplete checks a partial failure is
// reported too. A table with rows in it looks authoritative, so a missing feed
// there is more misleading than an empty one, not less.
func TestNodesViewWarnsWhenPopulatedButIncomplete(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(100, 30)
	v.feeds.discovered = []availableNode{{HostUUID: "peer", Name: "peer", IPAddress: "10.0.0.2"}}
	v.rebuild()

	v.Update(manualNodesMsg{err: errors.New("manual worker down")})
	if got := v.View(); !contains(got, "manual nodes") {
		t.Errorf("a populated table hides that a feed is missing:\n%s", got)
	}
}

// TestNodesViewClearsFeedWarningOnRecovery checks the warning is a live
// condition, not a permanent mark: a feed that starts working again stops
// being reported.
func TestNodesViewClearsFeedWarningOnRecovery(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(100, 30)

	v.Update(manualNodesMsg{err: errors.New("transient")})
	if v.feedWarning() == "" {
		t.Fatal("failure was not recorded")
	}

	v.Update(manualNodesMsg{})
	if got := v.feedWarning(); got != "" {
		t.Errorf("warning survived recovery: %q", got)
	}
}

// TestNodesFilterNarrowsWithoutLosingTheCluster checks the filter changes what
// is shown and nothing else. The cluster summary and the pairing-completion
// check are about the cluster, not about what the operator is looking at, so a
// filter must not shrink the member count or strand a pinned PIN.
func TestNodesFilterNarrowsWithoutLosingTheCluster(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(100, 30)
	v.identity.ClusterID = "cluster-1"
	v.feeds.members = []clusterNode{
		{NodeUUID: "a", Name: "alpha", State: "member"},
		{NodeUUID: "b", Name: "beta", State: "member"},
	}
	v.rebuild()

	if got := len(v.rows); got != 2 {
		t.Fatalf("unfiltered rows = %d, want 2", got)
	}

	v.filter = "alpha"
	v.rebuild()

	if len(v.rows) != 1 || v.rows[0].name != "alpha" {
		t.Errorf("filtered rows = %+v, want just alpha", v.rows)
	}
	if len(v.all) != 2 {
		t.Errorf("the filter dropped nodes from the full set: %d", len(v.all))
	}
	if !contains(v.clusterLine(), "2") {
		t.Errorf("member count followed the filter instead of the cluster: %q", v.clusterLine())
	}
	if !contains(v.View(), "showing 1 of 2") {
		t.Errorf("a filtered table does not say it is filtered:\n%s", v.View())
	}
}

// TestNodesFilterRetiresPinForAHiddenPeer checks a peer that joins while
// filtered out still retires its PIN. The pairing completed; whether the
// operator can currently see the row is irrelevant.
func TestNodesFilterRetiresPinForAHiddenPeer(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(100, 30)
	v.Update(nodeInviteMsg{name: "beta", inviteID: "inv", pin: "123456"})
	v.invitedKey = "b"
	v.filter = "alpha" // hides the very node we invited

	v.feeds.members = []clusterNode{{NodeUUID: "b", Name: "beta", State: "member"}}
	v.rebuild()

	if v.invitedKey != "" {
		t.Error("a peer that joined while filtered out left its invite pending")
	}
	if contains(v.status.render(), "123456") {
		t.Error("the PIN is still on screen after the hidden peer joined")
	}
}

// TestSecondInviteWaitsForTheFirst is the regression guard for two outbound
// invites crossing.
//
// The view tracks one target and the status line carries one PIN, while the
// replies do not say which request they answer. A second invite sent while the
// first was pending could show one peer's PIN while watching for the other to
// join, or have the older request's failure cancel the newer one.
func TestSecondInviteWaitsForTheFirst(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(100, 30)
	v.feeds.discovered = []availableNode{
		{HostUUID: "a", Name: "alpha", IPAddress: "10.0.0.1", Port: 9000},
		{HostUUID: "b", Name: "beta", IPAddress: "10.0.0.2", Port: 9000},
	}
	v.rebuild()
	press := func(k string) tea.Cmd {
		return v.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
	}
	inviteKey := nodeInviteKey.Help().Key
	addrKey := nodeInviteAddrKey.Help().Key

	v.selectedKey = "a"
	v.restoreSelection()
	if press(inviteKey) == nil {
		t.Fatal("the first invite was not sent")
	}

	// While the first is in flight, neither path may start another.
	v.selectedKey = "b"
	v.restoreSelection()
	if press(inviteKey) != nil {
		t.Error("a second invite was sent while the first was awaiting its reply")
	}
	press(addrKey)
	if v.mode == nodesInputInviteAddress {
		t.Error("invite-by-address opened while an invite was awaiting its reply")
	}

	// Once the PIN is showing, refusing must not take it off the screen.
	v.Update(nodeInviteMsg{name: "alpha", inviteID: "inv-a", pin: "123456"})
	if press(inviteKey) != nil {
		t.Error("a second invite was sent while the first PIN was still open")
	}
	if !contains(v.status.render(), "123456") {
		t.Errorf("refusing the second invite hid the first one's PIN: %q", v.status.render())
	}
	if v.invitedKey != "a" || v.outboundInviteID != "inv-a" {
		t.Errorf("the pending invite changed target: key %q, id %q", v.invitedKey, v.outboundInviteID)
	}

	// Cancelling frees the way.
	press(nodeCancelKey.Help().Key)
	if press(inviteKey) == nil {
		t.Error("an invite could not be sent after the pending one was cancelled")
	}
}

// TestRosterReadDoesNotOverwriteANewerPush checks a roster read that crossed a
// push does not replace it. Both are whole rosters, but the reply and the push
// arrive by different paths, and a reply landing second put the older roster
// back until the next change.
func TestRosterReadDoesNotOverwriteANewerPush(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(120, 30)
	// A read sent before the push, whose reply lands after it.
	stale := clusterMembersMsg{nodes: []clusterNode{{NodeUUID: "gone", Name: "gone", State: "member"}}, pushes: v.membersPushes}

	params := []byte(`{"nodes":[{"nodeUuid":"now","name":"now","state":"member"}]}`)
	v.Update(NotificationMsg{Msg: &rpc.Message{Method: "nodes:changed", Params: params}})
	v.Update(stale)

	if len(v.feeds.members) != 1 || v.feeds.members[0].NodeUUID != "now" {
		t.Errorf("roster = %+v, want the pushed one kept", v.feeds.members)
	}

	// A read sent after the push is current, and is taken.
	fresh := clusterMembersMsg{nodes: []clusterNode{{NodeUUID: "later", State: "member"}}, pushes: v.membersPushes}
	v.Update(fresh)
	if len(v.feeds.members) != 1 || v.feeds.members[0].NodeUUID != "later" {
		t.Errorf("roster = %+v, want the read sent after the push", v.feeds.members)
	}
}

// TestMalformedPINKeepsTheRequest is the regression guard for a pairing request
// lost to a typo. The cluster manager refuses a PIN that is not six digits with
// an error and leaves the session open, but the prompt was cleared before the
// answer went out, so there was nothing left to answer again.
func TestMalformedPINKeepsTheRequest(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(120, 30)
	v.Update(inviteReceived("inv-1", "peer"))

	if v.respondToInvite(true, "12345") == nil {
		t.Fatal("the answer was not sent")
	}
	if v.inbound == nil {
		t.Fatal("the request left the screen before it was settled")
	}
	if v.respondToInvite(true, "123456") != nil {
		t.Error("a second answer was sent while the first was in flight")
	}

	v.Update(pairingResultMsg{inviteID: "inv-1", from: "peer", err: errors.New("pin must be six digits")})
	if v.inbound == nil || v.inbound.InviteID != "inv-1" {
		t.Fatal("an answer the cluster manager refused took the request away")
	}
	if !contains(v.inboundPrompt(), "to accept") {
		t.Errorf("the request cannot be answered again: %q", v.inboundPrompt())
	}

	// A settled answer does take it away.
	v.respondToInvite(true, "123456")
	v.Update(pairingResultMsg{inviteID: "inv-1", from: "peer", state: inviteStatePaired})
	if v.inbound != nil {
		t.Error("a settled request stayed on screen")
	}
}

// TestInboundRequestsWaitTheirTurn is the regression guard for a pairing
// request being replaced by the next. Both are live sessions another machine
// is waiting on; the newer one used to overwrite the one being read, leaving
// no way back to it.
func TestInboundRequestsWaitTheirTurn(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(120, 30)
	v.Update(inviteReceived("first", "alpha"))
	v.Update(inviteReceived("second", "beta"))
	v.Update(inviteReceived("second", "beta")) // delivered twice

	if v.inbound.InviteID != "first" {
		t.Fatalf("the newer request replaced the one on screen: %q", v.inbound.InviteID)
	}
	if got := v.inboundPrompt(); !contains(got, "1 more waiting") {
		t.Errorf("the prompt does not say another request is waiting: %q", got)
	}

	v.Update(inviteEvent("cluster:invite-expired", "first"))
	if v.inbound == nil || v.inbound.InviteID != "second" {
		t.Fatalf("the waiting request did not come up next: %#v", v.inbound)
	}
	if contains(v.inboundPrompt(), "more waiting") {
		t.Errorf("a request delivered twice was queued twice: %q", v.inboundPrompt())
	}
}

// TestDroppedRequestTakesItsPINWithIt is the regression guard for a PIN typed
// for one request answering the next. The request on screen expired while its
// PIN field was open, the next one came up under the same field, and enter
// sent the first request's PIN as the answer to the second.
func TestDroppedRequestTakesItsPINWithIt(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(120, 30)
	v.Update(inviteReceived("first", "alpha"))
	v.Update(inviteReceived("second", "beta"))

	v.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	v.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1234")})
	if v.mode != nodesInputPin || v.input.Value() != "1234" {
		t.Fatalf("the PIN field did not take the digits (mode %v, value %q)", v.mode, v.input.Value())
	}

	v.Update(inviteEvent("cluster:invite-expired", "first"))
	if v.inbound == nil || v.inbound.InviteID != "second" {
		t.Fatalf("the waiting request did not come up next: %#v", v.inbound)
	}
	if v.mode == nodesInputPin || v.input.Value() != "" {
		t.Errorf("the PIN field outlived its request (mode %v, value %q)", v.mode, v.input.Value())
	}
	v.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if v.answering != "" {
		t.Errorf("enter answered %q with a PIN typed for another request", v.answering)
	}
}

// TestJoiningDeclinesEveryOtherRequest is the regression guard for requests
// left waiting after this machine joined a cluster. None could be accepted any
// more, yet each came up in turn looking answerable, and each sender went on
// showing its PIN until its invite expired.
func TestJoiningDeclinesEveryOtherRequest(t *testing.T) {
	c1, c2 := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		c1.Close()
		c2.Close()
	})
	client := rpc.NewClient(c1, c1)
	go client.Run(ctx)

	// The broker's side: answer every call, recording each pairing answer.
	type answer struct {
		InviteID string `json:"inviteId"`
		Accept   *bool  `json:"accept"`
	}
	answers := make(chan answer, 8)
	broker := rpc.NewCodec(c2, c2)
	go func() {
		for {
			req, err := broker.Read()
			if err != nil {
				return
			}
			if req.Method == "cluster:respond-to-invite" {
				var a answer
				_ = json.Unmarshal(req.Params, &a)
				answers <- a
			}
			_ = broker.Write(&rpc.Message{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{}`)})
		}
	}()

	v := newNodesView(client)
	v.SetSize(120, 30)
	v.Update(inviteReceived("a", "alpha"))
	v.Update(inviteReceived("b", "beta"))
	v.Update(inviteReceived("c", "gamma"))

	cmd := v.Update(pairingResultMsg{inviteID: "a", from: "alpha", state: inviteStatePaired})
	if v.inbound != nil || len(v.queued) != 0 {
		t.Fatalf("requests are still waiting after joining: %#v %v", v.inbound, v.queued)
	}
	if got := v.status.render(); !contains(got, "paired with alpha") || !contains(got, "declined 2 other") {
		t.Errorf("the outcome does not say the others were declined: %q", got)
	}

	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatal("joining issued no commands")
	}
	for _, c := range batch {
		go c()
	}
	declined := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(declined) < 2 {
		select {
		case a := <-answers:
			if a.Accept == nil || *a.Accept {
				t.Errorf("request %q was answered with accept=%v, want a decline", a.InviteID, a.Accept)
			}
			declined[a.InviteID] = true
		case <-deadline:
			t.Fatalf("only %v were declined", declined)
		}
	}
	if !declined["b"] || !declined["c"] || declined["a"] {
		t.Errorf("declined %v, want exactly the two still waiting", declined)
	}
}

// TestJoiningWithNothingWaitingDeclinesNothing checks the ordinary case still
// reads as it did: one request, accepted, and nothing else said.
func TestJoiningWithNothingWaitingDeclinesNothing(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(120, 30)
	v.Update(inviteReceived("a", "alpha"))
	v.Update(pairingResultMsg{inviteID: "a", from: "alpha", state: inviteStatePaired})
	if got := v.status.render(); !contains(got, "paired with alpha") || contains(got, "declined") {
		t.Errorf("a lone accept reported %q", got)
	}
}

// TestPairingStaysVisibleOverTheDetailScreen is the regression guard for a
// pairing request or a live PIN disappearing while a node's detail screen was
// open, which replaces the list they are shown on.
func TestPairingStaysVisibleOverTheDetailScreen(t *testing.T) {
	const height = 30
	v := newNodesView(nil)
	v.SetSize(120, height)
	v.feeds.discovered = []availableNode{{HostUUID: "u1", Name: "host", IPAddress: "10.0.0.1", Port: 1}}
	v.rebuild()
	v.openDetail()

	v.Update(nodeInviteMsg{name: "peer", inviteID: "out", pin: "123456"})
	if got := v.View(); !contains(got, "PIN 123456") {
		t.Errorf("a live PIN is hidden behind the detail screen")
	}

	v.Update(inviteReceived("in", "other peer"))
	got := v.View()
	if !contains(got, "pairing request from other peer") {
		t.Errorf("an inbound request is hidden behind the detail screen")
	}
	if rows := renderedRows(got); rows > height {
		t.Errorf("the pairing line pushed the frame to %d rows of %d", rows, height)
	}
}

// TestFailedCancelCanBeRetried checks a cancel that did not reach the cluster
// manager leaves a way to try again. The PIN was cleared before the call went
// out, so a failure left a live invite with nothing on screen and nothing to
// press.
func TestFailedCancelCanBeRetried(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(120, 30)
	v.Update(nodeInviteMsg{name: "peer", inviteID: "inv", pin: "123456"})

	cmd := v.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(nodeCancelKey.Help().Key)})
	if cmd == nil || v.outboundInviteID != "" {
		t.Fatal("cancel was not sent, or the PIN stayed tracked while it was")
	}

	v.Update(inviteCancelledMsg{invite: sentInvite{id: "inv", name: "peer", pin: "123456"}, err: errFake{}})
	if v.outboundInviteID != "inv" {
		t.Fatal("a failed cancel left the still-live invite untracked")
	}
	if !contains(v.status.render(), "123456") {
		t.Errorf("the still-live PIN is not shown again: %q", v.status.render())
	}
	if v.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(nodeCancelKey.Help().Key)}) == nil {
		t.Error("the cancel cannot be tried again")
	}
}

// TestRejectionAdviceMatchesTheReason checks a refused invite only suggests
// the remedy for the reason actually given. "Remove the existing relationship
// first" was attached to every rejection, including ones with no relationship
// to remove.
func TestRejectionAdviceMatchesTheReason(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(120, 30)

	v.Update(nodeInviteMsg{name: "peer", rejected: true, reason: reasonAlreadyClustered})
	if got := v.status.render(); !contains(got, "leave that cluster") {
		t.Errorf("an already-clustered peer is not told what to do: %q", got)
	}

	for _, reason := range []string{"", "evil-arbitrary-text"} {
		v.Update(nodeInviteMsg{name: "peer", rejected: true, reason: reason})
		got := v.status.render()
		if contains(got, "leave that cluster") || contains(got, "relationship") {
			t.Errorf("reason %q got advice meant for an already-clustered peer: %q", reason, got)
		}
		if !contains(got, "rejected the invite") {
			t.Errorf("reason %q: the rejection is not reported: %q", reason, got)
		}
	}
}

// TestInviteThatDidNotGoOutIsNotReportedSent is the regression guard for a
// failed invite read as a sent one. The manager answers "failed", with no PIN,
// when the first exchange does not complete, and anything short of "rejected"
// was reported as "invite sent" and left pending.
func TestInviteThatDidNotGoOutIsNotReportedSent(t *testing.T) {
	for _, state := range []string{"failed", "canceled"} {
		v := newNodesView(nil)
		v.SetSize(120, 30)
		v.invitedKey, v.inviteSending = "peer-uuid", true

		v.Update(inviteResultMsg("peer", "", inviteNodeResult{InviteID: "inv", State: state}, nil))
		got := v.status.render()
		if contains(got, "invite sent") || !contains(got, "did not go out") {
			t.Errorf("a %q reply was reported as %q", state, got)
		}
		if v.invitedKey != "" || v.outboundInviteID != "" {
			t.Errorf("a %q reply left the invite pending", state)
		}
	}

	pin := "123456"
	v := newNodesView(nil)
	v.SetSize(120, 30)
	v.Update(inviteResultMsg("peer", "", inviteNodeResult{InviteID: "inv", State: "pending", Pin: &pin}, nil))
	if got := v.status.render(); !contains(got, "PIN 123456") {
		t.Errorf("a pending invite did not show its PIN: %q", got)
	}
}

// TestCancelInviteClearsThePinImmediately checks the inviter's half of decline.
// Without it a PIN read to the wrong person could only be retired by waiting
// for it to expire, staying answerable the whole time.
func TestCancelInviteClearsThePinImmediately(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(100, 30)
	v.Update(nodeInviteMsg{name: "peer", inviteID: "inv-1", pin: "123456"})
	if !contains(v.status.render(), "123456") {
		t.Fatal("PIN was not pinned")
	}

	if cmd := v.cancelInvite(); cmd == nil {
		t.Error("cancelling produced no request")
	}
	if v.outboundInviteID != "" {
		t.Error("the invite is still tracked after cancelling")
	}
	if contains(v.status.render(), "123456") {
		t.Error("the PIN is still displayed after cancelling; it must stop being readable at once")
	}

	// Nothing pending is a no-op, not an error.
	if cmd := v.cancelInvite(); cmd != nil {
		t.Error("cancelling with no invite pending still sent a request")
	}
}

// TestWrongPinIsNotReportedAsSuccess is the regression guard for the worst lie
// this interface could tell.
//
// A wrong PIN is not a JSON-RPC error. The cluster manager tears the pairing
// session down and replies *successfully* with the invite, whose state is
// "failed" and whose reason says why. Reading only the transport error reported
// a green "accept pairing ok" for a pairing that had just been rejected — and
// because the prompt is cleared by then, nothing later corrected it. The
// operator would go looking for a peer that was never going to appear.
func TestWrongPinIsNotReportedAsSuccess(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(100, 30)

	v.Update(pairingResultMsg{from: "peer", state: "failed", reason: reasonIncorrectPIN})

	got := v.status.render()
	if contains(got, " ok") {
		t.Errorf("a rejected pairing reported success: %q", got)
	}
	if !contains(got, "wrong PIN") {
		t.Errorf("status %q does not say the PIN was wrong", got)
	}
	if !contains(got, "new invite") {
		t.Errorf("status %q does not say what to do next; the PIN is single-use", got)
	}
}

// TestPairingOutcomesAreDistinguished checks each terminal state gets its own
// answer, since they call for different things from the operator.
func TestPairingOutcomesAreDistinguished(t *testing.T) {
	cases := []struct {
		state, reason string
		want          string
	}{
		{state: "paired", want: "paired with peer"},
		{state: "failed", reason: reasonIncorrectPIN, want: "wrong PIN"},
		{state: "failed", reason: "unreachable", want: "failed"},
		{state: "declined", want: "declined"},
	}
	for _, tc := range cases {
		v := newNodesView(nil)
		v.SetSize(100, 30)
		v.Update(pairingResultMsg{from: "peer", state: tc.state, reason: tc.reason})
		if got := v.status.render(); !contains(got, tc.want) {
			t.Errorf("state=%q reason=%q rendered %q, want it to mention %q",
				tc.state, tc.reason, got, tc.want)
		}
	}
}

// TestNodesViewClearsInboundInviteOnExpiry checks an inbound prompt stops
// offering accept/decline once the invite is gone.
func TestNodesViewClearsInboundInviteOnExpiry(t *testing.T) {
	// Both the prompt and the assertion read the key off the binding. Spelling
	// it out meant that when accept moved from p to a, this went on checking
	// that p was absent — and p by then was "pair", which is always offered, so
	// it failed for a reason unrelated to what it tests.
	accept := nodePairKey.Help().Key

	v := newNodesView(nil)
	v.Update(inviteReceived("inv-1", "peer"))
	if !contains(v.inboundPrompt(), "to accept") {
		t.Fatal("no prompt after a pairing request arrived")
	}

	v.Update(inviteEvent("cluster:invite-expired", "inv-1"))

	if v.inbound != nil {
		t.Error("expired inbound invite is still pending; accept would target a dead invite")
	}
	if contains(v.inboundPrompt(), "to accept") {
		t.Error("still offering accept/decline for an expired invite")
	}
	if v.Help() == nil {
		t.Error("help bindings unexpectedly nil")
	}
	for _, b := range v.Help() {
		if b.Help().Key == accept && b.Help().Desc == nodePairKey.Help().Desc {
			t.Error("accept-pairing key still advertised with no pending invite")
		}
	}
}

// TestNodesViewSelectionSurvivesReorder is the guard for selection tracked by
// key rather than row index: the list re-sorts as nodes come and go, and an
// index would quietly move the operator onto a different machine.
func TestNodesViewSelectionSurvivesReorder(t *testing.T) {
	v := newNodesView(nil)
	v.feeds.discovered = []availableNode{
		{HostUUID: "a", Name: "aaa", LastSeen: time.Now().Unix()},
		{HostUUID: "z", Name: "zzz", LastSeen: time.Now().Unix()},
	}
	v.rebuild()

	v.selectedKey = "z"
	v.restoreSelection()
	if got := v.selectedRow(); got == nil || got.key != "z" {
		t.Fatalf("selection did not settle on the requested node")
	}

	// A new node sorting ahead of the selection must not steal the cursor.
	v.feeds.discovered = append(v.feeds.discovered,
		availableNode{HostUUID: "m", Name: "mmm", LastSeen: time.Now().Unix()})
	v.rebuild()

	if got := v.selectedRow(); got == nil || got.key != "z" {
		t.Errorf("selection moved to %v after the list grew", got)
	}
}

// TestNodesViewGuardsInviteOnEveryPath is the regression guard for re-inviting a
// node that already has a relationship. The old split tabs guarded the
// discovered-node path only, so the by-address path could still send a doomed
// invite.
func TestNodesViewGuardsInviteOnEveryPath(t *testing.T) {
	cases := []struct {
		name string
		node availableNode
	}{
		{"already a member of our cluster", availableNode{HostUUID: "k", Name: "peer", Trusted: true}},
		{"a member of another cluster", availableNode{HostUUID: "k", Name: "peer", Clustered: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := newNodesView(nil)
			v.feeds.discovered = []availableNode{tc.node}
			v.rebuild()
			v.selectedKey = "k"

			if cmd := v.inviteSelected(); cmd != nil {
				t.Error("invite was dispatched for a node that cannot accept one")
			}
			if v.invitedKey != "" {
				t.Error("invite recorded as pending despite being blocked")
			}
			if v.status.render() == "" {
				t.Error("invite blocked with no explanation to the operator")
			}
		})
	}
}

// TestNodesViewRefusesSelfInvite checks this machine cannot be invited to its
// own cluster.
func TestNodesViewRefusesSelfInvite(t *testing.T) {
	v := newNodesView(nil)
	v.feeds.discovered = []availableNode{{HostUUID: "me", Name: "this", LastSeen: time.Now().Unix()}}
	v.feeds.selfUUID = "me"
	v.rebuild()
	v.selectedKey = "me"

	if cmd := v.inviteSelected(); cmd != nil {
		t.Error("dispatched an invite to this machine")
	}
}

// contains is a substring check kept local to avoid importing strings for one
// assertion style.
func contains(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// assert the views used above still satisfy the interface the shell drives.
var _ View = (*nodesView)(nil)
var _ inputCapturer = (*nodesView)(nil)
var _ tea.Model = Model{}
