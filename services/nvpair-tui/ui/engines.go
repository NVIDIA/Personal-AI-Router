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
	Engine      string `json:"engine"`
	DisplayName string `json:"display_name"`
	Installed   bool   `json:"installed"`
	Running     bool   `json:"running"`
	Healthy     bool   `json:"healthy"`
	Port        int    `json:"port"`
}

// enginePull identifies one in-flight model download. Several can be
// registered per engine — the engine-manager queues rather than rejects them —
// so the cancel key needs the model, not just the engine.
type enginePull struct {
	engine string
	model  string
}

// enginesView manages local inference engines via the engine-manager
// control plane: an installed/running/healthy table plus lifecycle
// actions, kept live from engine:state-changed and engine:install-progress.
// It can also pull a model (engine:action{action:"pull_model"}) and cancel one
// (engine:cancel-pull), rendering the live engine:pull-progress feed the way
// remote pulls already show.
type enginesView struct {
	client     *rpc.Client
	table      table.Model
	order      []string
	byName     map[string]engineStatus
	status     string
	input      textinput.Model
	pulling    bool
	pullEngine string
	// active is every download this view started, oldest first, so the cancel
	// key can name the newest one on the selected engine.
	active []enginePull

	width, height int
}

type enginesLoadedMsg struct {
	engines []engineStatus
	err     error
}

type engineOpMsg struct {
	what   string
	engine string
	err    error
}

// enginePullDoneMsg retires a download from active once its request settles,
// whether it succeeded or failed. A failed entry left behind stays selectable,
// and the next cancel would target a download that is not running.
type enginePullDoneMsg struct {
	pull enginePull
	err  error
}

var (
	engStartKey      = key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "start"))
	engStopKey       = key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "stop"))
	engRestartKey    = key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "restart"))
	engInstallKey    = key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "install"))
	engUninstallKey  = key.NewBinding(key.WithKeys("u"), key.WithHelp("u", "uninstall"))
	engPullKey       = key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "pull model"))
	engCancelPullKey = key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "cancel pull"))
)

func newEnginesView(client *rpc.Client) *enginesView {
	ti := textinput.New()
	ti.Placeholder = "model name (e.g. llama3.2)"
	v := &enginesView{client: client, byName: map[string]engineStatus{}, input: ti}
	v.table = newTable(nil)
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
}

