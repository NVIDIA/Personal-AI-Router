// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"slices"

	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
)

type modelInventory struct {
	ByEngine       map[string][]string `json:"modelsByEngine"`
	LoadedByEngine map[string][]string `json:"loadedByEngine"`
}
type modelRow struct {
	engine, model, state string
}
type modelsView struct {
	client *rpc.Client
	table  table.Model
	rows   []modelRow
	status string
}
type modelsLoadedMsg struct {
	inventory modelInventory
	err       error
}
type modelActionMsg struct {
	what string
	row  modelRow
	err  error
}
type modelActionRequest struct {
	Engine string            `json:"engine"`
	Action string            `json:"action"`
	Params modelActionParams `json:"params"`
}
type modelActionParams struct {
	Model     string `json:"model"`
	Stream    *bool  `json:"stream,omitempty"`
	KeepAlive *int   `json:"keep_alive,omitempty"`
}

var (
	modelLoadKey   = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "load"))
	modelUnloadKey = key.NewBinding(key.WithKeys("u"), key.WithHelp("u", "unload"))
)

func newModelsView(client *rpc.Client) *modelsView {
	view := &modelsView{client: client, table: newTable(nil)}
	view.SetSize(80, 20)
	return view
}
func (v *modelsView) Title() string { return "Models" }

func (v *modelsView) Init() tea.Cmd {
	return tea.Batch(
		call(v.client, "engine:subscribe", nil, func(_ *rpc.Message, _ error) tea.Msg { return nil }),
		v.loadCmd(),
	)
}

func (v *modelsView) loadCmd() tea.Cmd {
	return call(v.client, "engine:models", nil, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return modelsLoadedMsg{err: err}
		}
		var inventory modelInventory
		err = decodeParams(msg.Result, &inventory)
		return modelsLoadedMsg{inventory: inventory, err: err}
	})
}

func (v *modelsView) SetSize(width, height int) {
	const engineWidth, stateWidth = 14, 8
	v.table.SetColumns([]table.Column{
		{Title: "ENGINE", Width: engineWidth},
		{Title: "MODEL", Width: clampWidth(width-engineWidth-stateWidth-2, 16)},
		{Title: "STATE", Width: stateWidth},
	})
	v.table.SetWidth(width)
	v.table.SetHeight(clampWidth(height-2, 1))
}

func (v *modelsView) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case modelsLoadedMsg:
		if msg.err != nil {
			v.status = "load models failed: " + msg.err.Error()
		} else {
			v.status = ""
			v.apply(msg.inventory)
		}
		return nil
	case modelActionMsg:
		if msg.err != nil {
			v.status = msg.what + " " + msg.row.engine + "/" + msg.row.model + " failed: " + msg.err.Error()
		} else {
			v.status = msg.what + " " + msg.row.engine + "/" + msg.row.model + " ok"
		}
		return nil
	case NotificationMsg:
		if msg.Msg.Method == "engine:state-changed" {
			return v.loadCmd()
		}
		if msg.Msg.Method != "engine:models-changed" {
			return nil
		}
		var changed struct {
			Models modelInventory `json:"models"`
		}
		if err := decodeParams(msg.Msg.Params, &changed); err != nil {
			v.status = "update models failed: " + err.Error()
		} else {
			v.status = ""
			v.apply(changed.Models)
		}
		return nil
	case tea.KeyMsg:
		return v.handleKey(msg)
	}
	return nil
}

func (v *modelsView) handleKey(msg tea.KeyMsg) tea.Cmd {
	what := ""
	switch {
	case key.Matches(msg, modelLoadKey):
		what = "load"
	case key.Matches(msg, modelUnloadKey):
		what = "unload"
	default:
		var cmd tea.Cmd
		v.table, cmd = v.table.Update(msg)
		return cmd
	}
	row, ok := v.selectedModel()
	if !ok {
		return nil
	}
	v.status = what + " " + row.engine + "/" + row.model + "..."
	request := newModelActionRequest(row, what)
	return call(v.client, "engine:action", request, func(_ *rpc.Message, err error) tea.Msg {
		return modelActionMsg{what: what, row: row, err: err}
	})
}

func newModelActionRequest(row modelRow, what string) modelActionRequest {
	request := modelActionRequest{
		Engine: row.engine,
		Action: what + "_model",
		Params: modelActionParams{Model: row.model},
	}
	if row.engine != "ollama" {
		return request
	}
	switch what {
	case "load":
		stream := false
		request.Action = "run_model"
		request.Params.Stream = &stream
	case "unload":
		keepAlive := 0
		request.Params.KeepAlive = &keepAlive
	}
	return request
}

func (v *modelsView) selectedModel() (modelRow, bool) {
	index := v.table.Cursor()
	if index < 0 || index >= len(v.rows) {
		return modelRow{}, false
	}
	return v.rows[index], true
}

func (v *modelsView) apply(inventory modelInventory) {
	engines := make([]string, 0, len(inventory.ByEngine))
	for engine := range inventory.ByEngine {
		engines = append(engines, engine)
	}
	slices.Sort(engines)

	var rows []modelRow
	var tableRows []table.Row
	for _, engine := range engines {
		models := append([]string(nil), inventory.ByEngine[engine]...)
		slices.Sort(models)
		for _, model := range models {
			loadedModels, known := inventory.LoadedByEngine[engine]
			state := "unknown"
			if known {
				state = "idle"
				if slices.Contains(loadedModels, model) {
					state = "loaded"
				}
			}
			row := modelRow{engine: engine, model: model, state: state}
			rows = append(rows, row)
			tableRows = append(tableRows, table.Row{engine, model, state})
		}
	}
	v.rows = rows
	v.table.SetRows(tableRows)
}

func (v *modelsView) View() string {
	if len(v.rows) == 0 {
		if v.status != "" {
			return statusErrStyle.Render(v.status)
		}
		return footerStyle.Render("No local models reported by running engines.")
	}
	out := v.table.View()
	if v.status != "" {
		out += "\n" + footerStyle.Render(v.status)
	}
	return out
}

func (v *modelsView) Help() []key.Binding {
	return []key.Binding{modelLoadKey, modelUnloadKey}
}
