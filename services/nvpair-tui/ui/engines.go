// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// engineStatus mirrors nvpair-engine-manager's EngineStatus snapshot, the
// element of engine:get-installed and the engine:state-changed payload.
type engineStatus struct {
	Engine           string `json:"engine"`
	DisplayName      string `json:"display_name"`
	Installed        bool   `json:"installed"`
	Running          bool   `json:"running"`
	Healthy          bool   `json:"healthy"`
	Port             int    `json:"port"`
	InstallSupported bool   `json:"install_supported"`
	InstallReason    string `json:"install_reason"`
	Managed          bool   `json:"managed"`
}

// enginesView manages local inference engines via the engine-manager
// control plane: an installed/running/healthy table plus lifecycle
// actions, kept live from engine:state-changed and engine:install-progress.
// It can also pull a model (engine:action{action:"pull_model"}), rendering
// the live engine:pull-progress feed the way remote pulls already show.
type enginesView struct {
	client            *rpc.Client
	table             table.Model
	order             []string
	byName            map[string]engineStatus
	status            string
	input             textinput.Model
	pulling           bool
	pullEngine        string
	modelAction       string
	models            table.Model
	showModels        bool
	modelNames        []string
	pendingLoadEngine string
	pendingLoadModel  string

	width, height int
}

type enginesLoadedMsg struct {
	engines []engineStatus
	err     error
}

type engineModelsMsg struct {
	engine string
	names  []string
	loaded map[string]bool
	err    error
}

type engineOpMsg struct {
	what   string
	engine string
	err    error
}

var (
	engStartKey     = key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "start"))
	engStopKey      = key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "stop"))
	engRestartKey   = key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "restart"))
	engInstallKey   = key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "install"))
	engUninstallKey = key.NewBinding(key.WithKeys("u"), key.WithHelp("u", "uninstall"))
	engPullKey      = key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "pull model"))
	engLoadKey      = key.NewBinding(key.WithKeys("L"), key.WithHelp("L", "load model"))
	engUnloadKey    = key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "unload model"))
	engDeleteKey    = key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "delete model"))
	engCancelKey    = key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "cancel model pull"))
	engModelsKey    = key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "models / refresh"))
	engImportKey    = key.NewBinding(key.WithKeys("I"), key.WithHelp("I", "import GGUF path"))
)

func newEnginesView(client *rpc.Client) *enginesView {
	ti := textinput.New()
	ti.Placeholder = "model name (e.g. llama3.2)"
	v := &enginesView{client: client, byName: map[string]engineStatus{}, input: ti}
	v.table = newTable(nil)
	v.models = newTable(nil)
	return v
}

func (v *enginesView) Title() string { return "Engines" }

func (v *enginesView) Init() tea.Cmd {
	return tea.Batch(
		call(v.client, "engine:subscribe", nil, func(_ *rpc.Message, _ error) tea.Msg { return nil }),
		v.loadCmd(),
	)
}

func (v *enginesView) loadCmd() tea.Cmd {
	return call(v.client, "engine:get-installed", nil, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return enginesLoadedMsg{err: err}
		}
		var r struct {
			Engines []engineStatus `json:"engines"`
		}
		_ = decodeParams(msg.Result, &r)
		return enginesLoadedMsg{engines: r.Engines}
	})
}

func (v *enginesView) SetSize(w, h int) {
	v.width, v.height = w, h
	const inst, run, heal, port = 10, 8, 8, 7
	name := clampWidth(w-inst-run-heal-port-2, 10)
	v.table.SetColumns([]table.Column{
		{Title: "ENGINE", Width: name},
		{Title: "INSTALLED", Width: inst},
		{Title: "RUNNING", Width: run},
		{Title: "HEALTHY", Width: heal},
		{Title: "PORT", Width: port},
	})
	v.table.SetWidth(w)
	v.table.SetHeight(clampWidth(h-2, 1))
	v.models.SetColumns([]table.Column{{Title: "MODEL ID", Width: clampWidth(w-18, 10)}, {Title: "STATE", Width: 14}})
	v.models.SetWidth(w)
	v.models.SetHeight(clampWidth(h-3, 1))
}

