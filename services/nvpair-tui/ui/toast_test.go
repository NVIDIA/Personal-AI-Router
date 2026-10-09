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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestToastExpires is the regression guard for status lines that never went
// away: an outcome must stop rendering once its TTL has passed.
func TestToastExpires(t *testing.T) {
	var s toast
	s.ok("accept invite ok")
	require.NotEmpty(t, s.render(), "a message just set should render")

	s.at = time.Now().Add(-toastTTL - time.Second)
	assert.Empty(t, s.render(), "message outlived its TTL and is still rendering")
}

// TestToastErrorsOutliveSuccesses checks a failure stays readable for longer
// than a success, since the operator needs time to act on it.
func TestToastErrorsOutliveSuccesses(t *testing.T) {
	var ok, bad toast
	ok.ok("done")
	bad.error("it broke")

	aged := time.Now().Add(-toastTTL - time.Second)
	ok.at, bad.at = aged, aged

	assert.Empty(t, ok.render(), "success should have expired by now")
	assert.NotEmpty(t, bad.render(), "error expired at the success TTL; it should last longer")

	bad.at = time.Now().Add(-toastErrorTTL - time.Second)
	assert.Empty(t, bad.render(), "error outlived even the error TTL")
}

// TestPinnedToastNeverExpires covers the pairing PIN. The operator reads it
// aloud to someone at another machine, so a timer must not remove it.
func TestPinnedToastNeverExpires(t *testing.T) {
	var s toast
	s.pin("invite sent - PIN 123456")
	s.at = time.Now().Add(-24 * time.Hour)

	assert.NotEmpty(t, s.render(), "pinned message expired; the PIN must stay until the invite resolves")
	// It is replaced by the outcome, which is how a pinned message ends: the
	// views set a new one rather than clearing to nothing.
	s.ok("peer joined the cluster")
	assert.NotContains(t, s.render(), "123456", "the PIN survived the message that replaced it")
}

// TestSetReplacesPinned checks a later ordinary message drops the sticky flag,
// so a pinned PIN cannot make every subsequent message permanent.
func TestSetReplacesPinned(t *testing.T) {
	var s toast
	s.pin("invite sent - PIN 123456")
	s.info("inviting other-host...")

	s.at = time.Now().Add(-toastTTL - time.Second)
	assert.Empty(t, s.render(), "message set after a pin inherited its stickiness")
}

func TestInviteOutcome(t *testing.T) {
	test := func(name, method string) {
		t.Run(name, func(t *testing.T) {
			outcome, ok := inviteOutcome(method)
			if assert.True(t, ok, "recognised as a terminal invite event") {
				assert.NotEmpty(t, outcome.label, "operator-facing label")
			}
		})
	}
	test("declined", "cluster:invite-declined")
	test("expired", "cluster:invite-expired")
	test("canceled", "cluster:invite-canceled")
	test("failed", "cluster:invite-failed")

	// An invite arriving is not an invite resolving.
	_, ok := inviteOutcome("cluster:invite-received")
	assert.False(t, ok, "invite-received treated as terminal")
	_, ok = inviteOutcome("discovery:nodes-changed")
	assert.False(t, ok, "unrelated notification treated as a terminal invite event")
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

	v.Update(inviteEvent(t, "cluster:invite-declined", "inv-1"))

	assert.Empty(t, v.invitedKey, "pending invite still tracked after it was declined")
	rendered := v.status.render()
	require.NotEmpty(t, rendered, "declined invite produced no status at all")
	assert.NotContains(t, rendered, "123456", "PIN still on screen after the invite was declined")
}

