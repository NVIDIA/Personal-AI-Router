// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

const vllmModelSelectionTimeout = 10 * time.Minute

// engineStatus mirrors nvpair-engine-manager's EngineStatus snapshot, the
// element of engine:get-installed and the engine:state-changed payload.
type engineStatus struct {
	Engine           string   `json:"engine"`
	DisplayName      string   `json:"display_name"`
	Installed        bool     `json:"installed"`
	Running          bool     `json:"running"`
	Healthy          bool     `json:"healthy"`
	Enabled          *bool    `json:"enabled"`
	Managed          *bool    `json:"managed"`
	Adopted          *bool    `json:"adopted"`
	Routable         *bool    `json:"routable"`
	InstallSupported *bool    `json:"install_supported"`
	InstallReason    string   `json:"install_reason"`
	Acceleration     string   `json:"acceleration"`
	Devices          []string `json:"devices"`
	Version          string   `json:"version"`
	SelectedModel    string   `json:"selected_model"`
	Port             int      `json:"port"`
}

func (e engineStatus) enabled() bool {
	if e.Engine == "vllm" {
		return e.Enabled != nil && *e.Enabled
	}
	if e.Enabled != nil {
		return *e.Enabled
	}
	return e.Running
}

func (e engineStatus) routable() bool {
	if e.Routable != nil {
		return *e.Routable
	}
	return e.Running && e.Healthy
}

func (e engineStatus) managed() bool {
	if e.Engine == "vllm" {
		return e.Managed != nil && *e.Managed
	}
	if e.Managed != nil {
		return *e.Managed
	}
	return e.Adopted == nil || !*e.Adopted
}

func (e engineStatus) installSupported() bool {
	return e.InstallSupported != nil && *e.InstallSupported
}

func (e engineStatus) installAllowed() bool {
	return !e.Installed && e.installSupported() && e.Adopted != nil && !*e.Adopted
}

