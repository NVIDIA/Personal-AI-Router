// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"strconv"
	"strings"

	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// settingItem describes one persisted per-node preference and its
// settings/get-*/set-* method pair.
type settingItem struct {
	suffix string // e.g. "force-ports" -> settings/get-force-ports
	label  string
	isBool bool
	boolV  bool
	strV   string
}

// settingsView edits the node-settings store: boolean prefs toggle in
// place, string prefs open an inline editor. Values are read on start and
// re-read after each successful write.
type settingsView struct {
	client        *rpc.Client
	items         []settingItem
	cursor        int
	input         textinput.Model
	editing       bool
	status        string
	width, height int
}

type settingLoadedMsg struct {
	idx   int
	isB   bool
	boolV bool
	strV  string
	err   error
}

type settingSavedMsg struct {
	idx int
	err error
}

var (
	settingsUpKey   = key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("up/k", "up"))
	settingsDownKey = key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("down/j", "down"))
	settingsEditKey = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "toggle/edit"))
)

func newSettingsView(client *rpc.Client) *settingsView {
	ti := textinput.New()
	return &settingsView{
		client: client,
		input:  ti,
		items: []settingItem{
			{suffix: "force-ports", label: "Force ports", isBool: true},
			{suffix: "cluster-auto-sync", label: "Cluster auto-sync", isBool: true},
			{suffix: "cluster-id", label: "Cluster ID"},
			{suffix: "cluster-friendly-name", label: "Cluster friendly name"},
		},
	}
}

func (v *settingsView) Title() string { return "Settings" }

func (v *settingsView) Init() tea.Cmd {
	cmds := make([]tea.Cmd, len(v.items))
	for i := range v.items {
		cmds[i] = v.loadCmd(i)
	}
	return tea.Batch(cmds...)
}

func (v *settingsView) loadCmd(idx int) tea.Cmd {
	it := v.items[idx]
	return call(v.client, "settings/get-"+it.suffix, nil, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return settingLoadedMsg{idx: idx, err: err}
		}
		if it.isBool {
			var r struct {
				Value bool `json:"value"`
			}
			_ = decodeParams(msg.Result, &r)
			return settingLoadedMsg{idx: idx, isB: true, boolV: r.Value}
		}
		var r struct {
			Value string `json:"value"`
		}
		_ = decodeParams(msg.Result, &r)
		return settingLoadedMsg{idx: idx, strV: r.Value}
	})
}

func (v *settingsView) SetSize(w, h int) { v.width, v.height = w, h }

func (v *settingsView) CapturingInput() bool { return v.editing }

func (v *settingsView) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case settingLoadedMsg:
		if msg.err == nil {
			v.items[msg.idx].boolV = msg.boolV
			v.items[msg.idx].strV = msg.strV
		}
		return nil
	case settingSavedMsg:
		if msg.err != nil {
			v.status = "save failed: " + msg.err.Error()
			return nil
		}
		v.status = v.items[msg.idx].label + " saved"
		return v.loadCmd(msg.idx)
	case tea.KeyMsg:
		return v.handleKey(msg)
	}
	return nil
}

func (v *settingsView) handleKey(msg tea.KeyMsg) tea.Cmd {
	if v.editing {
		switch msg.String() {
		case "enter":
			return v.submitString()
		case "esc":
			v.editing = false
			v.input.Blur()
			return nil
		}
		var cmd tea.Cmd
		v.input, cmd = v.input.Update(msg)
		return cmd
	}
	switch {
	case key.Matches(msg, settingsUpKey):
		if v.cursor > 0 {
			v.cursor--
		}
	case key.Matches(msg, settingsDownKey):
		if v.cursor < len(v.items)-1 {
			v.cursor++
		}
	case key.Matches(msg, settingsEditKey):
		return v.activate()
	}
	return nil
}

func (v *settingsView) activate() tea.Cmd {
	it := &v.items[v.cursor]
	if it.isBool {
		return v.saveBool(v.cursor, !it.boolV)
	}
	v.editing = true
	v.input.SetValue(it.strV)
	v.input.Focus()
	return textinput.Blink
}

func (v *settingsView) saveBool(idx int, val bool) tea.Cmd {
	it := v.items[idx]
	return call(v.client, "settings/set-"+it.suffix, map[string]bool{"value": val}, func(_ *rpc.Message, err error) tea.Msg {
		return settingSavedMsg{idx: idx, err: err}
	})
}

func (v *settingsView) submitString() tea.Cmd {
	v.editing = false
	v.input.Blur()
	idx := v.cursor
	it := v.items[idx]
	val := strings.TrimSpace(v.input.Value())
	return call(v.client, "settings/set-"+it.suffix, map[string]string{"value": val}, func(_ *rpc.Message, err error) tea.Msg {
		return settingSavedMsg{idx: idx, err: err}
	})
}

func (v *settingsView) View() string {
	var b strings.Builder
	for i, it := range v.items {
		cursor := "  "
		if i == v.cursor {
			cursor = "> "
		}
		var val string
		if it.isBool {
			val = strconv.FormatBool(it.boolV)
		} else if it.strV == "" {
			val = footerStyle.Render("(unset)")
		} else {
			val = it.strV
		}
		line := fmt.Sprintf("%s%-24s %s", cursor, it.label, val)
		if i == v.cursor {
			line = titleStyle.Render(line)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if v.editing {
		b.WriteString("\n" + v.items[v.cursor].label + ": " + v.input.View())
	}
	if v.status != "" {
		b.WriteString("\n" + footerStyle.Render(v.status))
	}
	return b.String()
}

func (v *settingsView) Help() []key.Binding {
	return []key.Binding{settingsUpKey, settingsDownKey, settingsEditKey}
}