// TestNodesViewRetiresPinOnPairingSuccess covers the one outcome with no
// terminal notification: success shows up as the invited node becoming a member.
func TestNodesViewRetiresPinOnPairingSuccess(t *testing.T) {
	v := newNodesView(nil)
	v.invitedKey = "peer-uuid"
	v.status.pin("invite sent to peer - PIN 123456")

	v.feeds.discovered = []availableNode{{HostUUID: "peer-uuid", Name: "peer", Trusted: true}}
	v.rebuild()

	assert.Empty(t, v.invitedKey, "pending invite still tracked after the peer joined")
	assert.NotContains(t, v.status.render(), "123456", "PIN still on screen after pairing completed")
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

	assert.Equal(t, "peer-uuid", v.invitedKey, "pending invite dropped while still unanswered")
	assert.Contains(t, v.status.render(), "123456", "PIN removed while the invite was still pending")
}

// inviteEvent builds a terminal invite notification carrying an inviteId.
func inviteEvent(t *testing.T, method, inviteID string) NotificationMsg {
	t.Helper()
	params, err := json.Marshal(map[string]string{"inviteId": inviteID})
	assert.NoError(t, err)
	return NotificationMsg{Msg: &rpc.Message{Method: method, Params: params}}
}

// inviteReceived is an inbound pairing request, as the cluster manager pushes it.
func inviteReceived(t *testing.T, inviteID, from string) NotificationMsg {
	t.Helper()
	params, err := json.Marshal(map[string]string{
		"inviteId": inviteID, "fromNodeName": from,
	})
	assert.NoError(t, err)
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
	v.Update(inviteReceived(t, "inv-1", "M2GT9CR405"))

	prompt := v.inboundPrompt()
	require.Contains(t, prompt, "M2GT9CR405", "the prompt names the machine asking")
	assert.NotContains(t, v.status.render(), "pairing request", "the request is announced on the status line as well")
	// And once in the rendered frame, not twice.
	assert.Equal(t, 1, strings.Count(v.View(), "pairing request from"), "one prompt in the frame")
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
	require.Contains(t, v.View(), "PIN 123456", "outbound PIN was not visible before the inbound invite arrived")

	v.Update(inviteReceived(t, "inbound", "other peer"))

	require.Equal(t, "outbound", v.outboundInviteID, "outbound invite is no longer pending")
	require.NotNil(t, v.inbound, "inbound invite was not recorded")
	require.Equal(t, "inbound", v.inbound.InviteID, "inbound invite was not recorded")
	assert.Contains(t, v.View(), "PIN 123456", "live outbound PIN disappeared after an unrelated inbound invite")
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
	v.Update(inviteReceived(t, "inv-2", "M2GT9CR405"))
	v.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(nodePairKey.Help().Key)})

	require.Equal(t, nodesInputPin, v.mode, "accept did not open the PIN field")
	prompt := v.inboundPrompt()
	assert.NotContains(t, prompt, "to accept", "still offering accept after it was pressed")
	assert.Contains(t, prompt, "PIN", "prompt says what is wanted now")
	// The request is still pending until the PIN is submitted, so the machine
	// it came from stays named.
	assert.Contains(t, prompt, "M2GT9CR405", "prompt lost track of who is pairing")
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
	assert.Equal(t, "mine", v.outboundInviteID, "an unattributable event cleared a live pairing session")
	assert.NotNil(t, v.inbound, "an unattributable event cleared a live pairing session")

	// A decline for a third, unrelated session touches neither.
	v.Update(inviteEvent(t, "cluster:invite-declined", "somebody-else"))
	assert.Equal(t, "mine", v.outboundInviteID, "an unrelated invite's decline cleared our outbound session")
	assert.NotEmpty(t, v.invitedKey, "an unrelated invite's decline cleared our outbound session")
	assert.NotNil(t, v.inbound, "an unrelated invite's decline cleared the inbound prompt")

	// A decline for our outbound invite clears that, and leaves the inbound
	// request alone.
	v.Update(inviteEvent(t, "cluster:invite-declined", "mine"))
	assert.Empty(t, v.outboundInviteID, "our own decline did not clear the outbound session")
	assert.Empty(t, v.invitedKey, "our own decline did not clear the outbound session")
	assert.NotNil(t, v.inbound, "our outbound decline also cleared the unrelated inbound prompt")

	// And the inbound one resolves on its own id.
	v.Update(inviteEvent(t, "cluster:invite-expired", "theirs"))
	assert.Nil(t, v.inbound, "the inbound prompt survived its own expiry")
}

