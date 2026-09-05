// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"strings"
	"time"

	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// manualRefreshInterval re-lists manual nodes so the probe-driven
// reachability columns stay current (nvpair-manual-nodes re-probes every
// 10s).
const manualRefreshInterval = 10 * time.Second

// manualNode is the subset of nvpair-manual-nodes' ManualNodeStatus shown.
type manualNode struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Address    string `json:"address"`
	OllamaUp   bool   `json:"ollama_up"`
	NodeInfoUp bool   `json:"node_info_up"`
}

// manualView manages user-added nodes that don't appear via mDNS: list,
// add by address, and remove. The list refreshes on a timer to reflect
// the manager's periodic reachability probes.
type manualView struct {
	client        *rpc.Client
	table         table.Model
	nodes         []manualNode
	input         textinput.Model
	adding        bool
	status        string
	width, height int
}

type manualNodesMsg struct {
	nodes []manualNode
	err   error
}

type manualTickMsg struct{}

type manualActionMsg struct {
	what string
	err  error
}

var (
	manualAddKey    = key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "add node"))
	manualRemoveKey = key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "remove node"))
)

func newManualView(client *rpc.Client) *manualView {
	ti := textinput.New()
	// nvpair-manual-nodes appends its own fixed ports (11434 for Ollama, 14318
	// for node-info) to whatever's entered, so a host:port form yields a
	// malformed URL and the node always reads down. Only a bare host works.
	ti.Placeholder = "host"
	v := &manualView{client: client, input: ti}
	v.table = newTable(nil)
	// Seed real columns before any RPC result can reach the table: a
	// zero-column table panics (index out of range) in table.SetRows if
	// data arrives before the first tea.WindowSizeMsg calls SetSize.
	v.SetSize(0, 0)
	return v
}

func (v *manualView) Title() string { return "Manual" }

func (v *manualView) Init() tea.Cmd {
	return tea.Batch(v.listCmd(), v.tickCmd())
}

func (v *manualView) listCmd() tea.Cmd {
	return call(v.client, "nodes/list", nil, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return manualNodesMsg{err: err}
		}
		var r struct {
			Nodes []manualNode `json:"nodes"`
		}
		_ = decodeParams(msg.Result, &r)
		return manualNodesMsg{nodes: r.Nodes}
	})
}

func (v *manualView) tickCmd() tea.Cmd {
	return tea.Tick(manualRefreshInterval, func(time.Time) tea.Msg { return manualTickMsg{} })
}

func (v *manualView) SetSize(w, h int) {
	v.width, v.height = w, h
	const ollama, nodeinfo = 8, 9
	id := clampWidth((w-ollama-nodeinfo-2)/2, 8)
	addr := clampWidth(w-ollama-nodeinfo-id-2, 10)
	v.table.SetColumns([]table.Column{
		{Title: "ID", Width: id},
		{Title: "ADDRESS", Width: addr},
		{Title: "OLLAMA", Width: ollama},
		{Title: "NODEINFO", Width: nodeinfo},
	})
	v.table.SetWidth(w)
	v.table.SetHeight(clampWidth(h-2, 1))
}

func (v *manualView) CapturingInput() bool { return v.adding }

func (v *manualView) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case manualNodesMsg:
		if msg.err == nil {
			v.setNodes(msg.nodes)
		}
		return nil
	case manualTickMsg:
		return tea.Batch(v.listCmd(), v.tickCmd())
	case manualActionMsg:
		if msg.err != nil {
			v.status = msg.what + " failed: " + msg.err.Error()
		} else {
			v.status = msg.what + " ok"
		}
		return v.listCmd()
	case tea.KeyMsg:
		return v.handleKey(msg)
	}
	return nil
}

func (v *manualView) handleKey(msg tea.KeyMsg) tea.Cmd {
	if v.adding {
		switch msg.String() {
		case "enter":
			return v.submitAdd()
		case "esc":
			v.adding = false
			v.input.Blur()
			return nil
		}
		var cmd tea.Cmd
		v.input, cmd = v.input.Update(msg)
		return cmd
	}
	switch {
	case key.Matches(msg, manualAddKey):
		v.adding = true
		v.input.SetValue("")
		v.input.Focus()
		return textinput.Blink
	case key.Matches(msg, manualRemoveKey):
		return v.removeSelected()
	}
	var cmd tea.Cmd
	v.table, cmd = v.table.Update(msg)
	return cmd
}

func (v *manualView) submitAdd() tea.Cmd {
	v.adding = false
	v.input.Blur()
	addr := strings.TrimSpace(v.input.Value())
	if addr == "" {
		v.status = "address required"
		return nil
	}
	return call(v.client, "node/add", map[string]string{"address": addr}, func(_ *rpc.Message, err error) tea.Msg {
		return manualActionMsg{what: "add " + addr, err: err}
	})
}

func (v *manualView) removeSelected() tea.Cmd {
	idx := v.table.Cursor()
	if idx < 0 || idx >= len(v.nodes) {
		return nil
	}
	id := v.nodes[idx].ID
	return call(v.client, "node/remove", map[string]string{"id": id}, func(_ *rpc.Message, err error) tea.Msg {
		return manualActionMsg{what: "remove " + id, err: err}
	})
}

func (v *manualView) setNodes(nodes []manualNode) {
	v.nodes = nodes
	rows := make([]table.Row, 0, len(nodes))
	for _, n := range nodes {
		rows = append(rows, table.Row{
			truncate(n.ID, 16),
			n.Address,
			yesNo(n.OllamaUp),
			yesNo(n.NodeInfoUp),
		})
	}
	v.table.SetRows(rows)
}

func (v *manualView) View() string {
	var b strings.Builder
	if len(v.nodes) == 0 {
		b.WriteString(footerStyle.Render("No manual nodes. Press a to add one by address."))
	} else {
		b.WriteString(v.table.View())
	}
	if v.adding {
		b.WriteString("\nadd node: " + v.input.View())
	}
	if v.status != "" {
		b.WriteString("\n" + footerStyle.Render(v.status))
	}
	return b.String()
}

func (v *manualView) Help() []key.Binding {
	return []key.Binding{manualAddKey, manualRemoveKey}
}
