// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"strconv"
	"strings"

	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// proxyNode mirrors a discovered upstream as reported by the proxy's
// nodes/list (ollama-proxy/discovery.go Node).
type proxyNode struct {
	ID   string `json:"id"`
	Host string `json:"host"`
	Port int    `json:"port"`
}

// proxyEngine is one of the two reverse proxies the broker fronts. Both
// speak the same routing/failover contract; only the JSON-RPC prefix and
// label differ.
type proxyEngine struct {
	label    string // "Ollama" / "LM Studio"
	prefix   string // "proxy" / "lmstudio-proxy"
	ready    bool
	port     int
	selected string
	nodes    []proxyNode
	table    table.Model
}

// proxiesView shows both reverse proxies: per-engine status (ready/port/
// selected node) and the focused engine's discovered upstreams, with
// actions to select a node and set the listen port.
type proxiesView struct {
	client      *rpc.Client
	engines     []*proxyEngine
	focus       int
	portInput   textinput.Model
	editingPort bool
	status      string

	width, height int
}

type proxyStatusMsg struct {
	idx   int
	ready bool
	port  int
	err   error
}

type proxyNodesMsg struct {
	idx   int
	nodes []proxyNode
	err   error
}

type proxySelectedMsg struct {
	idx int
	id  string
	err error
}

type proxyActionMsg struct {
	what string
	err  error
}

var (
	proxyFocusKey  = key.NewBinding(key.WithKeys("g"), key.WithHelp("g", "toggle engine"))
	proxySelectKey = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "select node"))
	proxyPortKey   = key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "set port"))
	proxyAutoKey   = key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "auto-select"))
)

func newProxiesView(client *rpc.Client) *proxiesView {
	ti := textinput.New()
	ti.Placeholder = "port"
	ti.CharLimit = 5
	v := &proxiesView{
		client:    client,
		portInput: ti,
		engines: []*proxyEngine{
			{label: "Ollama", prefix: "proxy", table: newTable(nil)},
			{label: "LM Studio", prefix: "lmstudio-proxy", table: newTable(nil)},
		},
	}
	return v
}

func (v *proxiesView) Title() string { return "Proxies" }

func (v *proxiesView) Init() tea.Cmd {
	var cmds []tea.Cmd
	for i, e := range v.engines {
		cmds = append(cmds,
			call(v.client, e.prefix+":subscribe", nil, func(_ *rpc.Message, _ error) tea.Msg { return nil }),
			v.statusCmd(i),
			v.nodesCmd(i),
			v.selectedCmd(i),
		)
	}
	return tea.Batch(cmds...)
}

func (v *proxiesView) statusCmd(idx int) tea.Cmd {
	e := v.engines[idx]
	return call(v.client, e.prefix+":get-status", nil, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return proxyStatusMsg{idx: idx, err: err}
		}
		var r struct {
			Ready bool `json:"ready"`
			Port  int  `json:"port"`
		}
		_ = decodeParams(msg.Result, &r)
		return proxyStatusMsg{idx: idx, ready: r.Ready, port: r.Port}
	})
}

func (v *proxiesView) nodesCmd(idx int) tea.Cmd {
	e := v.engines[idx]
	return call(v.client, e.prefix+":nodes/list", nil, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return proxyNodesMsg{idx: idx, err: err}
		}
		var r struct {
			Nodes []proxyNode `json:"nodes"`
		}
		_ = decodeParams(msg.Result, &r)
		return proxyNodesMsg{idx: idx, nodes: r.Nodes}
	})
}

func (v *proxiesView) selectedCmd(idx int) tea.Cmd {
	e := v.engines[idx]
	return call(v.client, e.prefix+":node/selected", nil, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return proxySelectedMsg{idx: idx, err: err}
		}
		var r struct {
			ID string `json:"id"`
		}
		_ = decodeParams(msg.Result, &r)
		return proxySelectedMsg{idx: idx, id: r.ID}
	})
}

func (v *proxiesView) SetSize(w, h int) {
	v.width, v.height = w, h
	const sel, port = 3, 7
	id := clampWidth((w-sel-port-2)/2, 10)
	host := clampWidth(w-sel-port-id-2, 10)
	cols := []table.Column{
		{Title: "SEL", Width: sel},
		{Title: "ID", Width: id},
		{Title: "HOST", Width: host},
		{Title: "PORT", Width: port},
	}
	for _, e := range v.engines {
		e.table.SetColumns(cols)
		e.table.SetWidth(w)
		e.table.SetHeight(clampWidth(h-6, 1))
	}
}

func (v *proxiesView) CapturingInput() bool { return v.editingPort }

