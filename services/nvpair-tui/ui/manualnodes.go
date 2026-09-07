// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
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
	OpenAIUp   bool   `json:"openai_up"`
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
	// A bare host uses the historical form: nvpair-manual-nodes appends its
	// fixed engine ports (11434 for Ollama, 14318 for node-info), so a
	// host:port form yields a malformed URL and the node always reads down.
	// A full http:// base URL adopts the endpoint at that exact URL instead.
	ti.Placeholder = "host or http://host:port/v1"
	v := &manualView{client: client, input: ti}
	v.table = newTable(nil)
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
	const ollama, nodeinfo, openai = 8, 9, 8
	id := clampWidth((w-ollama-nodeinfo-openai-2)/2, 8)
	addr := clampWidth(w-ollama-nodeinfo-openai-id-2, 10)
	v.table.SetColumns([]table.Column{
		{Title: "ID", Width: id},
		{Title: "ADDRESS", Width: addr},
		{Title: "OLLAMA", Width: ollama},
		{Title: "NODEINFO", Width: nodeinfo},
		{Title: "OPENAI", Width: openai},
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

// parseManualInput splits the operator's add input into the two node/add
// flavors: a bare host/address (the historical form; the prober appends its
// fixed engine ports) or a full OpenAI-compatible base URL (declared
// endpoints are probed at the URL itself, not the default ports).
func parseManualInput(s string) (address, baseURL string, err error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "://") {
		if !strings.HasPrefix(s, "http://") {
			return "", "", fmt.Errorf("only http:// base URLs are supported (https not yet)")
		}
		return "", s, nil
	}
	if s == "" {
		return "", "", fmt.Errorf("address required")
	}
	return s, "", nil
}

func (v *manualView) submitAdd() tea.Cmd {
	v.adding = false
	v.input.Blur()
	addr, baseURL, err := parseManualInput(v.input.Value())
	if err != nil {
		v.status = err.Error()
		return nil
	}
	params := map[string]string{"address": addr}
	label := addr
	if baseURL != "" {
		params = map[string]string{"openai_base_url": baseURL}
		label = baseURL
	}
	return call(v.client, "node/add", params, func(_ *rpc.Message, err error) tea.Msg {
		return manualActionMsg{what: "add " + label, err: err}
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
			yesNo(n.OpenAIUp),
		})
	}
	v.table.SetRows(rows)
}

func (v *manualView) View() string {
	var b strings.Builder
	if len(v.nodes) == 0 {
		b.WriteString(footerStyle.Render("No manual nodes. Press a to add one by host or OpenAI endpoint URL."))
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
