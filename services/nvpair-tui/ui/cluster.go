// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"net"
	"strconv"
	"strings"

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

// clusterInvite is the broker-facing view of a pairing session
// (cluster:invite-received push / cluster:invite-node result).
type clusterInvite struct {
	InviteID     string  `json:"inviteId"`
	FromNodeName string  `json:"fromNodeName"`
	Pin          *string `json:"pin"`
	State        string  `json:"state"`
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
	client   *rpc.Client
	table    table.Model
	identity clusterIdentity
	nodes    []clusterNode
	pending  *clusterInvite // most recent inbound invite awaiting a response
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

type clusterActionMsg struct {
	what     string
	pin      string
	rejected bool
	reason   string
	err      error
}

var (
	clInviteKey  = key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "invite node"))
	clRemoveKey  = key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "remove member"))
	clAcceptKey  = key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "accept invite"))
	clDeclineKey = key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "decline invite"))
	clLeaveKey   = key.NewBinding(key.WithKeys("L"), key.WithHelp("L", "leave cluster"))
)

func newClusterView(client *rpc.Client) *clusterView {
	ti := textinput.New()
	v := &clusterView{client: client, input: ti}
	v.table = newTable(nil)
	return v
}

func (v *clusterView) Title() string { return "Cluster" }

func (v *clusterView) Init() tea.Cmd {
	return tea.Batch(v.identityCmd(), v.nodesCmd())
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
		} else if msg.rejected {
			v.status = fmt.Sprintf("invite rejected (%s) - remove the existing relationship first", rejectReason(msg.reason))
		} else if msg.pin != "" {
			v.status = fmt.Sprintf("invite sent - PIN %s (read it to the joining node)", msg.pin)
		} else {
			v.status = msg.what + " ok"
		}
		return nil
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
	case "cluster:invite-received":
		var inv clusterInvite
		_ = decodeParams(msg.Params, &inv)
		v.pending = &inv
		v.status = "invite received from " + inv.FromNodeName + " - press a to accept, d to decline"
	}
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
		if v.pending != nil {
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
		// nvpair-cluster-manager treats "address" as a bare host and appends the
		// port itself (default 14321). If the operator typed host:port, split
		// it so the port lands in the manager's separate int field instead of
		// being glued onto the host (which would dial [host:port]:14321).
		params := map[string]any{"address": val}
		if host, portStr, err := net.SplitHostPort(val); err == nil {
			if port, perr := strconv.Atoi(portStr); perr == nil {
				params["address"] = host
				params["port"] = port
			}
		}
		v.status = "inviting " + val + "..."
		return inviteNodeCmd(v.client, params, func(res inviteNodeResult, err error) tea.Msg {
			if err != nil {
				return clusterActionMsg{what: "invite", err: err}
			}
			if res.State == "rejected" {
				return clusterActionMsg{what: "invite", rejected: true, reason: res.Reason}
			}
			pin := ""
			if res.Pin != nil {
				pin = *res.Pin
			}
			return clusterActionMsg{what: "invite", pin: pin}
		})
	case clusterInputPin:
		return v.respondToInvite(true, val)
	}
	return nil
}

func (v *clusterView) respondToInvite(accept bool, pin string) tea.Cmd {
	if v.pending == nil {
		return nil
	}
	params := map[string]any{"inviteId": v.pending.InviteID, "accept": accept}
	if accept && pin != "" {
		params["pin"] = pin
	}
	v.pending = nil
	what := "decline invite"
	if accept {
		what = "accept invite"
	}
	return call(v.client, "cluster:respond-to-invite", params, func(_ *rpc.Message, err error) tea.Msg {
		return clusterActionMsg{what: what, err: err}
	})
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