func (v *enginesView) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case engineModelsMsg:
		if msg.engine != "" && msg.engine != v.selectedEngine() {
			return nil
		}
		if msg.err != nil {
			v.showModels = false
			v.status = "Model inventory unavailable: " + msg.err.Error()
			return nil
		}
		v.modelNames = msg.names
		rows := make([]table.Row, 0, len(msg.names))
		for _, name := range msg.names {
			state := "downloaded"
			if v.selectedEngine() == "llamacpp" && !v.byName[v.selectedEngine()].Managed {
				state = "catalogue"
			}
			if msg.loaded[name] {
				state = "loaded"
			}
			rows = append(rows, table.Row{name, state})
		}
		v.models.SetRows(rows)
		v.showModels = true
		if msg.engine == v.pendingLoadEngine && v.pendingLoadModel != "" && msg.loaded[v.pendingLoadModel] {
			v.status = "Model loaded: " + v.pendingLoadModel
			v.pendingLoadEngine, v.pendingLoadModel = "", ""
		}
		return nil
	case enginesLoadedMsg:
		if msg.err != nil {
			v.status = "load engines failed: " + msg.err.Error()
			return nil
		}
		for _, e := range msg.engines {
			v.merge(e)
		}
		return nil

	case engineOpMsg:
		if errors.Is(msg.err, context.DeadlineExceeded) {
			// The RPC client dropped its waiter, not the backend operation.
			// Preserve pending residency so a later authoritative load can settle.
			loadedModel := strings.TrimPrefix(strings.TrimPrefix(msg.what, "load_model "), "run_model ")
			if v.status != "Model loaded: "+loadedModel {
				v.status = fmt.Sprintf("%s %s: no final response yet; outcome unknown. Refresh to observe completion.", msg.what, msg.engine)
			}
		} else if msg.err != nil {
			v.status = fmt.Sprintf("%s %s failed: %s", msg.what, msg.engine, msg.err.Error())
			if msg.engine == v.pendingLoadEngine {
				v.pendingLoadEngine, v.pendingLoadModel = "", ""
			}
		} else if strings.HasPrefix(msg.what, "load_model ") || strings.HasPrefix(msg.what, "run_model ") {
			if v.pendingLoadModel != "" {
				v.status = "Load requested; waiting for observed model residency."
			}
		} else {
			v.status = fmt.Sprintf("%s %s ok", msg.what, msg.engine)
		}
		if v.showModels && msg.engine == v.selectedEngine() {
			return v.loadModelsCmd()
		}
		return nil

	case NotificationMsg:
		switch msg.Msg.Method {
		case "engine:models-changed":
			var snapshot struct {
				Loaded map[string][]string `json:"loadedByEngine"`
			}
			if decodeParams(msg.Msg.Params, &snapshot) == nil {
				for _, name := range snapshot.Loaded[v.pendingLoadEngine] {
					if name == v.pendingLoadModel && name != "" {
						v.status = "Model loaded: " + name
						v.pendingLoadEngine, v.pendingLoadModel = "", ""
					}
				}
			}
			if v.showModels {
				return v.loadModelsCmd()
			}
		case "engine:state-changed":
			var e engineStatus
			_ = decodeParams(msg.Msg.Params, &e)
			if e.Engine != "" {
				v.merge(e)
			}
		case "engine:install-progress":
			var p struct {
				Engine  string `json:"engine"`
				Stage   string `json:"stage"`
				Percent int    `json:"percent"`
			}
			_ = decodeParams(msg.Msg.Params, &p)
			v.status = fmt.Sprintf("install %s: %s", p.Engine, p.Stage)
			if p.Percent >= 0 && p.Percent <= 100 {
				v.status += fmt.Sprintf(" (%d%%)", p.Percent)
			}
		case "engine:pull-progress":
			var p struct {
				Engine  string `json:"engine"`
				Stage   string `json:"stage"`
				Percent int    `json:"percent"`
				Message string `json:"message"`
			}
			_ = decodeParams(msg.Msg.Params, &p)
			// Terminal stages carry no meaningful percent (success is implicitly
			// 100%; error uses -1), so render them as outcomes rather than a
			// misleading "success (0%)". A late failure that arrives after the
			// synchronous call timed out still surfaces here.
			switch p.Stage {
			case "success":
				v.status = fmt.Sprintf("pull %s: done", p.Engine)
			case "error":
				detail := p.Message
				if detail == "" {
					detail = "failed"
				}
				v.status = fmt.Sprintf("pull %s failed: %s", p.Engine, detail)
			default:
				v.status = fmt.Sprintf("pull %s: %s", p.Engine, p.Stage)
				if p.Percent >= 0 && p.Percent <= 100 {
					v.status += fmt.Sprintf(" (%d%%)", p.Percent)
				}
			}
		}
		return nil

	case tea.KeyMsg:
		return v.handleKey(msg)
	}
	return nil
}