// TestInviteByAddressRetiresItsPin is the regression guard for the by-address
// path: it has no node UUID, so without matching on the address the PIN it
// pinned stayed on screen forever after the peer joined.
func TestInviteByAddressRetiresItsPin(t *testing.T) {
	v := newNodesView(nil)
	v.Update(nodeInviteMsg{
		name: "10.0.0.7", address: "10.0.0.7", inviteID: "inv", pin: "123456",
	})
	require.Equal(t, "10.0.0.7", v.invitedAddress, "invited host")
	require.Contains(t, v.status.render(), "123456", "PIN was not pinned")

	// The peer joins; discovery reports it with that address.
	v.feeds.discovered = []availableNode{{
		HostUUID: "peer-uuid", Name: "peer", IPAddress: "10.0.0.7", Trusted: true,
	}}
	v.rebuild()

	assert.Empty(t, v.invitedAddress, "pending by-address invite still tracked after the peer joined")
	assert.NotContains(t, v.status.render(), "123456", "PIN still on screen after the by-address peer joined")
}

// TestAddressMatches checks the host comparison used to recognise a peer invited
// by address, including a typed host:port form and the node's other addresses.
func TestAddressMatches(t *testing.T) {
	row := nodeRow{
		name:      "host-a",
		address:   "10.0.0.7",
		addresses: []string{"10.0.0.7", "192.168.1.9"},
	}
	test := func(name, in string, want bool) {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, addressMatches(row, in))
		})
	}
	test("primary address", "10.0.0.7", true)
	test("address with port", "10.0.0.7:14321", true)
	test("secondary address", "192.168.1.9", true)
	test("case insensitive hostname", "HOST-A", true)
	test("empty", "", false)
	test("different address", "10.0.0.8", false)
	test("different hostname", "other-host", false)
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

	assert.Nil(t, v.removeSelected(), "removal was dispatched without confirmation")
	require.Equal(t, "peer", v.confirmRemove, "selected node")

	// Any other key cancels.
	v.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	assert.Empty(t, v.confirmRemove, "a non-confirming key left the removal armed")

	// Re-arm and confirm.
	v.removeSelected()
	assert.NotNil(t, v.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}), "confirmation produced no removal command")
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
	assert.Contains(t, v.View(), "Discovery is browsing", "a quiet network does not read as one")

	v.Update(clusterMembersMsg{err: errors.New("worker not running")})
	got := v.View()
	assert.NotContains(t, got, "Discovery is browsing", "a failed read still claims discovery is simply looking")
	assert.Contains(t, got, "cluster members", "the failing feed is named")
	assert.Contains(t, got, "worker not running", "the reason is shown")
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
	assert.Contains(t, v.View(), "manual nodes", "a populated table hides that a feed is missing")
}

