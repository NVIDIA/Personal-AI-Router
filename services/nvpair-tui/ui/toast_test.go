// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

// TestTruncateIsRuneSafe is the regression guard for a byte-based cut. GPU and
// CPU model names reach truncate, and a slice landing inside a multi-byte
// sequence emits an invalid rune; the callers also pair it with %-Ns, which pads
// by rune count, so a byte cut narrowed the column too.
func TestTruncateIsRuneSafe(t *testing.T) {
	// 10 runes, 20 bytes.
	const wide = "ααααααααα™"
	got := truncate(wide, 5)

	if !utf8.ValidString(got) {
		t.Errorf("truncate produced invalid UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n != 5 {
		t.Errorf("truncate(%q, 5) is %d runes, want 5", wide, n)
	}

	// Short input is returned untouched even when its byte length exceeds max.
	if got := truncate("ααα", 5); got != "ααα" {
		t.Errorf("truncate shortened a string that already fits: %q", got)
	}
	// Degenerate widths must not panic.
	if got := truncate("abc", 1); got != "…" {
		t.Errorf("truncate(_, 1) = %q", got)
	}
	if got := truncate("abc", 0); got != "" {
		t.Errorf("truncate(_, 0) = %q", got)
	}
	if got := truncate("abc", -1); got != "" {
		t.Errorf("truncate(_, -1) = %q", got)
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
