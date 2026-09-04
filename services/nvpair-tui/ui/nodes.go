// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"strconv"
	"time"

	"nvpair-tui/pairing"
	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
)

// availableNode mirrors the broker's discovery boundary shape (the
// discovery:get-nodes element and discovery:nodes-changed payload entry).
type availableNode struct {
	ID        string `json:"id"`
	HostUUID  string `json:"hostUuid"`
	Name      string `json:"name"`
	IPAddress string `json:"ipAddress"`
	Port      int    `json:"port"`
	LastSeen  int64  `json:"lastSeen"` // Unix seconds
	// Trusted: this node is a paired cluster peer of ours. Clustered: it belongs
	// to some cluster (advertises a cluster-uuid), whether or not we're paired
	// with it. Either one makes it non-invitable — an already-clustered peer
	// rejects a fresh pairing (it must leave/be removed first).
	Trusted   bool `json:"trusted"`
	Clustered bool `json:"clustered"`
}

// key is the node's stable identity (hostUuid) for cluster actions; the name
// column is display only. Every discovered node carries a hostUuid, so there is
// no id fallback.
func (n availableNode) key() string {
	return n.HostUUID
}

// nodesView shows mDNS-discovered Ollama nodes. It subscribes to the
// discovery stream on start; the broker replays a baseline snapshot on
// subscribe and then pushes discovery:nodes-changed (a full list) on
// every change.
type nodesView struct {
	client *rpc.Client
	// pairs is the shared pairing service, so an invite sent from this tab is
	// the same call the Cluster tab and the control socket make.
	pairs         *pairing.Service
	table         table.Model
	nodes         []availableNode
	status        string
	width, height int
}

type discoverySubscribedMsg struct{ err error }

// nodeInviteMsg carries the outcome of a cluster:invite-node fired from the
// Nodes tab: the PIN to read to the joining node, an explicit rejection from an
// already-clustered peer, or the failure to surface.
type nodeInviteMsg struct {
	name     string
	pin      string
	rejected bool
	reason   string
	err      error
}

var niInviteKey = key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "invite to cluster"))

func newNodesView(client *rpc.Client, pairs *pairing.Service) *nodesView {
	v := &nodesView{client: client, pairs: pairs}
	v.table = newTable(nil)
	return v
}

func (v *nodesView) Title() string { return "Nodes" }

func (v *nodesView) Init() tea.Cmd {
	return call(v.client, "discovery:subscribe", nil, func(_ *rpc.Message, err error) tea.Msg {
		return discoverySubscribedMsg{err: err}
	})
}

func (v *nodesView) SetSize(w, h int) {
	v.width, v.height = w, h
	const port, age, status = 7, 8, 11
	name := clampWidth((w-port-age-status-2)/2, 10)
	addr := clampWidth(w-port-age-status-name-2, 10)
	v.table.SetColumns([]table.Column{
		{Title: "NAME", Width: name},
		{Title: "ADDRESS", Width: addr},
		{Title: "PORT", Width: port},
		{Title: "SEEN", Width: age},
		{Title: "STATUS", Width: status},
	})
	v.table.SetWidth(w)
	v.table.SetHeight(clampWidth(h-1, 1))
}

func (v *nodesView) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case discoverySubscribedMsg:
		if msg.err != nil {
			v.status = "discovery subscribe failed: " + msg.err.Error()
		}
		return nil

	case NotificationMsg:
		if msg.Msg.Method == "discovery:nodes-changed" {
			var nodes []availableNode
			_ = decodeParams(msg.Msg.Params, &nodes)
			v.setNodes(nodes)
		}
		return nil

	case nodeInviteMsg:
		if msg.err != nil {
			v.status = "invite failed: " + msg.err.Error()
		} else if msg.rejected {
			v.status = fmt.Sprintf("%s rejected the invite (%s) - remove the existing relationship first", msg.name, rejectReason(msg.reason))
		} else if msg.pin != "" {
			v.status = fmt.Sprintf("invite sent to %s - PIN %s (read it to that node)", msg.name, msg.pin)
		} else {
			v.status = "invite sent to " + msg.name
		}
		return nil

	case tea.KeyMsg:
		if key.Matches(msg, niInviteKey) {
			return v.inviteSelected()
		}
		var cmd tea.Cmd
		v.table, cmd = v.table.Update(msg)
		return cmd
	}
	return nil
}