// TestNodesViewClearsFeedWarningOnRecovery checks the warning is a live
// condition, not a permanent mark: a feed that starts working again stops
// being reported.
func TestNodesViewClearsFeedWarningOnRecovery(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(100, 30)

	v.Update(manualNodesMsg{err: errors.New("transient")})
	require.NotEmpty(t, v.feedWarning(), "failure was not recorded")

	v.Update(manualNodesMsg{})
	assert.Empty(t, v.feedWarning(), "warning survived recovery")
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

	require.Len(t, v.rows, 2, "unfiltered rows")

	v.filter = "alpha"
	v.rebuild()

	if assert.Len(t, v.rows, 1, "filtered rows") {
		assert.Equal(t, "alpha", v.rows[0].name, "filtered row")
	}
	assert.Len(t, v.all, 2, "the filter dropped nodes from the full set")
	assert.Contains(t, v.clusterLine(), "2", "member count followed the filter instead of the cluster")
	assert.Contains(t, v.View(), "showing 1 of 2", "a filtered table does not say it is filtered")
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

	assert.Empty(t, v.invitedKey, "a peer that joined while filtered out left its invite pending")
	assert.NotContains(t, v.status.render(), "123456", "the PIN is still on screen after the hidden peer joined")
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
	require.NotNil(t, press(inviteKey), "the first invite was not sent")

	// While the first is in flight, neither path may start another.
	v.selectedKey = "b"
	v.restoreSelection()
	assert.Nil(t, press(inviteKey), "a second invite was sent while the first was awaiting its reply")
	press(addrKey)
	assert.NotEqual(t, nodesInputInviteAddress, v.mode, "invite-by-address opened while an invite was awaiting its reply")

	// Once the PIN is showing, refusing must not take it off the screen.
	v.Update(nodeInviteMsg{name: "alpha", inviteID: "inv-a", pin: "123456"})
	assert.Nil(t, press(inviteKey), "a second invite was sent while the first PIN was still open")
	assert.Contains(t, v.status.render(), "123456", "refusing the second invite hid the first one's PIN")
	assert.Equal(t, "a", v.invitedKey, "pending invite target")
	assert.Equal(t, "inv-a", v.outboundInviteID, "pending invite id")

	// Cancelling frees the way.
	press(nodeCancelKey.Help().Key)
	assert.NotNil(t, press(inviteKey), "an invite could not be sent after the pending one was cancelled")
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

	if assert.Len(t, v.feeds.members, 1, "pushed roster is kept") {
		assert.Equal(t, "now", v.feeds.members[0].NodeUUID, "pushed roster is kept")
	}

	// A read sent after the push is current, and is taken.
	fresh := clusterMembersMsg{nodes: []clusterNode{{NodeUUID: "later", State: "member"}}, pushes: v.membersPushes}
	v.Update(fresh)
	if assert.Len(t, v.feeds.members, 1, "read sent after the push") {
		assert.Equal(t, "later", v.feeds.members[0].NodeUUID, "read sent after the push")
	}
}

// TestMalformedPINKeepsTheRequest is the regression guard for a pairing request
// lost to a typo. The cluster manager refuses a PIN that is not six digits with
// an error and leaves the session open, but the prompt was cleared before the
// answer went out, so there was nothing left to answer again.
func TestMalformedPINKeepsTheRequest(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(120, 30)
	v.Update(inviteReceived(t, "inv-1", "peer"))

	require.NotNil(t, v.respondToInvite(true, "12345"), "the answer was not sent")
	require.NotNil(t, v.inbound, "the request left the screen before it was settled")
	assert.Nil(t, v.respondToInvite(true, "123456"), "a second answer was sent while the first was in flight")

	v.Update(pairingResultMsg{inviteID: "inv-1", from: "peer", err: errors.New("pin must be six digits")})
	require.NotNil(t, v.inbound, "an answer the cluster manager refused took the request away")
	require.Equal(t, "inv-1", v.inbound.InviteID, "an answer the cluster manager refused took the request away")
	assert.Contains(t, v.inboundPrompt(), "to accept", "the request cannot be answered again")

	// A settled answer does take it away.
	v.respondToInvite(true, "123456")
	v.Update(pairingResultMsg{inviteID: "inv-1", from: "peer", state: inviteStatePaired})
	assert.Nil(t, v.inbound, "a settled request stayed on screen")
}

// TestInboundRequestsWaitTheirTurn is the regression guard for a pairing
// request being replaced by the next. Both are live sessions another machine
// is waiting on; the newer one used to overwrite the one being read, leaving
// no way back to it.
func TestInboundRequestsWaitTheirTurn(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(120, 30)
	v.Update(inviteReceived(t, "first", "alpha"))
	v.Update(inviteReceived(t, "second", "beta"))
	v.Update(inviteReceived(t, "second", "beta")) // delivered twice

	require.Equal(t, "first", v.inbound.InviteID, "the newer request replaced the one on screen")
	assert.Contains(t, v.inboundPrompt(), "1 more waiting", "the prompt says another request is waiting")

	v.Update(inviteEvent(t, "cluster:invite-expired", "first"))
	require.NotNil(t, v.inbound, "the waiting request did not come up next")
	require.Equal(t, "second", v.inbound.InviteID, "the waiting request did not come up next")
	assert.NotContains(t, v.inboundPrompt(), "more waiting", "a request delivered twice was queued twice")
}