func (v *enginesView) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
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
		if msg.err != nil {
			v.status = fmt.Sprintf("%s %s failed: %s", msg.what, msg.engine, msg.err.Error())
		} else {
			v.status = fmt.Sprintf("%s %s ok", msg.what, msg.engine)
		}
		return nil

	case NotificationMsg:
		switch msg.Msg.Method {
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
			v.status = fmt.Sprintf("install %s: %s (%d%%)", p.Engine, p.Stage, p.Percent)
		case "engine:pull-progress":
			var p struct {
				Engine  string `json:"engine"`
				Model   string `json:"model"`
				Stage   string `json:"stage"`
				Percent int    `json:"percent"`
				Message string `json:"message"`
			}
			_ = decodeParams(msg.Msg.Params, &p)
			// The engine-manager queues concurrent pulls per engine, so every
			// frame is attributed by model — without it one download's percent
			// would be rendered against another's name.
			what := p.Engine
			if p.Model != "" {
				what = p.Engine + " " + p.Model
			}
			// Terminal stages carry no meaningful percent (success is implicitly
			// 100%; error uses -1), so render them as outcomes rather than a
			// misleading "success (0%)". A late failure that arrives after the
			// synchronous call timed out still surfaces here.
			//
			// They also retire the entry, which is the only thing that does so
			// for a download whose own request outlived callTimeout. That
			// deadline is ignored deliberately, so without this the entry
			// would stay in active for the rest of the session and keep
			// offering a finished download as the cancel target.
			switch p.Stage {
			case "success":
				v.status = fmt.Sprintf("pull %s: done", what)
				v.retire(enginePull{engine: p.Engine, model: p.Model})
			case "error":
				detail := p.Message
				if detail == "" {
					detail = "failed"
				}
				v.status = fmt.Sprintf("pull %s failed: %s", what, detail)
				v.retire(enginePull{engine: p.Engine, model: p.Model})
			case "queued":
				// The engine already has a download running; this one starts
				// when that finishes. There is no percent to report yet.
				v.status = fmt.Sprintf("pull %s: queued", what)
			default:
				v.status = fmt.Sprintf("pull %s: %s (%d%%)", what, p.Stage, p.Percent)
			}
		}
		return nil

	case enginePullDoneMsg:
		v.retire(msg.pull)
		if msg.err != nil {
			v.status = fmt.Sprintf("pull %s %s failed: %s", msg.pull.engine, msg.pull.model, msg.err.Error())
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
	if key.Matches(msg, engPullKey) {
		engine := v.selectedEngine()
		if engine == "" {
			return nil
		}
		v.pullEngine = engine
		v.pulling = true
		v.input.SetValue("")
		v.input.Focus()
		return textinput.Blink
	}
	if key.Matches(msg, engCancelPullKey) {
		return v.cancelPull()
	}
	if cmd, handled := v.handleAction(msg); handled {
		return cmd
	}
	var cmd tea.Cmd
	v.table, cmd = v.table.Update(msg)
	return cmd
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
// notifications; the synchronous response can outlast callTimeout for a large
// model, so a deadline error here is expected and ignored (the progress feed is
// the real signal).
func (v *enginesView) submitPull() tea.Cmd {
	v.pulling = false
	v.input.Blur()
	model := strings.TrimSpace(v.input.Value())
	engine := v.pullEngine
	if model == "" || engine == "" {
		v.status = "model name required"
		return nil
	}
	pull := enginePull{engine: engine, model: model}
	v.active = append(v.active, pull)
	v.status = fmt.Sprintf("pull %s %s...", engine, model)
	params := pullParams(engine, model)
	return call(v.client, "engine:action", params, func(_ *rpc.Message, err error) tea.Msg {
		// A deadline here is the call timeout, not the download's outcome: the
		// pull runs on and the progress feed still reports it, so the entry has
		// to stay cancellable.
		if errors.Is(err, context.DeadlineExceeded) {
			return nil
		}
		return enginePullDoneMsg{pull: pull, err: err}
	})
}

// cancelPull stops the newest download this view started on the selected
// engine. The engine-manager acknowledges only after the transfer has stopped
// and its partial files are cleaned up, so the reply can be slow enough to hit
// the call timeout — which the progress feed then settles.
func (v *enginesView) cancelPull() tea.Cmd {
	engine := v.selectedEngine()
	if engine == "" {
		return nil
	}
	pull, ok := v.newestPull(engine)
	if !ok {
		v.status = "no download in progress on " + engine
		return nil
	}
	v.status = fmt.Sprintf("canceling %s %s...", pull.engine, pull.model)
	params := map[string]string{"engine": pull.engine, "model": pull.model}
	return call(v.client, "engine:cancel-pull", params, v.decodeCancel(pull))
}

// decodeCancel maps a cancel request's outcome onto the update loop.
func (v *enginesView) decodeCancel(pull enginePull) func(*rpc.Message, error) tea.Msg {
	return func(_ *rpc.Message, err error) tea.Msg {
		// A deadline here is the call timeout, not the cancel's outcome, the
		// same way it is for submitPull. Retiring the entry on it would drop
		// the only cancel target for a download that is still running, so a
		// second press would report nothing active on the engine while the
		// transfer carried on. Leave it and let the pull's own terminal
		// progress frame retire it.
		if errors.Is(err, context.DeadlineExceeded) {
			return nil
		}
		if err != nil {
			return engineOpMsg{what: "cancel " + pull.model, engine: pull.engine, err: err}
		}
		return enginePullDoneMsg{pull: pull}
	}
}

func (v *enginesView) newestPull(engine string) (enginePull, bool) {
	for i := len(v.active) - 1; i >= 0; i-- {
		if v.active[i].engine == engine {
			return v.active[i], true
		}
	}
	return enginePull{}, false
}

func (v *enginesView) retire(pull enginePull) {
	for i, active := range v.active {
		if active == pull {
			v.active = append(v.active[:i], v.active[i+1:]...)
			return
		}
	}
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
	v.status = what + " " + engine + "..."
	return call(v.client, method, map[string]string{"engine": engine}, func(_ *rpc.Message, err error) tea.Msg {
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
	if v.pulling {
		out += "\npull model: " + v.input.View()
	}
	if v.status != "" {
		out += "\n" + footerStyle.Render(v.status)
	}
	return out
}

func (v *enginesView) Help() []key.Binding {
	return []key.Binding{engStartKey, engStopKey, engRestartKey, engInstallKey, engUninstallKey, engPullKey, engCancelPullKey}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