func (v *proxiesView) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case proxyStatusMsg:
		if msg.err == nil {
			v.engines[msg.idx].ready = msg.ready
			v.engines[msg.idx].port = msg.port
		}
		return nil
	case proxyNodesMsg:
		if msg.err == nil {
			v.engines[msg.idx].nodes = msg.nodes
			v.refreshRows(msg.idx)
		}
		return nil
	case proxySelectedMsg:
		if msg.err == nil {
			v.engines[msg.idx].selected = msg.id
			v.refreshRows(msg.idx)
		}
		return nil
	case proxyActionMsg:
		if msg.err != nil {
			v.status = msg.what + " failed: " + msg.err.Error()
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

func (v *proxiesView) handleNotification(msg *rpc.Message) tea.Cmd {
	idx := -1
	switch {
	case strings.HasPrefix(msg.Method, "lmstudio-proxy:"):
		idx = 1
	case strings.HasPrefix(msg.Method, "proxy:"):
		idx = 0
	default:
		return nil
	}
	if strings.HasSuffix(msg.Method, ":ready") {
		var r struct {
			Port int `json:"port"`
		}
		_ = decodeParams(msg.Params, &r)
		v.engines[idx].ready = true
		if r.Port != 0 {
			v.engines[idx].port = r.Port
		}
		return nil
	}
	// Any node lifecycle / selection event: refresh that engine's list
	// and current selection rather than tracking deltas by hand.
	if strings.Contains(msg.Method, ":node/") {
		return tea.Batch(v.nodesCmd(idx), v.selectedCmd(idx))
	}
	return nil
}

func (v *proxiesView) handleKey(msg tea.KeyMsg) tea.Cmd {
	if v.editingPort {
		switch msg.String() {
		case "enter":
			return v.submitPort()
		case "esc":
			v.editingPort = false
			v.portInput.Blur()
			return nil
		}
		var cmd tea.Cmd
		v.portInput, cmd = v.portInput.Update(msg)
		return cmd
	}

	switch {
	case key.Matches(msg, proxyFocusKey):
		v.focus = (v.focus + 1) % len(v.engines)
		return nil
	case key.Matches(msg, proxyPortKey):
		v.editingPort = true
		v.portInput.SetValue(strconv.Itoa(v.engines[v.focus].port))
		v.portInput.Focus()
		return textinput.Blink
	case key.Matches(msg, proxySelectKey):
		return v.selectHighlighted()
	case key.Matches(msg, proxyAutoKey):
		return v.selectNode("")
	}
	var cmd tea.Cmd
	v.engines[v.focus].table, cmd = v.engines[v.focus].table.Update(msg)
	return cmd
}

func (v *proxiesView) selectHighlighted() tea.Cmd {
	e := v.engines[v.focus]
	idx := e.table.Cursor()
	if idx < 0 || idx >= len(e.nodes) {
		return nil
	}
	return v.selectNode(e.nodes[idx].ID)
}

func (v *proxiesView) selectNode(id string) tea.Cmd {
	e := v.engines[v.focus]
	return call(v.client, e.prefix+":node/select", map[string]string{"id": id}, func(_ *rpc.Message, err error) tea.Msg {
		return proxyActionMsg{what: "select", err: err}
	})
}

func (v *proxiesView) submitPort() tea.Cmd {
	v.editingPort = false
	v.portInput.Blur()
	port, err := strconv.Atoi(strings.TrimSpace(v.portInput.Value()))
	if err != nil || port <= 0 || port > 65535 {
		v.status = "invalid port"
		return nil
	}
	e := v.engines[v.focus]
	return call(v.client, e.prefix+":set-port", map[string]int{"port": port}, func(_ *rpc.Message, err error) tea.Msg {
		return proxyActionMsg{what: "set-port", err: err}
	})
}

func (v *proxiesView) refreshRows(idx int) {
	e := v.engines[idx]
	rows := make([]table.Row, 0, len(e.nodes))
	for _, n := range e.nodes {
		marker := ""
		if n.ID != "" && n.ID == e.selected {
			marker = "*"
		}
		rows = append(rows, table.Row{marker, n.ID, n.Host, strconv.Itoa(n.Port)})
	}
	e.table.SetRows(rows)
}

func (v *proxiesView) View() string {
	var b strings.Builder
	for i, e := range v.engines {
		b.WriteString(v.engineStatusLine(i, e))
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	focused := v.engines[v.focus]
	b.WriteString(titleStyle.Render(focused.label + " upstreams"))
	b.WriteByte('\n')
	if len(focused.nodes) == 0 {
		b.WriteString(footerStyle.Render("No upstreams discovered."))
	} else {
		b.WriteString(focused.table.View())
	}
	if v.editingPort {
		b.WriteString("\nset " + focused.label + " port: " + v.portInput.View())
	}
	if v.status != "" {
		b.WriteString("\n" + footerStyle.Render(v.status))
	}
	return b.String()
}

func (v *proxiesView) engineStatusLine(i int, e *proxyEngine) string {
	state := statusErrStyle.Render("down")
	if e.ready {
		state = statusOKStyle.Render(fmt.Sprintf("ready :%d", e.port))
	}
	sel := e.selected
	if sel == "" {
		sel = "auto"
	}
	marker := "  "
	if i == v.focus {
		marker = "> "
	}
	return fmt.Sprintf("%s%-10s %s  selected=%s", marker, e.label, state, sel)
}

func (v *proxiesView) Help() []key.Binding {
	return []key.Binding{proxyFocusKey, proxySelectKey, proxyAutoKey, proxyPortKey}
}