// TestDroppedRequestTakesItsPINWithIt is the regression guard for a PIN typed
// for one request answering the next. The request on screen expired while its
// PIN field was open, the next one came up under the same field, and enter
// sent the first request's PIN as the answer to the second.
func TestDroppedRequestTakesItsPINWithIt(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(120, 30)
	v.Update(inviteReceived(t, "first", "alpha"))
	v.Update(inviteReceived(t, "second", "beta"))

	v.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	v.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1234")})
	require.Equal(t, nodesInputPin, v.mode, "the PIN field did not take the digits")
	require.Equal(t, "1234", v.input.Value(), "the PIN field did not take the digits")

	v.Update(inviteEvent(t, "cluster:invite-expired", "first"))
	require.NotNil(t, v.inbound, "the waiting request did not come up next")
	require.Equal(t, "second", v.inbound.InviteID, "the waiting request did not come up next")
	assert.NotEqual(t, nodesInputPin, v.mode, "the PIN field outlived its request")
	assert.Empty(t, v.input.Value(), "the PIN field outlived its request")
	v.Update(tea.KeyMsg{Type: tea.KeyEnter})
	assert.Empty(t, v.answering, "enter answered with a PIN typed for another request")
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
				assert.NoError(t, json.Unmarshal(req.Params, &a))
				answers <- a
			}
			assert.NoError(t, broker.Write(&rpc.Message{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{}`)}))
		}
	}()

	v := newNodesView(client)
	v.SetSize(120, 30)
	v.Update(inviteReceived(t, "a", "alpha"))
	v.Update(inviteReceived(t, "b", "beta"))
	v.Update(inviteReceived(t, "c", "gamma"))

	cmd := v.Update(pairingResultMsg{inviteID: "a", from: "alpha", state: inviteStatePaired})
	require.Nil(t, v.inbound, "requests are still waiting after joining")
	require.Empty(t, v.queued, "requests are still waiting after joining")
	got := v.status.render()
	assert.Contains(t, got, "paired with alpha", "pairing outcome")
	assert.Contains(t, got, "declined 2 other", "the outcome says the others were declined")

	batch, ok := cmd().(tea.BatchMsg)
	require.True(t, ok, "joining issued no commands")
	for _, c := range batch {
		go c()
	}
	declined := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(declined) < 2 {
		select {
		case a := <-answers:
			if assert.NotNil(t, a.Accept, "request %q must be declined", a.InviteID) {
				assert.False(t, *a.Accept, "request %q must be declined", a.InviteID)
			}
			declined[a.InviteID] = true
		case <-deadline:
			require.FailNowf(t, "timed out waiting for declines", "only %v were declined", declined)
		}
	}
	assert.Contains(t, declined, "b", "decline the two still waiting")
	assert.Contains(t, declined, "c", "decline the two still waiting")
	assert.NotContains(t, declined, "a", "decline the two still waiting")
}

// TestJoiningWithNothingWaitingDeclinesNothing checks the ordinary case still
// reads as it did: one request, accepted, and nothing else said.
func TestJoiningWithNothingWaitingDeclinesNothing(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(120, 30)
	v.Update(inviteReceived(t, "a", "alpha"))
	v.Update(pairingResultMsg{inviteID: "a", from: "alpha", state: inviteStatePaired})
	got := v.status.render()
	assert.Contains(t, got, "paired with alpha", "lone accept")
	assert.NotContains(t, got, "declined", "lone accept")
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
	assert.Contains(t, v.View(), "PIN 123456", "a live PIN is hidden behind the detail screen")

	v.Update(inviteReceived(t, "in", "other peer"))
	got := v.View()
	assert.Contains(t, got, "pairing request from other peer", "an inbound request is hidden behind the detail screen")
	assert.LessOrEqual(t, renderedRows(got), height, "pairing line stays within the frame")
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
	require.NotNil(t, cmd, "cancel was not sent")
	require.Empty(t, v.outboundInviteID, "the PIN stayed tracked while cancel was sent")

	v.Update(inviteCancelledMsg{invite: sentInvite{id: "inv", name: "peer", pin: "123456"}, err: errFake{}})
	require.Equal(t, "inv", v.outboundInviteID, "a failed cancel left the still-live invite untracked")
	assert.Contains(t, v.status.render(), "123456", "the still-live PIN is shown again")
	assert.NotNil(t, v.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(nodeCancelKey.Help().Key)}), "the cancel cannot be tried again")
}

// TestRejectionAdviceMatchesTheReason checks a refused invite only suggests
// the remedy for the reason actually given. "Remove the existing relationship
// first" was attached to every rejection, including ones with no relationship
// to remove.
func TestRejectionAdviceMatchesTheReason(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(120, 30)

	v.Update(nodeInviteMsg{name: "peer", rejected: true, reason: reasonAlreadyClustered})
	assert.Contains(t, v.status.render(), "leave that cluster", "an already-clustered peer is told what to do")

	test := func(name, reason string) {
		t.Run(name, func(t *testing.T) {
			v.Update(nodeInviteMsg{name: "peer", rejected: true, reason: reason})
			got := v.status.render()
			assert.NotContains(t, got, "leave that cluster", "advice meant for an already-clustered peer")
			assert.NotContains(t, got, "relationship", "advice meant for an already-clustered peer")
			assert.Contains(t, got, "rejected the invite", "rejection is reported")
		})
	}
	test("no reason", "")
	test("unrecognised reason", "evil-arbitrary-text")
}

// TestInviteThatDidNotGoOutIsNotReportedSent is the regression guard for a
// failed invite read as a sent one. The manager answers "failed", with no PIN,
// when the first exchange does not complete, and anything short of "rejected"
// was reported as "invite sent" and left pending.
func TestInviteThatDidNotGoOutIsNotReportedSent(t *testing.T) {
	test := func(name, state string) {
		t.Run(name, func(t *testing.T) {
			v := newNodesView(nil)
			v.SetSize(120, 30)
			v.invitedKey, v.inviteSending = "peer-uuid", true
			v.Update(inviteResultMsg("peer", "", inviteNodeResult{InviteID: "inv", State: state}, nil))
			got := v.status.render()
			assert.NotContains(t, got, "invite sent")
			assert.Contains(t, got, "did not go out")
			assert.Empty(t, v.invitedKey, "invite must not remain pending")
			assert.Empty(t, v.outboundInviteID, "invite must not remain pending")
		})
	}
	test("failed", "failed")
	test("canceled", "canceled")

	pin := "123456"
	v := newNodesView(nil)
	v.SetSize(120, 30)
	v.Update(inviteResultMsg("peer", "", inviteNodeResult{InviteID: "inv", State: "pending", Pin: &pin}, nil))
	assert.Contains(t, v.status.render(), "PIN 123456", "a pending invite shows its PIN")
}

// TestCancelInviteClearsThePinImmediately checks the inviter's half of decline.
// Without it a PIN read to the wrong person could only be retired by waiting
// for it to expire, staying answerable the whole time.
func TestCancelInviteClearsThePinImmediately(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(100, 30)
	v.Update(nodeInviteMsg{name: "peer", inviteID: "inv-1", pin: "123456"})
	require.Contains(t, v.status.render(), "123456", "PIN was not pinned")

	assert.NotNil(t, v.cancelInvite(), "cancelling produced no request")
	assert.Empty(t, v.outboundInviteID, "the invite is still tracked after cancelling")
	assert.NotContains(t, v.status.render(), "123456", "the PIN is still displayed after cancelling; it must stop being readable at once")

	// Nothing pending is a no-op, not an error.
	assert.Nil(t, v.cancelInvite(), "cancelling with no invite pending still sent a request")
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
	assert.NotContains(t, got, " ok", "a rejected pairing reported success")
	assert.Contains(t, got, "wrong PIN", "status says the PIN was wrong")
	assert.Contains(t, got, "new invite", "status says what to do next; the PIN is single-use")
}

// TestPairingOutcomesAreDistinguished checks each terminal state gets its own
// answer, since they call for different things from the operator.
func TestPairingOutcomesAreDistinguished(t *testing.T) {
	test := func(name, state, reason, want string) {
		t.Run(name, func(t *testing.T) {
			v := newNodesView(nil)
			v.SetSize(100, 30)
			v.Update(pairingResultMsg{from: "peer", state: state, reason: reason})
			assert.Contains(t, v.status.render(), want)
		})
	}
	test("paired", "paired", "", "paired with peer")
	test("incorrect PIN", "failed", reasonIncorrectPIN, "wrong PIN")
	test("unreachable", "failed", "unreachable", "failed")
	test("declined", "declined", "", "declined")
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
	v.Update(inviteReceived(t, "inv-1", "peer"))
	require.Contains(t, v.inboundPrompt(), "to accept", "no prompt after a pairing request arrived")

	v.Update(inviteEvent(t, "cluster:invite-expired", "inv-1"))

	assert.Nil(t, v.inbound, "expired inbound invite is still pending; accept would target a dead invite")
	assert.NotContains(t, v.inboundPrompt(), "to accept", "still offering accept/decline for an expired invite")
	assert.NotNil(t, v.Help(), "help bindings unexpectedly nil")
	for _, b := range v.Help() {
		if b.Help().Key == accept && b.Help().Desc == nodePairKey.Help().Desc {
			assert.Fail(t, "accept-pairing key still advertised with no pending invite")
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
	got := v.selectedRow()
	require.NotNil(t, got, "selection did not settle on the requested node")
	require.Equal(t, "z", got.key, "selection did not settle on the requested node")

	// A new node sorting ahead of the selection must not steal the cursor.
	v.feeds.discovered = append(v.feeds.discovered,
		availableNode{HostUUID: "m", Name: "mmm", LastSeen: time.Now().Unix()})
	v.rebuild()

	got = v.selectedRow()
	if assert.NotNil(t, got, "selection moved after the list grew") {
		assert.Equal(t, "z", got.key, "selection moved after the list grew")
	}
}

// TestNodesViewGuardsInviteOnEveryPath is the regression guard for re-inviting a
// node that already has a relationship. The old split tabs guarded the
// discovered-node path only, so the by-address path could still send a doomed
// invite.
func TestNodesViewGuardsInviteOnEveryPath(t *testing.T) {
	test := func(name string, node availableNode) {
		t.Run(name, func(t *testing.T) {
			v := newNodesView(nil)
			v.feeds.discovered = []availableNode{node}
			v.rebuild()
			v.selectedKey = "k"

			assert.Nil(t, v.inviteSelected(), "invite was dispatched for a node that cannot accept one")
			assert.Empty(t, v.invitedKey, "invite recorded as pending despite being blocked")
			assert.NotEmpty(t, v.status.render(), "invite blocked with no explanation to the operator")
		})
	}
	test("already a member of our cluster", availableNode{HostUUID: "k", Name: "peer", Trusted: true})
	test("a member of another cluster", availableNode{HostUUID: "k", Name: "peer", Clustered: true})
}

// TestNodesViewRefusesSelfInvite checks this machine cannot be invited to its
// own cluster.
func TestNodesViewRefusesSelfInvite(t *testing.T) {
	v := newNodesView(nil)
	v.feeds.discovered = []availableNode{{HostUUID: "me", Name: "this", LastSeen: time.Now().Unix()}}
	v.feeds.selfUUID = "me"
	v.rebuild()
	v.selectedKey = "me"

	assert.Nil(t, v.inviteSelected(), "dispatched an invite to this machine")
}

// assert the views used above still satisfy the interface the shell drives.
var _ View = (*nodesView)(nil)
var _ inputCapturer = (*nodesView)(nil)
var _ tea.Model = Model{}