func (v *enginesView) CapturingInput() bool { return v.pulling }

func (v *enginesView) handleKey(msg tea.KeyMsg) tea.Cmd {
	if v.pulling {
		switch msg.String() {
		case "enter":
			return v.submitPull()
		case "esc":
			v.pulling = false
			v.input.Blur()
			return nil
		}
		var cmd tea.Cmd
		v.input, cmd = v.input.Update(msg)
		return cmd
	}
	if v.showModels && msg.String() == "esc" {
		v.showModels = false
		return nil
	}
	if key.Matches(msg, engModelsKey) {
		return v.loadModelsCmd()
	}
	var action string
	switch {
	case key.Matches(msg, engPullKey):
		action = "pull_model"
	case key.Matches(msg, engLoadKey):
		action = "load_model"
	case key.Matches(msg, engUnloadKey):
		action = "unload_model"
	case key.Matches(msg, engDeleteKey):
		action = "delete_model"
	case key.Matches(msg, engCancelKey):
		action = "cancel_pull"
	case key.Matches(msg, engImportKey):
		action = "import_model"
	}
	if action != "" {
		engine := v.selectedEngine()
		if engine == "" {
			return nil
		}
		if engine == "llamacpp" && !v.byName[engine].Managed {
			v.status = "Model actions require a PAIR-managed llama app."
			return nil
		}
		v.pullEngine = engine
		if action == "load_model" && engine == "ollama" {
			action = "run_model"
		}
		v.modelAction = action
		v.pulling = true
		v.input.SetValue("")
		if v.showModels && v.models.Cursor() >= 0 && v.models.Cursor() < len(v.modelNames) {
			v.input.SetValue(v.modelNames[v.models.Cursor()])
		}
		v.input.Focus()
		return textinput.Blink
	}
	if cmd, handled := v.handleAction(msg); handled {
		return cmd
	}
	var cmd tea.Cmd
	if v.showModels {
		v.models, cmd = v.models.Update(msg)
		return cmd
	}
	v.table, cmd = v.table.Update(msg)
	return cmd
}

func (v *enginesView) loadModelsCmd() tea.Cmd {
	engine := v.selectedEngine()
	if engine == "" {
		return nil
	}
	action := "list_models"
	if engine == "llamacpp" && v.byName[engine].Managed {
		action = "list_downloaded"
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		msg, err := v.client.Call(ctx, "engine:action", map[string]string{"engine": engine, "action": action})
		if err != nil {
			return engineModelsMsg{err: err}
		}
		var inventory struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			Models []struct {
				Name  string `json:"name"`
				Model string `json:"model"`
			} `json:"models"`
		}
		if err := decodeParams(msg.Result, &inventory); err != nil {
			return engineModelsMsg{err: err}
		}
		result := engineModelsMsg{engine: engine, loaded: map[string]bool{}}
		for _, model := range inventory.Data {
			if model.ID != "" {
				result.names = append(result.names, model.ID)
			}
		}
		for _, model := range inventory.Models {
			name := model.Name
			if name == "" {
				name = model.Model
			}
			if name != "" {
				result.names = append(result.names, name)
			}
		}
		msg, err = v.client.Call(ctx, "engine:models", nil)
		if err != nil {
			return engineModelsMsg{err: err}
		}
		var snapshot struct {
			Loaded map[string][]string `json:"loadedByEngine"`
		}
		if err := decodeParams(msg.Result, &snapshot); err != nil {
			return engineModelsMsg{err: err}
		}
		for _, name := range snapshot.Loaded[engine] {
			result.loaded[name] = true
		}
		return result
	}
}

// pullParams builds the engine:action{action:"pull_model"} params for a pull.
// The model name is sent under BOTH "name" and "model" — mirroring
// PullModelStream's own empty-params default — because the two engines key it
// differently: Ollama's pull_model is HTTP /api/pull (body key "name"), while
// LM Studio's is a CLI action `lms get {model}` resolved from the "model" key.
// Sending only one key silently no-ops the pull on the other engine.
func pullParams(engine, model string) map[string]any {
	return map[string]any{"engine": engine, "action": "pull_model", "params": map[string]string{"name": model, "model": model}}
}