func engineActionBlock(status engineStatus, what string) string {
	if status.Engine != "vllm" {
		if what == "update" {
			return "update is unavailable for this engine"
		}
		return ""
	}
	switch what {
	case "install":
		if !status.installAllowed() {
			if status.InstallReason != "" {
				return "install is unavailable for vllm: " + status.InstallReason
			}
			return "install is unavailable for vllm on this host or ownership state"
		}
	case "uninstall":
		if !status.managed() {
			return "uninstall is unavailable for externally managed vllm"
		}
	case "start", "stop", "restart", "update":
		if !status.managed() {
			return what + " is unavailable for externally managed vllm"
		}
	}
	return ""
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
	selecting         bool
	pullEngine        string
	modelAction       string
	models            table.Model
	showModels        bool
	modelNames        []string
	pendingLoadEngine string
	pendingLoadModel  string
	pullActive        bool
	pullModel         string
	pullOpID          string
	groupKnown        bool
	groupHeld         bool
	groupReason       string

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
	engUpdateKey    = key.NewBinding(key.WithKeys("U"), key.WithHelp("U", "update"))
	engPullKey      = key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "pull model"))
	engLoadKey      = key.NewBinding(key.WithKeys("L"), key.WithHelp("L", "load model"))
	engUnloadKey    = key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "unload model"))
	engDeleteKey    = key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "delete model"))
	engCancelKey    = key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "cancel model pull"))
	engModelsKey    = key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "models / refresh"))
	engImportKey    = key.NewBinding(key.WithKeys("I"), key.WithHelp("I", "import GGUF path"))
	engSelectKey    = key.NewBinding(key.WithKeys("M"), key.WithHelp("M", "select served model"))
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
		servingGroupStatusCmd(v.client),
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
	const enabled, run, route, owner, version, port = 8, 8, 9, 9, 12, 7
	name := clampWidth(w-enabled-run-route-owner-version-port-2, 10)
	v.table.SetColumns([]table.Column{
		{Title: "ENGINE", Width: name},
		{Title: "ENABLED", Width: enabled},
		{Title: "RUNNING", Width: run},
		{Title: "ROUTABLE", Width: route},
		{Title: "OWNER", Width: owner},
		{Title: "VERSION", Width: version},
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
			if v.selectedEngine() == "llamacpp" && !v.byName[v.selectedEngine()].managed() {
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
		if msg.what == "select model" && msg.err == nil {
			return v.loadCmd()
		}
		if v.showModels && msg.engine == v.selectedEngine() {
			return v.loadModelsCmd()
		}
		return nil

	case servingGroupMsg:
		v.groupKnown = msg.err == nil
		v.groupHeld = msg.err != nil || msg.status.Reserved == nil || *msg.status.Reserved || (msg.status.Run != nil && !msg.status.Run.CleanupConfirmed)
		v.groupReason = msg.status.Reason
		if msg.err != nil {
			v.groupReason = msg.err.Error()
		}
		return nil

	case NotificationMsg:
		switch msg.Msg.Method {
		case "engine:models-changed":
			var snapshot struct {
				Models struct {
					Loaded map[string][]string `json:"loadedByEngine"`
				} `json:"models"`
			}
			if decodeParams(msg.Msg.Params, &snapshot) == nil {
				for _, name := range snapshot.Models.Loaded[v.pendingLoadEngine] {
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
				Engine      string `json:"engine"`
				Stage       string `json:"stage"`
				Percent     int    `json:"percent"`
				Message     string `json:"message"`
				OperationID string `json:"operationId"`
			}
			_ = decodeParams(msg.Msg.Params, &p)
			if p.Engine == "vllm" && p.OperationID != "" {
				v.pullActive, v.pullOpID = true, p.OperationID
				if p.Message != "" && p.Stage != "canceled" {
					v.pullModel = p.Message
				}
			}
			// Terminal stages carry no meaningful percent (success is implicitly
			// 100%; error uses -1), so render them as outcomes rather than a
			// misleading "success (0%)". A late failure that arrives after the
			// synchronous call timed out still surfaces here.
			switch p.Stage {
			case "success":
				v.status = fmt.Sprintf("pull %s: done", p.Engine)
				if p.Message != "" {
					v.status += " · " + p.Message
				}
				if p.Engine == "vllm" {
					v.pullActive, v.pullModel, v.pullOpID = false, "", ""
				}
			case "error", "canceled":
				detail := p.Message
				if detail == "" {
					detail = p.Stage
				}
				v.status = fmt.Sprintf("pull %s %s: %s", p.Engine, p.Stage, detail)
				if p.Engine == "vllm" {
					v.pullActive, v.pullModel, v.pullOpID = false, "", ""
				}
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

func (v *enginesView) CapturingInput() bool { return v.pulling || v.selecting }

func (v *enginesView) handleKey(msg tea.KeyMsg) tea.Cmd {
	if v.pulling || v.selecting {
		switch msg.String() {
		case "enter":
			if v.selecting {
				return v.submitVLLMSelection()
			}
			return v.submitPull()
		case "esc":
			v.pulling, v.selecting = false, false
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
	if key.Matches(msg, engSelectKey) {
		return v.beginVLLMSelection()
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
		return v.beginModelAction(action)
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

// beginModelAction opens the model-ID prompt for a model action on the
// selected engine. vLLM acquires exact immutable Hub revisions only, and its
// cancel is bound to the operation the backend reported.
func (v *enginesView) beginModelAction(action string) tea.Cmd {
	engine := v.selectedEngine()
	if engine == "" {
		return nil
	}
	status := v.byName[engine]
	if engine == "vllm" {
		if v.vllmGroupBlocked() {
			v.status = "vllm action held: " + v.vllmGroupBlockReason()
			return nil
		}
		switch action {
		case "cancel_pull":
			return v.cancelVLLMPull()
		case "pull_model":
		default:
			v.status = "vllm supports pull and cancel; select the served model with M"
			return nil
		}
		if !status.Installed || !status.managed() {
			v.status = "vllm model acquisition requires an installed PAIR-managed runtime"
			return nil
		}
		if v.pullActive {
			v.status = "vllm already has an active model acquisition; cancel or wait for it"
			return nil
		}
	}
	if engine == "llamacpp" && !status.managed() {
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
	if engine == "vllm" {
		v.input.Placeholder = "owner/repository@40-character-commit"
	} else {
		v.input.Placeholder = "model name (e.g. llama3.2)"
	}
	if v.showModels && v.models.Cursor() >= 0 && v.models.Cursor() < len(v.modelNames) {
		v.input.SetValue(v.modelNames[v.models.Cursor()])
	}
	v.input.Focus()
	return textinput.Blink
}

func (v *enginesView) beginVLLMSelection() tea.Cmd {
	engine := v.selectedEngine()
	status := v.byName[engine]
	if engine != "vllm" {
		return nil
	}
	if v.vllmGroupBlocked() {
		v.status = "vllm action held: " + v.vllmGroupBlockReason()
		return nil
	}
	if !status.Installed || !status.managed() || status.Running {
		v.status = "model selection requires stopped PAIR-managed vllm"
		return nil
	}
	v.selecting = true
	v.input.SetValue("")
	v.input.Placeholder = "exact retained model ID"
	if v.showModels && v.models.Cursor() >= 0 && v.models.Cursor() < len(v.modelNames) {
		v.input.SetValue(v.modelNames[v.models.Cursor()])
	}
	v.input.Focus()
	return textinput.Blink
}

func (v *enginesView) loadModelsCmd() tea.Cmd {
	engine := v.selectedEngine()
	if engine == "" {
		return nil
	}
	action := "list_models"
	if (engine == "llamacpp" || engine == "vllm") && v.byName[engine].managed() {
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
				Key   string `json:"key"`
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
			if name == "" {
				name = model.Key
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
	v.input.SetValue("")
	engine := v.pullEngine
	if model == "" || engine == "" {
		v.status = "model name required"
		return nil
	}
	if engine == "vllm" && !validVLLMHubModel(model) {
		v.status = "vllm requires owner/repository@40-character-commit"
		return nil
	}
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
	return callWithTimeout(v.client, engineOperationTimeout, "engine:action", params, func(_ *rpc.Message, err error) tea.Msg {
		if action != "pull_model" {
			return engineOpMsg{what: action + " " + model, engine: engine, err: err}
		}
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			return engineOpMsg{what: "pull " + model, engine: engine, err: err}
		}
		return nil
	})
}

func validVLLMHubModel(value string) bool {
	parts := strings.Split(value, "@")
	if len(parts) != 2 || len(parts[1]) != 40 || strings.Count(parts[0], "/") != 1 {
		return false
	}
	for _, part := range strings.Split(parts[0], "/") {
		if part == "" || strings.ContainsFunc(part, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-')
		}) {
			return false
		}
	}
	return !strings.ContainsFunc(parts[1], func(r rune) bool { return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') })
}

func (v *enginesView) cancelVLLMPull() tea.Cmd {
	if !v.pullActive || v.pullModel == "" || v.pullOpID == "" {
		v.status = "no active vllm model acquisition to cancel"
		return nil
	}
	model, operationID := v.pullModel, v.pullOpID
	v.status = "canceling vllm model acquisition..."
	params := map[string]any{"engine": "vllm", "action": "cancel_pull", "params": map[string]string{"model": model, "operationId": operationID}}
	return call(v.client, "engine:action", params, func(msg *rpc.Message, err error) tea.Msg {
		if err == nil {
			var result struct {
				Accepted bool `json:"accepted"`
			}
			if decodeErr := decodeParams(msg.Result, &result); decodeErr != nil {
				err = decodeErr
			} else if !result.Accepted {
				err = errors.New("the acquisition already settled or was replaced")
			}
		}
		return engineOpMsg{what: "cancel pull", engine: "vllm", err: err}
	})
}

func (v *enginesView) submitVLLMSelection() tea.Cmd {
	v.selecting = false
	v.input.Blur()
	model := strings.TrimSpace(v.input.Value())
	v.input.SetValue("")
	if !validServingGroupModel(model) {
		v.status = "exact retained model ID required"
		return nil
	}
	v.status = "selecting vllm model " + model + "..."
	return callWithTimeout(v.client, vllmModelSelectionTimeout, "engine:vllm-select-model", map[string]string{"model": model}, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return engineOpMsg{what: "select model", engine: "vllm", err: err}
		}
		var result struct {
			Engine   string       `json:"engine"`
			Model    string       `json:"model"`
			Selected bool         `json:"selected"`
			Status   engineStatus `json:"status"`
		}
		if err := decodeParams(msg.Result, &result); err != nil {
			return engineOpMsg{what: "select model", engine: "vllm", err: err}
		}
		if result.Engine != "vllm" || result.Model != model || !result.Selected || result.Status.Engine != "vllm" || result.Status.SelectedModel != model {
			return engineOpMsg{what: "select model", engine: "vllm", err: fmt.Errorf("PAIR returned a different model selection")}
		}
		return engineOpMsg{what: "select model", engine: "vllm"}
	})
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
	case key.Matches(msg, engUpdateKey):
		// Only managed vLLM has an update; for other engines the key is unbound.
		if v.selectedEngine() != "vllm" {
			return nil, false
		}
		method, what = "engine:update", "update"
	default:
		return nil, false
	}
	engine := v.selectedEngine()
	if engine == "" {
		return nil, true
	}
	status := v.byName[engine]
	if status.Engine == "vllm" && v.vllmGroupBlocked() {
		v.status = "vllm action held: " + v.vllmGroupBlockReason()
		return nil, true
	}
	if blocked := engineActionBlock(status, what); blocked != "" {
		v.status = blocked
		return nil, true
	}
	if what == "install" && engine == "llamacpp" && !status.installSupported() {
		v.status = "Install unavailable: " + status.InstallReason
		return nil, true
	}
	if engine == "llamacpp" && status.Installed && !status.managed() {
		v.status = "External llama.cpp runtime: lifecycle remains with its owner."
		return nil, true
	}
	v.status = what + " " + engine + "..."
	return callWithTimeout(v.client, engineOperationTimeout, method, map[string]string{"engine": engine}, func(_ *rpc.Message, err error) tea.Msg {
		return engineOpMsg{what: what, engine: engine, err: err}
	}), true
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
		if e.Engine == "llamacpp" && e.Acceleration != "" {
			label += " " + strings.ToUpper(e.Acceleration)
		}
		port := "-"
		if e.Port != 0 {
			port = strconv.Itoa(e.Port)
		}
		owner := "external"
		if e.managed() {
			owner = "PAIR"
		}
		version := e.Version
		if version == "" {
			version = "-"
		}
		rows = append(rows, table.Row{
			label,
			yesNo(e.enabled()),
			yesNo(e.Running),
			yesNo(e.routable()),
			owner,
			version,
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
	if v.selecting {
		out += "\nselect retained vllm model: " + v.input.View()
	}
	if status, ok := v.byName[v.selectedEngine()]; ok && status.Engine == "vllm" && status.SelectedModel != "" {
		out += "\nselected model: " + truncate(status.SelectedModel, clampWidth(v.width-16, 16))
	}
	if status, ok := v.byName[v.selectedEngine()]; ok && status.Engine == "vllm" && !status.Installed && status.InstallSupported != nil && !*status.InstallSupported && status.InstallReason != "" {
		out += "\n" + footerStyle.Render("managed vllm prerequisite: "+status.InstallReason)
	}
	if v.status != "" {
		out += "\n" + footerStyle.Render(v.status)
	}
	return out
}

func (v *enginesView) Help() []key.Binding {
	if status, ok := v.byName[v.selectedEngine()]; ok && status.Engine == "vllm" {
		if v.vllmGroupBlocked() {
			return nil
		}
		var out []key.Binding
		if status.installAllowed() {
			out = append(out, engInstallKey)
		}
		if status.managed() {
			out = append(out, engStartKey, engStopKey, engRestartKey, engUpdateKey, engUninstallKey)
			if status.Installed {
				out = append(out, engModelsKey)
				if v.pullActive {
					out = append(out, engCancelKey)
				} else {
					out = append(out, engPullKey)
				}
			}
			if status.Installed && !status.Running {
				out = append(out, engSelectKey)
			}
		}
		return out
	}
	return []key.Binding{engStartKey, engStopKey, engRestartKey, engInstallKey, engUninstallKey, engModelsKey, engPullKey, engLoadKey, engUnloadKey, engDeleteKey, engCancelKey, engImportKey}
}

func (v *enginesView) vllmGroupBlocked() bool { return !v.groupKnown || v.groupHeld }

func (v *enginesView) vllmGroupBlockReason() string {
	if v.groupReason != "" {
		return v.groupReason
	}
	return "serving-group ownership is not known"
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
