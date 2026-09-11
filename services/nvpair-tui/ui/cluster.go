// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"strconv"
	"strings"

	"nvpair-tui/pairing"
	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// clusterIdentity is this node's principal, from cluster:get-node-id.
type clusterIdentity struct {
	NodeUUID  string `json:"nodeUuid"`
	NodeID    string `json:"nodeId"`
	Name      string `json:"name"`
	ClusterID string `json:"clusterId"`
}

// clusterNode mirrors nvpair-cluster-manager's ClusterNode (a member or
// pending invitee), the element of nodes:get-initial / nodes:changed.
type clusterNode struct {
	ID        string `json:"id"`
	NodeUUID  string `json:"nodeUuid"`
	Name      string `json:"name"`
	IPAddress string `json:"ipAddress"`
	Port      int    `json:"port"`
	State     string `json:"state"`
}

type clusterInputMode int

const (
	clusterInputNone clusterInputMode = iota
	clusterInputAddress
	clusterInputPin
)

// clusterView drives node pairing and membership: identity, the member
// roster (live from nodes:changed), outbound invites (showing the PIN to
// read to the joiner), and inbound invites (entering the PIN to accept).
type clusterView struct {
	client *rpc.Client
	// pairs is the process-wide pairing service. It owns which invites are
	// waiting and it is what the control socket drives too, which is how an
	// invite created or answered from a script shows up on this tab.
	pairs    *pairing.Service
	events   <-chan pairing.Event
	table    table.Model
	identity clusterIdentity
	nodes    []clusterNode
	input    textinput.Model
	mode     clusterInputMode
	status   string

	width, height int
}

type clusterIdentityMsg struct {
	id  clusterIdentity
	err error
}

type clusterNodesMsg struct {
	nodes []clusterNode
	err   error
}

// clusterActionMsg carries the outcome of a non-pairing cluster action fired
// from this tab (remove a member, leave the cluster). A pairing outcome
// arrives as a pairing event instead, so that one driven from the control
// socket is reported here identically to one driven from the keyboard.
type clusterActionMsg struct {
	what string
	err  error
}

var (
	clInviteKey  = key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "invite node"))
	clRemoveKey  = key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "remove member"))
	clAcceptKey  = key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "accept invite"))
	clDeclineKey = key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "decline invite"))
	clLeaveKey   = key.NewBinding(key.WithKeys("L"), key.WithHelp("L", "leave cluster"))
)

func newClusterView(client *rpc.Client, pairs *pairing.Service) *clusterView {
	ti := textinput.New()
	events, _ := pairs.Subscribe()
	v := &clusterView{client: client, pairs: pairs, events: events, input: ti}
	v.table = newTable(nil)
	return v
}

func (v *clusterView) Title() string { return "Cluster" }

func (v *clusterView) Init() tea.Cmd {
	return tea.Batch(v.identityCmd(), v.nodesCmd(), waitForPairingEvent(v.events))
}

func (v *clusterView) identityCmd() tea.Cmd {
	return call(v.client, "cluster:get-node-id", nil, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return clusterIdentityMsg{err: err}
		}
		var id clusterIdentity
		_ = decodeParams(msg.Result, &id)
		return clusterIdentityMsg{id: id}
	})
}

func (v *clusterView) nodesCmd() tea.Cmd {
	return call(v.client, "nodes:get-initial", nil, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return clusterNodesMsg{err: err}
		}
		var r struct {
			Nodes []clusterNode `json:"nodes"`
		}
		_ = decodeParams(msg.Result, &r)
		return clusterNodesMsg{nodes: r.Nodes}
	})
}

func (v *clusterView) SetSize(w, h int) {
	v.width, v.height = w, h
	const state, port = 12, 7
	id := clampWidth((w-state-port-2)/2, 8)
	name := clampWidth(w-state-port-id-2, 10)
	v.table.SetColumns([]table.Column{
		{Title: "ID", Width: id},
		{Title: "NAME", Width: name},
		{Title: "STATE", Width: state},
		{Title: "PORT", Width: port},
	})
	v.table.SetWidth(w)
	v.table.SetHeight(clampWidth(h-7, 1))
}

func (v *clusterView) CapturingInput() bool { return v.mode != clusterInputNone }