// submitPull issues engine:action{action:"pull_model"} for the selected engine.
// Live download progress and the terminal result arrive as engine:pull-progress
// notifications. Mutations share the backend's long-operation budget; a client
// deadline still cannot establish that the backend operation failed.
func (v *enginesView) submitPull() tea.Cmd {
	v.pulling = false
	v.input.Blur()
	model := strings.TrimSpace(v.input.Value())
	engine := v.pullEngine
	if model == "" || engine == "" {
		v.status = "model name required"
		return nil
	}
	v.status = fmt.Sprintf("pull %s: %s...", engine, model)
	params := pullParams(engine, model)
	action := v.modelAction
	if action == "" {
		action = "pull_model"
	}
	params["action"] = action
	if action == "load_model" || action == "run_model" {
		v.pendingLoadEngine, v.pendingLoadModel = engine, model
	}
	if action == "import_model" {
		params["params"] = map[string]string{"path": model}
	}
	v.status = fmt.Sprintf("%s %s: %s...", action, engine, model)
	return call(v.client, "engine:action", params, func(_ *rpc.Message, err error) tea.Msg {
		if action != "pull_model" {
			return engineOpMsg{what: action + " " + model, engine: engine, err: err}
		}
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			return engineOpMsg{what: "pull " + model, engine: engine, err: err}
		}
		return nil
	}, engineOperationTimeout)
}

func (v *enginesView) handleAction(msg tea.KeyMsg) (tea.Cmd, bool) {
	var method, what string
	switch {
	case key.Matches(msg, engStartKey):
		method, what = "engine:start", "start"
	case key.Matches(msg, engStopKey):
		method, what = "engine:stop", "stop"
	case key.Matches(msg, engRestartKey):
		method, what = "engine:restart", "restart"
	case key.Matches(msg, engInstallKey):
		method, what = "engine:install", "install"
	case key.Matches(msg, engUninstallKey):
		method, what = "engine:uninstall", "uninstall"
	default:
		return nil, false
	}
	engine := v.selectedEngine()
	if engine == "" {
		return nil, true
	}
	if what == "install" && engine == "llamacpp" && !v.byName[engine].InstallSupported {
		v.status = "Install unavailable: " + v.byName[engine].InstallReason
		return nil, true
	}
	if engine == "llamacpp" && v.byName[engine].Installed && !v.byName[engine].Managed {
		v.status = "External llama.cpp runtime: lifecycle remains with its owner."
		return nil, true
	}
	v.status = what + " " + engine + "..."
	return call(v.client, method, map[string]string{"engine": engine}, func(_ *rpc.Message, err error) tea.Msg {
		return engineOpMsg{what: what, engine: engine, err: err}
	}, engineOperationTimeout), true
}

func (v *enginesView) selectedEngine() string {
	idx := v.table.Cursor()
	if idx < 0 || idx >= len(v.order) {
		return ""
	}
	return v.order[idx]
}

func (v *enginesView) merge(e engineStatus) {
	if _, ok := v.byName[e.Engine]; !ok {
		v.order = append(v.order, e.Engine)
	}
	v.byName[e.Engine] = e
	v.refreshRows()
}

func (v *enginesView) refreshRows() {
	rows := make([]table.Row, 0, len(v.order))
	for _, name := range v.order {
		e := v.byName[name]
		label := e.Engine
		if e.DisplayName != "" {
			label = e.DisplayName
		}
		port := "-"
		if e.Port != 0 {
			port = strconv.Itoa(e.Port)
		}
		rows = append(rows, table.Row{
			label,
			yesNo(e.Installed),
			yesNo(e.Running),
			yesNo(e.Healthy),
			port,
		})
	}
	v.table.SetRows(rows)
}

func (v *enginesView) View() string {
	if len(v.order) == 0 {
		if v.status != "" {
			return statusErrStyle.Render(v.status)
		}
		return footerStyle.Render("No engines known on this host.")
	}
	out := v.table.View()
	if v.showModels {
		out = v.models.View()
	}
	if v.pulling {
		out += "\n" + v.modelAction + " (enter model ID; Esc cancels input): " + v.input.View()
	}
	if v.status != "" {
		out += "\n" + footerStyle.Render(v.status)
	}
	return out
}

func (v *enginesView) Help() []key.Binding {
	return []key.Binding{engStartKey, engStopKey, engRestartKey, engInstallKey, engUninstallKey, engModelsKey, engPullKey, engLoadKey, engUnloadKey, engDeleteKey, engCancelKey, engImportKey}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