// inviteSelected sends a cluster invite to the highlighted node. The node's
// discovery IP is the dial target; the manager appends the fixed cluster-manager
// port (14321), so we deliberately do NOT pass the row's Port (that's the
// discovery/Ollama port). The nodeId is passed too so the manager stamps it on
// the invite as the target identity. A PIN comes back to read to the joiner.
func (v *nodesView) inviteSelected() tea.Cmd {
	idx := v.table.Cursor()
	if idx < 0 || idx >= len(v.nodes) {
		v.status = "no node selected"
		return nil
	}
	n := v.nodes[idx]
	// A paired or otherwise-clustered node can't be invited again: the peer
	// rejects a fresh pairing until the existing relationship is removed. Guard
	// here so we don't fire a doomed invite (the cluster-manager rejects it too).
	if n.Trusted {
		v.status = n.Name + " is already paired (Connected) - remove it first to re-pair"
		return nil
	}
	if n.Clustered {
		v.status = n.Name + " is already in a cluster - it must leave before it can pair"
		return nil
	}
	// Identify the invite target by its stable UUID (the address is still the
	// dial target); the manager stamps this as the invite's target identity.
	req := pairing.InviteRequest{Address: n.IPAddress, NodeID: n.key()}
	name := n.Name
	v.status = "inviting " + name + "..."
	return inviteCmd(v.pairs, req, func(inv pairing.Invite, err error) tea.Msg {
		if err != nil {
			return nodeInviteMsg{name: name, err: err}
		}
		if inv.State == pairing.StateRejected {
			return nodeInviteMsg{name: name, rejected: true, reason: inv.Reason}
		}
		return nodeInviteMsg{name: name, pin: inv.PIN()}
	})
}

func (v *nodesView) setNodes(nodes []availableNode) {
	v.nodes = nodes
	rows := make([]table.Row, 0, len(nodes))
	for _, n := range nodes {
		rows = append(rows, table.Row{
			n.Name,
			n.IPAddress,
			strconv.Itoa(n.Port),
			ageUnix(n.LastSeen),
			nodeStatus(n),
		})
	}
	v.table.SetRows(rows)
}

// nodeStatus is the STATUS cell for a discovered node: "Connected" for a paired
// peer of ours, "In cluster" for a node clustered elsewhere (both non-invitable),
// empty for an invitable standalone node.
func nodeStatus(n availableNode) string {
	switch {
	case n.Trusted:
		return "Connected"
	case n.Clustered:
		return "In cluster"
	default:
		return ""
	}
}

// rejectReason renders a machine reason from a rejected invite as human text.
func rejectReason(reason string) string {
	switch reason {
	case "already-clustered":
		return "already in a cluster"
	case "":
		return "rejected by peer"
	default:
		return reason
	}
}

func (v *nodesView) View() string {
	var body string
	if len(v.nodes) == 0 {
		body = footerStyle.Render("No nodes discovered yet. Browsing for _nvpair-node-info._tcp ...")
	} else {
		body = v.table.View()
	}
	if v.status != "" {
		body += "\n" + footerStyle.Render(v.status)
	}
	return body
}

func (v *nodesView) Help() []key.Binding { return []key.Binding{niInviteKey} }

// ageUnix renders a Unix-seconds timestamp as a compact relative age.
func ageUnix(sec int64) string {
	if sec == 0 {
		return "-"
	}
	d := time.Since(time.Unix(sec, 0))
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}