func (v *clusterView) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case clusterIdentityMsg:
		if msg.err == nil {
			v.identity = msg.id
		}
		return nil
	case clusterNodesMsg:
		if msg.err == nil {
			v.setNodes(msg.nodes)
		}
		return nil
	case clusterActionMsg:
		if msg.err != nil {
			v.status = msg.what + " failed: " + msg.err.Error()
		} else {
			v.status = msg.what + " ok"
		}
		return nil
	case pairingEventMsg:
		v.applyPairingEvent(msg.Event)
		return waitForPairingEvent(v.events)
	case NotificationMsg:
		return v.handleNotification(msg.Msg)
	case tea.KeyMsg:
		return v.handleKey(msg)
	}
	return nil
}

func (v *clusterView) handleNotification(msg *rpc.Message) tea.Cmd {
	switch msg.Method {
	case "nodes:changed":
		var r struct {
			Nodes []clusterNode `json:"nodes"`
		}
		_ = decodeParams(msg.Params, &r)
		v.setNodes(r.Nodes)
	case "cluster:identity-changed":
		var r struct {
			ClusterID string `json:"clusterId"`
		}
		_ = decodeParams(msg.Params, &r)
		v.identity.ClusterID = r.ClusterID
	}
	// The pairing pushes (cluster:invite-received and the invite-canceled /
	// -expired that retract one) are folded into the pairing service by the
	// process's notification fan-out; this tab hears about them as events.
	return nil
}

func (v *clusterView) handleKey(msg tea.KeyMsg) tea.Cmd {
	if v.mode != clusterInputNone {
		switch msg.String() {
		case "enter":
			return v.submit()
		case "esc":
			v.cancelInput()
			return nil
		}
		var cmd tea.Cmd
		v.input, cmd = v.input.Update(msg)
		return cmd
	}

	switch {
	case key.Matches(msg, clInviteKey):
		v.beginInput(clusterInputAddress, "host (or host:port; default 14321)")
		return textinput.Blink
	case key.Matches(msg, clAcceptKey):
		if len(v.pairs.Pending()) > 0 {
			v.beginInput(clusterInputPin, "PIN from inviting node")
			return textinput.Blink
		}
		return nil
	case key.Matches(msg, clDeclineKey):
		return v.respondToInvite(false, "")
	case key.Matches(msg, clRemoveKey):
		return v.removeSelected()
	case key.Matches(msg, clLeaveKey):
		return v.leaveCluster()
	}
	var cmd tea.Cmd
	v.table, cmd = v.table.Update(msg)
	return cmd
}

func (v *clusterView) beginInput(mode clusterInputMode, placeholder string) {
	v.mode = mode
	v.input.SetValue("")
	v.input.Placeholder = placeholder
	v.input.Focus()
}

func (v *clusterView) cancelInput() {
	v.mode = clusterInputNone
	v.input.Blur()
}

func (v *clusterView) submit() tea.Cmd {
	val := strings.TrimSpace(v.input.Value())
	mode := v.mode
	v.cancelInput()
	switch mode {
	case clusterInputAddress:
		if val == "" {
			v.status = "address required"
			return nil
		}
		// The pairing service splits a typed "host:port" into the manager's
		// separate address and port fields; a bare host gets the manager's own
		// default port appended.
		v.status = "inviting " + val + "..."
		return inviteCmd(v.pairs, pairing.InviteRequest{Address: val}, func(pairing.Invite, error) tea.Msg {
			// The status line is written from the pairing event, so that an
			// invite from the control socket reads the same as this one.
			return nil
		})
	case clusterInputPin:
		return v.respondToInvite(true, val)
	}
	return nil
}

// respondToInvite answers the invite this tab is showing. Which one that is
// comes from the pairing service, so the keyboard and the control socket agree
// on it even when several arrived.
func (v *clusterView) respondToInvite(accept bool, pin string) tea.Cmd {
	waiting := v.pairs.Pending()
	if len(waiting) == 0 {
		v.status = "no invite is waiting for an answer"
		return nil
	}
	return respondCmd(v.pairs, waiting[0].InviteID, accept, pin)
}

// applyPairingEvent renders one pairing state change on the status line,
// whichever driver caused it. An invite created over the control socket shows
// its PIN here exactly like one created with `i`, and an accept made there
// clears a PIN prompt this tab left open.
func (v *clusterView) applyPairingEvent(ev pairing.Event) {
	switch ev.Kind {
	case pairing.EventInviteSent:
		v.status = fmt.Sprintf("invite sent - PIN %s (read it to the joining node)", ev.Invite.PIN())
	case pairing.EventInviteRejected:
		v.status = fmt.Sprintf("invite rejected (%s) - remove the existing relationship first", rejectReason(ev.Invite.Reason))
	case pairing.EventInviteFailed:
		v.status = "invite failed: " + ev.Err.Error()
	case pairing.EventInviteReceived:
		v.status = "invite received from " + ev.Invite.FromNodeName + " - press a to accept, d to decline"
	case pairing.EventInviteCleared:
		v.status = fmt.Sprintf("invite from %s %s", ev.Invite.FromNodeName, ev.Invite.State)
		v.closePinPromptIfIdle()
	case pairing.EventResponded:
		v.status = respondedStatus(ev)
		v.closePinPromptIfIdle()
	case pairing.EventRespondFailed:
		what := "decline invite"
		if ev.Accept {
			what = "accept invite"
		}
		v.status = what + " failed: " + ev.Err.Error()
		v.closePinPromptIfIdle()
	}
}

// closePinPromptIfIdle dismisses a PIN prompt once nothing is waiting for an
// answer any more — the case where a script accepted or declined the very
// invite the operator was still typing a PIN for.
func (v *clusterView) closePinPromptIfIdle() {
	if v.mode == clusterInputPin && len(v.pairs.Pending()) == 0 {
		v.cancelInput()
	}
}

// respondedStatus words the outcome of an answered invite. A wrong PIN is a
// successful response in a failed state rather than an error, so it is
// reported here and not on the failure path.
func respondedStatus(ev pairing.Event) string {
	if !ev.Accept {
		return "invite declined"
	}
	switch ev.Invite.State {
	case pairing.StatePaired:
		name := ev.Invite.FromNodeName
		if name == "" {
			name = "the inviting node"
		}
		return "paired with " + name
	case pairing.StateFailed:
		if ev.Invite.Reason == pairing.ReasonIncorrectPin {
			return "incorrect PIN - ask for it again and retry"
		}
		return "pairing failed"
	default:
		return "accept invite: " + ev.Invite.State
	}
}

// leaveCluster unjoins this node from its cluster (cluster:leave). The
// cluster-manager tears down local trust and pushes cluster:identity-changed
// (empty) + nodes:changed (empty), which refresh the view; the broker persists
// the now-unclustered state so the node stays out after a restart.
func (v *clusterView) leaveCluster() tea.Cmd {
	if v.identity.ClusterID == "" {
		v.status = "not in a cluster"
		return nil
	}
	return call(v.client, "cluster:leave", nil, func(_ *rpc.Message, err error) tea.Msg {
		return clusterActionMsg{what: "leave cluster", err: err}
	})
}

func (v *clusterView) removeSelected() tea.Cmd {
	idx := v.table.Cursor()
	if idx < 0 || idx >= len(v.nodes) {
		return nil
	}
	n := v.nodes[idx]
	// Remove by the stable nodeUuid, not the display name: a member that renamed
	// its PC still carries the same UUID, so keying on the (possibly stale) shown
	// name would silently fail to match. Fall back to nodeId only if the
	// manager didn't supply a UUID.
	params := map[string]string{}
	if n.NodeUUID != "" {
		params["nodeUuid"] = n.NodeUUID
	} else {
		params["nodeId"] = n.ID
	}
	return call(v.client, "nodes:remove", params, func(_ *rpc.Message, err error) tea.Msg {
		return clusterActionMsg{what: "remove " + n.ID, err: err}
	})
}

func (v *clusterView) setNodes(nodes []clusterNode) {
	v.nodes = nodes
	rows := make([]table.Row, 0, len(nodes))
	for _, n := range nodes {
		rows = append(rows, table.Row{
			truncate(n.ID, 14),
			n.Name,
			n.State,
			strconv.Itoa(n.Port),
		})
	}
	v.table.SetRows(rows)
}

func (v *clusterView) View() string {
	var b strings.Builder
	cluster := v.identity.ClusterID
	if cluster == "" {
		cluster = "(none - invite a node to form one)"
	}
	b.WriteString(titleStyle.Render("This node"))
	b.WriteByte('\n')
	b.WriteString(fmt.Sprintf("  name=%s  nodeId=%s\n", v.identity.Name, truncate(v.identity.NodeID, 24)))
	b.WriteString("  cluster=" + cluster + "\n\n")

	b.WriteString(titleStyle.Render("Members"))
	b.WriteByte('\n')
	if len(v.nodes) == 0 {
		b.WriteString(footerStyle.Render("No members."))
	} else {
		b.WriteString(v.table.View())
	}

	if v.mode != clusterInputNone {
		b.WriteString("\n" + v.input.View())
	}
	if v.status != "" {
		b.WriteString("\n" + footerStyle.Render(v.status))
	}
	return b.String()
}

func (v *clusterView) Help() []key.Binding {
	return []key.Binding{clInviteKey, clAcceptKey, clDeclineKey, clRemoveKey, clLeaveKey}
}
